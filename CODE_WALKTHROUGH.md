# Code walkthrough

This guide follows one request through the Go rate limiter, then connects that path to the Redis script, tests, and local deployment. It describes the implementation in this repository—not a generic rate limiter.

For the system diagram, algorithm rationale, Redis keys, persistence behavior, and concurrency guarantees, see [ARCHITECTURE.md](./ARCHITECTURE.md). For the quickest setup and API reference, see [README.md](./README.md).

## 1. Find the main pieces

| File | Responsibility |
| --- | --- |
| [`main.go`](./main.go) | Data types, startup, HTTP routes and handlers, validation, Redis key construction, response conversion |
| [`redis_script.go`](./redis_script.go) | Atomic policy deletion and quota evaluation implemented as Redis Lua scripts |
| [`main_test.go`](./main_test.go) | Policy/identity validation and Redis-backed algorithm/concurrency tests |
| [`integration_test.go`](./integration_test.go) | HTTP endpoint tests and evaluations through two independent API servers |
| [`docker-compose.yml`](./docker-compose.yml) | Redis, two API instances, and Nginx for the local multi-instance demo |
| [`.github/workflows/ci.yml`](./.github/workflows/ci.yml) | Redis-backed race tests, static analysis, and build on pushes and pull requests |

## 2. Startup: from process to HTTP server

Start at `main()` in [`main.go`](./main.go):

1. The process reads `REDIS_URL`, defaulting to `redis://redis:6379/0`, parses it, and creates a Redis client.
2. It pings Redis with a five-second deadline. If Redis is unreachable, startup fails rather than serving requests that cannot be evaluated.
3. It chooses an instance ID from `INSTANCE_ID`, or falls back to the machine hostname. Compose sets this to `api1` or `api2`.
4. It constructs a `service`, which holds the Redis client, admin token, instance ID, and process-local atomic metrics counters.
5. `newHandler` registers the HTTP endpoints. The HTTP server applies read/write timeouts and supports graceful shutdown on `SIGINT`/`SIGTERM`.

`service` does not keep a local quota map or cache. Both API processes consult the same Redis state.

## 3. Follow an evaluation request

The local example policy in the README gives `tenant-42` in namespace `search` two rules:

- `burst`: token bucket, capacity 20, full refill period 10 seconds.
- `sustained`: fixed window, capacity 100, period 60 seconds.

Send a request to `POST /v1/evaluate`:

```sh
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search","cost":1}'
```

### Step 1: route and validate

`newHandler` maps `POST /v1/evaluate` to `service.evaluate`.

`evaluate` uses `decodeJSON` to read one JSON value, reject unknown fields, and cap the body at 1 MiB. It then calls `validateIdentity` to check that identifier and namespace are non-empty and within their size limits. Cost defaults to 1; values outside 1 through 1,000,000 are rejected.

The service currently trusts the identifier supplied in the request. It does not authenticate the caller or verify that the caller owns that identifier. A production-facing deployment must put a trusted identity gateway in front of it or add authentication and tenant binding here.

### Step 2: derive the policy key

`evaluate` calls `runQuotaScript`. The `policyKey` helper hashes `identifier + NUL + namespace` with SHA-256 and creates a Redis key of this form:

```text
ratelimit:{<pair-hash>}:policy
```

The raw tenant identifier is not embedded in the Redis key. The hash tag in braces groups policy and quota keys together for a Redis Cluster slot. The local Compose setup uses standalone Redis, not a Redis Cluster.

### Step 3: run the quota script in Redis

`runQuotaScript` passes the policy key, mode (`consume` for evaluation or `peek` for state inspection), and cost to `quotaScript` in [`redis_script.go`](./redis_script.go).

Redis executes the whole Lua script without interleaving commands from another client. The script:

1. Reads the policy JSON from the policy key.
2. Gets the current time from Redis.
3. Loops over the configured rules, deriving one quota-state key for each rule and reading its stored value and timestamp.
4. Calculates available quota for each rule.
5. Decides whether all rules can pay the requested cost.
6. Writes state for every rule only if the mode is `consume` and every rule permits the request.

If any rule denies the request, it returns a denial without writing any rule's quota state. This avoids charging the burst rule when the sustained rule rejects the same request.

### Step 4: understand the two algorithm branches

Inside `quotaScript`, each rule selects one algorithm:

**Token bucket**

- The saved `value` is the number of tokens; `timestamp` records when that value was saved.
- On evaluation, the script calculates elapsed milliseconds using Redis server time, refills tokens at `capacity / period`, and caps the result at capacity.
- It permits cost `c` when the bucket has at least `c` tokens. On an allowed decision it saves `tokens - c`.
- Token values can be fractional. A read-only state call calculates a current balance but does not persist the calculated refill.

**Fixed window**

- The script rounds the current Redis time down to the start of its aligned window.
- If stored state is from a different window, usage is treated as zero for the current window.
- It permits cost `c` when `current usage + c <= capacity`; allowed decisions store the increased usage.
- The reset is the end of the current window. This simple algorithm can allow a burst spanning two adjacent windows.

These are implemented in one loop so rules use the same Redis timestamp and a single atomic decision. A policy can use a mix of both algorithms, up to eight rules.

### Step 5: convert and return the result

`runQuotaScript` decodes the Lua JSON into Go types and converts per-rule epoch-millisecond reset values into UTC timestamps.

`evaluate` increments process-local counters, sets `X-RateLimit-Remaining` and `X-RateLimit-Reset`, and adds `Retry-After` to denials. It returns HTTP 200 for either a computed allow or deny; check the JSON `allowed` field to know the decision. A missing policy returns 404, invalid input returns 4xx, and Redis errors return 503.

