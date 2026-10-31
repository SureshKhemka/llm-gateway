# CostSpike

**Alert:** `CostSpike` (deploy/observability/alerts.yml)

## Meaning

Spend over the last 15 minutes is more than 3x the same window yesterday.

## Impact

Budgets will be reached early; possibly a runaway client or a routing change sending traffic to expensive tiers.

## Diagnosis

1. `GET /admin/v1/costs?group_by=tenant,model&from=<1h ago>`: which tenant and model?
2. Route reasons in the request explorer: a new policy version, a disabled cheap deployment, or a cache outage (hit rate panel) can all shift traffic to expensive tiers.
3. Audit log (`GET /admin/v1/audit`): recent policy activations or config changes.

## Mitigation

- Recent policy change: `gwctl policies rollback NAME`.
- Runaway key: revoke or rate-limit it.
- Cache outage: restore Redis / the embedder.

## Escalation

FinOps and the tenant owner.
