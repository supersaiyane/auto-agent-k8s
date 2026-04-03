# API Reference

Base URL: `http://localhost:8080`

## Health

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Liveness probe. Returns `ok`. |
| `/readyz` | GET | Readiness probe. Returns `ready` after informers sync. |
| `/metrics` | GET | Prometheus metrics (scrape endpoint). |

## Agent Status

### `GET /api/status`
```json
{
  "version": "1.0.0",
  "mode": "fix",
  "nodeName": "desktop-worker",
  "podName": "auto-agent-xyz",
  "isLeader": true,
  "ready": true,
  "uptime": "2h15m30s",
  "startedAt": "2026-04-03T09:00:00Z",
  "eventCount": 500
}
```

### `GET /api/stats`
```json
{
  "total": 500,
  "byType": { "incident": 450, "action": 40, "scaling": 10 }
}
```

## Events

### `GET /api/events?limit=50&type=incident`
Returns agent events. Filter by `type` (incident/action/scaling/info).

### `GET /api/k8s-events?namespace=default`
Returns raw Kubernetes events. Optional `namespace` filter.

## Cluster

### `GET /api/cluster`
All namespaces with pod health summary (running/pending/failed/crashloop/notready).

### `GET /api/namespace/{name}`
Detailed resources: pods (name, ready, status, restarts, node, IP, age), deployments, services.

### `GET /api/nodes`
Node health: status, roles, version, OS, CPU, memory, pod count, pressure, cordoned state.

## Remediation

### `GET /api/fixes`
```json
{
  "fixed": [...],        "fixedCount": 2,
  "pending": [...],      "pendingCount": 1,
  "failed": [...],       "failedCount": 0
}
```

## Cost

### `GET /api/cost`
Full cluster cost: per-node, per-namespace, top workloads. Includes pricing config and source.

### `GET /api/resources`
All namespaces with pod efficiency (right-sized/overuse/underuse/no-limits).

### `GET /api/resources/{namespace}`
Per-pod detail: CPU/memory requests/limits, efficiency, advice, monthly cost.

## Intelligence

### `GET /api/compliance?days=30`
Compliance report: total incidents, auto-remediated, MTTR, remediation rate.

### `GET /api/baselines`
Learning mode baselines: per-workload CPU avg, stddev, auto-tuned thresholds.

### `GET /api/deploys`
Recent deployment changes tracked for incident correlation.

### `GET /api/dry-run`
Dry-run simulation log (when `AUTO_MODE=dry-run`).

## Terminal

### `POST /api/kubectl`
```json
{ "command": "get pods -n default" }
```
Response:
```json
{ "output": "NAME  READY  STATUS...", "error": "" }
```
Read-only commands only: get, describe, logs, top, version, help.

## Slack

### `POST /api/slack/actions`
Callback endpoint for Slack interactive button clicks (Approve/Rollback/Silence).
