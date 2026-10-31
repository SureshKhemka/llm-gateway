# GatewayHighErrorRate

**Alert:** `GatewayHighErrorRate` (deploy/observability/alerts.yml)

## Meaning

More than 5% of requests returned a 5xx over 5 minutes.

## Impact

Clients see `upstream_unavailable`, `overloaded`, `budget_unavailable` or `internal_error`.

## Diagnosis

1. Grafana *Errors by code*: which `code` dominates?
2. `upstream_unavailable`: providers failing; check *Circuit state* and `GET /admin/v1/health` (last error per deployment).
3. `overloaded`: load shedding (`server.max_in_flight`) or queues full; see DeploymentQueueBacklog.
4. `budget_unavailable`: Redis unreachable and budgets fail closed.
5. `internal_error`: a bug; take a request ID from logs and `GET /admin/v1/requests/{id}`.

## Mitigation

- Provider outage: confirm fallbacks exist for the affected tiers (another provider/pool); temporarily disable the deployment (`enabled: false`) so routing stops trying it.
- Overload: scale out (HPA/replicas) or raise deployment `concurrency` if the backend has headroom.
- Redis outage: restore Redis; only as a last resort set `storage.budgets_fail_open: true` (budgets then aren't enforced; record the decision).

## Escalation

Provider owner (LM Studio host, cloud account) for upstream failures; gateway owners for internal errors.
