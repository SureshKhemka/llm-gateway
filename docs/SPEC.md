# Cost-Aware LLM Gateway — Product Spec

Oct 31, 2026 · @Suresh Khemka

## 1. Overview

The Cost-Aware LLM Gateway is a self-hosted, OpenAI- and Anthropic-compatible endpoint that serves each request with the cheapest model able to meet its quality and latency target, and enforces spend limits per team. It is a production product, not a demo: multi-tenant, observable, resilient and governed by configuration.

This spec states what the product must do and how well. It deliberately says nothing about internal architecture, components, languages or storage; those are decided in a separate design document.

**Requirement keywords.** MUST, SHOULD and COULD follow RFC 2119 meaning. Each requirement has a stable ID (for example RTE-04) so tests and design decisions can trace back to it.

### Problem

- Applications hard-code one model each, so easy requests (greetings, extraction, classification) pay frontier-model prices.
- When that model or provider fails, the application fails; there is no shared failover.
- Nobody can answer "what did team X spend on LLMs this week, and on what?" without custom work.
- There is no central place to cap spend, rate-limit, or keep confidential data on approved models.
- Switching or adding a provider means code changes in every application.

### What the product does

- Accepts requests in OpenAI or Anthropic format, so existing SDKs work by changing only the base URL and key.
- Routes each request across a catalog of local and cloud models by difficulty, cost, latency, capability and policy.
- Escalates from a cheap model to a stronger one only when the cheap answer fails validation.
- Caches exact and semantically similar requests.
- Fails over across models and providers when one is slow or down.
- Enforces budgets, quotas and rate limits per tenant, project, key and end user.
- Attributes every token and every rupee or dollar to who spent it, and reports savings against a baseline.
- Proves its routing choices with evaluation suites, and tunes policy from the results.

### Who it is for

Platform teams who run shared LLM access for many internal applications, the developers who call it, and the finance and security people who need to govern it. Section 3 details the personas.

## 2. Goals, non-goals and success metrics

The product succeeds when it cuts blended LLM cost by at least 40% against an always-strongest-model baseline while losing no more than 2 points of quality on the reference eval suite.

### Goals

1. **Lower cost at equal quality.** Serve each request on the cheapest model that meets its quality floor.
2. **Drop-in adoption.** Existing OpenAI and Anthropic SDK clients work by changing only base URL and API key.
3. **No avoidable failures.** A failing model or provider never causes a client-visible error while a healthy eligible alternative exists.
4. **Hard spend governance.** No tenant, project, key or user exceeds a hard budget.
5. **Full attribution.** Every request records who sent it, which model served it, why, how long it took and what it cost.
6. **Explainable, evidence-based routing.** Every routing decision can be explained, and routing policy is tuned from eval data.

### Non-goals

- Hosting or serving models itself; it calls inference servers and providers.
- Training, fine-tuning or distillation.
- Prompt management, prompt templates or a prompt CMS.
- Agent orchestration or workflow execution.
- General API gateway duties for non-LLM traffic.
- Invoicing external customers; it produces chargeback reports only.

### Success metrics

| Metric | Target | Measured by |
| --- | --- | --- |
| Cost reduction vs baseline policy | ≥ 40% | Workload replay of the reference corpus (section 8) |
| Quality delta vs baseline policy | ≤ 2 points on a 100-point scale | Reference eval suite, same corpus |
| Gateway-added latency, non-streaming, cache miss | p95 ≤ 15 ms, p99 ≤ 40 ms | Load test against simulated provider |
| Gateway-added time to first token, streaming | p95 ≤ 20 ms | Load test against simulated provider |
| Cache hit rate on reference replay | ≥ 25% (exact + semantic) | Workload replay |
| Request success during a single-provider outage | ≥ 99.9% of requests that have another eligible model | Chaos scenario 8 |
| Hard-budget overshoot | 0 | Budget scenario 6, repeated 100 times under concurrency |
| Per-request cost accuracy | within 1% of provider-reported cost | Reconciliation test |

## 3. Personas and glossary

Five personas use the product; the platform operator and the application developer are primary.

### Personas

| Persona | Wants | Main touchpoints |
| --- | --- | --- |
| Platform operator (primary) | Configure providers, models, prices and policies; keep the gateway healthy; prove savings | Config files, admin API, console, dashboards, alerts |
| Application developer (primary) | Drop-in endpoint, predictable behaviour, per-request control, clear errors and cost per call | Client API, response headers, route-explain, playground |
| Tenant admin / team lead | Issue keys, set team budgets, see team spend and quality | Console, admin API, budget alerts |
| FinOps / finance | Spend by team, project and model; savings; chargeback | Cost reports, CSV export |
| Security and compliance reviewer | Know where data goes, who changed what, that secrets and prompts are protected | Audit log, data-residency rules, logging settings |

### Glossary

