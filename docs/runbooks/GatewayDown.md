# GatewayDown

**Alert:** `GatewayDown` (deploy/observability/alerts.yml)

## Meaning

Prometheus cannot scrape a gateway instance for 1 minute.

## Impact

If every instance is down, all LLM traffic fails. If one is down, the load balancer should already route around it (readiness).

## Diagnosis

1. `kubectl get pods -l app.kubernetes.io/name=llm-gateway` / `docker compose ps`: crash-looping, OOMKilled or not scheduled?
2. Logs: `kubectl logs <pod> --previous`; look for `startup failed` (bad config, unreachable Postgres/Redis with no fallback, port in use).
3. `curl /livez` and `/readyz` on the pod IP; readiness is false while draining or before the first snapshot.
4. Configuration: `gateway -check -config <path>` reproduces validation errors with file and path.

## Mitigation

- Bad configuration: revert the ConfigMap/file; a running instance keeps its last good snapshot, so only restarts fail.
- OOMKilled: raise memory limits; check `cache.max_entries` and the number of concurrent streams.
- Dependency outage at startup: Redis/Postgres must be reachable when configured; fix the dependency or temporarily remove `storage.*` (single-instance mode).

## Escalation

Platform on-call if pods can't be scheduled; gateway owners for crash loops.
