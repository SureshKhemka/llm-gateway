# Progress

Milestones follow spec §9.2. Requirement coverage by test is in
[`docs/TRACEABILITY.md`](TRACEABILITY.md) (regenerate with `make traceability`).

Verification environment for every entry below, unless stated otherwise: Linux cloud
container (16 vCPU, 15 GB), Go 1.26, no GPU, no Docker daemon, no LM Studio. The LM
Studio suite (`make lmstudio`) has to be run on the Mac; its results are recorded here only
after a real run.

## M1 — Core proxy ✅

**Shipped**

- OpenAI-compatible `POST /v1/chat/completions` (non-streaming and SSE streaming, tools,
  `response_format` json_object/json_schema, system prompts, multi-turn, stop, sampling
  parameters, image parts; unknown fields are forwarded verbatim), `POST /v1/embeddings`,
  `GET /v1/models` (filtered per key).
- OpenAI-compatible provider adapter (LM Studio, vLLM, Ollama, OpenRouter, OpenAI) with
  connect / time-to-first-token / inter-token idle / total timeouts.
- Static catalog from declarative YAML: deployments with capabilities, context window,
  tier, latency profile, residency tags and **versioned price cards** (cost uses the card
  in effect when the request ran). Free local models are just a zero price card.
- Explicit model selection by deployment ID or `provider/upstream-model`.
- Per-request cost (input / cached input / output / reasoning split), baseline cost, gateway
  headers `X-Gw-*` on every response; on streams as HTTP trailers and on the final usage
  chunk (`x_gateway`). Usage counted locally and flagged `estimated` when the provider omits it.
- Client disconnect cancels the upstream request; only delivered tokens are billed.
- Config: multi-file merge, `${ENV}` expansion, secrets only as `env:`/`file:` references,
  strict unknown-field rejection, validation errors as `file: path: reason`.
- Minimal config (one LM Studio endpoint) works through model discovery; discovered
  models in a non-minimal config stay unregistered until approved.
- Simulated provider (`cmd/simprovider`, `internal/sim`): multiple named instances,
  runtime fault injection (latency, slow tokens, error rate, 429 + Retry-After, timeouts,
  mid-stream disconnects, malformed JSON, missing usage, down, refusal, empty answers),
  tier-dependent answer quality via a corpus oracle, deterministic embeddings.
- Structured per-request event (no prompt/response content, key prefix only), async
  record writer with WAL spill, in-memory store.
- `cmd/traceability` generates `docs/TRACEABILITY.md` from `// Covers:` comments.
- LM Studio integration suite (`-tags lmstudio`), which skips any scenario whose model role
  is not loaded and names the missing role.

**Requirement IDs covered by tests:** API-01, API-03, API-04, API-07, API-09, API-10,
API-11, API-13, PRV-01, PRV-03, PRV-04, PRV-05, PRV-06, CST-01, CST-02, CFG-02, CFG-04,
CFG-07, ENV-04, NFR-16, NFR-20 (OpenAI Python SDK), OBS-01 (partial).

**Exit scenario 1 (OpenAI part): passing.** `scenarios/test_s01_dropin_openai.py` drives the
official `openai` Python SDK (3.42.0) against the real `gateway` and `simprovider`
binaries: chat, streaming with usage, tool calls, strict JSON schema, embeddings, model
listing and SDK error mapping. The run here passed 7 of 7.

**Remaining / needs real hardware**

- Run `make lmstudio` on the Mac to confirm chat, streaming, embeddings and discovery
  against LM Studio. It has not been run yet, and its results will be recorded here.
- The Node SDK, LangChain and Claude Code verifications (NFR-20) come with M6 and M8.

## M2 — Routing ✅

**Shipped**

- Virtual models (`auto`, `auto-fast`, `auto-cheap`, plus operator-defined ones with
  optional deployment subsets), backed by declarative, versioned policies.