| Term | Meaning |
| --- | --- |
| Provider | An upstream endpoint that runs models: LM Studio, vLLM, Ollama, OpenRouter, OpenAI, Anthropic, or the simulated provider |
| Model deployment | One model at one provider, with its capabilities, limits and price card |
| Pool | Two or more deployments of the same model treated as interchangeable |
| Virtual model | A name clients request (for example `auto`, `auto-fast`, `auto-cheap`) that the gateway resolves to a deployment per request |
| Price card | Per-million-token prices for input, cached input, output and reasoning tokens, with an effective date |
| Quality tier | An operator-assigned capability level per deployment (tier 0 = smallest, tier 3 = strongest) |
| Difficulty score | The gateway's estimate of how capable a model a request needs, mapped to a minimum tier |
| Candidate set | Deployments that satisfy every hard constraint for a request |
| Routing policy | A named, versioned set of rules that picks a deployment from the candidate set |
| Cascade | Try a cheap deployment first and escalate to a stronger one only if the answer fails validation |
| Fallback | Retry on another candidate after an upstream failure |
| Route decision record | The stored explanation of how one request was routed |
| Baseline model | The model a tenant would have used without the gateway; used to compute savings |
| Tenant | A team or business unit; owns projects, keys, budgets and data |
| Virtual API key | A gateway-issued key that identifies tenant, project and scopes; never a provider key |
| Soft / hard budget | A spend threshold that alerts or degrades (soft) or blocks (hard) |
| Exact / semantic cache | Reuse of a stored response for an identical request, or for one whose meaning is similar enough |
| Eval suite | A labelled prompt set with graders, used to measure quality per model and policy |
| Shadow traffic | Requests copied to a candidate model or policy whose output is recorded but not returned |

## 4. Functional requirements: traffic

These requirements cover what happens to a request: the client API, the model catalog, routing, resilience and caching. Routing (4.3) is the heart of the product.

### 4.1 Client API

| ID | Requirement | Priority |
| --- | --- | --- |
| API-01 | Expose an OpenAI-compatible Chat Completions endpoint (`/v1/chat/completions`) with streaming (SSE), tool/function calling, `response_format` (JSON mode and JSON schema), system prompts, multi-turn messages, stop sequences, sampling parameters and image inputs. | MUST |
| API-02 | Expose an Anthropic-compatible Messages endpoint (`/v1/messages`) with streaming, tool use, system prompts and image inputs. | MUST |
| API-03 | Expose an OpenAI-compatible Embeddings endpoint (`/v1/embeddings`). | MUST |
| API-04 | Expose `/v1/models`, listing only the virtual and concrete models the caller's key may use. | MUST |
| API-05 | Expose an OpenAI-compatible Responses endpoint (`/v1/responses`) for stateless requests. | SHOULD |
| API-06 | Serve a request in either client format from a provider that speaks either format, translating messages, tool calls, stop reasons, usage and streaming events without loss. | MUST |
| API-07 | Let clients request a concrete model (for example `lmstudio/qwen3-4b`) or a virtual model (`auto`, `auto-fast`, `auto-cheap`, or operator-defined). | MUST |
| API-08 | Accept per-request routing hints as headers and as an equivalent body metadata field: max cost per request, latency target, minimum quality tier, required capabilities, allowed or denied providers, cache bypass, session ID, end-user ID and free-form tags. | MUST |
| API-09 | Return on every response: request ID, served model, provider, policy name and version, route reason code, cache status (exact hit, semantic hit, miss, bypass), input and output tokens, cost of this request, remaining budget and number of fallback attempts. | MUST |
| API-10 | Return errors in the client format's error shape with stable gateway codes: `budget_exceeded`, `rate_limited`, `no_eligible_model`, `upstream_unavailable`, `policy_blocked`, `context_too_long`, `invalid_request`, `unauthorized`, `forbidden`; with matching HTTP statuses. | MUST |
| API-11 | Cancel the upstream request when the client disconnects, and bill only tokens actually generated. | MUST |
| API-12 | Support an idempotency-key header: a repeat with the same key within 24 hours returns the original response and is not billed again. | SHOULD |
| API-13 | Always know token usage for a request, counting tokens itself when the provider omits usage and flagging such counts as estimated. Include usage in the final stream chunk when the client asks for it. | MUST |

### 4.2 Providers and model catalog

