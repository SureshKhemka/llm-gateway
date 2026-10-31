# Cost-Aware LLM Gateway — Design

Status: living document, kept current with each milestone (see `docs/PROGRESS.md`).
Spec: `docs/SPEC.md`. Requirement IDs in brackets.

## 1. Shape of the system

One stateless Go binary (`cmd/gateway`) runs N instances behind a load balancer. All
cross-instance state lives in **Redis** (budgets, reservations, rate limits, cache,
idempotency, session pins, circuit-breaker hints) and **Postgres** (durable records).
Each instance holds an immutable, atomically swapped **runtime snapshot** built from
config files + admin-API state; hot reload builds a new snapshot and swaps the pointer, so
in-flight requests keep the snapshot they started with [PRV-07, CFG-03].

```
 client ──► HTTP server ──► auth ──► parse (OpenAI | Anthropic) ──► canonical request
                                                                     │
      ┌──────────────────────────────────────────────────────────────┘
      ▼
 guardrails (size caps, PII, sensitivity) ─► rate limits ─► cache lookup ─► router
      ─► budget reserve ─► executor (attempt loop: breaker, concurrency slot, timeouts,
         retries, fallback, cascade validation) ─► provider adapter ─► upstream
      ─► settle budget ─► cost record + decision record + event + metrics ─► response
```

Other binaries: `cmd/simprovider` (fault-injecting simulated provider, ENV-04),
`cmd/workload` (corpus replay, ENV-05), `cmd/gwctl` (CLI, ADM-04), `cmd/traceability`
(test→requirement report, NFR-22).

## 2. Packages

| Package | Responsibility |
| --- | --- |
| `internal/api` | Wire types: OpenAI chat/embeddings/models/responses, Anthropic messages; error shapes [API-10] |
| `internal/translate` | OpenAI ⇄ Anthropic request/response/stream translation [API-06] |
| `internal/config` | Declarative YAML schema, loading, `${ENV}` / `env:` secret refs, validation with file+path errors, diff [CFG-*] |
| `internal/catalog` | Deployments, pools, capabilities, versioned price cards, virtual models; snapshot [PRV-03..05] |
| `internal/provider` | Provider interface + adapters: `openai` (LM Studio, vLLM, Ollama, OpenRouter, OpenAI), `anthropic`; discovery [PRV-01, PRV-06] |
| `internal/tokens` | Token estimation when usage is missing [API-13] |
| `internal/cost` | Cost from usage × price card in effect; baseline; savings [CST-01..03] |
| `internal/router` | Policies, precedence, hints narrowing, candidate filter, difficulty, objectives, degradation, pinning, A/B, shadow, decision records, explain [RTE-*] |
| `internal/resilience` | Health tracker, circuit breaker, latency EWMA, concurrency limiter, backoff [RES-*, RTE-07] |
| `internal/governance` | Tenancy, virtual keys, roles, budgets + reservations, rate limits, alerts [TEN-*, BUD-*] |
| `internal/cache` | Exact + semantic cache, scoping, invalidation [CCH-*] |
| `internal/guard` | Request caps, residency, PII/secret detection [GRD-*] |
| `internal/store` | Persistence interface; `memory` and `postgres` implementations; async record writer |
| `internal/observe` | Request events, Prometheus metrics, OpenTelemetry tracing [OBS-*] |
| `internal/eval` | Suites, graders, runs, Pareto, recommendations, promotion gate [EVL-*] |
| `internal/admin` | Versioned admin REST API, OpenAPI doc, role checks, console + playground (embedded) [ADM-*] |
| `internal/server` | HTTP wiring, request lifecycle, graceful drain [RES-10] |

## 3. Canonical request model

Internally every request is an OpenAI Chat Completions request (`api.ChatRequest`) plus a
`RequestContext` (principal, hints, tags, end user, session, sensitivity, trace).
Unknown JSON fields are preserved in `Extra` and forwarded to OpenAI-format upstreams so
nothing is silently lost. The Anthropic endpoint translates in and out at the edge; an
Anthropic-format upstream is reached by translating the canonical request back. Streams
are canonical `ChatChunk`s; the edge encoder writes OpenAI SSE or Anthropic event SSE.

### 3.1 Anthropic format (as implemented) [API-02, API-06]

`/v1/messages` (plus `count_tokens`) parses the Anthropic body, keeps it on the canonical
request (`ChatRequest.Anthropic`) and translates it to canonical form for routing.

