# BudgetNearLimit

**Alert:** `BudgetNearLimit` (deploy/observability/alerts.yml)

## Meaning

A budget's spend plus open reservations reached 90% of its hard limit for the current period.

## Impact

None yet. Policies with `degrade` steps already route this scope to cheaper tiers (route reason `budget_degraded`); at 100% requests get `budget_exceeded` (402).

## Diagnosis

1. `gwctl budgets list` or console *Budgets*: which scope, current spend, period end.
2. `GET /admin/v1/costs?group_by=model,key&tenant=...`: what is driving spend (one key? one model? cache misses?).
3. Unexpected growth: compare with the CostSpike alert and the request explorer.

## Mitigation

- Expected growth: grant a temporary increase with a reason (`gwctl budgets grant ID --extra-usd X --expires ... --reason ...`), or raise `hard_usd` in config.
- Runaway client: revoke or rate-limit its key (`gwctl keys revoke`, key `rate_limits`).
- Make the scope cheaper: add `degrade` steps or lower `max_tier` for its policy.

## Escalation

The budget owner (tenant admin / FinOps).
