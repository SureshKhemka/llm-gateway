# Eval guide

Evals answer "is this cheaper routing still good enough?" before a policy change ships,
and keep answering it on live traffic afterwards.

## 1. Suites

A suite is a set of items. Upload JSON (`{"id", "name", "items": [...]}`) or JSONL (one
item per line, `Content-Type: application/x-ndjson`, `?id=&name=`):

```sh
gwctl evals upload -f support-suite.json
curl -X POST "$GW/admin/v1/evals/suites?id=support&name=Support" -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/x-ndjson" --data-binary @items.jsonl
```

```json
{"id": "inv-1", "category": "extraction", "difficulty": 1,
 "prompt": "Extract the invoice number from: Invoice INV-2041 total 12,400",
 "grader": "exact", "reference": "INV-2041"}
```

| Field | Meaning |
|---|---|
| `prompt` or `messages` | the request (`system`, `images`, `tools`, `schema` optional) |
| `grader` | how the answer is scored (below) |
| `reference` | expected answer for `exact`, `contains`, `regex`, `embedding`, `tool_call` |
| `rubric` | instructions for `llm_judge` |
| `schema` | JSON Schema for `json_schema` |
| `difficulty` | your label (0–3); used to check and tune the difficulty estimator |
| `category`, `group` | reporting breakdowns |
| `paraphrase` | marks paraphrase pairs for semantic-cache threshold tuning |

### Graders

| Grader | Score |
|---|---|
| `exact` | 1 if the normalised answer equals `reference` |
| `contains` | 1 if `reference` appears in the answer |
| `regex` | 1 if `reference` (a regex) matches |
| `json_schema` | 1 if the answer is valid JSON matching `schema` |
| `tool_call` | 1 if the model called the expected tool with matching arguments |
| `non_empty` | 1 if the answer is non-empty and not truncated |
| `embedding` | cosine similarity to `reference` (`evals.embedder`) |
| `llm_judge` | 0–1 from `evals.judge` using `rubric` (judge cost is charged to the eval budget) |
| `webhook` | your service scores it (`evals.grader_webhook`) |

Prefer deterministic graders; use `llm_judge` for open-ended answers and spot-check it.

## 2. Runs

```yaml
# run.yaml
suite: support
tenant: team-a            # billed to the tenant's eval budget (budgets with scope: eval)
targets:
  - {kind: deployment, name: sima/qwen-4b}
  - {kind: deployment, name: sima/qwen-27b}
  - {kind: policy, name: default, version: 1}     # the active version
  - {kind: policy, name: default, version: 2}     # the candidate
baseline: sima/qwen-27b
concurrency: 8
```

```sh
gwctl evals run -f run.yaml --wait
gwctl evals status run_abc
```

`all_deployments: true` adds every chat deployment as a target. Eval traffic runs
through the normal pipeline (routing, cascade, guardrails) but is tagged `eval`, never
served from or written to the cache, and charged only to `eval`-scope budgets, so it
cannot eat a tenant's production budget.

The report gives, per target: quality (mean score × 100), cost, latency, per-category
and per-difficulty breakdowns, failed items, and a cost/quality Pareto chart
(`GET /admin/v1/evals/runs/{id}/pareto.svg`, also in the console).

## 3. Gating a policy change

`gwctl policies activate NAME --version N --eval-run RUN` succeeds only if the run
contains version N and the active version on the same suite, and N's quality is at most
`evals.gate_max_drop` points below the active version's. Otherwise activation is refused
(with the numbers), unless an `--override-reason` is given; overrides are audited.

## 4. Tuning

`POST /admin/v1/evals/runs/{id}/recommend` (`gwctl evals recommend RUN --policy NAME`)
needs a run with at least one deployment per tier plus a rule-based policy target. It
returns:

* **difficulty thresholds** that minimise predicted cost while predicted quality stays
  at or above the target (default: the baseline's quality minus `gate_max_drop`), with
  the predicted cost and quality of the current and proposed thresholds;
* **a semantic-cache threshold** (when `evals.embedder` is set): the lowest similarity at
  which no two prompts with different answers but the same salient entities collide,
  and the paraphrase recall at that threshold.

Apply the proposal as a new policy version, run the suite again, then activate.

## 5. Replay and online evals

* **Replay** (`POST /admin/v1/evals/replay`): builds a suite from captured production
  requests in a time window (tenants must opt in to body capture) and compares a
  candidate policy's cost and quality with what the originals cost.
* **Online sampling** (`evals.online_sample_rate`): a share of live responses is graded
  by the judge in the background; scores appear in `GET /admin/v1/quality` by policy and
  model, alongside client feedback from `POST /v1/feedback`.
* **Shadow traffic** (`policies[].shadow`): mirrors a sample of live requests to a
  candidate policy; its answers are discarded, its cost charged to a `shadow` budget,
  and its quality compared in the quality report.

## 6. Synthetic workload

`go run ./cmd/workload gen` writes a labelled corpus (`testdata/corpus/corpus.jsonl`,
544 items across extraction, classification, summarisation, code, reasoning, vision and
paraphrase pairs); `workload replay` drives it through a gateway and `workload compare`
reports cost and quality against a baseline. The simulated providers answer from the
corpus as an oracle, with per-tier accuracy, so routing decisions have measurable
quality consequences.
