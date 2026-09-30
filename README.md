# Local multi-instance rate limiter

A small HTTP rate-limiting service for per-identifier and per-resource quotas across multiple service instances. Both API instances share policy and quota state in Redis; a Redis Lua script evaluates and updates every rule atomically, so concurrent requests cannot spend the same remaining quota twice. See [ARCHITECTURE.md](./ARCHITECTURE.md) for the request flow, contention reasoning, and known limits.

This is a runnable interview/demo baseline, not a claim that the default local deployment is production-hardened. The Compose Redis instance has no authentication or TLS, configuration uses a demo token by default, and there is no HA Redis topology. Use secrets, network isolation, TLS, monitoring, backups, and a highly available Redis deployment before exposing it to untrusted networks.

## Start locally

Requires Docker Compose:

```sh
docker compose up --build
```

The proxy listens on `http://localhost:8080` and distributes requests over `api1` and `api2`. The instances are also directly available at `http://localhost:8081` and `http://localhost:8082`, respectively, for per-instance metrics scraping and testing. Both instances use the same Redis database. Set `ADMIN_TOKEN` in the environment before starting Compose to replace the local demo token:

```sh
ADMIN_TOKEN='replace-with-a-local-secret' docker compose up --build
```

Check service health and the active API instance:

```sh
curl -s http://localhost:8080/healthz
```

## Configure burst and sustained rules

Create or replace a policy with `PUT /v1/rules`. The pair of rules below permits up to 20 requests in a token bucket refilled at 20 tokens per 10 seconds, while also enforcing a sustained fixed-window limit of 100 requests per minute.

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

`capacity` is the maximum number of tokens or fixed-window requests. For token buckets, `period_seconds` is the time to refill an empty bucket to full capacity. For fixed windows, it is the aligned window duration. Each policy can combine either supported algorithm across up to eight named rules.

**Algorithm selection:** token bucket is the burst rule: it starts full and refills continuously, allowing short bursts while smoothing sustained traffic. Fixed window is a straightforward sustained quota with an aligned reset. It can allow a boundary burst across adjacent windows, so combine it with a token bucket if strict burst control matters. Rules compose as AND constraints: a request spends capacity in all rules or none.

Read a policy, delete one named rule, or delete the whole policy with the same identifier and namespace:

```sh
curl -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search'

curl -i -X DELETE -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search&name=burst'

curl -i -X DELETE -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search'
```

Policy changes take effect without restarting the service. `PUT` replaces the complete rules list, so it supports creating and updating rules; use `DELETE` with `name` to remove an individual rule, or without `name` to delete the policy. Updating a rule's algorithm, capacity, or period starts fresh state for that changed rule; unchanged rules retain their state.

## Evaluate and inspect quota

Each successful evaluation responds with an `allowed` decision, aggregate remaining capacity, reset time, instance identifier, and per-rule consumption. A denial includes `retry_after_ms` and a `Retry-After` header. Evaluation responses use HTTP 200 for both allow and deny; a requested cost larger than any configured rule capacity uses 400, other invalid requests use 4xx, and backend failures use 503.

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  http://localhost:8080/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search","cost":1}'

curl -s \
  'http://localhost:8080/v1/state?identifier=tenant-42&namespace=search'
```

`cost` is optional and defaults to 1. State inspection is read-only and returns current consumed and remaining capacity and reset timestamps per rule. Policies are independent for each `(identifier, namespace)` pair.

## Endpoints

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `PUT` | `/v1/rules?identifier=...&namespace=...` | Create or replace policy (admin bearer token) |
| `GET` | `/v1/rules?identifier=...&namespace=...` | Read policy (admin bearer token) |
| `DELETE` | `/v1/rules?identifier=...&namespace=...&name=...` | Delete one rule (omit `name` to delete policy; admin bearer token) |
| `POST` | `/v1/evaluate` | Atomically evaluate and consume quota |
| `GET` | `/v1/state?identifier=...&namespace=...` | Read current quota state without consuming |
| `GET` | `/healthz` | API and Redis health |
| `GET` | `/metrics` | Prometheus text-format counters |

The evaluation and state APIs are intentionally open for this local demo. Place them behind the caller's authenticated gateway before production use. Metrics are process-local counters and should be scraped from each instance; avoid adding raw tenant identifiers as metric labels because that creates unbounded cardinality.

## Local multi-instance demo

The Compose file starts two API containers (`api1`, `api2`), one Redis, and an Nginx round-robin proxy. Make repeated evaluation requests and compare the `instance_id` values: both instances share the same per-tenant quota. To verify the atomic path under load, send concurrent requests against a fresh low-capacity test policy and confirm that no more than the configured capacity is allowed.

## Tests

Unit and HTTP/Redis integration tests:

```sh
go test ./...
```

Redis-backed tests use `REDIS_TEST_URL`; without it, those tests are skipped. Start a Redis instance, then run:

```sh
REDIS_TEST_URL=redis://localhost:6379/0 go test -race ./...
```

The suite includes direct Lua algorithm tests, HTTP endpoint lifecycle tests, and a concurrent test issuing 100 requests through two independent API instances and asserting exactly 17 are allowed for a capacity-17 rule. GitHub Actions starts Redis and runs the complete race-enabled suite, `go vet`, and the build on pushes and pull requests.

## Known limitations

The Compose configuration is for a local demo: Redis is a single unauthenticated, non-TLS instance with no HA or failover, and it stores both policies and quota state. A production variant could keep policies in ZooKeeper and push versioned updates into API-instance memory, but must handle propagation lag, watcher reconnects, and snapshot bootstrap; this does not eliminate Redis as the shared quota-state dependency. The service returns `503` if Redis is unavailable (fail closed); evaluation and state endpoints rely on an authenticated upstream gateway. Metrics are per-process, Redis is the serialization bottleneck for each hot key, and the solution does not include idempotency, policy audit/versioning, or load benchmarks. See [ARCHITECTURE.md](./ARCHITECTURE.md) for the contention model and follow-up items before production exposure.
