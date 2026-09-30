package main

import "github.com/redis/go-redis/v9"

var deleteRuleScript = redis.NewScript(`
local policy_key = KEYS[1]
local policy_raw = redis.call("GET", policy_key)
if not policy_raw then
  return "NO_POLICY"
end

local policy = cjson.decode(policy_raw)
local kept_rules = {}
local found = false
for _, rule in ipairs(policy.rules) do
  if rule.name == ARGV[1] then
    found = true
  else
    kept_rules[#kept_rules + 1] = rule
  end
end

if not found then
  return "NO_RULE"
end
if #kept_rules == 0 then
  redis.call("DEL", policy_key)
else
  policy.rules = kept_rules
  redis.call("SET", policy_key, cjson.encode(policy))
end
return "OK"
`)

var quotaScript = redis.NewScript(`
local policy_key = KEYS[1]
local policy_raw = redis.call("GET", policy_key)
if not policy_raw then
  return "NO_POLICY"
end

local policy = cjson.decode(policy_raw)
local mode = ARGV[1]
local cost = tonumber(ARGV[2])
local time_parts = redis.call("TIME")
local now_ms = tonumber(time_parts[1]) * 1000 + math.floor(tonumber(time_parts[2]) / 1000)
local prefix = string.gsub(policy_key, ":policy$", "")
local allowed = true
local minimum_remaining = nil
local aggregate_reset = nil
local denied_reset = nil
local retry_after_ms = 0
local results = {}
local computed = {}

for _, rule in ipairs(policy.rules) do
  local capacity = tonumber(rule.capacity)
  if mode == "consume" and cost > capacity then
    return "COST_EXCEEDS_CAPACITY"
  end
  local period_ms = tonumber(rule.period_seconds) * 1000
  local state_key = prefix .. ":state:" .. rule.name .. ":" .. rule.algorithm .. ":" .. capacity .. ":" .. period_ms
  local state = redis.call("HMGET", state_key, "value", "timestamp")
  local current_value = tonumber(state[1])
  local timestamp = tonumber(state[2])
  local consumed
  local remaining
  local reset_at
  local retry_ms = 0

  if rule.algorithm == "fixed_window" then
    local window_start = math.floor(now_ms / period_ms) * period_ms
    if timestamp ~= window_start then
      current_value = 0
    end
    current_value = current_value or 0
    consumed = current_value
    remaining = math.max(0, capacity - current_value)
    reset_at = window_start + period_ms
    if mode == "consume" and current_value + cost > capacity then
      allowed = false
      retry_ms = math.max(0, reset_at - now_ms)
    end
    computed[#computed + 1] = {
      key = state_key,
      value = current_value,
      timestamp = window_start,
      expires = period_ms * 2
    }
  else
    local tokens = current_value
    if tokens == nil then
      tokens = capacity
      timestamp = now_ms
    else
      local elapsed = math.max(0, now_ms - (timestamp or now_ms))
      tokens = math.min(capacity, tokens + elapsed * capacity / period_ms)
    end
    consumed = capacity - tokens
    remaining = tokens
    reset_at = now_ms + math.ceil((capacity - tokens) * period_ms / capacity)
    if mode == "consume" and tokens < cost then
      allowed = false
      retry_ms = math.ceil((cost - tokens) * period_ms / capacity)
    end
    computed[#computed + 1] = {
      key = state_key,
      value = tokens,
      timestamp = now_ms,
      expires = math.max(period_ms * 2, 60000)
    }
  end

  minimum_remaining = minimum_remaining and math.min(minimum_remaining, remaining) or remaining
  if mode == "consume" and retry_ms > 0 then
    denied_reset = denied_reset and math.max(denied_reset, now_ms + retry_ms) or now_ms + retry_ms
    retry_after_ms = math.max(retry_after_ms, retry_ms)
  else
    aggregate_reset = aggregate_reset and math.min(aggregate_reset, reset_at) or reset_at
  end
  results[#results + 1] = {
    name = rule.name,
    algorithm = rule.algorithm,
    capacity = capacity,
    consumed = consumed,
    remaining = remaining,
    reset_at_ms = reset_at,
    retry_after_ms = retry_ms
  }
end

if mode == "consume" and allowed then
  for index, rule in ipairs(policy.rules) do
    local state = computed[index]
    local value = state.value
    if rule.algorithm == "fixed_window" then
      value = value + cost
    else
      value = value - cost
    end
    redis.call("HSET", state.key, "value", value, "timestamp", state.timestamp)
    redis.call("PEXPIRE", state.key, state.expires)
    results[index].consumed = results[index].capacity - value
    if rule.algorithm == "fixed_window" then
      results[index].remaining = math.max(0, results[index].capacity - value)
    else
      results[index].remaining = value
      results[index].reset_at_ms = now_ms + math.ceil((results[index].capacity - value) * tonumber(rule.period_seconds) * 1000 / results[index].capacity)
    end
  end
  minimum_remaining = nil
  aggregate_reset = nil
  for _, result in ipairs(results) do
    minimum_remaining = minimum_remaining and math.min(minimum_remaining, result.remaining) or result.remaining
    aggregate_reset = aggregate_reset and math.min(aggregate_reset, result.reset_at_ms) or result.reset_at_ms
  end
end

if mode == "consume" and not allowed then
  aggregate_reset = denied_reset
end

return cjson.encode({
  allowed = (mode == "peek") or allowed,
  remaining = minimum_remaining or 0,
  reset_at_ms = aggregate_reset or now_ms,
  retry_after_ms = retry_after_ms,
  rules = results
})
`)