- **Same-format pass-through.** An Anthropic-format upstream receives the original body
  with only `model` (and a clamped `max_tokens`) changed: cache_control, thinking,
  citations, server tools and unknown fields survive. Non-streaming responses are forwarded
  verbatim (model rewritten). Stream events are carried on canonical chunks (`Raw`) and
  replayed verbatim to Anthropic clients, while the gateway meters them through the
  canonical view.
- **Cross-format translation** covers system prompts (string or blocks), multi-turn
  messages, images (base64 and URL), tool definitions, tool_use/tool_result in both
  directions (results become `tool` messages and back), tool_choice and
  parallel_tool_calls, stop sequences, sampling, JSON-schema output ↔
  `output_config.format`, reasoning effort, stop reasons, usage (cache reads reported as
  cached prompt tokens) and streaming events.
- **What cannot cross:** server tools, non-text documents and other block types are
  rejected with a 400 that says they need an Anthropic-format deployment. Thinking text
  crosses as `reasoning_content`, but signatures do not.
- The upstream adapter is raw HTTP rather than the Anthropic SDK, because a protocol
  gateway needs wire-level fidelity: verbatim pass-through, its own TTFT/idle watchdog,
  and event replay. It sends `anthropic-version: 2023-06-01` unless provider headers
  override it.
- `model_aliases` (globs such as `claude-* → auto`) let Anthropic clients keep their model
  names. A client `max_tokens` above a deployment's limit is clamped per attempt, never an
  exclusion. Claude Code sends 32,000; excluding on it left no candidates.

## 4. Configuration [CFG-01..07]

Files (`gateway.yaml`, optionally a directory of `*.yaml` merged in lexical order):
`server`, `storage` (redis, postgres DSNs as secret refs), `providers`, `deployments`
(with `prices:` list of `{effective_from, input, cached_input, output, reasoning}`),
`virtual_models`, `policies`, `tenants` (→ `projects` → `keys` metadata), `budgets`,
`rate_limits`, `alerts`, `cache`, `guardrails`, `observability`.

- Secrets: only `env:NAME` / `${NAME}` references or `secret://` manager refs; an inline
  value in a secret field fails validation [CFG-04, PRV-08].
- Validation reports `file:path: reason` and the running snapshot is untouched on error [CFG-02].
- **Which source wins:** objects defined in files are `managed: file` and read-only via the
  admin API (409 `managed_by_file`). Objects created through the admin API are stored in
  Postgres as `managed: api`. A file object with the same ID as an API object replaces it
  on the next reload and the takeover is audited [CFG-01].
- Minimal config: one LM Studio provider; deployments are discovered from `/v1/models`
  and default policies (`auto`, `auto-fast`, `auto-cheap`) apply [CFG-07, PRV-06]. Discovered
  models are usable by concrete name only once approved (or when `auto_approve: true` in the
  minimal-config mode, which is the default for a single-provider file).
- Reload: SIGHUP, file watcher (poll), or `POST /admin/v1/config/reload`; `POST /admin/v1/config/diff`
  returns what would change [CFG-03, CFG-05]. Policy versions are kept forever and
  `POST /admin/v1/policies/{name}/rollback` activates any prior version [CFG-06].

## 5. Storage

### 5.1 Postgres (durable)

```
config_versions(id, created_at, actor, source, digest, body jsonb)
policies(name, version, created_at, actor, body jsonb, active bool, eval_run_id, override_reason)
tenants(id, name, created_at, managed)   projects(id, tenant_id, name, managed)
keys(id, prefix, hash bytea unique, tenant_id, project_id, name, scopes jsonb, roles text[],
     created_at, expires_at, revoked_at, rotated_from, overlap_until, managed)
admin_credentials(id, prefix, hash, role, tenant_id, created_at, revoked_at)
budgets(id, scope_kind, scope_id, period, timezone, soft_pct int[], hard_usd numeric, managed)
budget_grants(id, budget_id, extra_usd, expires_at, actor, reason)
cost_records(request_id pk, ts, tenant_id, project_id, key_id, end_user, tags jsonb,
     requested_model, deployment, provider, policy, policy_version, route_reason,
     cache_status, input_tokens, cached_input_tokens, output_tokens, reasoning_tokens,
     cost_usd numeric(20,10), baseline_cost_usd, cache_saved_usd, estimated bool,
     overhead_usd, attempts int, status, error_code, latency_ms, ttft_ms, shadow bool, eval_run_id)
cost_rollups_hourly(bucket, tenant_id, project_id, key_id, model, provider, policy, ... sums)
decision_records(request_id pk, ts, tenant_id, body jsonb)
audit_log(id bigserial, ts, actor, role, action, object_kind, object_id, before jsonb, after jsonb, reason)
eval_suites, eval_items, eval_runs, eval_results
feedback(request_id, ts, score, comment)
prompt_bodies(request_id, tenant_id, expires_at, body bytea /* encrypted, redacted */)
```

