package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidatePolicyAcceptsBurstAndSustainedRules(t *testing.T) {
	policy := Policy{Rules: []RateRule{
		{Name: "burst", Algorithm: "token_bucket", Capacity: 10, PeriodSeconds: 1},
		{Name: "sustained", Algorithm: "fixed_window", Capacity: 100, PeriodSeconds: 60},
	}}
	if err := validatePolicy(policy); err != nil {
		t.Fatalf("validatePolicy() error = %v", err)
	}
}

func TestValidatePolicyRejectsInvalidRules(t *testing.T) {
	tests := []struct {
		name  string
		rules []RateRule
	}{
		{name: "no rules"},
		{name: "duplicate names", rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 10, PeriodSeconds: 1},
			{Name: "burst", Algorithm: "fixed_window", Capacity: 100, PeriodSeconds: 60},
		}},
		{name: "unknown algorithm", rules: []RateRule{
			{Name: "burst", Algorithm: "sliding_window", Capacity: 10, PeriodSeconds: 1},
		}},
		{name: "invalid capacity", rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 0, PeriodSeconds: 1},
		}},
		{name: "invalid period", rules: []RateRule{
			{Name: "burst", Algorithm: "fixed_window", Capacity: 10, PeriodSeconds: 86_401},
		}},
		{name: "invalid name", rules: []RateRule{
			{Name: "burst:unsafe", Algorithm: "token_bucket", Capacity: 10, PeriodSeconds: 1},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePolicy(Policy{Rules: test.rules}); err == nil {
				t.Fatal("validatePolicy() error = nil, want error")
			}
		})
	}
}

func TestValidateIdentity(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		namespace  string
		wantError  bool
	}{
		{name: "valid", identifier: "tenant-42", namespace: "search"},
		{name: "missing identifier", namespace: "search", wantError: true},
		{name: "missing namespace", identifier: "tenant-42", wantError: true},
		{name: "oversized identifier", identifier: string(make([]byte, 257)), namespace: "search", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateIdentity(test.identifier, test.namespace)
			if (err != nil) != test.wantError {
				t.Fatalf("validateIdentity() error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}

func TestQuotaAlgorithmsAndAtomicMultiRuleDecision(t *testing.T) {
	redisClient := testRedis(t)
	identifier, namespace := uniqueIdentifier(t), "algorithm-unit"
	cleanupPolicy(t, redisClient, identifier, namespace)
	defer cleanupPolicy(t, redisClient, identifier, namespace)

	policy := Policy{
		Identifier: identifier,
		Namespace:  namespace,
		Rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 3, PeriodSeconds: 3600},
			{Name: "sustained", Algorithm: "fixed_window", Capacity: 1, PeriodSeconds: 3600},
		},
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := redisClient.Set(ctx, policyKey(identifier, namespace), raw, 0).Err(); err != nil {
		t.Fatalf("store policy: %v", err)
	}

	limiter := &service{redis: redisClient}
	first, err := limiter.runQuotaScript(ctx, identifier, namespace, 1, true)
	if err != nil {
		t.Fatalf("first quota evaluation: %v", err)
	}
	if !first.Allowed || !closeTo(first.Rules[0].Remaining, 2, 0.001) || first.Rules[1].Remaining != 0 {
		t.Fatalf("first decision = %+v, want allowed, token bucket remaining 2, fixed window remaining 0", first)
	}

	second, err := limiter.runQuotaScript(ctx, identifier, namespace, 1, true)
	if err != nil {
		t.Fatalf("second quota evaluation: %v", err)
	}
	if second.Allowed || !closeTo(second.Rules[0].Remaining, 2, 0.001) || second.Rules[1].Remaining != 0 {
		t.Fatalf("second decision = %+v, want denied with no partial burst-rule consumption", second)
	}

	state, err := limiter.runQuotaScript(ctx, identifier, namespace, 0, false)
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if !closeTo(state.Rules[0].Consumed, 1, 0.001) || !closeTo(state.Rules[0].Remaining, 2, 0.001) ||
		state.Rules[1].Consumed != 1 || state.Rules[1].Remaining != 0 {
		t.Fatalf("quota state = %+v, want one token spent and one fixed-window request spent", state.Rules)
	}

	burstStateKey := fmt.Sprintf("%s:state:burst:token_bucket:3:3600000",
		strings.TrimSuffix(policyKey(identifier, namespace), ":policy"))
	refillTimestamp := time.Now().Add(-30 * time.Minute).UnixMilli()
	if err := redisClient.HSet(ctx, burstStateKey, "value", 0, "timestamp", refillTimestamp).Err(); err != nil {
		t.Fatalf("seed partially depleted token bucket: %v", err)
	}
	refilled, err := limiter.runQuotaScript(ctx, identifier, namespace, 0, false)
	if err != nil {
		t.Fatalf("read refilled token bucket: %v", err)
	}
	if !closeTo(refilled.Rules[0].Remaining, 1.5, 0.01) {
		t.Fatalf("refilled token bucket remaining = %v, want approximately 1.5", refilled.Rules[0].Remaining)
	}
}

func TestConcurrentQuotaScriptUnit(t *testing.T) {
	redisClient := testRedis(t)
	identifier, namespace := uniqueIdentifier(t), "concurrent-unit"
	cleanupPolicy(t, redisClient, identifier, namespace)
	defer cleanupPolicy(t, redisClient, identifier, namespace)

	policy := Policy{
		Identifier: identifier,
		Namespace:  namespace,
		Rules: []RateRule{
			{Name: "burst", Algorithm: "token_bucket", Capacity: 9, PeriodSeconds: 86_400},
		},
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := redisClient.Set(ctx, policyKey(identifier, namespace), raw, 0).Err(); err != nil {
		t.Fatalf("store policy: %v", err)
	}

	limiter := &service{redis: redisClient}
	const attempts = 64
	results := make(chan bool, attempts)
	errorsFound := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := limiter.runQuotaScript(ctx, identifier, namespace, 1, true)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result.Allowed
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	allowed := 0
	for decision := range results {
		if decision {
			allowed++
		}
	}
	if allowed != 9 {
		t.Fatalf("concurrent script allows = %d, want exactly 9 against capacity 9", allowed)
	}
}

func closeTo(value, want, tolerance float64) bool {
	return math.Abs(value-want) <= tolerance
}
