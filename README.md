# Redis-backed rate limiter

A Go HTTP service that answers: **may this identifier perform this operation now?**

Each policy is selected by an `(identifier, namespace)` pair—for example, tenant `tenant-42` accessing `search`. A policy can contain up to eight named limits. The request is allowed only when every limit permits it.

The local demo runs two API instances against the same Redis. Both instances use a Redis Lua script to evaluate and update quota state atomically, so a tenant does not get a separate allowance per API replica.

## Run the service

Choose **Docker Compose** to run the complete demo stack, or **local Go processes** if you do not have Docker. Run these commands from a terminal in the repository root (the directory containing `docker-compose.yml` and `go.mod`).

### Option A: Docker Compose (recommended)

Install Docker Desktop, or Docker Engine with the Compose plugin. Confirm both commands work:

```sh
docker --version
docker compose version
```

Build the API image and start Redis, two API instances, and the Nginx proxy:

```sh
docker compose up --build
```

Keep this terminal open. In a second terminal, check that all services report `Up` or `healthy` and that the proxy can reach the API and Redis:

```sh
docker compose ps
curl -i http://localhost:8080/healthz
```

The health response should be HTTP 200 and include `"status":"ok"`. The local endpoints are:

| Component | Address | Role |
| --- | --- | --- |
| Nginx proxy | `http://localhost:8080` | Distributes requests between both API instances |
| API instance 1 | `http://localhost:8081` | Direct access to instance 1 |
| API instance 2 | `http://localhost:8082` | Direct access to instance 2 |
| Redis | Internal to Compose | Shared policy and quota state |

Create a small policy, then evaluate it. The admin token defaults to `local-demo-token`:

```sh
curl -i -X PUT \
  -H 'Authorization: Bearer local-demo-token' \
  -H 'Content-Type: application/json' \
  'http://localhost:8080/v1/rules?identifier=demo-tenant&namespace=search' \
  -d '{"rules":[{"name":"demo-limit","algorithm":"fixed_window","capacity":2,"period_seconds":60}]}'

curl -sS -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/v1/evaluate \
  -d '{"identifier":"demo-tenant","namespace":"search","cost":1}'
```

Repeat the evaluation three times: the first two responses should say `"allowed":true`; the third should say `"allowed":false`. To see the shared-quota behavior directly, send requests to `http://localhost:8081/v1/evaluate` and `http://localhost:8082/v1/evaluate`; both instances use the same Redis quota.

Stop the stack with `Ctrl-C` in the first terminal, or run this from the repository root in another terminal:

```sh
docker compose down
```

`docker compose down` preserves the named Redis volume. To also delete the persisted policies and quota data, run `docker compose down -v`.

To use a different admin token, set it when starting the stack and send that same value in the `Authorization` header:

```sh
ADMIN_TOKEN='replace-with-a-local-secret' docker compose up --build
```

### Option B: Run Go locally, with Redis

This runs the API directly on your computer. You need Go 1.23 or later, Redis, and `curl`. Install Redis if needed (for example, `sudo apt-get install redis-server` on Ubuntu, or `brew install redis` on macOS).

Open three terminals, all at the repository root:

1. **Terminal 1 — start Redis** and leave it running:

   ```sh
   redis-server --bind 127.0.0.1 --port 6379 --appendonly yes
   ```

2. **Terminal 2 — start API instance 1** and leave it running:

   ```sh
   REDIS_URL=redis://127.0.0.1:6379/0 ADMIN_TOKEN=local-demo-token INSTANCE_ID=api1 HTTP_ADDR=:8080 go run .
   ```

3. **Terminal 3 — start API instance 2** and leave it running (optional, but useful for demonstrating shared quotas):

   ```sh
   REDIS_URL=redis://127.0.0.1:6379/0 ADMIN_TOKEN=local-demo-token INSTANCE_ID=api2 HTTP_ADDR=:8081 go run .
   ```

   The first `go run` may download Go modules. Wait for the log `listening on ...` from each API process.

4. **Terminal 4 — run the complete printed demo workflow:**

   ```sh
   bash scripts/demo-workflow.sh
   ```

   The script checks both health endpoints, creates a fresh policy, reads it through the other instance, sends two allowed evaluations and one denied evaluation across both instances, reads quota state, and prints each instance's Prometheus counters. It needs `curl` and `jq`. To use a non-default admin token or different ports, set `ADMIN_TOKEN`, `API1_URL`, and `API2_URL` before invoking it.

   To demonstrate concurrent traffic against a burst limit of 20 tokens per 10 seconds and a sustained fixed window of 50 requests per 60 seconds, run:

   ```sh
   bash scripts/concurrent-demo.sh
   ```

   It launches 30 requests concurrently, alternating between the two API instances, then prints each request ID, its allow/deny result, per-rule quota signals (including `consumed`), and the allowed/denied ID lists. For an allowed evaluation, `consumed` reflects usage after that request; for a denied evaluation, quota is unchanged and it reflects existing usage. The final state output also includes `consumed`. The exact winners may vary between runs because requests race; the total allowed should be around 20 (the token bucket may refill slightly while the concurrent requests are in flight).

   To perform these steps manually instead, verify the API and try a request:

   ```sh
   curl -i http://localhost:8080/healthz
   curl -i -X PUT \
     -H 'Authorization: Bearer local-demo-token' \
     -H 'Content-Type: application/json' \
     'http://localhost:8080/v1/rules?identifier=demo-tenant&namespace=search' \
     -d '{"rules":[{"name":"demo-limit","algorithm":"fixed_window","capacity":2,"period_seconds":60}]}'
   curl -sS -X POST \
     -H 'Content-Type: application/json' \
     http://localhost:8081/v1/evaluate \
     -d '{"identifier":"demo-tenant","namespace":"search","cost":1}'
   ```

   `curl` health should return HTTP 200 with `"status":"ok"`. The policy is created through instance 1 and the evaluation above is sent through instance 2; both share the Redis quota. Repeat the evaluation until it returns `"allowed":false`.

