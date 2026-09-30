#!/usr/bin/env bash
set -euo pipefail

API1_URL="${API1_URL:-http://localhost:8080}"
API2_URL="${API2_URL:-http://localhost:8081}"
ADMIN_TOKEN="${ADMIN_TOKEN:-local-demo-token}"
REQUEST_COUNT=30
IDENTIFIER="concurrent-demo-$(date +%s)-$$"
NAMESPACE="search"
RULES_URL="$API1_URL/v1/rules?identifier=$IDENTIFIER&namespace=$NAMESPACE"
TEMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TEMP_DIR"
}
trap cleanup EXIT

for command in curl jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'Required command not found: %s\n' "$command" >&2
    exit 1
  fi
done

printf '30-request concurrent rate-limit demo\n'
printf 'Tenant: %s | Namespace: %s\n' "$IDENTIFIER" "$NAMESPACE"
printf 'API 1: %s | API 2: %s\n\n' "$API1_URL" "$API2_URL"

printf '%s\n' '1. Check both API instances'
curl -fsS "$API1_URL/healthz" | jq -c .
curl -fsS "$API2_URL/healthz" | jq -c .
printf '\n'

printf '%s\n' '2. Create the burst and sustained rules'
curl -fsS -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  "$RULES_URL" \
  -d '{"rules":[{"name":"burst","algorithm":"token_bucket","capacity":20,"period_seconds":10},{"name":"sustained","algorithm":"fixed_window","capacity":50,"period_seconds":60}]}' | jq .
printf '\n'

printf '%s\n' '3. Launch all 30 evaluations concurrently (odd IDs via API 1; even IDs via API 2)'
pids=()
for request_id in $(seq 1 "$REQUEST_COUNT"); do
  if (( request_id % 2 == 1 )); then
    api_url="$API1_URL"
    instance="api1"
  else
    api_url="$API2_URL"
    instance="api2"
  fi

  (
    printf '%s\n' "$instance" > "$TEMP_DIR/$request_id.instance"
    curl -fsS -X POST \
      -H 'Content-Type: application/json' \
      "$api_url/v1/evaluate" \
      -d "{\"identifier\":\"$IDENTIFIER\",\"namespace\":\"$NAMESPACE\",\"cost\":1}" \
      > "$TEMP_DIR/$request_id.json"
  ) &
  pids+=("$!")
done

failed=0
for pid in "${pids[@]}"; do
  if ! wait "$pid"; then
    failed=1
  fi
done
if (( failed != 0 )); then
  printf 'At least one evaluation request failed. Check that both APIs are reachable.\n' >&2
  exit 1
fi
printf '\n'

printf '%s\n' '4. Per-request outcomes'
printf '%s\n' 'For allowed requests, consumed is the post-request usage; for denied requests, it is unchanged usage.'
allowed_ids=()
denied_ids=()
for request_id in $(seq 1 "$REQUEST_COUNT"); do
  response="$TEMP_DIR/$request_id.json"
  instance="$(<"$TEMP_DIR/$request_id.instance")"
  if jq -e '.allowed == true' "$response" >/dev/null; then
    allowed_ids+=("$request_id")
  else
    denied_ids+=("$request_id")
  fi

  jq -r --arg id "$request_id" --arg instance "$instance" '
    "request=\($id) instance=\($instance) allowed=\(.allowed) remaining=\(.remaining) rules=[" +
    ([.rules[] | "\(.name):consumed=\(.consumed),remaining=\(.remaining)" +
      (if (.retry_after_ms // 0) > 0 then ",blocked,retry_after_ms=\(.retry_after_ms)" else "" end)
    ] | join("; ")) + "]"
  ' "$response"
done
printf '\n'

printf 'Allowed (%d): %s\n' "${#allowed_ids[@]}" "${allowed_ids[*]:-none}"
printf 'Denied  (%d): %s\n' "${#denied_ids[@]}" "${denied_ids[*]:-none}"
printf '\n'

printf '%s\n' '5. Final shared quota state'
curl -fsS "$API1_URL/v1/state?identifier=$IDENTIFIER&namespace=$NAMESPACE" |
  jq .

printf '\nConcurrency demo complete.\n'
