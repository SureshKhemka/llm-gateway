# CircuitOpen

**Alert:** `CircuitOpen` (deploy/observability/alerts.yml)

## Meaning

A deployment's circuit breaker has been open for 1 minute: recent requests failed (error rate or consecutive failures) or p95 latency exceeded its threshold.

## Impact

Traffic for that deployment goes to fallbacks (other deployments of the same or a higher tier). Cost may rise; quality is preserved by tier rules.

## Diagnosis

1. `GET /admin/v1/health` or console *Health*: breaker reason, last error, cold flag.
2. Cold (`model not listed by provider`): LM Studio evicted or never loaded the model; see docs/LMSTUDIO.md (JIT auto-evict).
3. 429s: the provider is rate limiting; check its `Retry-After` and our request rate.
4. Timeouts: compare TTFT with `timeouts.ttft`; large prompts on a slow model.

## Mitigation

- Reload the model in LM Studio (`lms load ...`); the breaker half-opens after `cool_down` and closes on the first successful probe.
- Persistent provider issue: set the deployment `enabled: false` until fixed.
- Thresholds too tight for a slow local model: raise `resilience.breaker.latency_p95_ms` or the deployment timeouts.

## Escalation

Owner of the provider/host.
