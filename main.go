package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxRequestBody = 1 << 20

var ruleNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)
var errCostExceedsCapacity = errors.New("cost exceeds configured rule capacity")

type RateRule struct {
	Name          string `json:"name"`
	Algorithm     string `json:"algorithm"`
	Capacity      int64  `json:"capacity"`
	PeriodSeconds int64  `json:"period_seconds"`
}

type Policy struct {
	Identifier string     `json:"identifier"`
	Namespace  string     `json:"namespace"`
	Rules      []RateRule `json:"rules"`
}

type policyRequest struct {
	Rules []RateRule `json:"rules"`
}

type evaluationRequest struct {
	Identifier string `json:"identifier"`
	Namespace  string `json:"namespace"`
	Cost       int64  `json:"cost,omitempty"`
}

type ruleSignal struct {
	Name         string  `json:"name"`
	Algorithm    string  `json:"algorithm"`
	Capacity     int64   `json:"capacity"`
	Consumed     float64 `json:"consumed"`
	Remaining    float64 `json:"remaining"`
	ResetAt      string  `json:"reset_at"`
	RetryAfterMS int64   `json:"retry_after_ms,omitempty"`
}

type scriptResult struct {
	Allowed      bool               `json:"allowed"`
	Remaining    float64            `json:"remaining"`
	ResetAtMS    int64              `json:"reset_at_ms"`
	RetryAfterMS int64              `json:"retry_after_ms"`
	Rules        []scriptRuleSignal `json:"rules"`
}

type quotaResult struct {
	Allowed      bool
	Remaining    float64
	ResetAtMS    int64
	RetryAfterMS int64
	Rules        []ruleSignal
}

type scriptRuleSignal struct {
	Name         string  `json:"name"`
	Algorithm    string  `json:"algorithm"`
	Capacity     int64   `json:"capacity"`
	Consumed     float64 `json:"consumed"`
	Remaining    float64 `json:"remaining"`
	ResetAtMS    int64   `json:"reset_at_ms"`
	RetryAfterMS int64   `json:"retry_after_ms"`
}

type response struct {
	Allowed      bool         `json:"allowed"`
	Remaining    float64      `json:"remaining"`
	ResetAt      string       `json:"reset_at,omitempty"`
	RetryAfterMS int64        `json:"retry_after_ms,omitempty"`
	InstanceID   string       `json:"instance_id,omitempty"`
	Rules        []ruleSignal `json:"rules,omitempty"`
}