Records are written by an asynchronous batch writer (flush ≤ 1 s, bounded queue spilling to
a local WAL file when Postgres is down, replayed on recovery) so the hot path never waits on
Postgres [NFR-07, NFR-08]. Aggregates are computed from `cost_records` with `GROUP BY`, so
they always reconcile with per-request rows [CST-05]; hourly rollups for >13-month retention
are produced by the same query and verified against raw rows before raw rows age out [CST-06].
Migrations are numbered, forward + down SQL files embedded in the binary [NFR-18].

The `store.Store` interface has a `memory` implementation used by the default test suite
and single-node dev, and a `postgres` implementation exercised when `GATEWAY_TEST_PG_DSN`
is set (`scripts/pg-test.sh` starts a throwaway cluster).

### 5.2 Redis (shared hot state) [BUD-05]

| Key | Use |
| --- | --- |
| `b:{scope}:{id}:{period}` hash `{spent,reserved}` nano-USD | budgets + reservations |
| `rl:{scope}:{id}:{metric}:{minute}` | RPM / ITPM / OTPM counters (sliding window over 2 buckets) |
| `cc:{scope}:{id}` zset of request IDs with expiry score | concurrent requests |
| `ce:{tenant}:{hash}` | exact cache entry |
| `cs:{tenant}:{policy}` + `csv:{tenant}:{id}` | semantic cache index + vectors |
| `idem:{key_id}:{idem_key}` | idempotent replay (24 h) |
| `pin:{tenant}:{session}` | session → deployment |
| `alert:{budget}:{period}:{threshold}` | once-per-threshold alert latch |
| `revoked` pub/sub + `keyver` | key revocation within 5 s, config reload fan-out |

All multi-key budget operations are Lua scripts, so check-and-reserve across every scope is
atomic and concurrent requests cannot jointly overshoot [BUD-03]. Tests use miniredis.
If Redis is unavailable, budgeted requests fail closed (`upstream_unavailable` is wrong
here; we return 503 `budget_unavailable`) unless `budgets.fail_open: true`; caching and
rate limits fail open [NFR-08].

## 6. Request lifecycle

1. **Auth.** `Authorization: Bearer` / `x-api-key` → SHA-256 hash lookup in the snapshot's
   key index (refreshed from Postgres; revocations pushed over Redis pub/sub, polling every
   2 s as a backstop → effect < 5 s) [TEN-02]. Produces a `Principal`.
2. **Parse + hints.** Headers `x-gw-*` or body `metadata.gateway` [API-08]. Hints are
   intersected with the effective policy and key scopes: they can only narrow [RTE-10].
3. **Guardrails.** Size/message/output caps [GRD-01]; PII scan for non-local candidates
   [GRD-04]; sensitivity → allowed residency tags [GRD-02].
4. **Rate limits.** RPM, ITPM (estimated input), concurrency checked atomically per
   key/project/tenant; OTPM debited after [BUD-04].
5. **Idempotency + cache.** Idempotency key replay [API-12]; exact then semantic lookup if
   cacheable [CCH-*].
6. **Route.** Build candidates, estimate difficulty, rank by objective, apply budget
   degradation and session pin → ordered attempt list + decision record [RTE-*].
7. **Reserve.** Worst-case cost of the most expensive deployment in the attempt list is
   reserved against every scope [BUD-03].
8. **Execute.** Attempt loop (§8). Streaming commits to a deployment at the first token.
9. **Settle.** Reservation replaced by actual cost (including judge/classifier/cascade/
   fallback overhead) [CST-01]; tokens debited from OTPM; records enqueued; event emitted;
   metrics updated; response carries `x-gw-*` headers [API-09].

## 7. Routing algorithm [RTE-01..17]