Stop each process with `Ctrl-C` in its terminal. This local Redis command stores data in Redis's configured data directory; for throwaway demo data, use Docker Compose's Redis volume instead.

### Common issues

- **`docker: command not found`**: install and start Docker Desktop or Docker Engine with the Compose plugin, or use Option B.
- **`connection refused` on port 6379**: Redis is not running. Start it before starting the API.
- **`address already in use`**: another process is using the requested port. Stop that process or choose a different `HTTP_ADDR` and matching `curl` URL.
- **API exits with `connect to Redis`**: check that Redis is reachable at the `REDIS_URL` configured for the API.

## Configure a policy

Create or replace a policy using `PUT /v1/rules`. This example gives one tenant a burst limit of 20 tokens, refilled to full over 10 seconds, and a sustained fixed-window limit of 100 requests per minute:

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

`identifier` names the customer or API key being limited. `namespace` names the resource or operation being protected. The combination selects one policy, so the same tenant can have independent limits for `search`, `uploads`, and other resources.

### Choose an algorithm

| Algorithm | What it does | Useful for | Trade-off |
| --- | --- | --- | --- |
| `token_bucket` | Starts full and replenishes continuously at `capacity / period_seconds`. A request spends tokens equal to its `cost`. | Short bursts while controlling the average rate over time. | State can contain fractional tokens; retry/reset values are estimates based on current state. |
| `fixed_window` | Allows up to `capacity` units during each aligned window of `period_seconds`. | Simple periodic quotas, such as 100 requests per minute. | Requests near the end of one window and start of the next can create a boundary burst. |

Use token bucket when smoothing bursts matters. Use fixed window when a simple aligned quota and reset time are useful. Combine them when you want both burst shaping and a sustained ceiling. Rules compose as AND constraints: **all rules must allow the request, and a denied request consumes quota from none of them.**

`capacity` must be 1–1,000,000; `period_seconds` must be 1–86,400. A policy has 1–8 rules, and rule names must be unique.

### Read or change configuration

`GET` reads the complete policy. `PUT` replaces the complete rules list. `DELETE` with `name` removes one named rule; without `name`, it deletes the entire policy. These endpoints require the admin bearer token:

```sh
curl -sS \
  -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search'

curl -i -X DELETE \
  -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search&name=burst'

curl -i -X DELETE \
  -H 'Authorization: Bearer local-demo-token' \
  'http://localhost:8080/v1/rules?identifier=tenant-42&namespace=search'
```

Policies are stored in Redis and take effect without restarting API instances. Changing a rule's name, algorithm, capacity, or period selects a new quota-state key; unchanged rule state is retained. Recreating an identical rule before its old state expires can reuse that state.

## Evaluate requests

Send an evaluation request through the proxy:

```sh
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/v1/evaluate \
  -d '{"identifier":"tenant-42","namespace":"search","cost":1}'
```

`cost` is optional and defaults to 1. It must be positive and cannot exceed any rule's capacity.

Example response (timestamps and fractional values vary with request timing):

```json
{
  "allowed": true,
  "remaining": 19,
  "reset_at": "2026-09-30T08:40:00Z",
  "instance_id": "api1",
  "rules": [
    {
      "name": "burst",
      "algorithm": "token_bucket",
      "capacity": 20,
      "consumed": 1,
      "remaining": 19,
      "reset_at": "2026-09-30T08:40:00.5Z"
    },
    {
      "name": "sustained",
      "algorithm": "fixed_window",
      "capacity": 100,
      "consumed": 1,
      "remaining": 99,
      "reset_at": "2026-09-30T08:41:00Z"
    }
  ]
}
```

The response also includes `X-RateLimit-Remaining` and `X-RateLimit-Reset` headers; a denial includes `Retry-After`. A successfully computed allow **or deny** returns HTTP 200—callers must check the JSON `allowed` field. The limiter does not execute the protected business operation; an upstream service or gateway decides whether to proceed or return HTTP 429 to its own caller. If the limiter cannot reach Redis, it returns 503 and does not grant permission.

