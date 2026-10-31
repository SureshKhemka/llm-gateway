# BudgetExhausted

**Alert:** `BudgetExhausted` (deploy/observability/alerts.yml)

## Meaning

A budget reached 100% of its hard limit; requests in its scope are rejected with `budget_exceeded` (HTTP 402).

## Impact

The tenant/project/key cannot make LLM calls until the period resets or the limit is raised.

## Diagnosis

1. Same as BudgetNearLimit. Confirm it isn't an eval or shadow budget (those only stop evals/shadow traffic).
2. Reservations: long-running streams reserve their worst-case cost; spend can sit just under the limit while reservations block new requests.

## Mitigation

- If the work must continue: `gwctl budgets grant ID --extra-usd X --expires <period end> --reason "..."` (audited, takes effect within a second on every instance).
- Otherwise communicate the reset time (period end in the budget's timezone).

## Escalation

The budget owner; FinOps for grants above policy.
