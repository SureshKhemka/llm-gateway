# API reference

The gateway exposes three surfaces:

| Surface | Base path | Auth | Contract |
|---|---|---|---|
| OpenAI-compatible inference | `/v1/...` | virtual API key (`Authorization: Bearer gw-…` or `X-Api-Key`) | OpenAI Chat Completions / Embeddings / Models |
| Anthropic-compatible inference | `/v1/messages` | virtual API key (`x-api-key` or bearer) | Anthropic Messages (`anthropic-version: 2023-06-01`) |
| Admin API | `/admin/v1/...` | admin key (bearer), role-based | [`docs/api/openapi.json`](api/openapi.json) (OpenAPI 3.1, generated from the route table) |

Health and metrics (`/livez`, `/readyz`, `/metrics`) need no credentials; put them on a
private network or the separate admin listener (`server.admin_listen`).

## Inference endpoints

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/chat/completions` | Streaming (`stream: true`, SSE) and non-streaming. Tools, JSON mode / `response_format: json_schema`, images. `model` is a deployment ID (`sima/qwen-4b`), a virtual model (`auto`) or an alias (`claude-*`). |
| `POST` | `/v1/messages` | Anthropic Messages, streaming and non-streaming, tools, images, system prompts. Same routing as chat completions. |
| `POST` | `/v1/messages/count_tokens` | Anthropic token counting (estimate). |
| `POST` | `/v1/embeddings` | Routed to an `embeddings`-capable deployment. |
| `GET` | `/v1/models` | Deployments, virtual models and aliases the key may use. |
| `POST` | `/v1/route/explain` | Dry run: the route decision for a request body without calling a model (candidates, exclusions with reasons, estimated cost). |
| `GET` | `/v1/route/decisions/{id}` | The stored route decision for a request ID (`X-Request-Id`). |
| `POST` | `/v1/feedback` | `{"request_id", "score" (0–1), "comment"}`: quality feedback for online evals. |

### Request hints (headers)

Hints can only narrow what the key's scopes and the policy allow.

| Header | Meaning |
|---|---|
| `X-Gw-Max-Cost-Usd` | Per-request cost ceiling; candidates whose worst-case estimate exceeds it are excluded. |
| `X-Gw-Latency-Target-Ms` | Prefer deployments expected to finish within this time. |
| `X-Gw-Min-Tier` | Minimum quality tier (0–3). |
| `X-Gw-Capabilities` | Comma-separated required capabilities (`tools`, `json_schema`, `vision`, `reasoning`, `embeddings`). |
| `X-Gw-Allow-Providers` / `X-Gw-Deny-Providers` | Provider allow/deny lists. |
| `X-Gw-Sensitivity` | Data sensitivity level; restricts residency (`guardrails.sensitivity`). |
| `X-Gw-Cache` | `bypass`, `refresh` or `no-store`. |
| `X-Gw-Session-Id` | Pins a conversation to one deployment while it stays healthy. |
| `X-Gw-End-User` | End-user ID for attribution and end-user budgets. |
| `X-Gw-Tags` | Comma-separated cost tags (chargeback). |
| `X-Gw-Timeout-Ms` / `X-Gw-Ttft-Timeout-Ms` | Total and first-token timeouts for this request. |
| `X-Gw-Priority` | Queue priority when a deployment is at its concurrency limit. |
| `Idempotency-Key` | Retries with the same key return the first result instead of re-billing. |

### Response headers

| Header | Meaning |
|---|---|
| `X-Request-Id` | Request ID (cost record, route decision, traces). |
| `X-Gw-Served-Model`, `X-Gw-Provider` | Deployment and provider that produced the answer. |
| `X-Gw-Policy`, `X-Gw-Policy-Version` | Policy that routed the request. |
| `X-Gw-Route-Reason` | Why this deployment won: `cheapest_meeting_tier`, `fastest_within_cost`, `highest_tier_within_cost`, `best_weighted_score` (by objective), `below_difficulty_tier`, `closest_to_latency_target` (no candidate meets the latency target), `cascade_start`, `budget_degraded`, `session_pinned`. |
| `X-Gw-Cache` | `exact_hit`, `semantic_hit`, `miss`, `bypass`. |
| `X-Gw-Cost-Usd`, `X-Gw-Baseline-Cost-Usd` | Cost of this request and of the baseline model (savings = difference). |
| `X-Gw-Input-Tokens`, `X-Gw-Output-Tokens`, `X-Gw-Usage-Estimated` | Billed usage; `Usage-Estimated: true` when the provider didn't report usage. |
| `X-Gw-Fallback-Attempts` | Attempts that failed before this one. |
| `X-Gw-Budget-Remaining-Usd` | Remaining budget of the tightest budget on the request. |
| `X-Gw-Error-Code` | Gateway error code (also in the error body). |

For streams, headers go out before the cost is known, so the cost headers
(`X-Gw-Input-Tokens`, `X-Gw-Output-Tokens`, `X-Gw-Cost-Usd`, `X-Gw-Baseline-Cost-Usd`,
`X-Gw-Usage-Estimated`, `X-Gw-Budget-Remaining-Usd`) are sent as HTTP trailers. OpenAI
streams also carry a `gateway` object (request ID, served model, route reason, cache
status, tokens, cost) on the final chunk.

### Errors

Errors use each API's own error shape (OpenAI `{"error": {...}}`, Anthropic
`{"type": "error", "error": {...}}`) with a gateway `code`:

| Code | HTTP | When |
|---|---|---|
| `budget_exceeded` | 402 | A hard budget is exhausted. 402, not 429, so SDKs don't auto-retry a dead budget. |
| `rate_limited` | 429 | Request/token rate limit; `Retry-After` is set. |
| `no_eligible_model` | 422 | Constraints exclude every deployment; the message lists the exclusions. |
| `upstream_unavailable` | 503 | Every eligible deployment failed or its circuit is open. |
| `overloaded` | 503 | Load shedding (`server.max_in_flight`) or queue wait exceeded. |
| `budget_unavailable` | 503 | Budget store unreachable and the budget fails closed. |
| `policy_blocked` | 403 | Guardrail (PII `block`, webhook, model not allowed for the key). |
| `context_too_long` | 400 | Prompt exceeds every eligible context window. |
| `invalid_request` | 400 | Malformed body or hint. |
| `unauthorized` / `forbidden` | 401 / 403 | Bad key / key or role lacks the scope. |
| `not_found` | 404 | Unknown model or object. |
| `conflict` | 409 | Admin API: object is file-managed, or a concurrent change. |
| `internal_error` | 500 | Bug; the request ID is in the logs. |

## Admin API

All endpoints below are under the admin key's role (`org_admin`, `tenant_admin`,
`finops_viewer`, `auditor`); `tenant_admin` is limited to its tenant. Write operations
are audited (actor, before, after, reason). The console and `gwctl` use the same API.
Objects defined in configuration files are read-only through the API (`409 conflict`);
API-managed objects are stored in Postgres and merged into the configuration.

`GET /admin/v1/openapi.json` serves the machine-readable document; this table is
generated from it.

| Method | Path | Summary | Permission | Roles |
|---|---|---|---|---|
| `GET` | `/admin/v1/audit` | Audit log | `audit:read` | auditor, org_admin |
| `GET` | `/admin/v1/budgets` | List budgets with current-period status | `budgets:read` | auditor, finops_viewer, org_admin, tenant_admin |
| `POST` | `/admin/v1/budgets` | Create or update an API-managed budget | `budgets:write` | org_admin, tenant_admin |
| `POST` | `/admin/v1/budgets/{id}/grants` | Grant a temporary budget increase | `budgets:write` | org_admin, tenant_admin |
| `POST` | `/admin/v1/cache/invalidate` | Invalidate cache entries | `cache:write` | org_admin, tenant_admin |
| `GET` | `/admin/v1/catalog` | Deployments, price cards and discovered (unregistered) models | `health:read` | auditor, finops_viewer, org_admin, tenant_admin |
| `POST` | `/admin/v1/catalog/{id...}` | Approve a discovered model with metadata | `catalog:write` | org_admin |
| `GET` | `/admin/v1/chargeback` | Monthly chargeback report per tenant | `costs:read` | finops_viewer, org_admin, tenant_admin |
| `GET` | `/admin/v1/config` | Effective configuration (secret references only) | `config:read` | auditor, org_admin |
| `POST` | `/admin/v1/config/diff` | Preview what reloading the configuration files would change | `config:read` | auditor, org_admin |
| `POST` | `/admin/v1/config/reload` | Re-read configuration files | `config:write` | org_admin |
| `GET` | `/admin/v1/costs` | Aggregate cost, tokens and savings (JSON or CSV) | `costs:read` | finops_viewer, org_admin, tenant_admin |
| `GET` | `/admin/v1/deployments` | List deployments with tier, prices and capabilities | `config:read` | auditor, org_admin |
| `POST` | `/admin/v1/deployments` | Create or update an API-managed deployment (price card, tier, capabilities) | `config:write` | org_admin |
| `DELETE` | `/admin/v1/deployments/{id...}` | Delete an API-managed deployment | `config:write` | org_admin |
| `POST` | `/admin/v1/evals/replay` | Replay captured production requests against a candidate policy | `evals:run` | org_admin |
| `GET` | `/admin/v1/evals/runs` | List eval runs | `evals:read` | org_admin, tenant_admin |
| `POST` | `/admin/v1/evals/runs` | Start an eval run across deployments and policy versions | `evals:run` | org_admin |
| `GET` | `/admin/v1/evals/runs/{id}` | An eval run's status and report | `evals:read` | org_admin, tenant_admin |
| `GET` | `/admin/v1/evals/runs/{id}/pareto.svg` | Quality-versus-cost chart for a run | `evals:read` | org_admin, tenant_admin |
| `POST` | `/admin/v1/evals/runs/{id}/recommend` | Recommend difficulty and semantic-cache thresholds | `evals:read` | org_admin, tenant_admin |
| `GET` | `/admin/v1/evals/suites` | List eval suites | `evals:read` | org_admin, tenant_admin |
| `POST` | `/admin/v1/evals/suites` | Create or replace an eval suite (JSON or JSONL body) | `evals:run` | org_admin |
| `GET` | `/admin/v1/health` | Deployment health and circuit breakers | `health:read` | auditor, finops_viewer, org_admin, tenant_admin |
| `GET` | `/admin/v1/keys` | List virtual keys (metadata only) | `keys:read` | auditor, org_admin, tenant_admin |
| `POST` | `/admin/v1/keys` | Create a virtual key; the secret is returned once | `keys:write` | org_admin, tenant_admin |
| `POST` | `/admin/v1/keys/{id}/revoke` | Revoke a key (effective on every instance within seconds) | `keys:write` | org_admin, tenant_admin |
| `POST` | `/admin/v1/keys/{id}/rotate` | Rotate a key with an overlap window | `keys:write` | org_admin, tenant_admin |
| `GET` | `/admin/v1/overview` | Live overview: requests/s, spend and savings today, cache hit rate, errors | `costs:read` | finops_viewer, org_admin, tenant_admin |
| `POST` | `/admin/v1/playground/explain` | Playground: route decision and per-candidate estimates (no model call) | `playground:use` | org_admin, tenant_admin |
| `POST` | `/admin/v1/playground/run` | Playground: run a prompt on one or more models side by side | `playground:use` | org_admin, tenant_admin |
| `GET` | `/admin/v1/policies` | Policies with their versions | `policies:read` | auditor, org_admin, tenant_admin |
| `POST` | `/admin/v1/policies` | Create a new (inactive) policy version | `policies:write` | org_admin |
| `POST` | `/admin/v1/policies/{name}/activate` | Activate a policy version (promotion gate) | `policies:write` | org_admin |
| `GET` | `/admin/v1/policies/{name}/diff` | Field-level diff between two policy versions | `policies:read` | auditor, org_admin, tenant_admin |
| `POST` | `/admin/v1/policies/{name}/rollback` | Roll back to a prior policy version | `policies:write` | org_admin |
| `GET` | `/admin/v1/providers` | List providers (secret references only) | `config:read` | auditor, org_admin |
| `POST` | `/admin/v1/providers` | Create or update an API-managed provider (api_key must be env: or file:) | `config:write` | org_admin |
| `DELETE` | `/admin/v1/providers/{id}` | Delete an API-managed provider | `config:write` | org_admin |
| `GET` | `/admin/v1/quality` | Online quality signals by route | `costs:read` | finops_viewer, org_admin, tenant_admin |
| `GET` | `/admin/v1/requests` | Recent requests (request explorer) | `requests:read` | org_admin, tenant_admin |
| `GET` | `/admin/v1/requests/{id}` | One request's cost record and route decision | `requests:read` | org_admin, tenant_admin |
| `GET` | `/admin/v1/tenants` | List tenants (tenant admins see their own) | `tenants:read` | auditor, finops_viewer, org_admin, tenant_admin |
| `POST` | `/admin/v1/tenants` | Create a tenant | `tenants:write` | org_admin |
| `POST` | `/admin/v1/tenants/{tenant}/projects` | Create a project in a tenant | `keys:write` | org_admin, tenant_admin |
| `GET` | `/admin/v1/whoami` | The calling admin's identity and permissions | `any admin` | auditor, finops_viewer, org_admin, tenant_admin |
