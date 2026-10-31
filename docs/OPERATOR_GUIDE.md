# Operator guide

How to run, configure, observe and maintain the gateway. Companion documents:
[API reference](API.md), [policy authoring](POLICY_GUIDE.md), [evals](EVAL_GUIDE.md),
[LM Studio settings](LMSTUDIO.md), [security](SECURITY.md), [runbooks](runbooks/),
[design](DESIGN.md).

## 1. Install and run

### Local demo (one command)

```sh
deploy/demo/up.sh            # Docker Compose: gateway, Redis, Postgres, simulators, Prometheus, Grafana
LMSTUDIO=1 deploy/demo/up.sh # also route to LM Studio on the host (host.docker.internal:1234)
DEMO_NO_DOCKER=1 deploy/demo/up.sh   # no Docker: local binaries (+ redis-server if installed)
deploy/demo/down.sh          # tear down (KEEP_DATA=1 keeps the Postgres volume and keys)
```

`up.sh` generates the demo's keys once into `deploy/demo/.run/env` (git-ignored) and
prints them with the URLs: OpenAI and Anthropic base URLs, the console, Grafana
(`/d/llm-gateway`) and Prometheus. Two tenants (`team-a`, `team-b`) and an `org_admin`
are seeded. Behind a TLS-intercepting proxy pass its CA: `BUILD_CA=/path/ca.pem`.

### Container image

```sh
docker build -t llm-gateway .                       # distroless, non-root, ~23 MB
docker run -p 8080:8080 -v $PWD/my-config:/etc/gateway/conf -e ... llm-gateway -config /etc/gateway/conf
```

The image holds `gateway` (entrypoint), `simprovider`, `gwctl`, `workload` and
`loadtest`.

### Kubernetes (Helm)

```sh
kubectl create secret generic gw-secrets --from-literal=REDIS_URL=redis://... \
  --from-literal=POSTGRES_DSN=postgres://... --from-literal=OPENROUTER_KEY=...
helm install gw deploy/helm/llm-gateway --set existingSecret=gw-secrets \
  --set-file config=gateway.yaml --set serviceMonitor.enabled=true --set prometheusRule.enabled=true
```

The chart runs non-root with a read-only root filesystem, `/readyz` readiness and
`/livez` liveness probes, a PodDisruptionBudget, optional HPA, Ingress (buffering off
for streams), ServiceMonitor and PrometheusRule (the alerts in
`deploy/observability/alerts.yml`). Set `terminationGracePeriodSeconds` above
`server.drain_timeout`.

### Dependencies

| Dependency | Needed for | Without it |
|---|---|---|
| Redis | Budgets, reservations, rate limits, concurrency and cache shared across instances; key-revocation pub/sub | Single instance only: in-process counters and cache |
| Postgres | API-managed objects, policy versions, keys, cost/decision records, rollups, audit log | In-memory store plus write-ahead log (`storage.wal_dir`); not durable across restarts, and keeps only the most recent 200,000 cost records and 20,000 route decisions (budgets are unaffected: they live in the counters) |

Postgres migrations run at startup and are reversible (`internal/store/migrations`).
Cost and decision writes are buffered; if Postgres is unreachable they spill to the
WAL and are replayed when it comes back.

## 2. Configuration

Configuration is YAML, from one file or a directory (files are merged: list sections
are concatenated, other top-level sections must not repeat). Values may use
`${VAR:-default}` interpolation. Secrets are never literal: `env:NAME` or
`file:/path` references only (validation rejects anything else).