Policy resolution [RTE-09]: for the generic virtual model `auto`, key → project → tenant →
global default. A specific virtual model (`auto-fast`, `auto-cheap`, operator-defined)
selects its own policy — that is what asking for it means — while key/tenant hard limits
(max cost, providers, allow-lists, residency) still apply as constraints. Request hints
then narrow the result and never widen it (§6, RTE-10). A policy is `{name, version, objective, difficulty: {estimator,
thresholds}, max_cost_usd, latency_target_ms, min_tier, allowed/denied providers,
residency, cascade: {enabled, max_escalations, validators, judge}, degrade: [{at_pct, max_tier}],
cache: {semantic_threshold, allow_tools}, split: [...], shadow: {...}}`.

1. **Candidate set.** Start with deployments the virtual model maps to (default: all chat
   deployments). Exclude, recording each reason: disabled; circuit open / unhealthy; missing
   capability (tools, vision, json_schema, embeddings); `context_window < prompt_tokens +
   max_output`; not in key/tenant allow-list; provider denied; residency mismatch; tier
   outside `[min_tier, max_tier]`; estimated cost > max cost; cold while a warm one exists
   for latency-sensitive requests [RTE-01, PRV-10]. Empty → `no_eligible_model` with reasons
   [RTE-02].
2. **Difficulty** score in [0,1] and minimum tier [RTE-03]:
   - `rules` (default): weighted signals — prompt length, code fences/keywords, tools present,
     reasoning cues ("step by step", "prove", "debug", "why"), conversation depth, JSON
     schema, arithmetic word problems, explicit `min_tier` hint. Score → tier via policy
     thresholds (default 0.2/0.45/0.7). Regexes scan the first and last 4 KB of very long
     prompts (length is measured on the whole prompt).
   - `similarity`: k-NN over embedded labelled examples [RTE-04].
   - `classifier`: a tier-0 model prompted to output a tier digit; its cost and latency are
     charged to the request [RTE-16].
3. **Objective** [RTE-05]: estimated cost = input tokens × input price (with cached-input
   discount when the provider reports prompt caching, RTE-17) + expected output × output
   price; estimated latency = observed TTFT EWMA + expected output / observed TPS
   (falling back to the static profile) [RTE-07].
   - Hard constraints (capabilities, context, allow-lists, residency, policy/hint tier
     bounds, max cost) exclude. The **difficulty-derived** tier is a preference: candidates
     below it stay at the end of the attempt list so a request is served rather than failed
     when nothing at its tier is available (reason `below_difficulty_tier`). A latency target
     excludes slow candidates unless that would leave none.
   - `min_cost` (default): among candidates at or above the difficulty tier, sort by
     estimated cost (ties: higher tier, then lower latency).
   - `min_latency`: filter cost ≤ max, sort by latency. `max_quality`: sort by tier desc, cost.
   - `weighted`: minimise `w_c·cost_norm + w_l·latency_norm − w_q·tier_norm`.
4. **Budget degradation** [RTE-08]: utilisation of the tightest applicable budget ≥ a
   policy threshold caps the max tier (e.g. 80% → tier ≤ 1, 95% → tier 0), reason code
   `budget_degraded`.
5. **Pinning / split / shadow**: a healthy, eligible pinned deployment for the session moves
   to the front [RTE-11]; A/B splits choose a policy variant by `hash(end_user|session) mod 100`
   [RTE-12]; shadow sampling enqueues an async copy charged to the tenant's shadow budget
   [RTE-13].
6. **Ordered attempt list** = ranked candidates; pools collapse to one entry and pick a
   member by least in-flight or weight [PRV-09].

The decision record holds everything above and is stored by request ID [RTE-14];
`POST /v1/route/explain` runs steps 1–6 only, never calls a model, and bills nothing
[RTE-15]. Rule-based routing is pure in-memory work on the snapshot (target p95 ≤ 5 ms,
measured by `BenchmarkRoute`).

## 8. Execution, resilience and cascade [RES-*, RTE-06]

```
for attempt in ordered list (≤ max_attempts, within total deadline):
    breaker.Allow(dep)?           no → skip (reason circuit_open)
    limiter.Acquire(dep, queueWait) timeout → skip (reason concurrency_full) [RES-07]
    call with connect / TTFT / idle / total timeouts [RES-06]
    retriable error (conn, timeout, 429, 5xx, context-length) →
        record, backoff with jitter honouring Retry-After [RES-05], next candidate [RES-03]
    non-retriable (400-class) → return error
    cascade on & !valid(answer) → record, escalate to next candidate with tier > current
                                   (≤ max_escalations) [RTE-06]
    success → return
```