- Policy precedence: key → project → tenant → default for `auto`; specific virtual models
  carry their own policy. Hints arrive as headers (`X-Gw-Max-Cost-Usd`,
  `X-Gw-Latency-Target-Ms`, `X-Gw-Min-Tier`, `X-Gw-Capabilities`, `X-Gw-Allow-Providers`,
  `X-Gw-Deny-Providers`, `X-Gw-Cache`, `X-Gw-Session-Id`, `X-Gw-End-User`, `X-Gw-Tags`,
  `X-Gw-Sensitivity`, timeouts, `Idempotency-Key`) or as body `metadata.gateway`, and can only
  narrow the policy.
- Candidate filtering with a recorded reason for every exclusion: capabilities (tools,
  vision, JSON schema, requested capabilities), context window, max output, key/tenant
  allow-lists, provider allow/deny, residency, tier bounds, max cost, and health (hook for
  M3). An empty set returns `no_eligible_model` (HTTP 422) with the exclusion list.
- Rule-based difficulty estimator built on generic signals: length, code, code-writing asks,
  reasoning cues, multi-step structure, arithmetic word problems, task verbs, tools,
  structured output, images and conversation depth. Its score maps to a tier through the
  policy thresholds. There are plug-in points for similarity and classifier estimators (M5).
- Objectives: `min_cost` (default), `min_latency`, `max_quality` and `weighted`. A latency
  target is applied as a filter. Pool members are balanced by weighted least in-flight.
- Rolling observed latency (EWMA of TTFT and tokens/s per deployment) replaces the static
  profile once it has 3 recent samples.
- Every request gets a route decision record (candidates with estimated cost and latency,
  exclusions, difficulty signals, policy and version, attempts, timing), retrievable at
  `GET /v1/route/decisions/{id}` and scoped to the caller's tenant. `POST /v1/route/explain`
  plans a request without calling a model or billing anything.