| ID | Requirement | Priority |
| --- | --- | --- |
| PRV-01 | Support any OpenAI-compatible provider (covers LM Studio, vLLM, Ollama, OpenRouter, OpenAI) and the Anthropic API. | MUST |
| PRV-02 | Support Google Gemini, AWS Bedrock and Azure OpenAI. | COULD |
| PRV-03 | Describe each deployment by: ID, provider, upstream model name, capabilities (chat, tools, vision, JSON schema, embeddings, reasoning), context window, max output tokens, price card, quality tier, latency profile, concurrency limit, data-residency tags (for example `local`, `in-region`, `external`) and enabled flag. | MUST |
| PRV-04 | Let local deployments carry a configurable price (zero, or an amortised compute cost per million tokens) so cost trade-offs stay meaningful. | MUST |
| PRV-05 | Version price cards with effective dates; a cost record always uses the price in effect when the request ran. | MUST |
| PRV-06 | Discover models from providers that list them (including LM Studio's `/v1/models`) and show new ones as unregistered until an operator approves them and sets their metadata. | SHOULD |
| PRV-07 | Add, change, disable or remove providers and deployments at runtime, without restart and without dropping in-flight requests. | MUST |
| PRV-08 | Store provider credentials only as secret references; never return them from any API, console page or log. | MUST |
| PRV-09 | Balance load across a pool by least in-flight requests or configured weights. | SHOULD |
| PRV-10 | Track whether a local deployment is loaded (warm) or unloaded (cold); never send a latency-sensitive request to a cold deployment while a warm eligible one exists. | SHOULD |

### 4.3 Routing

For a virtual model, the gateway first narrows to deployments that can serve the request at all, then picks one by policy, then optionally escalates.

| ID | Requirement | Priority |
| --- | --- | --- |
| RTE-01 | Build the candidate set from enabled, healthy deployments that meet every hard constraint: capabilities the request needs (tools present means tool support; images mean vision; JSON schema means structured output), context window ≥ prompt tokens + requested output tokens, key and tenant allow-lists, data-residency rules and request hints. | MUST |
| RTE-02 | Return `no_eligible_model` with the list of exclusion reasons when the candidate set is empty. | MUST |
| RTE-03 | Estimate request difficulty and map it to a minimum quality tier. Support at least: rule-based signals (prompt length, code, tools, reasoning cues, conversation depth, explicit hint) and a small-model classifier. Selectable per policy. | MUST |
| RTE-04 | Support difficulty estimation by similarity to labelled example prompts. | SHOULD |
| RTE-05 | Pick among candidates by the policy's objective: minimise cost subject to quality floor and latency target (default); minimise latency subject to max cost; maximise quality subject to max cost; or a weighted score. | MUST |
| RTE-06 | Support cascade mode: try the cheapest eligible deployment, validate the answer (JSON-schema validity, empty or truncated output, refusal detection, optional judge-model check), and escalate one tier on failure up to a configured limit. Record every attempt and the total cost. Cascade applies to non-streaming requests, and to streaming requests only when the policy buffers output. | MUST |
| RTE-07 | Use rolling observed latency (time to first token, tokens per second) per deployment, not only the static profile. | MUST |
| RTE-08 | Shift routing to cheaper tiers automatically as a scope approaches its budget, at operator-set thresholds (for example 80% and 95%), before the hard stop. | MUST |
| RTE-09 | Keep policies declarative, named and versioned; assign them to a tenant, project, key or virtual model. Precedence: request hint, then key, then project, then tenant, then global default. | MUST |
| RTE-10 | Allow request hints only to narrow operator policy (for example lower max cost), never to widen it (raise max cost, enable a denied provider). | MUST |
| RTE-11 | Pin requests that share a session ID to the same deployment while it stays healthy and eligible. | SHOULD |
| RTE-12 | Split traffic by weight between policies or deployments for A/B tests, sticky by end-user or session ID. | SHOULD |
| RTE-13 | Mirror a sampled share of requests to a candidate policy or deployment as shadow traffic; record its output, cost and latency without returning it; charge it to a separate shadow budget. | SHOULD |
| RTE-14 | Store a route decision record per request: candidates, exclusions with reasons, difficulty score, estimated cost and latency per candidate, chosen deployment, policy version, cascade and fallback attempts. Retrievable by request ID. | MUST |
| RTE-15 | Offer a route-explain call that returns the decision for a request without calling any model and at no cost. | MUST |
| RTE-16 | Keep rule-based routing overhead at p95 ≤ 5 ms. Classifier or judge calls report their own latency and cost, charged to the request. | MUST |
| RTE-17 | Account for provider prompt-caching discounts when estimating cost. | COULD |

### 4.4 Resilience

| ID | Requirement | Priority |
| --- | --- | --- |
| RES-01 | Track health per deployment from active probes and live outcomes, as healthy, degraded or unhealthy. | MUST |
| RES-02 | Apply a circuit breaker per deployment with configurable error-rate and latency thresholds and half-open recovery. | MUST |
| RES-03 | On a retriable failure (connection error, timeout, 429, 5xx, upstream context-length error) fall back to the next-best candidate, up to a configured attempt count and total deadline. | MUST |
| RES-04 | Never switch models after the first token has been streamed to the client; end the stream with a well-formed error event instead. | MUST |
| RES-05 | Retry with exponential backoff and jitter, honouring upstream `Retry-After`. | MUST |
| RES-06 | Enforce connect, time-to-first-token, inter-token idle and total timeouts, configurable per deployment and overridable per request within bounds. | MUST |
| RES-07 | Enforce a concurrency limit per deployment with a bounded queue wait; overflow spills to the next candidate. | MUST |
| RES-08 | Never let fallback violate a hard constraint (budget, data residency, capability, allow-list). | MUST |
| RES-09 | Shed load by priority when the gateway itself is saturated, rejecting lower-priority keys first. | SHOULD |
| RES-10 | On shutdown, stop accepting new requests and drain in-flight streams up to a configured timeout. | MUST |

### 4.5 Caching

| ID | Requirement | Priority |
| --- | --- | --- |
| CCH-01 | Serve identical requests from an exact-match cache, keyed on everything that affects output, with a configurable TTL. | MUST |
| CCH-02 | Serve similar requests from a semantic cache when similarity ≥ a per-policy threshold and output-affecting parameters match. | MUST |
| CCH-03 | Scope cache entries to a tenant by default; share across tenants only when both opt in. | MUST |
| CCH-04 | Cache by default only requests that are deterministic enough (temperature ≤ a configurable threshold, no tools unless the policy allows) and not flagged no-cache. | MUST |
| CCH-05 | Return cache hits in the requested format, streamed if the client asked for streaming. | MUST |
| CCH-06 | Bill cache hits at zero or a configured cache price, and record cost avoided (what the routed model would have cost). | MUST |
| CCH-07 | Let clients bypass the cache or force a refresh per request. | MUST |
| CCH-08 | Let admins invalidate entries by tenant, policy, model or age; bound cache size. | MUST |
| CCH-09 | Guard against wrong semantic hits: optional cheap judge check before serving a hit, and sampling of hits for review. | SHOULD |

## 5. Functional requirements: governance

These requirements decide who may spend what, how spend is counted and where data may go. Currency is USD throughout; a display currency (for example INR) is a SHOULD in CST-09.

### 5.1 Tenancy, identity and access

| ID | Requirement | Priority |
| --- | --- | --- |
| TEN-01 | Organise access as organisation → tenant → project → virtual API key. | MUST |
| TEN-02 | Issue virtual API keys that are shown once at creation, stored only in non-reversible form, identifiable by a visible prefix, optionally expiring, rotatable with an overlap window and revocable with effect within 5 seconds. | MUST |
| TEN-03 | Scope each key: allowed models and virtual models, endpoints, providers, max cost per request, rate limits, priority, IP allow-list and default tags. | MUST |
| TEN-04 | Protect admin functions with separate admin credentials and roles: org admin, tenant admin, FinOps viewer, auditor. | MUST |
| TEN-05 | Support single sign-on via OIDC for console and admin API. | SHOULD |
| TEN-06 | Isolate tenants completely: no tenant can see another's requests, logs, cache entries, keys or spend. | MUST |
| TEN-07 | Attribute requests to an optional end-user ID for cost reporting and per-user limits within a tenant. | SHOULD |

### 5.2 Budgets, quotas and rate limits

| ID | Requirement | Priority |
| --- | --- | --- |
| BUD-01 | Set budgets at tenant, project, key and end-user level, over daily, weekly, monthly or custom periods, resetting in a configured time zone. | MUST |
| BUD-02 | Raise alerts and optionally degrade routing (RTE-08) at soft thresholds; block with `budget_exceeded` at the hard limit. | MUST |
| BUD-03 | Before calling upstream, reserve the request's worst-case cost (input tokens × input price + max output tokens × output price) against every applicable budget; reject if any would be exceeded; settle to actual cost afterwards. Concurrent requests must never jointly overshoot a hard budget. | MUST |
| BUD-04 | Enforce rate limits on requests per minute, input tokens per minute, output tokens per minute and concurrent requests, per key, project and tenant. Return 429 with `Retry-After` and standard rate-limit headers. | MUST |
| BUD-05 | Enforce budgets and limits consistently across all gateway instances; running more instances must not multiply any limit. | MUST |
| BUD-06 | Apply budget and limit changes within 10 seconds, without restart. | MUST |
| BUD-07 | Send threshold alerts to webhooks (generic JSON and Slack-compatible), once per threshold per period. | MUST |
| BUD-08 | Grant temporary budget increases with an expiry and an audit record. | SHOULD |

### 5.3 Cost accounting and reporting

| ID | Requirement | Priority |
| --- | --- | --- |
| CST-01 | Compute each request's cost from tokens actually used and the price card in effect, split into input, cached input, output and reasoning tokens. Include classifier, judge, cascade and fallback overhead in the same request's cost. | MUST |
| CST-02 | Mark cost as estimated whenever token counts are estimated (API-13). | MUST |
| CST-03 | Compute for every request what it would have cost on the scope's baseline model; report routing savings and cache savings separately. | MUST |
| CST-04 | Aggregate cost, tokens, requests and savings by time bucket and by tenant, project, key, end user, model, provider, policy and tag; serve via API and CSV export. | MUST |
| CST-05 | Make aggregates reconcile exactly with the sum of per-request records. | MUST |
| CST-06 | Retain per-request cost records for at least 13 months (configurable) and aggregates for at least 3 years. | MUST |
| CST-07 | Produce a monthly chargeback report per tenant. | SHOULD |
| CST-08 | Forecast period-end spend per scope from trailing usage. | COULD |
| CST-09 | Display costs in a configured secondary currency using an operator-set exchange rate. | SHOULD |

### 5.4 Guardrails and data handling

| ID | Requirement | Priority |
| --- | --- | --- |
| GRD-01 | Cap request size, message count and max output tokens per key and tenant. | MUST |
| GRD-02 | Enforce data-residency rules: requests tagged with a sensitivity level (for example `confidential`) route only to deployments carrying an approved residency tag (for example `local`). | MUST |
| GRD-03 | Store prompt and response bodies only when a tenant opts in, with a retention period; apply redaction before storage. Off by default. | MUST |
| GRD-04 | Detect secrets and PII (email, phone, card numbers, Aadhaar, PAN, API keys) in prompts bound for non-local deployments, with modes off, log-only, redact and block. | SHOULD |
| GRD-05 | Call operator-defined pre- and post-request webhooks for custom checks, with a timeout and a fail-open or fail-closed setting. | COULD |

## 6. Functional requirements: operations

These requirements make the gateway observable, provable and manageable. Routing policy must be justified by measured quality-versus-cost data, not intuition.

### 6.1 Observability

| ID | Requirement | Priority |
| --- | --- | --- |
| OBS-01 | Emit one structured event per request carrying: request ID, tenant, project, key prefix, end user, tags, requested and served model, provider, policy and version, route reason, candidate count, difficulty score, cache status, each cascade and fallback attempt with its error, token counts, cost, baseline cost, time to first token, total latency, tokens per second, status and error code. | MUST |
| OBS-02 | Expose metrics in Prometheus format: request rate, errors, gateway overhead, time to first token and total latency histograms, tokens, cost, savings, cache hit rate, fallback and escalation rates, circuit-breaker state, budget utilisation and queue depth; labelled by tenant, model, provider and policy with bounded label cardinality. | MUST |
| OBS-03 | Emit OpenTelemetry traces with spans for authentication, routing, cache lookup and each upstream attempt; continue incoming trace context; follow the OpenTelemetry GenAI semantic conventions. | MUST |
| OBS-04 | Export request traces to LangSmith and Langfuse when configured. | SHOULD |
| OBS-05 | Expose liveness and readiness endpoints; readiness is false until configuration is loaded and at least one provider is reachable. | MUST |
| OBS-06 | Keep an append-only audit log of every admin and configuration change: actor, time, object, before and after values, and reason where required. | MUST |
| OBS-07 | Ship importable dashboards for the metrics in OBS-02 and alert rules for the top failure modes. | SHOULD |

### 6.2 Evaluation and policy tuning

| ID | Requirement | Priority |
| --- | --- | --- |
| EVL-01 | Manage eval suites: prompts with reference answers or rubrics, tagged by category and labelled difficulty; importable from JSONL. | MUST |
| EVL-02 | Run a suite against any set of deployments and policies; record per item the quality score, cost and latency. | MUST |
| EVL-03 | Provide graders: exact and regex match, JSON-schema validity, embedding similarity and LLM-as-judge with a configurable judge model and rubric. | MUST |
| EVL-04 | Support custom graders via webhook. | SHOULD |
| EVL-05 | Report per run: the quality-versus-cost Pareto frontier across deployments and policies, per-category breakdown, and each policy's delta against baseline with confidence intervals. | MUST |
| EVL-06 | Report difficulty-estimator accuracy against labelled difficulty, as a confusion matrix. | SHOULD |
| EVL-07 | Recommend policy settings (difficulty-to-tier thresholds, semantic-cache threshold) that minimise cost at a target quality; an operator reviews and promotes the result as a new policy version. | SHOULD |
| EVL-08 | Gate promotion: a new policy version cannot activate unless its latest eval meets the configured quality floor relative to the active version; an override needs an admin and a recorded reason. | SHOULD |
| EVL-09 | Collect online quality signals: client feedback against a request ID, and asynchronous judge grading of a sampled share of production traffic; report quality by route. | SHOULD |
| EVL-10 | Replay a captured window of production requests (where body logging is opted in) against a candidate policy to estimate cost and quality change. | SHOULD |
| EVL-11 | Charge eval runs to their own budget and tag them so their spend is separable. | MUST |

### 6.3 Admin console and APIs

| ID | Requirement | Priority |
| --- | --- | --- |
| ADM-01 | Make every capability manageable through a versioned REST admin API with a published OpenAPI document. | MUST |
| ADM-02 | Provide a web console with: live overview (requests per second, spend today, savings, cache hit rate, errors), provider and deployment health, model catalog and price cards, policies (view, edit, diff, roll back), tenants, projects and keys, budgets and alerts, request explorer with route decision view, cost reports, eval runs with Pareto charts, and audit log. | MUST |
| ADM-03 | Provide a playground: send a prompt, see the route decision and each candidate's estimated cost, the response and the actual cost; compare responses across deployments side by side. | SHOULD |
| ADM-04 | Provide a CLI for keys, policies, budgets, config apply and eval runs. | SHOULD |
| ADM-05 | Enforce role permissions identically in console, admin API and CLI. | MUST |

### 6.4 Configuration

| ID | Requirement | Priority |
| --- | --- | --- |
| CFG-01 | Express all configuration (providers, deployments, price cards, policies, tenants, keys metadata, budgets, limits) as declarative files suited to version control; the same state is manageable through the admin API, with a documented rule for which source wins. | MUST |
| CFG-02 | Validate configuration on load with precise errors (file, path, reason); reject invalid configuration without affecting the running state. | MUST |
| CFG-03 | Reload configuration without restart or dropped requests. | MUST |
| CFG-04 | Read secrets only from environment variables or a secrets manager reference, never inline values. | MUST |
| CFG-05 | Show a diff of what a configuration change would do before applying it. | SHOULD |
| CFG-06 | Retain every policy version and roll back to any prior version in one action. | MUST |
| CFG-07 | Start with a minimal file naming one LM Studio endpoint and work with discovered models and default policies. | MUST |

## 7. Non-functional requirements

The gateway must add no more than 15 ms at p95 to a request and stay available at 99.95% when run as two or more instances. Latency targets exclude upstream model time and semantic-cache embedding time; throughput targets are measured against the simulated provider.

| ID | Category | Requirement | Target |
| --- | --- | --- | --- |
| NFR-01 | Performance | Gateway-added latency, non-streaming, cache miss, rule-based routing | p95 ≤ 15 ms, p99 ≤ 40 ms |
| NFR-02 | Performance | Gateway-added time to first token, streaming | p95 ≤ 20 ms |
| NFR-03 | Performance | Throughput per instance on 4 vCPU / 8 GB | ≥ 1,000 requests/s non-streaming; ≥ 5,000 concurrent streams |
| NFR-04 | Performance | Exact-cache hit latency end to end | p95 ≤ 10 ms |
| NFR-05 | Reliability | Availability of the gateway itself with ≥ 2 instances | 99.95% monthly |
| NFR-06 | Reliability | Loss of any one instance affects only requests in flight on it | 0 failed requests started after the loss |
| NFR-07 | Reliability | Usage and cost records survive crashes | Recovery point ≤ 60 s; records lost in that window are reconciled from upstream usage where available |
| NFR-08 | Reliability | Degraded dependencies | If cache or analytics are unavailable, requests still route and are billed; budgets fail closed by default, configurable |
| NFR-09 | Scalability | Horizontal scaling | Near-linear throughput to 10 instances |
| NFR-10 | Scalability | Catalog and tenancy size | 10,000 tenants, 100,000 keys, 500 deployments, 1,000 policy versions without degrading NFR-01 |
| NFR-11 | Security | Transport | TLS on all external interfaces; option to serve admin API on a separate listener |
| NFR-12 | Security | Data at rest | Secrets and stored bodies encrypted; keys stored non-reversibly |
| NFR-13 | Security | Standards | Admin API and console meet OWASP ASVS Level 2 |
| NFR-14 | Security | Outbound safety | Provider and webhook URLs protected against SSRF (deny internal ranges unless allow-listed) |
| NFR-15 | Security | Supply chain | No unpatched critical or high CVEs at release; SBOM published; signed release images |
| NFR-16 | Privacy | Logging | No prompt or response content, provider key or virtual key in any log by default |
| NFR-17 | Operability | Packaging | One container image and a Helm chart; one-command local run on macOS for development |
| NFR-18 | Operability | Upgrades | Rolling upgrade from version N−1 to N with zero downtime; versioned, reversible data migrations |
| NFR-19 | Operability | Backup | Documented and tested backup and restore of configuration, keys and cost records |
| NFR-20 | Compatibility | Clients | Verified with official OpenAI Python and Node SDKs, Anthropic Python and TypeScript SDKs, LangChain, and Claude Code via its Anthropic base-URL setting |
| NFR-21 | Quality | Tests | ≥ 85% line coverage on routing, budget, cost and cache logic; contract tests per provider type; load, soak (24 h) and chaos suites in CI |
| NFR-22 | Quality | Traceability | Every MUST requirement maps to at least one automated test by ID |
| NFR-23 | Documentation | Deliverables | Operator guide, API reference, policy authoring guide, eval guide and runbooks for every shipped alert |

## 8. Local test environment and demo scenarios

Every key feature must be demonstrable on one MacBook Pro (M5, 48 GB) running LM Studio with five or six small models loaded at once, plus a fault-injecting simulated provider. No cloud account is needed; a cloud provider is an optional extra.

### 8.1 Environment requirements

| ID | Requirement | Priority |
| --- | --- | --- |
| ENV-01 | Use LM Studio 0.4 or later as the primary provider, via its OpenAI-compatible and Anthropic-compatible endpoints, with several models loaded concurrently. | MUST |
| ENV-02 | Document the LM Studio settings the demo needs: JIT auto-evict disabled (it keeps only one JIT-loaded model in memory by default), models preloaded, max concurrent predictions set per model, and context lengths kept to what the demo needs so all models fit. | MUST |
| ENV-03 | Assign the local models synthetic price cards that mimic real-world price ratios between tiers, so savings are visible in dollars. | MUST |
| ENV-04 | Ship a simulated provider as part of the product's test tooling. It speaks OpenAI and Anthropic formats, runs at least two named instances, and injects on command: added latency, slow token rate, error rates, 429s with `Retry-After`, timeouts, mid-stream disconnects, malformed JSON output and missing usage. | MUST |
| ENV-05 | Ship a workload generator that replays a labelled corpus of ≥ 500 prompts across categories (FAQ and greetings, extraction, classification, summarisation, code, multi-step reasoning, tool use, JSON-schema output, image input, long context) with exact repeats and paraphrases, at configurable rate and tenant mix. | MUST |
| ENV-06 | Ship a reference eval suite drawn from the same corpus, with graders, for the quality-versus-cost comparison. | MUST |
| ENV-07 | Bring up and tear down the full demo (gateway, its dependencies, simulated provider, dashboards, seeded tenants and keys) with one command each. | MUST |
| ENV-08 | Run the same demo on a local kind cluster from the Helm chart, with LM Studio on the host. | SHOULD |
| ENV-09 | Add an optional cloud provider (OpenRouter or Anthropic) under a small hard budget to show hybrid local-plus-cloud routing. | COULD |
| ENV-10 | Switch existing clients (the email router and the Kubernetes drift workflow) to the gateway by changing only base URL and key. | SHOULD |

### 8.2 Suggested local model lineup

Any equivalent models work; what matters is one model per tier plus vision and embeddings. Approximate memory assumes 4-bit quantisation and short contexts.

| Role | Example model | Approx. memory | Synthetic price (input / output per 1M tokens) |
| --- | --- | --- | --- |
| Tier 0: classifier, trivial requests | Gemma 4 E2B | 2 GB | $0.05 / $0.20 |
| Tier 1: simple Q&A, extraction | Qwen 3.5 4B | 3 GB | $0.15 / $0.60 |
| Tier 2: reasoning, code, summarisation | Qwen 3.5 9B | 6 GB | $0.50 / $2.00 |
| Tier 3: hard requests, judge | Qwen 3.6 27B or Gemma 4 26B-A4B | 16–17 GB | $3.00 / $15.00 |
| Vision | Qwen 3 VL 8B | 5 GB | $0.50 / $2.00 |
| Embeddings: semantic cache, similarity | nomic-embed-text-v1.5 | < 1 GB | $0.02 input only |

Total is roughly 33 GB before KV cache, leaving headroom on 48 GB if contexts stay at 8K–16K tokens.

### 8.3 Demo scenarios

Each scenario is scripted, runs unattended and asserts its pass condition.

| # | Scenario | What happens | Pass when |
| --- | --- | --- | --- |
| 1 | Drop-in compatibility | OpenAI and Anthropic SDKs point at the gateway; run chat, streaming, tool calls, JSON schema, embeddings | All succeed unchanged; response headers show served model, cost and route reason |
| 2 | Difficulty routing | Send a greeting, an extraction, and a multi-step code task to `auto` | They land on tier 0, tier 1 and tier 2–3; route-explain shows why |
| 3 | Savings vs baseline | Replay 500 prompts under `auto`, then under an always-tier-3 baseline; run the eval suite on both | Cost down ≥ 40%, quality down ≤ 2 points, shown on the dashboard |
| 4 | Cascade | A JSON-schema request where tier 1 returns invalid JSON (forced via simulated provider) | Escalates to tier 2; decision record shows both attempts and total cost |
| 5 | Caching | Replay repeats and paraphrases; then send with cache bypass | Exact and semantic hits served at zero cost with cost avoided recorded; bypass skips cache |
| 6 | Budget degradation and hard stop | Tenant with a $0.50 monthly budget runs the replay | At 80% routing shifts cheaper; at 100% requests get `budget_exceeded`; alert webhook fires once per threshold; no overshoot |
| 7 | Rate limiting | Burst one key past its limit while another tenant runs normally | First key gets 429 with `Retry-After`; other tenant unaffected |
| 8 | Provider failover | Unload a model in LM Studio and kill a simulated provider mid-replay | Circuit opens, traffic moves, no client errors where alternatives exist; recovery after half-open |
| 9 | Latency-aware routing | Add 3 s latency to one deployment under a latency-target policy | Traffic moves off it within one minute |
| 10 | Mid-stream failure | Simulated provider drops a stream after 50 tokens | Client gets a clean error event; billed only for delivered tokens |
| 11 | Data residency and PII | A `confidential` request with a cloud provider configured; a prompt containing an email and a PAN | Only local deployments are candidates; PII detections appear in log-only mode |
| 12 | Capability routing | Requests with an image, with tools, and with a prompt longer than small models' context | Go to vision, tool-capable and long-context deployments respectively |
| 13 | Eval-driven tuning | Run the eval suite across all deployments; accept recommended thresholds; try to promote a worse policy; roll back | Pareto chart shown; new version active; worse version blocked by gate; rollback works |
| 14 | Hot reload | Change a price card and add a deployment while streams run | Takes effect within 10 s; no in-flight stream dropped |
| 15 | Observability | Inspect a cascaded request | Trace shows a span per attempt; per-request event and metrics match; request explorer shows the decision |
| 16 | Tenant isolation | Tenant A's admin queries tenant B's spend, requests and cache | All denied |
| 17 | Claude Code through the gateway | Point Claude Code's Anthropic base URL at the gateway | Coding session works on local models; spend attributed to its key |

LM Studio behaviour referenced here: [LM Studio local server guide (BestLLMfor, updated 2026-09-30)](https://bestllmfor.com/guide/lm-studio-serveur-api/).

## 9. Acceptance criteria and release scope

Version 1.0 is done when every MUST requirement passes its automated tests and demo scenarios 1–16 run green, unattended, from one command.

### 9.1 Acceptance criteria for 1.0

- [ ] Every MUST requirement is covered by at least one automated test, and a generated traceability report maps test to requirement ID.
- [ ] Demo scenarios 1–16 are scripted and pass unattended on the reference MacBook setup; scenario 17 passes if Claude Code is available.
- [ ] Success metrics in section 2 are met and recorded in a benchmark report.
- [ ] NFR-01 to NFR-04 are met in a published load-test report; the 24-hour soak shows no memory growth or error-rate drift.
- [ ] The chaos suite (provider kill, slowdown, 429 storm, mid-stream drop, instance kill) passes.
- [ ] Security checks pass: no unpatched critical or high CVEs, secret scan clean, threat model written and reviewed, ASVS Level 2 checklist complete.
- [ ] Rolling upgrade from the previous release candidate completes with zero failed requests.
- [ ] Documentation in NFR-23 is complete.

### 9.2 Milestones

Each milestone ships something demonstrable and builds on the last; the scenarios listed are its exit test.

1. **M1 Core proxy.** OpenAI chat, embeddings and models endpoints with streaming; OpenAI-compatible and LM Studio providers; static catalog; explicit model selection; per-request cost; config files with validation; simulated provider. Exit: scenario 1 (OpenAI part).
2. **M2 Routing.** Virtual models, constraint filtering, rule-based difficulty, cost-minimising objective, route decision record and route-explain, baseline savings. Exit: scenarios 2, 3, 12.
3. **M3 Resilience.** Health, circuit breakers, fallback, retries, timeouts, concurrency limits, latency-aware routing. Exit: scenarios 8, 9, 10.
4. **M4 Governance.** Tenants, virtual keys, admin roles, budgets with reservation, degradation, rate limits, alerts, audit log. Exit: scenarios 6, 7, 16.
5. **M5 Caching and cascade.** Exact and semantic cache, cascade escalation, classifier difficulty. Exit: scenarios 4, 5.
6. **M6 Formats and data control.** Anthropic endpoint and cross-format translation, data-residency rules, PII detection. Exit: scenarios 1 (Anthropic part), 11, 17.
7. **M7 Evals and tuning.** Eval suites, graders, Pareto reports, recommendations, promotion gates, A/B and shadow traffic. Exit: scenario 13.
8. **M8 Production hardening.** Console and playground, tracing, dashboards, hot reload, multi-instance consistency, Helm chart, load, soak, chaos and security suites, documentation. Exit: scenarios 14, 15 and all acceptance criteria.

## 10. Out of scope, assumptions and open questions

Seven decisions are still open; none blocks M1, but the first three should be settled before M2.

### Out of scope for 1.0

- Image, audio and video generation endpoints; realtime and voice APIs.
- Batch APIs and asynchronous job queues.
- Multi-region active-active deployment.
- Multiple independent organisations on one installation (SaaS-style); 1.0 serves one organisation with many tenants.
- Automatic scraping of provider price lists; operators enter prices.

### Assumptions

- Token counts reported by a provider are trusted when present.
- Operators assign quality tiers to deployments; evals (6.2) validate and adjust them.
- The local demo runs on macOS with LM Studio 0.4 or later; production targets Linux containers on Kubernetes.
- The design document, written next, will choose languages, components and storage against these requirements.

### Open questions

- [ ] Default difficulty estimator: rule-based, or small-model classifier, given the classifier's own latency on local hardware?
- [ ] Should cascade use a judge model by default, or only deterministic validators (schema, empty, refusal)?
- [ ] Budget-exceeded status: 402 or 429? Which do the target SDKs handle more gracefully?
- [ ] Default semantic-cache similarity threshold: fixed default, or derived from the first eval run?
- [ ] Which cloud providers, if any, are in 1.0 scope beyond the optional demo?
- [ ] Should end-user budgets (BUD-01) be MUST or SHOULD for 1.0?
- [ ] Product name.
