# Cost-aware LLM gateway

An OpenAI- and Anthropic-compatible gateway that routes each request to the cheapest
model that can handle it, with budgets, caching, fallbacks, evals and a console.
Built for local models served by LM Studio, with optional cloud providers.

* **Drop-in:** OpenAI Chat Completions / Embeddings and Anthropic Messages (Claude Code
  works through it). Point an SDK's base URL at the gateway and use `model: auto`.
* **Routing:** difficulty estimation, capability and context checks, cost/latency/quality
  objectives, cascades with validators, budget-driven degradation, A/B and shadow.
* **Governance:** tenants, projects, virtual keys, hard budgets with reservations (no
  overshoot), rate limits, audit log, chargeback.
* **Resilience:** health probes, circuit breakers, retries with backoff, fallbacks,
  load shedding, graceful drain.
* **Caching:** exact and semantic, tenant-scoped, with an entity guard against
  look-alike prompts.
* **Evals:** suites, graders, Pareto reports, threshold recommendations, gated policy
  activation, replay and online sampling.
* **Operations:** Prometheus metrics, Grafana dashboard, alert rules with runbooks,
  OpenTelemetry traces, Helm chart, admin API (OpenAPI), console, `gwctl`.

## Quick start

```sh
deploy/demo/up.sh        # Docker: gateway, Redis, Postgres, simulators, Prometheus, Grafana
# or: DEMO_NO_DOCKER=1 deploy/demo/up.sh
```

It prints the URLs and generated keys. Then:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="<printed key>")
r = client.chat.completions.create(model="auto", messages=[{"role": "user", "content": "hello"}])
```

Add LM Studio on the host with `LMSTUDIO=1 deploy/demo/up.sh` (settings:
[docs/LMSTUDIO.md](docs/LMSTUDIO.md)). Stop with `deploy/demo/down.sh`.

## Documentation

| | |
|---|---|
| [Operator guide](docs/OPERATOR_GUIDE.md) | install, configure, observe, scale, back up |
| [API reference](docs/API.md) | endpoints, hint headers, errors; admin API ([OpenAPI](docs/api/openapi.json)) |
| [Policy authoring](docs/POLICY_GUIDE.md) | how routing decisions are made and tuned |
| [Evals](docs/EVAL_GUIDE.md) | suites, graders, gating, tuning |
| [Runbooks](docs/runbooks/) | one per shipped alert |
| [Security](docs/SECURITY.md) | threat model, ASVS L2 checklist |
| [Design](docs/DESIGN.md) | architecture, storage, routing algorithm |
| [Progress](docs/PROGRESS.md) | what shipped per milestone, measured results, open items |
| [Traceability](docs/TRACEABILITY.md) | spec requirement → tests |

## Development

```sh
make test          # default suite: no GPU, Docker or LM Studio needed
make race          # with the race detector
make scenarios     # official SDKs, Claude Code and a browser against real binaries
make lmstudio      # LM Studio suite (on the Mac with LM Studio running)
make load chaos soak coverage supply-chain
```
