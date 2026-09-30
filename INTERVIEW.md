# Interview guide

Use this guide to explain and demonstrate the rate limiter. Start with what the service actually guarantees, show the running behavior, and be direct about what is not implemented. This guide describes the Go/Redis project in this repository.

For setup and API commands, see [README.md](./README.md). For a guided tour through the source, see [CODE_WALKTHROUGH.md](./CODE_WALKTHROUGH.md). For the detailed flow diagram and correctness analysis, see [ARCHITECTURE.md](./ARCHITECTURE.md).

## 1. Explain the project in one minute

> “This is a rate-limit decision service. A caller sends an identifier, a resource namespace, and optionally a request cost. The service loads that pair's policy and decides whether all its configured quota rules allow the request.
>
> The demo has two Go API instances sharing Redis. Each evaluation runs one Lua script in Redis to read the policy and all rule states, calculate the decision, and update every rule only if they all allow it. That gives the instances one shared quota instead of one quota per process.
>
> The policy can combine a token bucket for burst control with a fixed window for a periodic sustained limit. The service returns a decision and quota signals; it does not execute the caller's business operation. The local Compose setup demonstrates multiple instances, but Redis is a single-node dependency and this is not a hardened production deployment.”

Avoid saying the limiter is a gateway or that the API authenticates tenants: callers currently supply `identifier` and `namespace`, and evaluation does not authenticate them.

## 2. State assumptions before discussing the design

These choices shape the implementation. If requirements change, revisit the algorithm and failure contract.

| Assumption | Current behavior |
| --- | --- |
| Quota scope | One policy per `(identifier, namespace)` pair |
| Rule composition | A request must pass every rule; an allowed cost is deducted from each |
| Supported algorithms | Token bucket and fixed window, with up to eight rules per policy |
| Request cost | Positive integer; defaults to 1 |
| Decision endpoint | HTTP 200 means the evaluation completed; inspect `allowed` for the decision |
| Redis outage | Fail closed with HTTP 503; never manufacture an allow |
| Retry semantics | Each evaluation is a new attempt; there is no idempotency-key deduplication |
| Policy source | Redis stores policies and quota state in this demo |
| Caller identity | Trusted to an upstream gateway; the service itself does not authenticate evaluators |

Useful clarifying questions in a real design discussion:

- Is the quota per tenant, per API key, per endpoint, or a combination?
- Does the product want a burst allowance, a strict rolling-window limit, or both?
- Can different operations have different costs?
- Should a Redis outage fail open or fail closed?
- Must a retried logical request avoid being charged twice?
- How quickly must policy changes become effective across instances?

## 3. Run a short local demo

From the repository root:

```sh
docker compose up --build
```

The stack exposes Nginx at port 8080, API instance 1 at 8081, and API instance 2 at 8082. Redis is shared inside the Compose network. The default admin token is `local-demo-token`.

Configure a policy through the proxy:

```sh
curl -i -X PUT \
  -H 'Authorization: Bearer local-demo-token' \
  -H 'Content-Type: application/json' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search' \
  -d '{
    "rules": [
      {"name":"burst","algorithm":"token_bucket","capacity":20,"period_seconds":10},
      {"name":"sustained","algorithm":"fixed_window","capacity":100,"period_seconds":60}
    ]
  }'
```

Evaluate a few requests, then query quota state:

```sh
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search","cost":1}'

curl -sS \
  'http://localhost:8080/v1/state?identifier=tenant-42&namespace=search'
```

To demonstrate that two API instances share state, call the instances directly and compare `instance_id`:

```sh
curl -sS -X POST -H 'Content-Type: application/json' \
  http://localhost:8081/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search"}'

curl -sS -X POST -H 'Content-Type: application/json' \
  http://localhost:8082/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search"}'
```

Explain that token buckets refill while the demo is running, so do not promise an exact number of successful calls by manually clicking or curling. The deterministic contention assertion belongs in the automated test, which uses a very slow refill rate.

## 4. Walk through one request

Start at `main()` and `newHandler` in [`main.go`](./main.go), then follow:

