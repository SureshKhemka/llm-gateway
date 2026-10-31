# Cost-Aware LLM Gateway — build instructions

The product spec is @docs/SPEC.md. Requirement IDs in it (API-01, RTE-06, BUD-03, ...) are the contract: every test names the IDs it covers (NFR-22).

## Decisions already made

- **Stack:** Go for the gateway, simulated provider, workload generator, eval tooling and CLI. Postgres for durable state (config/policy versions, keys, cost records, audit log). Redis for cross-instance budgets, reservations, rate limits and cache (BUD-05).
- **Design doc first:** write `docs/DESIGN.md` (components, storage schema, request lifecycle, routing algorithm) before M1 code; keep it current.
- **Open-question defaults (spec §10):**
  - `budget_exceeded` returns HTTP 402 (OpenAI and Anthropic SDKs auto-retry 429, which would hammer a dead budget). `rate_limited` stays 429.
  - Default difficulty estimator: rule-based (meets RTE-16); small-model classifier is opt-in per policy.
  - Cascade uses deterministic validators (schema, empty/truncated, refusal) by default; judge model is opt-in.
  - Semantic-cache threshold: fixed configurable default, tunable via EVL-07.
  - End-user budgets: SHOULD for 1.0.
  - Cloud providers: Anthropic and OpenRouter only, optional (ENV-09).

## Build order

Build milestones M1–M8 in order (spec §9.2). For each milestone:

1. Implement it with tests covering its exit scenarios.
2. `go vet ./...` and `go test ./...` green.
3. Update `docs/PROGRESS.md`: what shipped, requirement IDs covered, what remains or needs real hardware.
4. Commit and push.

## Testing

- **Default suite (`go test ./...`)** runs against the simulated provider (ENV-04) and must pass in a cloud container with no GPU, no Docker and no LM Studio. Prefer in-process dependencies for tests (e.g. miniredis); if Postgres can't run in-process, document the environment setup script that installs it.
- **LM Studio integration suite:** build tag `lmstudio`, enabled by `LMSTUDIO_BASE_URL` (default `http://localhost:1234/v1`). Run on the user's Mac:
  `LMSTUDIO_BASE_URL=http://localhost:1234/v1 go test -tags lmstudio ./...`
  It discovers models via `/v1/models` and skips (with a clear message naming the missing model role) any scenario whose tier or capability isn't loaded.
- Load, soak and chaos suites (NFR-03, 24 h soak) ship as runnable scripts; their pass/fail must be recorded from a real run, not assumed.

## Local LM Studio inventory (user's MacBook Pro M5 Pro, 48 GB, as of 2026-10-31)

Loaded at `localhost:1234`: `google/gemma-4-31b`, `qwen/qwen3.8-27b`, `qwen3.6-27b-ud-mlx`, `gemma-4-26b-a4b-it-mlx`, `mistralai/devstral-small-2-2512`, `whiterabbitneo-13b-v1-mlx`, `text-embedding-nomic-embed-text-v1.5`.

That covers tier 3, code and embeddings. Tier 0–2 small models and a vision model (spec §8.2) are not loaded yet, so scenarios 2, 3 and 12 need them added before they can pass locally.

## Conventions

- Secrets only via environment variables or secret references (CFG-04, PRV-08); never commit keys; no prompts, responses or keys in logs (NFR-16).
- One-command local run on macOS (NFR-17); host-side LM Studio is reached from containers via `host.docker.internal`.
