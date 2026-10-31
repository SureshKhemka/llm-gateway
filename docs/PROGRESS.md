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