The `Retry-After` value is an estimate, not a reservation. Another request may consume the available quota before a caller retries.

## 4. Read quota without spending it

`GET /v1/state?identifier=tenant-42&namespace=search` calls `runQuotaScript` in `peek` mode:

```sh
curl -sS \
  'http://localhost:8080/v1/state?identifier=tenant-42&namespace=search'
```

The script reads the policy and state and calculates the current quota snapshot, but skips the writes used by `consume` mode. The endpoint reports consumed and remaining values and reset times for each rule. It does not reserve that balance; another evaluation can change it immediately.

## 5. Follow a policy update

Policy endpoints are registered in `newHandler` and implemented by `getRules`, `putRules`, and `deleteRules`:

- All require `Authorization: Bearer <ADMIN_TOKEN>`. `authorizeAdmin` compares the presented bearer value to the configured token.
- `PUT /v1/rules?identifier=...&namespace=...` accepts a `rules` array, validates the full replacement policy, and saves its JSON with Redis `SET`.
- `GET /v1/rules?...` reads and decodes the policy JSON.
- `DELETE /v1/rules?...&name=burst` invokes `deleteRuleScript`. Redis removes the named rule from the policy atomically, or deletes the policy if that was its last rule.
- `DELETE /v1/rules?...` without `name` deletes the whole policy.

Every evaluation reads the current policy from Redis, so instances observe completed policy changes without process restarts or in-memory cache invalidation. A policy update and an evaluation are ordered by Redis: the evaluation uses the policy visible at the point its script runs.

## 6. What Redis stores

For each identifier/namespace pair, Redis holds a policy JSON key:

```text
ratelimit:{<pair-hash>}:policy
```

Each configured rule has a separate hash for its quota value and timestamp:

```text
ratelimit:{<pair-hash>}:state:<name>:<algorithm>:<capacity>:<period-ms>
```

State keys get an expiry when a successful evaluation writes them. The expiry removes inactive quota state; it is not the algorithm's reset time. Policy keys have no TTL. Compose enables Redis AOF and a persistent named volume to support local restart recovery, but this single Redis instance is not a backup or an HA setup. More details are in [ARCHITECTURE.md](./ARCHITECTURE.md#4-redis-keys-persistence-and-expiry).

## 7. Metrics and health

`GET /metrics` formats the service's atomic counters as Prometheus text:

- evaluations attempted by the endpoint,
- allow and deny decisions,
- service/backend errors.

These counters live in each API process, reset on restart, and are not tenant-labelled. Scrape both instances separately and aggregate them in a monitoring system. Metrics are unauthenticated in the local demo.

`GET /healthz` pings Redis and returns 200 if it can reach Redis, otherwise 503. It combines service responsiveness and dependency readiness; a separate process-only liveness endpoint is not implemented.

## 8. Where to see the behavior tested

Run tests that do not need Redis:

```sh
go test ./...
```

Redis-backed tests are skipped unless `REDIS_TEST_URL` is set. For the full suite, point it at a reachable Redis:

```sh
REDIS_TEST_URL=redis://localhost:6379/0 go test -race ./...
```

In [`main_test.go`](./main_test.go):

- policy validation accepts valid mixed rules and rejects invalid algorithms, duplicate names, and out-of-range values;
- the Redis-backed algorithm test checks token refill and that a denial by one rule does not partially spend another;
- the concurrent script test launches simultaneous calls against one capacity and asserts the quota is not overspent.

In [`integration_test.go`](./integration_test.go):

- the HTTP lifecycle test exercises admin authentication, policy creation/read/deletion, evaluation, state inspection, and metrics;
- the cross-instance test creates two independent HTTP servers with separate Redis clients and sends 100 concurrent evaluations; both share Redis, and the test verifies the aggregate allowance and state.

GitHub Actions supplies Redis and runs `go test -race ./...`, `go vet ./...`, and `go build ./...` on pushes and pull requests.

## 9. Start tracing it in the repository

If reading the implementation directly, a useful order is:

1. [`main.go`](./main.go): `main` → `newHandler` → `evaluate`.
2. [`main.go`](./main.go): `runQuotaScript` and `policyKey`.
3. [`redis_script.go`](./redis_script.go): `quotaScript`, from policy lookup through final JSON response.
4. [`main.go`](./main.go): `scriptRuleSignalToPublic` and the response-writing portion of `evaluate`.
5. [`integration_test.go`](./integration_test.go): `TestConcurrentQuotaEvaluationsAcrossInstances`.
6. [`docker-compose.yml`](./docker-compose.yml) and [`nginx.conf`](./nginx.conf): see how the two instances share Redis and how requests reach them locally.

## 10. Important boundaries

- There is no request ID or idempotency key. If Redis commits quota but the HTTP response is lost, a retry can consume quota again.
- Redis scripts serialize requests on the active Redis primary, but do not guarantee rollback after a script runtime error, persistence durability, or correct operation through an unconfigured failover.
- This demo accepts tenant identity from request input, uses a shared admin token, and has unauthenticated state and metrics endpoints. It must not be exposed to an untrusted network as-is.
- Policies live in Redis here. A durable configuration store such as ZooKeeper with versioned policy snapshots is a documented production alternative, not an implemented feature. Shared quota enforcement would still need a coordinated state backend.
- The Compose stack has one Redis node; adding API instances does not remove Redis as a bottleneck or single point of failure.

See [ARCHITECTURE.md](./ARCHITECTURE.md#8-what-this-implementation-does-not-claim) for the full limitations and production follow-up list.
