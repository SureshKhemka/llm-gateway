# GatewayOverheadHigh

**Alert:** `GatewayOverheadHigh` (deploy/observability/alerts.yml)

## Meaning

The latency the gateway itself adds (total minus upstream time) has p95 above 15 ms for 10 minutes (NFR-01 target).

## Impact

Slower responses for every client; usually a sign of CPU saturation.

## Diagnosis

1. CPU usage of gateway pods; throttling (`container_cpu_cfs_throttled_seconds_total`).
2. Redis latency (budgets, rate limits, cache lookups are on the request path).
3. Semantic cache: embedding lookups add an embedding call on misses; check the embedder's latency.
4. Profile: set `GATEWAY_PPROF=127.0.0.1:6060` and `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30` (port-forward).

## Mitigation

- Scale out or raise CPU limits.
- Move Redis closer (same zone) or scale it.
- Disable the semantic cache (`cache.semantic: false`) or the classifier difficulty estimator for affected policies if they dominate.

## Escalation

Gateway owners with the profile attached.