Fallback candidates come from the same filtered set, so hard constraints are never
relaxed [RES-08]. Switching to a different candidate happens immediately; retrying the
same deployment (a single-candidate plan, or a second pass over the list) waits a
jittered exponential backoff, never less than the upstream's `Retry-After` [RES-05].
Upstream 404 for the chosen model (an unloaded LM Studio model) and 401/403 (a bad
provider key) are deployment faults: they fall back and count against health.
Health: each deployment has a rolling window of outcomes and latency, plus active probes
of `/v1/models` every 15 s (unreachable → unhealthy; reachable but model not listed →
unhealthy and *cold*); state healthy/degraded/unhealthy [RES-01, PRV-10]. Breaker: closed →
open when, over the window, the error rate or p95 latency exceeds its threshold (with a
minimum volume) or after N consecutive failures; → half-open after the cool-down, which
admits exactly one probe request; success closes, failure re-opens [RES-02]. Routing reads
breaker state without side effects; the executor takes the half-open probe slot. Streaming: the first chunk is awaited
before headers are written; after that a failure ends the stream with a well-formed
error event and bills delivered tokens (counted locally when usage is missing) [RES-04,
API-11, API-13]. Client disconnect cancels the upstream context [API-11]. Load shedding:
a global in-flight cap rejects lower-priority keys first [RES-09]. Shutdown: stop accepting,
drain in-flight up to `server.drain_timeout` [RES-10].

## 9. Governance [TEN-*, BUD-*]

- Hierarchy: organisation (one per install) → tenants → projects → keys [TEN-01].
- Virtual keys: `gw-<env>-<8 char id>_<32 random>`; stored as SHA-256 (keys are high-entropy
  so a fast hash is non-reversible in practice); prefix visible; optional expiry; rotation
  creates a new key and sets `overlap_until` on the old one [TEN-02]. Scopes per §6 [TEN-03].
- File-defined (bootstrap) keys can be rotated and revoked through the API: the
  revocation is stored as a document and applied on every rebuild.
- Admin credentials are separate from virtual keys, with roles `org_admin`, `tenant_admin`
  (bound to a tenant), `finops_viewer`, `auditor`. One permission table is used by the
  admin API, console (calls the same API) and CLI (calls the same API) [TEN-04, ADM-05].
  Every admin query is filtered by the caller's tenant binding [TEN-06].
- Budgets: per scope, period daily/weekly/monthly/custom with time zone; soft thresholds
  alert/degrade; hard limit blocks with HTTP 402 `budget_exceeded` (decision in CLAUDE.md)
  [BUD-01, BUD-02]. Temporary grants with expiry + audit [BUD-08]. Alert webhooks (generic
  JSON and Slack `text`) fire once per threshold per period via a Redis `SETNX` latch [BUD-07].
- Rate limits: per key/project/tenant, 429 `rate_limited` + `Retry-After` +
  `x-ratelimit-limit-*`/`-remaining-*`/`-reset-*` headers [BUD-04].
- Changes apply via snapshot reload: immediately on the instance that served the admin
  call, and on every other instance at its next refresh (`server.reload_every`, default
  5 s) [BUD-06].
- Reservation (as implemented): before the first upstream call the request is admitted
  (rate limits) and its applicable budgets resolved. Each attempt must be covered by a
  worst-case reservation for *that* deployment — `ceil(prompt_tokens × 1.3) × input price
  + output cap × max(output, reasoning) price` — so a fallback to a pricier deployment
  grows the reservation or is skipped (`budget_exceeded` if nothing fits). With a hard
  budget in scope and no client `max_tokens`, the gateway sets `max_tokens` to
  `defaults.max_output_tokens` so the reservation is a true bound; estimated output counts
  are clamped to that cap. Settlement replaces the reservation with actual cost.
- Degradation caps the tier as a strong preference: deployments above the cap stay at the
  end of the attempt list, so a request only a bigger model can serve (vision, long
  context) is still served.

## 10. Caching [CCH-*]