1. `service.evaluate` decodes and validates the request.
2. `policyKey` hashes the identifier/namespace pair to derive a policy key.
3. `runQuotaScript` calls `quotaScript` in [`redis_script.go`](./redis_script.go).
4. The Redis script loads policy and quota state, computes each rule, and conditionally writes state.
5. `evaluate` converts the result into HTTP headers and JSON.

The API performs one client-to-Redis script invocation for the decision. The Lua script itself issues multiple Redis commands, but Redis does not interleave other clients' commands in the middle of that script. This is different from saying it is one Redis command internally.

If one rule rejects a request, the script does not charge any rule. If all rules accept, it updates every rule. `GET /v1/state` uses the same script in read-only mode: it returns a snapshot, not a reservation for a later request.

## 5. Explain algorithm selection with an example

For the example policy:

- The token bucket has capacity 20 and replenishes to full over 10 seconds, which is 2 tokens per second. It allows a short burst while controlling the longer-term average.
- The fixed window allows 100 units in each aligned 60-second interval. It is simple to explain and provides a window reset, but can allow a boundary burst across adjacent windows.
- The rules are ANDed. A request costs against both only if both can afford it. If either denies, neither is charged.

Token bucket does **not** mean “at most 20 in every rolling 10-second interval.” If the requirement is a strict rolling window, select a sliding-window algorithm or define an approximation explicitly. Sliding window is not implemented here.

## 6. Practice common design questions

### Why use Redis instead of a Go map?

Each API process would otherwise have its own counter. With two instances and a per-instance capacity of 20, a tenant could potentially spend 40 by reaching both. A shared Redis state means both instances contend on the same quota.

### Why Lua instead of separate GET and SET calls?

If two instances each read one token before either writes, both could allow and overspend. The Lua script groups policy/state reads, the decision, and successful updates into one non-interleaved operation on the Redis primary. A local mutex would only coordinate goroutines in one process, not other API instances.

### What does atomic mean here—and what does it not mean?

It means another Redis client cannot interleave commands in the middle of a script on the active primary. It does not guarantee write rollback if a script errors after writing, persistence to disk before every reply, zero-loss replica failover, or exactly-once behavior from the HTTP caller's point of view.

### What happens if Redis goes down?

The service returns 503 because it cannot safely make a globally shared quota decision. This is fail closed: callers must not proceed as if the request were allowed. The current `/healthz` also pings Redis, so it is a combined dependency/readiness check; there is no separate process-only liveness endpoint.

### What if Redis commits but the response is lost?

The quota may already be consumed even though the caller did not receive the response. Retrying can consume quota again. We return an instance ID, but that is not an idempotency key. Avoid claiming retries are free or deduplicated; a production solution needs a stable client operation ID and an atomic stored result with a retention policy.

### How are policies updated?

An operator calls the authenticated `PUT /v1/rules` API. The policy JSON is stored in Redis, and evaluations read the current policy there, so there is no per-instance in-memory cache to invalidate. This is convenient for the demo but couples policy availability to Redis. ZooKeeper or another durable configuration source with versioned snapshots and update notifications is a possible production alternative; Redis is still needed here for shared quota counters.

### What does state expiry mean?

Quota-state hashes get a TTL to clean up inactive state; the TTL is not the reset time. Token-bucket expiry is delayed long enough that an expired bucket would have refilled to full. Policy keys do not expire. An identical rule recreated before its prior state expires can reuse that state.

### How many Redis connections does each API instance use?

Each API process constructs one concurrency-safe `go-redis` client, which maintains a connection pool. We do not configure pool options explicitly. In the pinned client version, the default base pool size is 10 times `GOMAXPROCS` per client; connections are opened as traffic needs them, not all at startup. Since `MaxActiveConns` is unset, the base size is not a hard maximum. For production, set explicit pool limits based on load testing and monitor pool waits and Redis connection counts.

### What is the Redis durability guarantee?

Compose enables AOF and stores Redis data in a named volume, but leaves Redis's default `appendfsync everysec` policy. Redis may acknowledge a write before it is fsynced, so a host or power failure can lose roughly the most recent second of acknowledged policy or quota updates. This is basic local restart recovery, not zero-loss durability, replication, backup, or HA. A production service needs a stated recovery-point objective plus tested backups and failover.