```yaml
server:   {listen: ":8080", admin_listen: "", drain_timeout: 20s, reload_every: 2s, max_in_flight: 0,
           max_body_bytes: 0, tls_cert_file: "", tls_key_file: "", trust_proxy_ips: []}
storage:  {redis_url: env:REDIS_URL, postgres_dsn: env:POSTGRES_DSN, wal_dir: /var/lib/gateway/wal,
           body_key: env:BODY_KEY, budgets_fail_open: false}
providers:     # id, type (openai|anthropic), base_url, api_key (ref), headers, discover, auto_approve,
               # allow_private_network, timeouts {connect, ttft, idle, total}, probe_every
deployments:   # id, provider, model, tier (0-3), capabilities, context_window, max_output_tokens,
               # prices [{effective_from, input, output, cached_input}], latency, concurrency,
               # queue_wait, residency, pool, weight, timeouts, enabled
virtual_models: [{name: auto, policy: default}]
model_aliases:  [{match: "claude-*", model: auto}]
policies:       # see POLICY_GUIDE.md
defaults:       {policy: default, baseline: sima/qwen-27b, max_output_tokens: 4096}
tenants:        # id, name, policy, rate_limits, caps, baseline, cache, body_logging, pii,
                # allowed_models, projects [{id, keys [{id, secret (ref), scopes}]}]
admins:         [{id, role: org_admin|tenant_admin|finops_viewer|auditor, tenant, secret: env:...}]
budgets:        # id, scope (tenant|project|key|end_user|eval|shadow), scope_id, period
                # (daily|weekly|monthly|custom), hard_usd, soft_pct, alerts, timezone
alerts:         [{id, type: webhook|slack, url: env:..., allow_private_network}]
cache:          {enabled, ttl, max_entries, max_temperature, semantic, semantic_threshold,
                 embedder, price_usd, judge_hits, sample_rate}
guardrails:     {pii: off|log|redact|block, caps, sensitivity: {confidential: [local]},
                 pre_webhook, post_webhook, webhook_timeout, fail_open}
resilience:     {max_attempts, total_deadline, backoff_base, backoff_max,
                 breaker: {window, min_requests, error_rate, latency_p95_ms, cool_down, consecutive_failures},
                 timeouts, max_timeouts}
observability:  {log_requests, otlp_endpoint, otlp_headers, trace_sample, langfuse, langsmith}
evals:          {judge, embedder, gate_max_drop, online_sample_rate, grader_webhook}
```

`gateway -check -config PATH` validates without starting. Validation errors name the
file and path (`gateway.yaml: providers[0].type: ...`).

### Applying changes

* **Files:** edit and save. The gateway polls every `server.reload_every` (also on
  `SIGHUP` and `POST /admin/v1/config/reload`). A new snapshot is built and swapped
  atomically; in-flight requests finish on the snapshot they started with. An invalid
  file is rejected and the running configuration is kept.
* **Preview:** `POST /admin/v1/config/diff` (or `gwctl config diff`) shows what a reload
  would change.
* **Admin API / console / gwctl:** tenants, projects, keys, budgets, grants, policies,
  providers and deployments can be created through the API. They live in Postgres and
  are merged into the file configuration; an ID defined in a file always wins, and the
  API refuses to change it (`409 conflict`). All writes are audited.

### Keys

Virtual keys (`gw-<env>-...`) are shown once at creation and stored as SHA-256 hashes.
`gwctl keys rotate ID` issues a replacement with an overlap window; `gwctl keys revoke
ID` takes effect on every instance within a second (Redis pub/sub). Scopes restrict
models, endpoints, providers, cost per request, rate limits, IPs, residency and policy.

## 3. Observability

* **Metrics:** `GET /metrics` (Prometheus). Request, error, token, cost and savings
  counters by tenant/model/policy; gateway-overhead, TTFT and duration histograms;
  circuit state, in-flight, queue depth and budget utilisation gauges. Labels are
  bounded (no request IDs or user IDs).