type stateResponse struct {
	Identifier string       `json:"identifier"`
	Namespace  string       `json:"namespace"`
	Remaining  float64      `json:"remaining"`
	ResetAt    string       `json:"reset_at"`
	InstanceID string       `json:"instance_id"`
	Rules      []ruleSignal `json:"rules"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type service struct {
	redis      *redis.Client
	adminToken string
	instanceID string
	evaluated  atomic.Uint64
	allowed    atomic.Uint64
	denied     atomic.Uint64
	errors     atomic.Uint64
}

func main() {
	redisURL := env("REDIS_URL", "redis://redis:6379/0")
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("parse REDIS_URL: %v", err)
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Fatalf("connect to Redis: %v", err)
	}

	instanceID, err := os.Hostname()
	if err != nil {
		log.Fatalf("get hostname: %v", err)
	}
	if configured := os.Getenv("INSTANCE_ID"); configured != "" {
		instanceID = configured
	}

	s := &service{
		redis:      client,
		adminToken: os.Getenv("ADMIN_TOKEN"),
		instanceID: instanceID,
	}
	server := &http.Server{
		Addr:              env("HTTP_ADDR", ":8080"),
		Handler:           newHandler(s),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("instance=%s listening on %s", instanceID, server.Addr)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(shutdown)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server: %v", err)
		}
	case <-shutdown:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown: %v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("close HTTP server: %v", closeErr)
			}
		}
		if err := <-serverErrors; !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server after shutdown: %v", err)
		}
	}
}

func newHandler(s *service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("POST /v1/evaluate", s.evaluate)
	mux.HandleFunc("GET /v1/state", s.state)
	mux.HandleFunc("GET /v1/rules", s.getRules)
	mux.HandleFunc("PUT /v1/rules", s.putRules)
	mux.HandleFunc("DELETE /v1/rules", s.deleteRules)
	return withSecurityHeaders(mux)
}

func (s *service) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.redis.Ping(ctx).Err(); err != nil {
		s.errors.Add(1)
		writeError(w, http.StatusServiceUnavailable, "redis unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "instance_id": s.instanceID})
}

func (s *service) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP ratelimiter_evaluations_total Number of quota evaluation requests.\n# TYPE ratelimiter_evaluations_total counter\nratelimiter_evaluations_total %d\n", s.evaluated.Load())
	fmt.Fprintf(w, "# HELP ratelimiter_decisions_total Quota decisions by result.\n# TYPE ratelimiter_decisions_total counter\nratelimiter_decisions_total{result=\"allow\"} %d\nratelimiter_decisions_total{result=\"deny\"} %d\n", s.allowed.Load(), s.denied.Load())
	fmt.Fprintf(w, "# HELP ratelimiter_errors_total Redis and service errors.\n# TYPE ratelimiter_errors_total counter\nratelimiter_errors_total %d\n", s.errors.Load())
}

func (s *service) evaluate(w http.ResponseWriter, r *http.Request) {
	var req evaluationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateIdentity(req.Identifier, req.Namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Cost == 0 {
		req.Cost = 1
	}
	if req.Cost < 1 || req.Cost > 1_000_000 {
		writeError(w, http.StatusBadRequest, "cost must be between 1 and 1000000")
		return
	}

	result, err := s.runQuotaScript(r.Context(), req.Identifier, req.Namespace, req.Cost, true)
	s.evaluated.Add(1)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			writeError(w, http.StatusNotFound, "no rate-limit policy configured")
			return
		}
		if errors.Is(err, errCostExceedsCapacity) {
			writeError(w, http.StatusBadRequest, "cost exceeds a configured rule capacity")
			return
		}
		s.errors.Add(1)
		log.Printf("evaluate quota: %v", err)
		writeError(w, http.StatusServiceUnavailable, "quota backend unavailable")
		return
	}
	if result.Allowed {
		s.allowed.Add(1)
	} else {
		s.denied.Add(1)
	}
	remaining := math.Floor(result.Remaining)
	rules := floorRuleRemaining(result.Rules)
	w.Header().Set("X-RateLimit-Remaining", formatNumber(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(result.ResetAtMS/1000, 10))
	if !result.Allowed {
		w.Header().Set("Retry-After", strconv.FormatInt((result.RetryAfterMS+999)/1000, 10))
	}
	writeJSON(w, http.StatusOK, response{
		Allowed:      result.Allowed,
		Remaining:    remaining,
		ResetAt:      time.UnixMilli(result.ResetAtMS).UTC().Format(time.RFC3339Nano),
		RetryAfterMS: result.RetryAfterMS,
		InstanceID:   s.instanceID,
		Rules:        rules,
	})
}

func (s *service) state(w http.ResponseWriter, r *http.Request) {
	identifier, namespace := r.URL.Query().Get("identifier"), r.URL.Query().Get("namespace")
	if err := validateIdentity(identifier, namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.runQuotaScript(r.Context(), identifier, namespace, 0, false)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			writeError(w, http.StatusNotFound, "no rate-limit policy configured")
			return
		}
		s.errors.Add(1)
		log.Printf("read quota state: %v", err)
		writeError(w, http.StatusServiceUnavailable, "quota backend unavailable")
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{
		Identifier: identifier,
		Namespace:  namespace,
		Remaining:  math.Floor(result.Remaining),
		ResetAt:    time.UnixMilli(result.ResetAtMS).UTC().Format(time.RFC3339Nano),
		InstanceID: s.instanceID,
		Rules:      floorRuleRemaining(result.Rules),
	})
}

func floorRuleRemaining(rules []ruleSignal) []ruleSignal {
	floored := make([]ruleSignal, len(rules))
	for i, rule := range rules {
		rule.Remaining = math.Floor(rule.Remaining)
		floored[i] = rule
	}
	return floored
}

func (s *service) getRules(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	identifier, namespace := r.URL.Query().Get("identifier"), r.URL.Query().Get("namespace")
	if err := validateIdentity(identifier, namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := s.redis.Get(r.Context(), policyKey(identifier, namespace)).Bytes()
	if errors.Is(err, redis.Nil) {
		writeError(w, http.StatusNotFound, "no rate-limit policy configured")
		return
	}
	if err != nil {
		s.errors.Add(1)
		log.Printf("get rate-limit policy: %v", err)
		writeError(w, http.StatusServiceUnavailable, "configuration backend unavailable")
		return
	}
	var policy Policy
	if err := json.Unmarshal(raw, &policy); err != nil {
		s.errors.Add(1)
		writeError(w, http.StatusInternalServerError, "stored rate-limit policy is invalid")
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *service) putRules(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	identifier, namespace := r.URL.Query().Get("identifier"), r.URL.Query().Get("namespace")
	if err := validateIdentity(identifier, namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request policyRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy := Policy{Identifier: identifier, Namespace: namespace, Rules: request.Rules}
	if err := validatePolicy(policy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		s.errors.Add(1)
		writeError(w, http.StatusInternalServerError, "could not encode rate-limit policy")
		return
	}
	if err := s.redis.Set(r.Context(), policyKey(identifier, namespace), raw, 0).Err(); err != nil {
		s.errors.Add(1)
		log.Printf("store rate-limit policy: %v", err)
		writeError(w, http.StatusServiceUnavailable, "configuration backend unavailable")
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *service) deleteRules(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	identifier, namespace := r.URL.Query().Get("identifier"), r.URL.Query().Get("namespace")
	if err := validateIdentity(identifier, namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if name := r.URL.Query().Get("name"); name != "" {
		if !ruleNamePattern.MatchString(name) {
			writeError(w, http.StatusBadRequest, "invalid rule name")
			return
		}
		result, err := deleteRuleScript.Run(r.Context(), s.redis, []string{policyKey(identifier, namespace)}, name).Text()
		if err != nil {
			s.errors.Add(1)
			log.Printf("delete rate-limit rule: %v", err)
			writeError(w, http.StatusServiceUnavailable, "configuration backend unavailable")
			return
		}
		switch result {
		case "NO_POLICY":
			writeError(w, http.StatusNotFound, "no rate-limit policy configured")
		case "NO_RULE":
			writeError(w, http.StatusNotFound, "rate-limit rule not found")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	deleted, err := s.redis.Del(r.Context(), policyKey(identifier, namespace)).Result()
	if err != nil {
		s.errors.Add(1)
		log.Printf("delete rate-limit policy: %v", err)
		writeError(w, http.StatusServiceUnavailable, "configuration backend unavailable")
		return
	}
	if deleted == 0 {
		writeError(w, http.StatusNotFound, "no rate-limit policy configured")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) runQuotaScript(ctx context.Context, identifier, namespace string, cost int64, consume bool) (quotaResult, error) {
	mode := "peek"
	if consume {
		mode = "consume"
	}
	raw, err := quotaScript.Run(ctx, s.redis, []string{policyKey(identifier, namespace)}, mode, cost).Text()
	if err != nil {
		return quotaResult{}, err
	}
	if raw == "NO_POLICY" {
		return quotaResult{}, redis.Nil
	}
	if raw == "COST_EXCEEDS_CAPACITY" {
		return quotaResult{}, errCostExceedsCapacity
	}
	var result scriptResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return quotaResult{}, fmt.Errorf("decode quota script response: %w", err)
	}
	publicResult := quotaResult{
		Allowed:      result.Allowed,
		Remaining:    result.Remaining,
		ResetAtMS:    result.ResetAtMS,
		RetryAfterMS: result.RetryAfterMS,
		Rules:        make([]ruleSignal, 0, len(result.Rules)),
	}
	for _, rule := range result.Rules {
		publicResult.Rules = append(publicResult.Rules, scriptRuleSignalToPublic(rule))
	}
	return publicResult, nil
}

func scriptRuleSignalToPublic(rule scriptRuleSignal) ruleSignal {
	return ruleSignal{
		Name:         rule.Name,
		Algorithm:    rule.Algorithm,
		Capacity:     rule.Capacity,
		Consumed:     rule.Consumed,
		Remaining:    rule.Remaining,
		ResetAt:      time.UnixMilli(rule.ResetAtMS).UTC().Format(time.RFC3339Nano),
		RetryAfterMS: rule.RetryAfterMS,
	}
}

func (s *service) authorizeAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.adminToken == "" {
		writeError(w, http.StatusServiceUnavailable, "configuration API disabled: ADMIN_TOKEN is required")
		return false
	}
	if r.Header.Get("Authorization") != "Bearer "+s.adminToken {
		writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
		return false
	}
	return true
}

func validateIdentity(identifier, namespace string) error {
	if strings.TrimSpace(identifier) == "" || len(identifier) > 256 {
		return errors.New("identifier is required and must be at most 256 characters")
	}
	if strings.TrimSpace(namespace) == "" || len(namespace) > 128 {
		return errors.New("namespace is required and must be at most 128 characters")
	}
	return nil
}

func validatePolicy(policy Policy) error {
	if len(policy.Rules) == 0 || len(policy.Rules) > 8 {
		return errors.New("rules must contain between 1 and 8 entries")
	}
	seen := make(map[string]struct{}, len(policy.Rules))
	for _, rule := range policy.Rules {
		if !ruleNamePattern.MatchString(rule.Name) {
			return fmt.Errorf("invalid rule name %q: use 1-32 letters, digits, underscores, or hyphens", rule.Name)
		}
		if _, ok := seen[rule.Name]; ok {
			return fmt.Errorf("duplicate rule name %q", rule.Name)
		}
		seen[rule.Name] = struct{}{}
		if rule.Algorithm != "token_bucket" && rule.Algorithm != "fixed_window" {
			return fmt.Errorf("rule %q algorithm must be token_bucket or fixed_window", rule.Name)
		}
		if rule.Capacity < 1 || rule.Capacity > 1_000_000 {
			return fmt.Errorf("rule %q capacity must be between 1 and 1000000", rule.Name)
		}
		if rule.PeriodSeconds < 1 || rule.PeriodSeconds > 86_400 {
			return fmt.Errorf("rule %q period_seconds must be between 1 and 86400", rule.Name)
		}
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func policyKey(identifier, namespace string) string {
	digest := sha256.Sum256([]byte(identifier + "\x00" + namespace))
	return "ratelimit:{" + hex.EncodeToString(digest[:]) + "}:policy"
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