### How is `retry_after_ms` calculated, and which rule is blocking?

Every blocking rule reports its own retry estimate. A fixed-window rule uses time remaining until the aligned window ends. A token bucket estimates how long it takes to refill the missing tokens: `ceil((cost - available_tokens) * period_ms / capacity)`. The top-level retry is the **maximum** across blocking rules, since all blockers must clear. Zero retry values are omitted from JSON, so the top-level field normally appears on denials only. It is only an estimate—another request can use quota before the retry arrives. Check `rules[].name` and `rules[].retry_after_ms` to see the individual blockers.

### Do two APIs really share state in the tests?

The HTTP contention integration test runs two independent `httptest` API servers with separate Redis clients against the same Redis service and sends concurrent requests to both. It verifies the combined allowed count and then reads shared quota state through an instance. The local Compose demo runs actual separate API containers. Be precise: the integration test demonstrates two API server instances, not two separately launched OS processes.

### Where is the bottleneck?

Different policy keys can be processed independently over time, but a single hot policy is serialized on Redis to preserve exact shared quota. First measure throughput and tail latency. A Redis Cluster or managed Redis can distribute different policy keys across primaries, but one tenant's key remains on one shard. Splitting one tenant's quota would require coordination or accepting approximate limits.

## 7. Be clear about observability and security

- `/metrics` exposes process-local evaluation, allow/deny, and error counters. Scrape each API instance and aggregate externally; counters reset when a process restarts.
- Metrics have no per-tenant labels, avoiding unbounded cardinality. The endpoint is unauthenticated in this demo.
- `/v1/evaluate` and `/v1/state` accept an identifier supplied by the caller and are unauthenticated. The admin policy endpoints use one shared bearer token.
- The public README includes the local demo token. It is not a production secret.
- Put the service behind a trusted identity gateway, restrict metrics to operators, store admin credentials in a secret manager, and use TLS/network controls before exposing it beyond a trusted local environment.

## 8. Tests and what they prove

Run unit checks:

```sh
go test ./...
go vet ./...
go build ./...
```

Redis-backed tests are skipped when `REDIS_TEST_URL` is absent. For the full suite:

```sh
REDIS_TEST_URL=redis://localhost:6379/0 go test -race ./...
```

The GitHub Actions workflow starts Redis and runs the race-enabled test suite, `go vet`, and build on pushes and pull requests.

| Test | Evidence |
| --- | --- |
| Validation tests in [`main_test.go`](./main_test.go) | Accepts valid rules and rejects invalid identities/policies |
| Algorithm test in [`main_test.go`](./main_test.go) | Exercises token refill, fixed-window use, and no partial charge on a denied multi-rule decision |
| Concurrent script test in [`main_test.go`](./main_test.go) | Concurrent quota evaluations do not exceed the configured capacity |
| HTTP lifecycle test in [`integration_test.go`](./integration_test.go) | Exercises admin auth, policy CRUD, evaluation, state inspection, and metrics |
| Cross-instance contention test in [`integration_test.go`](./integration_test.go) | 100 concurrent evaluations across two API servers share a capacity-17 policy; slow refill makes the expected count meaningful |

These tests do not establish Redis failover durability, production throughput, or outage recovery across a real Redis cluster. The local environment may not have Redis available; rely on CI's Redis-backed run when reporting those tests as verified.

## 9. Honest limitations and next steps

The local Compose topology has one Redis node, without authentication, TLS, Sentinel/Cluster failover, or tested backup/restore. It uses AOF plus a named volume for basic local restart recovery, not as a high-availability durability guarantee.

Other production work includes:

- authenticate callers and bind credentials to tenant identity;
- split liveness and readiness checks;
- add idempotency for safe retries and request IDs for correlation;
- use durable/versioned policy management and audit history;
- establish HA Redis, persistence, backups, and a tested failover contract;
- restrict metrics access and add latency histograms/tracing;
- benchmark throughput and tail latency, especially for hot tenant keys.

The strongest claim to make is: **the shared-state decision is atomic with respect to concurrent evaluations on the active Redis primary, and the Redis-backed integration tests exercise that behavior across two API server instances.** Do not claim zero-loss failover or full production hardening.
