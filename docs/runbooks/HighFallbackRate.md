# HighFallbackRate

**Alert:** `HighFallbackRate` (deploy/observability/alerts.yml)

## Meaning

More than 10% of requests needed at least one fallback attempt over 10 minutes.

## Impact

Higher latency (failed attempt + retry) and often higher cost (fallbacks may be pricier).

## Diagnosis

1. *Fallbacks and escalations* panel: which `model` fails first?
2. `GET /admin/v1/health`: last error for that deployment; is its circuit flapping?
3. Cascade escalations are separate (validators rejecting output); see `gw_cascade_escalations_total`.

## Mitigation

- Fix or disable the failing deployment.
- If it fails only under load, lower its `concurrency` so excess requests queue or go elsewhere without failing first.

## Escalation

Owner of the failing provider.