* **Dashboards:** `deploy/observability/grafana/dashboards/llm-gateway.json` (import, or
  use the demo's provisioned Grafana).
* **Alerts:** `deploy/observability/alerts.yml`; every alert has a runbook in
  [`docs/runbooks/`](runbooks/).
* **Traces:** OpenTelemetry (`observability.otlp_endpoint`); one root span per request
  with routing, cache, budget, each upstream attempt and cascade steps as children.
  Langfuse/LangSmith exporters are optional.
* **Logs:** JSON on stderr. Request logs carry IDs, model, tokens, cost and timings, never
  prompts, responses or keys. Prompt/response capture is opt-in per tenant
  (`body_logging`), encrypted with `storage.body_key`, and has its own retention.
* **Console:** `/console/` — live overview, spend, requests and route decisions,
  health, catalog approval, budgets, keys, policies, evals, playground.
* **Profiling:** set `GATEWAY_PPROF=127.0.0.1:6060` to serve `net/http/pprof` on a
  loopback listener.

## 4. Scaling and capacity

Instances are stateless; shared state lives in Redis and Postgres. Run at least two
behind a load balancer (the chart defaults to 2 with a PDB). Readiness turns false on
`SIGTERM` so load balancers stop sending new requests; in-flight requests and streams
get `server.drain_timeout` to finish.

Measured on a 4-vCPU container that also ran the simulator and load generator
(`scripts/load.sh`, `cmd/loadtest`): ~4,900 non-streaming req/s at p95 98 ms with 0
errors, and 5,000 concurrent streams with 0 errors. Gateway-added latency in process:
p95 0.5 ms, p99 0.7 ms. See `docs/PROGRESS.md` for the full numbers and conditions.

Memory: without Redis the in-process cache holds up to `cache.max_entries` responses
(default 100,000) and 20,000 semantic buckets (one per distinct conversation prefix),
each bucket capped at `cache.max_semantic_per_bucket` vectors. Size the pod for that or
use Redis.

Per-deployment `concurrency` and `queue_wait` protect local models (LM Studio serves a
limited number of parallel predictions); excess requests queue by priority and then
fall back to other deployments.

## 5. Backup and restore

```sh
POSTGRES_DSN=... GATEWAY_CONFIG=/etc/gateway/conf scripts/backup.sh backup /backups/$(date +%F)
scripts/backup.sh verify /backups/2026-10-31
POSTGRES_DSN=... scripts/backup.sh restore /backups/2026-10-31 /etc/gateway/conf
```

The backup is a `pg_dump` custom archive (API-managed objects, policy versions, hashed
keys, cost/decision records, rollups, audit log), a tarball of the configuration files,
row counts and SHA-256 checksums. Restore verifies the checksums, restores in one
transaction and compares row counts. Secret values are not included (the files hold
references); back up your secret store separately. For the demo's containerised
Postgres set `PG_EXEC="docker compose -f deploy/demo/docker-compose.yaml --env-file
deploy/demo/.run/env exec -T postgres"`.

Schedule backups at least daily; cost records older than the retention window are
rolled up hourly (`cost_rollups_hourly`) and pruned, so totals survive.

## 6. Upgrades

Rolling updates (`maxUnavailable: 0`). Migrations are additive within a minor version
and each has a down migration. Back up before upgrading across minor versions.

## 7. Release checks

`scripts/supply-chain.sh`: govulncheck (fails on any reachable vulnerability), CycloneDX
SBOMs into `dist/`, gitleaks over history and working tree, and, with `IMAGE` and
`COSIGN_KEY`, image build, signature and SBOM attestation.

## 8. Test suites

| Suite | Command | Needs |
|---|---|---|
| Default | `go test ./...` | nothing (simulated providers, miniredis) |
| Race | `go test -race ./...` | — |
| Postgres store | `scripts/pg-test.sh` | PostgreSQL server binaries |
| Scenarios (real SDKs, Claude Code, browser) | `make scenarios` | Python, `openai`, `anthropic`, Playwright |
| LM Studio | `LMSTUDIO_BASE_URL=http://localhost:1234/v1 go test -tags lmstudio ./...` | LM Studio with models loaded |
| Load | `scripts/load.sh` | — |
| Chaos | `scripts/chaos.sh` | — |
| Soak | `scripts/soak.sh` (24 h default) | a host that can run for a day |
| Demo stack | `DEMO_TEST_DOCKER=1 go test ./deploy/demo` | Docker |
