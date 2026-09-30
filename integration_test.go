package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const testAdminToken = "integration-test-token"

type testInstance struct {
	service *service
	server  *httptest.Server
	client  *http.Client
}

func startTestInstance(t *testing.T, redisClient *redis.Client, instanceID string) *testInstance {
	t.Helper()
	s := &service{
		redis:      redisClient,
		adminToken: testAdminToken,
		instanceID: instanceID,
	}
	server := httptest.NewServer(newHandler(s))
	t.Cleanup(server.Close)
	return &testInstance{service: s, server: server, client: server.Client()}
}

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	redisURL := os.Getenv("REDIS_TEST_URL")
	if redisURL == "" {
		t.Skip("REDIS_TEST_URL is not set; Redis-backed integration test skipped")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("Redis integration service is unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Redis client: %v", err)
		}
	})
	return client
}

func uniqueIdentifier(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("integration-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
}

func cleanupPolicy(t *testing.T, client *redis.Client, identifier, namespace string) {
	t.Helper()
	policy := policyKey(identifier, namespace)
	prefix := strings.TrimSuffix(policy, ":policy")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keys, _, err := client.Scan(ctx, 0, prefix+":state:*", 100).Result()
	if err != nil {
		t.Errorf("scan test quota keys: %v", err)
		return
	}
	keys = append(keys, policy)
	if err := client.Del(ctx, keys...).Err(); err != nil {
		t.Errorf("delete test quota keys: %v", err)
	}
}

func requestJSON(t *testing.T, client *http.Client, method, endpoint, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, endpoint, requestBody)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send %s %s: %v", method, endpoint, err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, raw
}

func TestHTTPIntegrationRuleLifecycleAndAlgorithms(t *testing.T) {
	redisClient := testRedis(t)
	identifier, namespace := uniqueIdentifier(t), "catalog"
	cleanupPolicy(t, redisClient, identifier, namespace)
	defer cleanupPolicy(t, redisClient, identifier, namespace)

	instance := startTestInstance(t, redisClient, "http-test")
	baseURL := instance.server.URL
	policyURL := fmt.Sprintf("%s/v1/rules?identifier=%s&namespace=%s",
		baseURL, url.QueryEscape(identifier), url.QueryEscape(namespace))

	unauthorized, _ := requestJSON(t, instance.client, http.MethodPut, policyURL, "", policyRequest{
		Rules: []RateRule{{Name: "burst", Algorithm: "token_bucket", Capacity: 2, PeriodSeconds: 3600}},
	})
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized policy update status = %d, want %d", unauthorized.StatusCode, http.StatusUnauthorized)
	}

	configured, raw := requestJSON(t, instance.client, http.MethodPut, policyURL, testAdminToken, policyRequest{
		Rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 2, PeriodSeconds: 3600},
			{Name: "sustained", Algorithm: "fixed_window", Capacity: 2, PeriodSeconds: 3600},
		},
	})
	if configured.StatusCode != http.StatusOK {
		t.Fatalf("policy update status = %d, want %d: %s", configured.StatusCode, http.StatusOK, raw)
	}

	evaluateURL := baseURL + "/v1/evaluate"
	var decisions []response
	for i := 0; i < 3; i++ {
		resp, body := requestJSON(t, instance.client, http.MethodPost, evaluateURL, "", evaluationRequest{
			Identifier: identifier, Namespace: namespace,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("evaluation status = %d, want %d: %s", resp.StatusCode, http.StatusOK, body)
		}
		var decision response
		if err := json.Unmarshal(body, &decision); err != nil {
			t.Fatalf("decode evaluation: %v", err)
		}
		decisions = append(decisions, decision)
		if i == 2 && resp.Header.Get("Retry-After") == "" {
			t.Fatal("denied evaluation has no Retry-After header")
		}
	}
	if !decisions[0].Allowed || !decisions[1].Allowed || decisions[2].Allowed {
		t.Fatalf("decisions = %v, want allow, allow, deny", []bool{decisions[0].Allowed, decisions[1].Allowed, decisions[2].Allowed})
	}
	if decisions[1].Remaining != 0 || len(decisions[1].Rules) != 2 {
		t.Fatalf("second decision = %+v, want zero remaining and both algorithm signals", decisions[1])
	}

	stateURL := fmt.Sprintf("%s/v1/state?identifier=%s&namespace=%s",
		baseURL, url.QueryEscape(identifier), url.QueryEscape(namespace))
	stateResp, stateBody := requestJSON(t, instance.client, http.MethodGet, stateURL, "", nil)
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d, want %d: %s", stateResp.StatusCode, http.StatusOK, stateBody)
	}
	var state stateResponse
	if err := json.Unmarshal(stateBody, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Identifier != identifier || len(state.Rules) != 2 {
		t.Fatalf("state = %+v, want identifier and two rule signals", state)
	}
	for _, rule := range state.Rules {
		if !closeTo(rule.Consumed, 2, 0.001) || rule.Remaining > 0.001 || rule.ResetAt == "" {
			t.Errorf("state rule = %+v, want consumed 2, remaining 0, and reset timestamp", rule)
		}
	}

	ruleDeleteURL := policyURL + "&name=sustained"
	deletedRule, _ := requestJSON(t, instance.client, http.MethodDelete, ruleDeleteURL, testAdminToken, nil)
	if deletedRule.StatusCode != http.StatusNoContent {
		t.Fatalf("rule delete status = %d, want %d", deletedRule.StatusCode, http.StatusNoContent)
	}
	readPolicy, policyBody := requestJSON(t, instance.client, http.MethodGet, policyURL, testAdminToken, nil)
	if readPolicy.StatusCode != http.StatusOK {
		t.Fatalf("policy read status = %d, want %d: %s", readPolicy.StatusCode, http.StatusOK, policyBody)
	}
	var stored Policy
	if err := json.Unmarshal(policyBody, &stored); err != nil {
		t.Fatalf("decode stored policy: %v", err)
	}
	if len(stored.Rules) != 1 || stored.Rules[0].Name != "burst" {
		t.Fatalf("stored rules = %+v, want only burst", stored.Rules)
	}

	metrics, metricsBody := requestJSON(t, instance.client, http.MethodGet, baseURL+"/metrics", "", nil)
	if metrics.StatusCode != http.StatusOK || !strings.Contains(string(metricsBody), `ratelimiter_decisions_total{result="deny"} 1`) {
		t.Fatalf("metrics response status=%d body=%s", metrics.StatusCode, metricsBody)
	}
}