The top-level `remaining` is the smallest remaining amount across the rules, rounded down to a whole unit. Per-rule `remaining` values are rounded down the same way. This makes the response a conservative count of complete cost-1 units available; Redis still retains fractional token-bucket balances and uses them for admission and refill calculations. Retry times are estimates; a subsequent request may see a different balance.

`retry_after_ms` is omitted when it is zero (normally, on an allowed response). On a denial, it is based on the rules currently blocking the request. A fixed-window rule reports time until its window resets; a token bucket reports time until it has enough tokens for the requested cost. When multiple rules block, the top-level value is the longest of those waits. The per-rule signals identify which rule is blocking and include its retry estimate.

## Inspect current quota

Read state without spending quota:

```sh
curl -sS \
  'http://localhost:8080/v1/state?identifier=tenant-42&namespace=search'
```

The response reports consumed and remaining amounts, plus reset timestamps, per rule. It is a snapshot: another request may change the state immediately after it is read.

## Endpoints

| Method | Path | Purpose | Authentication |
| --- | --- | --- | --- |
| `POST` | `/v1/evaluate` | Atomically evaluate and consume quota | None in this demo |
| `GET` | `/v1/state` | Read quota state without consuming | None in this demo |
| `PUT` | `/v1/rules` | Create or replace policy | Admin bearer token |
| `GET` | `/v1/rules` | Read policy | Admin bearer token |
| `DELETE` | `/v1/rules` | Delete a named rule or whole policy | Admin bearer token |
| `GET` | `/healthz` | Check API and Redis connectivity | None |
| `GET` | `/metrics` | Read this process's Prometheus counters | None in this demo |

For the state and policy endpoints, supply `identifier` and `namespace` as query parameters. Evaluation takes both in its JSON request body.

## Tests and CI

For a guided tour of the implementation and tests, see [CODE_WALKTHROUGH.md](./CODE_WALKTHROUGH.md). For an interview demo outline and common design questions, see [INTERVIEW.md](./INTERVIEW.md).

Run the validation commands:

```sh
go test ./...
go vet ./...
go build ./...
```

Tests that exercise Redis are skipped unless `REDIS_TEST_URL` points to a reachable Redis instance. To run the complete suite locally:

```sh
REDIS_TEST_URL=redis://localhost:6379/0 go test -race ./...
```

The Redis-backed tests check both algorithms, multi-rule all-or-nothing behavior, HTTP policy/evaluation/state endpoints, and 100 concurrent evaluations across two independent API instances. The contention test requires exactly 17 allows against a fresh capacity-17 policy with a slow refill rate; the test deliberately makes refill negligible during the request burst. GitHub Actions starts Redis and runs the race-enabled tests, `go vet`, and the build on pushes and pull requests.

## Observability

Each API instance exposes `/metrics` with process-local counters for evaluations, allow/deny decisions, and errors. Scrape both instances and aggregate metrics in your monitoring system. The counters reset when an instance restarts; no tenant identifiers are emitted as metric labels.

`/healthz` currently combines process responsiveness with a Redis ping. It can be used as a basic readiness check, but there is no separate process-only liveness endpoint.

## Design limitations and production follow-up

The local stack is intended for a trusted laptop/interview demo:

- **Redis is a single point of failure.** Compose runs one Redis node without HA, authentication, or TLS. The service fails closed with 503 if Redis is unavailable. Lua atomicity protects against concurrent interleaving on the active Redis primary; it does not provide failover or lossless durability.
- **Policies and quota state share Redis, and writes are not zero-loss durable.** Compose enables AOF and a persistent volume, but leaves Redis at its default `appendfsync everysec`; an abrupt host/power failure can lose roughly the most recent second of acknowledged writes. AOF is not replication or a backup. Define a recovery-point objective and test backup/restore and failover for production. A production design could store policies in ZooKeeper or another durable configuration system and push versioned snapshots into API-instance memory. Redis would still be needed for shared, globally correct quota counters.
- **Identity is trusted from the request.** The service accepts the supplied `identifier` and `namespace`; evaluation and state endpoints do not authenticate callers. Put it behind a trusted identity gateway or add caller authentication and bind credentials to tenant identity before public exposure.
- **The admin credential is demo-grade.** The default admin token is public in this README. Replace it locally and use a managed secret plus stronger authorization in any real deployment.
- **Metrics are basic and unauthenticated.** Counters are per-process, with no latency histograms or tracing. Restrict access to `/metrics` to trusted operators/scrapers in production.
- **A lost response can lead to repeat consumption.** The service has no idempotency key or retry-deduplication mechanism. If Redis commits a decision but the HTTP response is lost, retrying can charge quota again.
- **Hot keys serialize.** Redis can distribute work for different policies, but evaluations for one very busy identifier/namespace are serialized to preserve exact shared quotas.
- **There is no policy audit or version precondition.** `PUT` replaces the full policy. Updates are visible immediately through Redis, but there is no audit log, history, or compare-and-swap version check.

See [ARCHITECTURE.md](./ARCHITECTURE.md) for detailed request sequencing, Redis keys and TTLs, concurrency behavior, and failure analysis.
