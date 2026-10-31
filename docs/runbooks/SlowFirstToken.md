# SlowFirstToken

**Alert:** `SlowFirstToken` (deploy/observability/alerts.yml)

## Meaning

Time to first token p95 above 10 s for a served model over 10 minutes.

## Impact

Interactive clients (chat UIs, Claude Code) feel stuck; clients may time out.

## Diagnosis

1. Which model? Local models: is LM Studio overloaded (parallel slots full) or loading the model (cold)?
2. *In flight and queued by deployment*: requests waiting for a concurrency slot add to TTFT.
3. Long prompts: prompt processing time grows with context; check input token rates.

## Mitigation

- Raise LM Studio's Max Concurrent Predictions together with the deployment's `concurrency` if memory allows; otherwise lower `queue_wait` so requests fall back sooner.
- Route latency-sensitive traffic with `objective: min_latency` or `latency_target_ms`.
- Preload models (docs/LMSTUDIO.md) so none is loaded on the request path.

## Escalation

Owner of the provider/host.