- Baseline cost on every cost record (the scope's baseline deployment, same tokens), with
  routing savings and cache savings aggregated separately.
- Labelled reference corpus (`testdata/corpus/corpus.jsonl`, 544 prompts in 11 categories,
  90 paraphrases, deterministic generator `workload gen`), deterministic graders, and
  `cmd/workload replay|compare` with RPS, concurrency, tenant mix and exact-repeat control.

**Requirement IDs covered by tests:** RTE-01, RTE-02, RTE-03, RTE-05, RTE-07, RTE-09,
RTE-10, RTE-14, RTE-15, RTE-16, CST-03, API-08, ENV-05, ENV-06 (corpus + graders; the full
eval suite comes with M7).

**Exit scenarios**

| Scenario | Result |
| --- | --- |
| 2 Difficulty routing | Pass. The greeting goes to tier 0, the extraction to tier 1, and the multi-step code task to tier 2–3; explain matches the served model. |
| 3 Savings vs baseline | Pass against the simulator. In the in-process test and in a real-binary run (`gateway` + `simprovider` + `workload replay`, 544 requests, 0 errors), cost fell **50.1%** ($0.3217 vs $0.6446) with quality **±0.00 points** (100.00 vs 100.00). |
| 12 Capability routing | Pass. The image request goes to `qwen-vl-8b`, the tool request to a tools-capable model, and a ~20k-token prompt to a ≥32k-context model, with recorded exclusion reasons. |

Rule-based routing overhead over 1,632 corpus decisions: p50 0.13 ms, p95 2.7 ms, p99 7.4 ms
(target p95 ≤ 5 ms).

**Caveats and what remains**

- The estimator's signal weights were tuned against this corpus, which is also the eval
  set, so scenario 3's quality figure is an in-sample result. The simulator's tier
  skill model (always right at or above the item's difficulty, 60% → 30% → 15% below it) is
  synthetic. The meaningful check is a replay against LM Studio on the Mac with real
  graders (M7). It needs the tier 0–2 models CLAUDE.md lists as not loaded yet.
- Budget degradation (RTE-08), session pinning (RTE-11) and health exclusion are wired as
  hooks and get their tests in M3 and M4. A/B splits and shadow traffic are in M7.
- Scenario 3 asks for the result to be "shown on the dashboard"; the console ships in M8.

## M3 — Resilience ✅

**Shipped**

- Per-deployment health from live outcomes and active probes (`GET /v1/models` every
  15 s), with three states: healthy, degraded and unhealthy. A model a reachable provider no
  longer lists is marked *cold*.
- Circuit breaker per deployment. It opens on error rate, p95 latency or consecutive
  failures over a rolling window; after the cool-down it goes half-open and admits exactly
  one probe; it closes on success and re-opens on failure.
- Attempt loop: deployments with an open breaker are skipped. A retriable failure
  (connection, timeout, 429, 5xx, context length, model unloaded/404, provider auth)
  falls back to the next candidate. Retries of the same deployment use jittered exponential
  backoff and honour `Retry-After`. The loop stops at the attempt limit and the total
  deadline. Fallback only ever uses the already-filtered candidate set.
- Timeouts for connect, TTFT, inter-token idle and total, configurable per deployment and
  provider. Per-request overrides (`X-Gw-Timeout-Ms`, `X-Gw-Ttft-Timeout-Ms`) are clamped to
  configured maxima.
- Concurrency limit per deployment with a bounded queue wait; overflow spills to the next
  candidate. Pools balance by weighted least in-flight.
- Latency-aware routing uses the rolling TTFT/TPS from M2. Cold deployments are avoided
  for latency-sensitive requests while a warm one exists.
- Session pinning (`X-Gw-Session-Id`) keeps a session on its deployment while that
  deployment stays healthy and eligible.
- Load shedding by key priority at gateway saturation (`server.max_in_flight`).
- Graceful drain: readiness flips to 503, new requests are rejected, and in-flight streams
  finish up to `drain_timeout`.
- Simulator: `fail_next` fault, model add/remove control endpoints, one process per
  instance in scenario runs.

**Requirement IDs covered by tests:** RES-01, RES-02, RES-03, RES-04, RES-05, RES-06,
RES-07, RES-08, RES-09, RES-10, PRV-09, PRV-10, RTE-07, RTE-11, API-11.

**Exit scenarios** (each run both in-process as Go tests and against real `gateway` and
`simprovider` processes with the OpenAI SDK, `scenarios/test_s08_s10_resilience.py`):

| Scenario | Result |
| --- | --- |
| 8 Provider failover | Pass. After a model is unloaded and then a whole simulator process is killed mid-replay, breakers open and traffic moves, with **0 client errors**. After the model is reloaded and the process restarted, a half-open probe closes the circuit and traffic returns to the cheapest deployment. |
| 9 Latency-aware routing | Pass. After 3 s is added to the preferred deployment, traffic moves to its twin. It took 6.0 s in the Go test (EWMA α 0.5) and was also well under the 60 s limit in the real-process run (default α 0.2). |
| 10 Mid-stream failure | Pass. The simulator drops the stream after 50 tokens. The client gets exactly 50 tokens, then an `upstream_unavailable` error event, then `[DONE]`; the OpenAI SDK raises `APIError`. The model is never switched, and the cost record bills ~50 output tokens, flagged estimated, with status 502. |

The scenario suite (10 scenarios so far) passed 3 of 3 consecutive runs.

**Remaining**

- Session pins, breaker state and latency stats are per instance (in-process). M4 moves
  budgets and limits to Redis. Cross-instance breaker hints and pins are M8 multi-instance
  work, and DESIGN §5.2 lists the keys.
- Scenario 8 on LM Studio (unloading a model in the LM Studio UI) needs the Mac.

## M4 — Governance ✅

**Shipped**

- **Shared counters:** Redis, using Lua scripts for atomic multi-key reserve, settle and
  sliding-window checks, concurrency sets, alert latches and pub/sub. An in-memory
  implementation covers single-node development. `storage.redis_url` and
  `storage.postgres_dsn` switch the process to Redis and Postgres; when they are unset, the
  gateway logs that it is in single-node mode.
- **Budgets:** tenant, project, key, end-user (per user within a tenant), eval and shadow
  scopes. Periods are daily, weekly (starting Monday), monthly or custom, resetting in the
  budget's time zone.
  - Before any upstream call, each attempt must be covered by a worst-case reservation. A
    pricier fallback grows the reservation or is skipped.
  - Settlement uses actual cost. Responses carry `X-Gw-Budget-Remaining-Usd`.
  - The hard limit returns HTTP 402 `budget_exceeded`.
  - Soft thresholds degrade routing to cheaper tiers. Above the cap, deployments are only
    used for requests nothing cheaper can serve.
  - Temporary grants have an expiry and a mandatory reason, and are audited.
- **Alerts:** generic JSON and Slack-compatible webhooks, once per threshold per period
  across instances (Redis `SETNX` latch). Webhook URLs are secret references behind an
  SSRF guard that refuses private and metadata addresses unless the sink opts in.
- **Rate limits:** RPM, input TPM, output TPM and concurrent requests, per key, project
  and tenant, consistent across instances. A 429 carries `Retry-After` and
  `X-Ratelimit-Limit/Remaining/Reset-*` headers.
- **Virtual keys:**
  - Generated as `gw-<env>-<id>_<secret>`, shown once, stored as a SHA-256 hash, with a
    visible prefix.
  - Optional expiry. Rotation creates a new key, with an overlap window for the old one.
  - Revocation is broadcast over pub/sub; another instance rejected the key within
    milliseconds in the test (requirement: 5 s).
  - Scopes: models, endpoints, providers, max cost per request, rate limits, priority, IP
    allow-list, default tags, policy and baseline.
- **Admin API `/admin/v1`:** whoami, tenants, projects, keys (create, rotate, revoke,
  list), budgets (list with status, create/update, grants), costs (JSON or CSV,
  group-by, time buckets, filters, display currency), monthly chargeback, request
  explorer (cost record plus decision), audit log, health, effective config, config
  reload, cache invalidation hook.
  - Four roles (org admin, tenant admin bound to one tenant, FinOps viewer, auditor),
    enforced from one permission table.
  - The OpenAPI 3.1 document is generated from the same route table.
  - Objects defined in files return 409 on API edits, because files win.
  - The admin API can be served on a separate listener (`server.admin_listen`).
- **Audit log:** append-only, enforced in Postgres by a trigger. Every admin mutation
  records actor, role, before, after and reason.
- **Postgres store:** embedded, reversible migrations; SQL aggregation that reconciles
  exactly with raw rows; hourly rollups for records older than 13 months, verified before
  raw rows are deleted. `scripts/pg-test.sh` starts a throwaway cluster.

**Requirement IDs covered by tests:** TEN-01, TEN-02, TEN-03, TEN-04, TEN-06, TEN-07,
BUD-01, BUD-02, BUD-03, BUD-04, BUD-05, BUD-06, BUD-07, BUD-08, RTE-08, CST-01, CST-04,
CST-05, CST-06, CST-07, CST-09, OBS-06, ADM-01 (partial; the console comes in M8), ADM-05
(API side), NFR-07, NFR-08, NFR-14, NFR-18 (migrations).

**Exit scenarios**

| Scenario | Result |
| --- | --- |
| 6 Budget degradation and hard stop | Pass, with Redis-backed counters (miniredis). Replaying the corpus under a $0.50 monthly budget, routing degraded at 80% (1,035 requests degraded, ≥90% of them served at tier ≤ 1). At 100% requests got `budget_exceeded` (402). **Spend was $0.4982 of $0.50**, so no overshoot, and the budget counters agree with the sum of cost records. The webhook and the Slack sink each fired exactly once for 80% and once for 100%. |
| 7 Rate limiting | Pass. A key with a 5 RPM limit gets 429 with `Retry-After` and `X-Ratelimit-*` headers on request 6; another tenant is unaffected. Two instances sharing Redis admit exactly 5 requests between them. |
| 16 Tenant isolation | Pass. Tenant A's admin is denied tenant B's spend, request list, request detail, keys, cache invalidation, key creation, key revocation and chargeback. Unfiltered queries return only tenant A's data. Tenant B's admin can see its own data. |

Hard-budget overshoot under concurrency: 100 runs × 40 goroutines × 5 reservations
against Redis Lua scripts. **0 overshoots**, and every reservation was settled.

Postgres store suite passed against real PostgreSQL 16 in this container
(`GATEWAY_TEST_PG_DSN`): aggregate reconciliation, append-only audit (UPDATE/DELETE
rejected), rollup reconciliation, and down/up migrations. The `gateway` binary also ran
against real Redis and Postgres: chat, key creation and cost aggregation each wrote rows
to `cost_records`, `keys` and `audit_log`.

**Remaining**

- OIDC single sign-on (TEN-05, SHOULD) is not implemented. Admin credentials are bearer
  secrets from configuration.
- Rollups are produced and verified, but the reporting API reads raw rows only, so
  reports older than 13 months need a rollup-aware query (follow-up).
- Budgets reset by period key; there is no carry-over or proration, and the spec asks for
  neither.

## M5 — Caching and cascade ✅

**Shipped**

- **Exact cache:** keyed on every output-affecting field (messages, model or virtual model,
  policy@version, sampling, max tokens, stop, seed, tools, tool choice, response format,
  penalties, reasoning effort, unknown fields). Configurable TTL.
- **Semantic cache:** buckets share all parameters and earlier turns. The last user message
  is embedded with the configured embeddings deployment, and a hit needs cosine ≥ the
  policy or cache threshold plus an exact match of salient entities. There is an optional
  judge check on semantic hits and sampling of hits for review.
- **Backends:** Redis, shared by all instances, with size bounds per namespace and per
  bucket; and an in-memory LRU.
- **Scoping and cacheability:** entries are scoped to the tenant, and a shared namespace is
  used only when both tenants opt in. Only deterministic requests are cached (temperature ≤
  threshold, no tools unless the policy allows them, n = 1, not eval or shadow traffic).
  Clients can send `X-Gw-Cache: bypass | refresh | no-store`.
- **Serving hits:** hits are returned in the client's format and replayed as a stream when
  requested; a streamed miss populates the cache. Hits bill `cache.price_usd` (default 0)
  and record the cost avoided.
- **Invalidation:** admin invalidation by tenant, policy, model or age
  (`POST /admin/v1/cache/invalidate`).
- **Cascade:** cheapest-first, with validators for JSON schema, non-empty, not truncated,
  no refusal and an opt-in judge. Each failed validation escalates one tier, up to
  `max_escalations`. Every attempt is recorded in the decision record, and failed attempts
  are billed as overhead on the same request. Buffered streaming cascade is supported.
- **Difficulty estimators:** a small-model classifier that is selectable per policy, whose
  cost and latency are charged to the request and reported in the decision record; and a
  k-NN similarity estimator over labelled examples. The rule-based estimator stays the
  default.
- **Simulator:** answers the gateway's classifier and judge prompts from its oracle.

**Requirement IDs covered by tests:** CCH-01, CCH-02, CCH-03, CCH-04, CCH-05, CCH-06,
CCH-07, CCH-08, CCH-09, RTE-03 (classifier), RTE-04, RTE-06, RTE-16 (classifier cost
and latency), CST-01 (overhead).

**Exit scenarios**

| Scenario | Result |
| --- | --- |
| 4 Cascade | Pass. The simulator's tier-1 model returns malformed JSON for a JSON-schema request. The gateway records `qwen-4b` as `validation:json_schema`, escalates and serves `qwen-9b` (reason `cascade_escalated`). The decision record shows both attempts, and `X-Gw-Cost-Usd` equals the sum of both attempts' costs (the failed one is in `overhead_usd`). |
| 5 Caching | Pass. Corpus replay with 20% exact repeats plus the paraphrases: **27.3% hit rate** (94 exact, 84 semantic, 0 wrong semantic answers), quality 100.00. Every hit billed $0 with cost avoided recorded. `bypass` skipped the cache and called the model; `refresh` re-populated it. |

**Notes**

- The semantic threshold (0.80 in the demo config) was chosen for the simulator's
  bag-of-words embeddings. With `nomic-embed-text` on LM Studio it needs re-tuning. The
  eval recommendations in M7 produce it.
- The entity guard is a gateway design choice beyond the spec's wording. Without it, the
  same replay served wrong answers on 112 of 284 semantic hits at threshold 0.85.
- The k-NN estimator scored 97.5% tier accuracy on held-out corpus prompts, using the
  simulator embeddings.

## M6 — Formats and data control ✅

**Shipped**

- **Anthropic-compatible endpoints:** `POST /v1/messages` (non-streaming and streaming,
  tools, system prompts as string or blocks, images, multi-turn, stop sequences) and
  `POST /v1/messages/count_tokens`. Errors use Anthropic's shape (with the gateway code),
  and `/v1/models` answers in Anthropic's shape when `anthropic-version` is sent.
- **Translation in both directions between the OpenAI and Anthropic formats:** requests,
  responses, tool calls, tool results, stop reasons, usage and streaming events.
  Anthropic-to-Anthropic traffic passes through losslessly (raw body and raw events).
- **Anthropic-format upstream adapter** for the Anthropic API or LM Studio's
  Anthropic-compatible endpoint. The simulator now also serves `/v1/messages`, with the
  same faults.
- **`model_aliases`** (`claude-* → auto`), so Claude Code and the Anthropic SDKs need only
  a base URL and a key.
- **Data residency:** sensitivity labels (`X-Gw-Sensitivity` or `metadata.gateway`) map to
  allowed residency tags. Unknown labels are rejected, and residency is a hard constraint
  that fallback never relaxes.
- **PII and secret detection:** email, phone, card numbers (Luhn-checked), Aadhaar
  (Verhoeff-checked), PAN and API keys. There is a mode per tenant:
  - `log` records the types only, never the values.
  - `redact` sends a redacted copy to non-local deployments while local ones get the
    original.
  - `block` drops non-local deployments, or returns 403 `policy_blocked` if nothing local
    remains.
- **Caps:** request bytes and message count per key, tenant and globally (rejected), and
  max output tokens (clamped).
- **Opt-in body logging:** off by default. Bodies are redacted, then AES-256-GCM encrypted
  with `storage.body_key`, and kept for a retention period with purge. Enabling body logging
  without a key is a configuration error.
- **Pre/post-request webhooks** with a timeout, fail-open/closed behaviour and the SSRF
  guard.

**Requirement IDs covered by tests:** API-02, API-06, API-07, API-10, GRD-01, GRD-02,
GRD-03, GRD-04, GRD-05, NFR-12, NFR-20 (Anthropic Python SDK, Claude Code).

**Exit scenarios**

| Scenario | Result |
| --- | --- |
| 1 Drop-in (Anthropic part) | Pass. The official `anthropic` Python SDK (2.3.0) runs against the real binaries: messages with system blocks, streaming, tool use plus the tool_result round trip, streaming tool use, images, and error mapping. It works over an OpenAI-format upstream (translated) and an Anthropic-format upstream (pass-through). The full scenario suite (21 tests) passed. |
| 11 Data residency and PII | Pass. With a cheaper external "cloud" deployment configured, a `confidential` request has only local candidates, and the cloud one is excluded for `residency`. A prompt with an email and a PAN logs `pii_detected: [email, pan]` in log-only mode, and the values never appear in the log. Redact and block modes are covered by separate tests. |
| 17 Claude Code through the gateway | Pass with the **real Claude Code CLI (2.1.42)** in this container. It ran headless (`claude -p`) with `ANTHROPIC_BASE_URL` set to the gateway, its key, and `ANTHROPIC_CUSTOM_HEADERS: X-Gw-Sensitivity: confidential`. The session made a tool call and then answered, both on local `sima/qwen-27b` (~20.6k prompt tokens each), with spend attributed to its key. Note: the answers came from the simulator. A real coding session on LM Studio models still needs to be run on the Mac. |

**Bugs found by running Claude Code**

Claude Code sends `max_tokens: 32000` with a ~21–31k-token prompt. The router treated the
client's max_tokens as a hard requirement and excluded every deployment, so the request
failed with `no_eligible_model`. Fix: max_tokens is clamped per attempt to the
deployment's limit and is no longer a reason to exclude it.