func TestConcurrentQuotaEvaluationsAcrossInstances(t *testing.T) {
	redisClient := testRedis(t)
	identifier, namespace := uniqueIdentifier(t), "concurrent"
	cleanupPolicy(t, redisClient, identifier, namespace)
	defer cleanupPolicy(t, redisClient, identifier, namespace)

	instance1 := startTestInstance(t, redisClient, "instance-one")
	instance2 := startTestInstance(t, testRedis(t), "instance-two")
	policyURL := fmt.Sprintf("%s/v1/rules?identifier=%s&namespace=%s",
		instance1.server.URL, url.QueryEscape(identifier), url.QueryEscape(namespace))
	configured, body := requestJSON(t, instance1.client, http.MethodPut, policyURL, testAdminToken, policyRequest{
		Rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 17, PeriodSeconds: 3600},
			{Name: "sustained", Algorithm: "fixed_window", Capacity: 23, PeriodSeconds: 3600},
		},
	})
	if configured.StatusCode != http.StatusOK {
		t.Fatalf("policy update status = %d, want %d: %s", configured.StatusCode, http.StatusOK, body)
	}

	const requests = 100
	var wait sync.WaitGroup
	allowed := make(chan bool, requests)
	errorsFound := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			instance := instance1
			if index%2 == 1 {
				instance = instance2
			}
			resp, err := instance.client.Post(instance.server.URL+"/v1/evaluate", "application/json",
				strings.NewReader(fmt.Sprintf(`{"identifier":%q,"namespace":%q}`, identifier, namespace)))
			if err != nil {
				errorsFound <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				result, _ := io.ReadAll(resp.Body)
				errorsFound <- fmt.Errorf("evaluation status=%d body=%s", resp.StatusCode, result)
				return
			}
			var decision response
			if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
				errorsFound <- err
				return
			}
			allowed <- decision.Allowed
			if index%2 == 0 && decision.InstanceID != "instance-one" {
				errorsFound <- fmt.Errorf("instance one request handled by %q", decision.InstanceID)
			}
			if index%2 == 1 && decision.InstanceID != "instance-two" {
				errorsFound <- fmt.Errorf("instance two request handled by %q", decision.InstanceID)
			}
		}(i)
	}
	wait.Wait()
	close(allowed)
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	allowedCount := 0
	for decision := range allowed {
		if decision {
			allowedCount++
		}
	}
	if allowedCount != 17 {
		t.Fatalf("allowed requests = %d, want exactly 17 under concurrent contention", allowedCount)
	}

	stateURL := fmt.Sprintf("%s/v1/state?identifier=%s&namespace=%s",
		instance2.server.URL, url.QueryEscape(identifier), url.QueryEscape(namespace))
	stateResp, stateBody := requestJSON(t, instance2.client, http.MethodGet, stateURL, "", nil)
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("shared state status = %d, want %d: %s", stateResp.StatusCode, http.StatusOK, stateBody)
	}
	var state stateResponse
	if err := json.Unmarshal(stateBody, &state); err != nil {
		t.Fatalf("decode shared state: %v", err)
	}
	if len(state.Rules) != 2 || !closeTo(state.Rules[0].Consumed, 17, 0.01) || state.Rules[0].Remaining > 0.01 ||
		state.Rules[1].Consumed != 17 || state.Rules[1].Remaining != 6 {
		t.Fatalf("shared quota state = %+v, want token bucket 17/17 and window 17/23 consumed", state.Rules)
	}
}
