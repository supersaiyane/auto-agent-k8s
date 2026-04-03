# CRD Reference: AutoRemediationPolicy

## Overview

`AutoRemediationPolicy` lets you customize agent behavior per workload. Applied per-namespace, matched by label selector.

```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata:
  name: <policy-name>
  namespace: <namespace>
```

## Spec Fields

### targetSelector
```yaml
spec:
  targetSelector:
    matchLabels:
      app: api
      tier: backend
```
Matches pods/deployments with these labels. Use `matchLabels` for exact match. Omit for all workloads in namespace.

### actions

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `restartStuckPods` | bool | false | Allow agent to restart stuck pods |
| `bumpMemoryPercent` | int | 20 | Memory increase % for OOM GitOps PRs |
| `scale.enabled` | bool | false | Allow agent to scale this workload |
| `scale.minReplicas` | int | 1 | Min replicas floor |
| `scale.maxReplicas` | int | 50 | Max replicas cap |
| `scale.step` | int | 2 | Replicas to add per scale-up |
| `scale.allowHPAOverride` | bool | false | Scale even if HPA exists |

### escalation

| Field | Type | Description |
|-------|------|-------------|
| `slackChannel` | string | Channel name for routing (e.g., `#prod-alerts`) |
| `runbookURL` | string | URL to runbook JSON for automated execution |
| `ticketing.provider` | string | `github`, `jira`, or `none` |
| `ticketing.projectOrRepo` | string | GitHub `owner/repo` or Jira project key |
| `ticketing.assignees` | []string | Auto-assign tickets |
| `ticketing.labels` | []string | Labels/tags on tickets |

### safety

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cooldown` | string | global | Override cooldown between actions |
| `maxActionsPerHour` | int | global | Per-workload rate limit |
| `requireApproval` | bool | false | Block automated fixes, alert only |

### anomalies

```yaml
spec:
  anomalies:
    - name: high_error_rate
      promql: 'rate(http_errors_total{app="api"}[5m]) > 0.1'
      zscoreThreshold: 2.0
      minSamples: 12
```

## Examples

### Production API (strict)
```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata: { name: api-strict, namespace: prod }
spec:
  targetSelector: { matchLabels: { app: api } }
  actions:
    bumpMemoryPercent: 25
    scale: { enabled: true, minReplicas: 5, maxReplicas: 50, step: 2 }
  escalation:
    slackChannel: "#prod-critical"
    runbookURL: "https://wiki.internal/api-runbook.json"
    ticketing: { provider: jira, projectOrRepo: PROD }
  safety:
    cooldown: "10m"
    maxActionsPerHour: 2
    requireApproval: false
```

### Staging (permissive)
```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata: { name: staging-all, namespace: staging }
spec:
  targetSelector: {}
  actions:
    restartStuckPods: true
    bumpMemoryPercent: 50
    scale: { enabled: true, maxReplicas: 10 }
  safety:
    cooldown: "1m"
    maxActionsPerHour: 20
```

### Approval-Required (critical data)
```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata: { name: db-policy, namespace: prod }
spec:
  targetSelector: { matchLabels: { tier: database } }
  safety:
    requireApproval: true
  escalation:
    slackChannel: "#db-team"
    ticketing: { provider: github, projectOrRepo: "org/db-ops" }
```
