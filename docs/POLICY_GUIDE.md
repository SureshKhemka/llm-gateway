# Policy authoring guide

A routing policy decides which deployment serves a request. Policies are versioned:
files define version 1 (or the `version` you give), and the admin API / `gwctl
policies apply` adds new inactive versions that you activate after an eval passes.

## 1. Which policy applies

For the generic virtual model `auto`, the first policy found wins:

1. the key's `scopes.policy`
2. the project's `policy`
3. the tenant's `policy`
4. the virtual model's `policy`
5. `defaults.policy`
6. the built-in `default` (min_cost)

Other virtual models (`auto-fast`, `auto-cheap`, or ones you define) always use their
own policy. Requests for a concrete deployment (`model: sima/qwen-4b`) skip routing but
still go through scopes, budgets, guardrails and fallbacks (within the same pool).

Request hints (headers, see [API.md](API.md)) only **narrow** the policy: a client can
ask for a higher minimum tier, a lower cost ceiling or fewer providers, never the
reverse. Key scopes are hard limits on top of any policy.

The route decision for every request records the policy, its version, its source
(`key`, `tenant`, …), each candidate with its estimated cost and latency, and the
reason each excluded deployment was excluded. Use `POST /v1/route/explain` or the
console playground to see it without calling a model.

## 2. Anatomy of a policy

```yaml
policies:
  - name: support-bot
    description: Cheapest model that meets the estimated difficulty; escalate on bad output.
    objective: min_cost            # min_cost | min_latency | max_quality | weighted
    weights: {cost: 0.6, latency: 0.2, quality: 0.2}   # only for objective: weighted
    min_tier: 1                    # hard floor (0..3)
    max_tier: 2                    # hard ceiling
    max_cost_usd: 0.02             # per-request worst-case estimate
    latency_target_ms: 4000
    allowed_providers: [sima, simb]
    denied_providers: [cloud]
    residency: [local]             # deployments must carry one of these tags
    expected_output_tokens: 300    # used for cost estimates when max_tokens is absent
    max_attempts: 3                # fallbacks after the first attempt fails
    baseline: sima/qwen-27b        # what savings are measured against
    difficulty:
      estimator: rules             # rules (default) | similarity | classifier
      thresholds: [0.2, 0.45, 0.7] # score cut-offs: tier 1 from 0.2, tier 2 from 0.45, tier 3 from 0.7
    cascade:
      enabled: true
      max_escalations: 2
      validators: [non_empty, not_truncated, no_refusal, json_schema]   # + judge (opt-in)
      judge: sima/qwen-27b
      judge_min_score: 0.7
      buffer_streams: false        # true: validate streamed answers before sending (adds latency)
    degrade:                       # budget-driven tier caps (soft)
      - {at_pct: 80, max_tier: 1}
      - {at_pct: 95, max_tier: 0}
    cache:
      semantic_threshold: 0.85     # override the global threshold for this policy
      allow_tools: false
      disabled: false
    split:                         # A/B: share of traffic routed by another policy
      - {policy: support-bot-v2, weight: 10}
    shadow:                        # mirror a sample to a candidate; responses are discarded
      {policy: support-bot-v2, sample_rate: 0.05}
```

### Objectives

| Objective | Picks | Use for |
|---|---|---|
| `min_cost` | the cheapest eligible deployment at or above the difficulty tier | default; most traffic |
| `min_latency` | the fastest eligible deployment (observed TTFT + throughput) within `max_cost_usd` | interactive UIs |
| `max_quality` | the highest tier within `max_cost_usd` | high-stakes requests |
| `weighted` | the best weighted score of normalised cost, latency and tier | explicit trade-offs |

### Difficulty

Difficulty is a **soft floor**: the router prefers deployments at or above the
estimated tier, and only uses lower tiers when nothing at the floor is eligible (route
reason `below_difficulty_tier`). `min_tier` is the hard floor.

* `rules` (default, no model call): prompt length, code, maths, multi-step and
  reasoning cues, structured-output and tool requirements, images, conversation depth.
* `similarity`: k-nearest neighbours over labelled examples (`examples`: JSONL with
  `prompt` and `difficulty`), embedded with `embedder`.
* `classifier`: asks a small model (`classifier` deployment) to rate the prompt. Costs a
  request per call; the cost is attributed to the tenant.

Tune `thresholds` with an eval (see [EVAL_GUIDE.md](EVAL_GUIDE.md)): the recommendation
endpoint proposes thresholds that hit a target quality at the lowest cost.

### Cascade

With `cascade.enabled`, the router starts at the cheapest eligible tier and escalates
when a validator fails. Deterministic validators are free:

| Validator | Fails when |
|---|---|
| `non_empty` | the answer is empty or whitespace |
| `not_truncated` | `finish_reason` is `length` |
| `no_refusal` | the answer is a refusal ("I can't help with that") |
| `json_schema` | the request asked for a JSON schema and the answer doesn't match |
| `judge` | the judge model scores below `judge_min_score` (opt-in; costs a judge call) |

Every attempt is billed and visible in the trace and route decision. Streaming
responses are validated after they finish unless `buffer_streams: true`.

### Budget degradation

`degrade` steps cap the tier when a budget covering the request passes `at_pct` of its
hard limit. The cap is soft: deployments above it move to the end of the candidate list
instead of being excluded, so requests that need a capability only a higher tier has
still succeed. At 100% the hard limit returns `budget_exceeded` (402).

## 3. Lifecycle of a change

```sh
gwctl policies apply -f support-bot.yaml           # new inactive version N
gwctl evals run -f run.yaml --wait                 # compare version N with the active one
gwctl policies activate support-bot --version N --eval-run run_abc
gwctl policies rollback support-bot                # back to the previous version
gwctl policies diff support-bot                    # what changed between versions
```

Activation requires an eval run in which version N's quality is within
`evals.gate_max_drop` points of the active version on the same suite. Without one,
`--override-reason` is required and is audited. Split and shadow let you try a version
on live traffic before activating it.

## 4. Patterns

* **Cheap by default, expensive when needed:** `objective: min_cost` + `rules`
  difficulty + `cascade` with deterministic validators. This is the demo's default and
  saved 50% of cost at equal quality on the replayed workload.
* **Confidential data stays local:** `guardrails.sensitivity: {confidential: [local]}`
  and send `X-Gw-Sensitivity: confidential`, or set `residency: [local]` on the policy.
* **Interactive coding assistant (Claude Code):** a virtual model or alias with
  `objective: min_latency`, `min_tier: 2`, `capabilities` including `tools`.
* **Batch jobs:** `objective: min_cost`, `max_tier: 1`, a separate project with its own
  budget.
* **Protect a local GPU:** set `concurrency` and `queue_wait` on the deployment; the
  router queues by priority, then falls back to another pool.
