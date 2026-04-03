# Configuration Reference

All configuration lives in `deployment/03-config.yaml` (ConfigMap + Secret).

## Agent Mode

```yaml
AUTO_MODE: "fix"
```

| Mode | Behavior | When to use |
|------|----------|-------------|
| `observe` | Detect + alert only. No actions taken. | First deployment, gaining trust |
| `suggest` | Detect + alert + recommend actions | Team review before enabling fix |
| `fix` | Detect + alert + auto-remediate | Production — agent fixes issues |
| `dry-run` | Detect + simulate fixes + log what WOULD happen | Testing guardrails and policies |

**Decision tree:**
```
New cluster? → observe (1 week)
  → Confident in detections? → suggest (1 week)
    → Comfortable with recommendations? → fix
    → Want to test fix logic first? → dry-run
```

## Namespace Control

```yaml
NAMESPACE_ALLOWLIST: "default,prod,staging"
```

Agent ONLY watches namespaces in this list. Everything else is ignored. Start narrow, expand as confidence grows.

## Scaling

| Variable | Default | Description |
|----------|---------|-------------|
| `SCALE_CPU_THRESHOLD` | `0.8` | CPU ratio (0-1) to trigger scale-up |
| `MAX_SCALE_STEP` | `2` | Max replicas to add per scale-up |
| `MAX_REPLICAS` | `50` | Global max replicas cap |
| `MIN_REPLICAS` | `1` | Global min replicas floor |
| `COOLDOWN_UP` | `2m` | Min time between scale-ups per deployment |
| `COOLDOWN_DOWN` | `10m` | Min time between scale-downs |
| `HPA_COEXISTENCE` | `true` | Skip scaling when HPA exists |
| `SCALE_WINDOW` | `5m` | Prometheus CPU query window |

## Safety

| Variable | Default | Description |
|----------|---------|-------------|
| `MAX_ACTIONS_PER_10M` | `10` | Global rate limit — max actions per 10 min |
| `DEDUP_TTL_SECONDS` | `300` | Don't re-process same workload within 5 min |
| `EXCLUDED_ANNOTATION` | `auto-agent.io/disable` | Pods with this annotation are ignored |
| `QUIET_HOURS` | _(empty)_ | UTC time windows. Example: `"02:00-06:00"` |

## Metrics

| Variable | Default | Description |
|----------|---------|-------------|
| `METRICS_PROVIDER` | `metrics-server` | `prometheus` or `metrics-server` |
| `PROMETHEUS_URL` | _(empty)_ | Prometheus endpoint for scaling + anomalies |

## LLM Diagnosis

| Variable | Default | Description |
|----------|---------|-------------|
| `LLM_ENABLED` | `false` | Enable AI-powered root cause analysis |
| `LLM_API_URL` | _(empty)_ | OpenAI-compatible endpoint |
| `LLM_MODEL` | _(empty)_ | Model name (e.g., `gpt-4o-mini`) |
| `LLM_TIMEOUT_SEC` | `10` | Request timeout |

## Cost Estimation

```yaml
COST_PROVIDER: ""          # kubecost | opencost | manual | (empty for built-in)
```

| Provider | What happens |
|----------|-------------|
| `kubecost` | deploy.sh auto-installs Kubecost, uses its API for real costs |
| `opencost` | deploy.sh auto-installs OpenCost |
| `manual` | deploy.sh prompts for CPU/memory prices during deploy |
| _(empty)_ | Uses built-in instance-type pricing (40+ AWS/GCP/Azure types) |

## Logging

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_STORE` | `efs` | `s3`, `efs`, or `none` |
| `LOG_EFS_PATH` | `/var/log/auto-agent` | Filesystem path for logs |
| `LOG_FORMAT` | _(empty)_ | Set to `json` for structured logging (ELK/Loki) |
| `AUDIT_LOG_PATH` | `/var/log/auto-agent/audit.jsonl` | Persistent action audit log |

## Secrets

Set in `deployment/03-config.yaml` under the Secret:

| Secret Key | For |
|-----------|-----|
| `SLACK_WEBHOOK_URL` | Slack incoming webhook |
| `LLM_API_KEY` | LLM API authentication |
| `GIT_TOKEN` | GitHub/GitLab GitOps PRs |
| `GITHUB_TOKEN` | GitHub Issues |
| `JIRA_TOKEN` | Jira tickets |
| `JIRA_EMAIL` | Jira Basic Auth email |
| `PAGERDUTY_ROUTING_KEY` | PagerDuty Events API |
| `OPSGENIE_API_KEY` | OpsGenie Alerts API |

## CRD: AutoRemediationPolicy

Per-workload configuration that overrides global settings:

```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata:
  name: api-policy
  namespace: prod
spec:
  targetSelector:
    matchLabels:
      app: api
  actions:
    restartStuckPods: true
    bumpMemoryPercent: 30        # OOM PR bumps by 30% instead of default 20%
    scale:
      enabled: true
      minReplicas: 3
      maxReplicas: 20
      step: 2
  escalation:
    slackChannel: "#prod-alerts"
    runbookURL: "https://wiki.internal/runbooks/api"
    ticketing:
      provider: jira
      projectOrRepo: OPS
  safety:
    cooldown: "5m"
    maxActionsPerHour: 3
    requireApproval: true        # Blocks automated fix — alert only
  anomalies:
    - name: high_error_rate
      promql: 'rate(http_errors_total{app="api"}[5m]) > 0.1'
      zscoreThreshold: 2.0
```

See [CRD Reference](17-crd-reference.md) for full field documentation.