Cache key = SHA-256 of tenant scope + normalised output-affecting fields (messages, model
or virtual model + policy, tools, response_format, temperature, top_p, max tokens, stop,
seed). Cacheable when temperature ≤ `cache.max_temperature` (default 0.3), no tools unless
the policy allows, and not `no-cache` [CCH-04]. Semantic: embed the last user message +
system prompt via the embeddings deployment; per tenant+policy+param-hash bucket, cosine
similarity over stored vectors (bounded count per bucket, LRU) ≥ threshold [CCH-02].
Cross-tenant sharing only when both tenants set `cache.shared: true` (shared namespace)
[CCH-03]. **Entity guard:** a semantic candidate is only a hit when the prompts carry the
same salient tokens (numbers, quoted strings, mid-sentence capitalised words). Templated
prompts that differ only in an invoice number or a name embed almost identically but need
different answers. In the reference replay this guard took wrong semantic hits from 112
to 0 at threshold 0.85 [CCH-09]. A request without `temperature` is assumed to run at
`cache.default_temperature` (1.0, the provider default) and is not cached. Hits are re-encoded to the client format and streamed when requested [CCH-05];
billed at `cache.price_usd` (default 0) with cost avoided = estimated cost on the routed
deployment [CCH-06]. `x-gw-cache: bypass|refresh` [CCH-07]. Admin invalidation by tenant /
policy / model / age; size bounded by max entries + TTL [CCH-08]. Optional judge check and
hit sampling into `cache_samples` [CCH-09].

### Cascade (as implemented) [RTE-06]

A cascade policy ignores the difficulty estimate and orders candidates cheapest-first. After
each answer the validators run in order: `json_schema` (schema or JSON validity when
`response_format` asks for it), `non_empty`, `not_truncated` (finish reason `length`),
`no_refusal` (refusal field, `content_filter`, or a refusal opener), and optionally `judge`
(a judge deployment scores 0–10; below `judge_min_score`×10 fails). On failure the attempt
is recorded as `cascade/invalid`, its cost becomes overhead, and execution continues with
candidates of a higher tier (≤ `max_escalations`). When escalations run out, the last
answer is returned with reason `cascade_exhausted` and is not cached. Streaming requests
cascade only with `buffer_streams: true`: the validated answer is replayed as a stream.

### Cost convention

`X-Gw-Cost-Usd` and `cost_records.cost_usd` are the request's total. That is the served
attempt's token cost (split into input, cached input, output and reasoning) plus
`overhead_usd`: failed cascade attempts, classifier and judge calls, and the semantic-cache
lookup embedding on a miss. A cache hit bills exactly `cache.price_usd`; the gateway
absorbs that hit's lookup embedding [CST-01, CCH-06].

## 11. Observability [OBS-*]

- One JSON event per request on stdout (fields per OBS-01); never prompt/response text or
  full keys — only key prefix [NFR-16].
- Prometheus `/metrics` with bounded labels (tenant/model/provider/policy limited to
  configured IDs; unknowns collapse to `other`) [OBS-02].
- OpenTelemetry spans `gateway.request` → `auth`, `route`, `cache.lookup`,
  `upstream.attempt` (GenAI semconv attributes `gen_ai.request.model`,
  `gen_ai.usage.input_tokens`, ...); W3C trace context propagated [OBS-03]; OTLP exporter,
  Langfuse/LangSmith via their OTLP endpoints [OBS-04].
- `/livez`, `/readyz` (config loaded ∧ ≥1 provider reachable) [OBS-05].
- Audit log append-only (no update/delete paths; Postgres role has INSERT/SELECT only) [OBS-06].

## 12. Evaluation [EVL-*]

Suites import from JSONL `{id, category, difficulty, messages, reference, rubric, schema, grader}`.
A run executes items against deployments (direct) and policies (through the router with
`x-gw-eval-run`), charged to the eval budget and tagged [EVL-11]. Graders: exact, regex,
json_schema, embedding similarity, llm_judge, webhook [EVL-03, EVL-04]. Report: per
target mean quality (0–100) with bootstrap 95% CI, cost, latency; Pareto frontier;
per-category table; delta vs baseline [EVL-05]; difficulty confusion matrix [EVL-06].
Recommendation: grid-search difficulty thresholds (and semantic threshold) on recorded
per-item results to minimise cost subject to quality ≥ target [EVL-07]. Promotion gate:
activation requires a run on the candidate version with quality ≥ active − `gate.max_drop`;
override needs `org_admin` + reason, audited [EVL-08].

