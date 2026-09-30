#!/usr/bin/env bash
set -euo pipefail

API1_URL="${API1_URL:-http://localhost:8080}"
API2_URL="${API2_URL:-http://localhost:8081}"
ADMIN_TOKEN="${ADMIN_TOKEN:-local-demo-token}"
IDENTIFIER="demo-$(date +%s)"
NAMESPACE="search"
RULES_URL="$API1_URL/v1/rules?identifier=$IDENTIFIER&namespace=$NAMESPACE"
STATE_URL="$API2_URL/v1/state?identifier=$IDENTIFIER&namespace=$NAMESPACE"

for command in curl jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'Required command not found: %s\n' "$command" >&2
    exit 1
  fi
done

printf 'Rate limiter end-to-end demo\n'
printf 'Tenant: %s | Namespace: %s\n\n' "$IDENTIFIER" "$NAMESPACE"

printf '%s\n' '1. Check both API instances'
curl -fsS "$API1_URL/healthz" | jq .
curl -fsS "$API2_URL/healthz" | jq .
printf '\n'

printf '%s\n' '2. Create a two-request fixed-window policy through API instance 1'
curl -fsS -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  "$RULES_URL" \
  -d '{"rules":[{"name":"demo-limit","algorithm":"fixed_window","capacity":2,"period_seconds":60}]}' | jq .
printf '\n'

printf '%s\n' '3. Read the policy through API instance 2 (runtime config is shared)'
curl -fsS \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$API2_URL/v1/rules?identifier=$IDENTIFIER&namespace=$NAMESPACE" | jq .
printf '\n'

evaluate() {
  local api_url="$1"
  local expected="$2"
  local result

  result="$(curl -fsS -X POST \
    -H 'Content-Type: application/json' \
    "$api_url/v1/evaluate" \
    -d "{\"identifier\":\"$IDENTIFIER\",\"namespace\":\"$NAMESPACE\",\"cost\":1}")"
  printf '%s\n' "$result" | jq .
  if ! printf '%s\n' "$result" | jq -e --argjson expected "$expected" '.allowed == $expected' >/dev/null; then
    printf 'Unexpected decision; expected allowed=%s\n' "$expected" >&2
    exit 1
  fi
}

printf '%s\n' '4. Evaluation 1 through API instance 1 (expected allow)'
evaluate "$API1_URL" true
printf '\n'

printf '%s\n' '5. Evaluation 2 through API instance 2 (expected allow; shared Redis quota)'
evaluate "$API2_URL" true
printf '\n'

printf '%s\n' '6. Evaluation 3 through API instance 1 (expected deny; quota exhausted)'
evaluate "$API1_URL" false
printf '\n'

printf '%s\n' '7. Inspect current quota state through API instance 2'
curl -fsS "$STATE_URL" | jq .
printf '\n'

printf '%s\n' '8. Show per-instance Prometheus counters'
printf '%s\n' '--- API instance 1 ---'
curl -fsS "$API1_URL/metrics" | grep -E '^(ratelimiter_evaluations_total|ratelimiter_decisions_total)'
printf '%s\n' '--- API instance 2 ---'
curl -fsS "$API2_URL/metrics" | grep -E '^(ratelimiter_evaluations_total|ratelimiter_decisions_total)'
printf '\nDemo complete: policy CRUD, cross-instance evaluation, denial, quota state, and metrics verified.'
