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
