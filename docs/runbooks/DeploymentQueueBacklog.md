# DeploymentQueueBacklog

**Alert:** `DeploymentQueueBacklog` (deploy/observability/alerts.yml)

## Meaning

More than 50 requests are waiting for a concurrency slot on one deployment for 2 minutes.

## Impact

Requests wait up to `queue_wait`, then fall back or fail with `overloaded`.

## Diagnosis

1. Is the deployment slow (TTFT/latency panels) or is demand up (requests by served model)?
2. Is routing concentrating traffic there (e.g. it's the only deployment of the needed tier or capability)?

## Mitigation

- Add capacity: another deployment of the same tier/pool (another LM Studio host, or a cloud deployment with a small budget).
- Raise `concurrency` if the backend has headroom (match LM Studio's Max Concurrent Predictions).
- Lower `queue_wait` so traffic falls back sooner; lower-priority keys are shed first.

## Escalation

Capacity owner for the backend.