## 13. Simulated provider [ENV-04]

`cmd/simprovider` serves OpenAI (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) and
Anthropic (`/v1/messages`) formats for any number of named instances (`--instance a:9001
--instance b:9002`). Responses are deterministic functions of the prompt; models have a
synthetic "skill" tier so low tiers can be made to fail hard prompts. Faults are set at
runtime via `POST /_sim/faults` (per instance, optionally per model): `latency_ms`,
`tokens_per_sec`, `error_rate`, `status`, `rate_limit` (429 + Retry-After), `timeout`,
`drop_after_tokens`, `malformed_json`, `omit_usage`, `down`. The same package is embedded
in tests via `httptest`.

## 14. Deployment [NFR-17, ENV-07]

- **Image:** one distroless, non-root image (`Dockerfile`) with `gateway` as entrypoint
  plus `simprovider`, `gwctl`, `workload` and `loadtest`. The toolchain is pinned
  (`toolchain` in `go.mod`, `golang:1.26.10` builder) so stdlib fixes are picked up
  deliberately.
- **Helm chart** (`deploy/helm/llm-gateway`): Deployment (2 replicas, `maxUnavailable: 0`,
  readiness/liveness on `/readyz`/`/livez`, read-only root filesystem, non-root,
  termination grace above `drain_timeout`), ConfigMap with the gateway YAML (hot-reloaded,
  no restart), Service, PDB, optional HPA, Ingress (proxy buffering off for streams),
  ServiceMonitor and PrometheusRule (`files/alerts.yml`, kept identical to
  `deploy/observability/alerts.yml` by test). Redis and Postgres are external.
- **Demo:** `deploy/demo/up.sh` / `down.sh`. With Docker: Compose stack of gateway, Redis,
  Postgres, the simulator (two instances), Prometheus (with the alert rules) and Grafana
  (provisioned dashboard). Without Docker: local binaries, `redis-server` if present,
  memory store. Secrets are generated once into `deploy/demo/.run/env`. `LMSTUDIO=1` adds
  `deploy/demo/overlays/lmstudio.yaml` (host LM Studio via `host.docker.internal`).

## 15. Operations (M8)

- **Admin API completeness [ADM-01]:** providers and deployments are API-manageable
  objects (`DocProvider`, `DocDeployment`) merged like tenants and budgets: files win, the
  API refuses to change a file-defined ID, and a rebuild validates the merged result (so
  a literal `api_key` or a provider still referenced by a deployment is rejected and the
  stored change is rolled back). The OpenAPI document is generated from the route table
  and committed as `docs/api/openapi.json`; a test fails if it drifts.
- **CLI [ADM-04]:** `cmd/gwctl` is a thin client of the admin API (same permissions).
- **Admin authentication throttle [NFR-13]:** 10 failed authentications per address per
  minute lock that address out for a minute (`429` + `Retry-After`).
- **Bounded single-node state [NFR-05]:** without Postgres the memory store keeps the
  most recent 200k cost records and 20k decisions (≈150 MB); without Redis the memory cache keeps
  `max_entries` responses and at most 20k semantic buckets, each expiring with the TTL.
  Both bounds came out of the soak test.
- **Breaker statistics:** the window's failure count is maintained incrementally and its
  p95 is recomputed lazily (at most every 200 ms under load, and only when a latency
  threshold is configured). Sorting the window on every request had been ~45% of
  request-path CPU.
- **Latency target with no feasible candidate [RTE-05]:** when no candidate's estimate
  meets `latency_target_ms`, candidates are ordered by estimated latency instead of cost
  (route reason `closest_to_latency_target`); found by the chaos suite's slowdown phase.
- **Backups [NFR-19]:** `scripts/backup.sh` (pg_dump custom archive + config tarball +
  row counts + SHA-256), restore in one transaction with checksum and row-count checks.
- **Release gate [NFR-15]:** `scripts/supply-chain.sh` — govulncheck, CycloneDX SBOMs,
  gitleaks, optional cosign signing.
- **Suites [NFR-21]:** `cmd/loadtest` + `scripts/load.sh` (throughput, streams),
  `cmd/chaos` + `scripts/chaos.sh` (provider kill/recovery, slowdown, 429 storm,
  mid-stream drop, instance kill behind a retrying LB), `scripts/soak.sh` (steady load,
  memory/goroutine/FD growth checks).
