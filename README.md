# Auto-Agent for Kubernetes

A production-ready, autonomous Kubernetes remediation agent that detects, diagnoses, and fixes cluster issues in real-time.

## What It Does

Auto-Agent runs as a DaemonSet on every node and automatically handles the most common Kubernetes operational issues:

| Issue | Detection | Remediation |
|-------|-----------|-------------|
| **CrashLoopBackOff** | Pod informer event | Delete pod to clear backoff, collect logs, create ticket |
| **OOMKilled** | Container termination state | Open GitOps PR to bump memory limits |
| **ImagePullBackOff** | Pod informer event | Delete pod to retry, alert with image details |
| **Node Pressure** | Node condition change | Cordon node, evict non-critical pods, uncordon on recovery |
| **Pending Pods** | Stuck >5 minutes | Diagnose reason (resources, affinity, PVC), create ticket |
| **NotReady Pods** | Running but failing probes >3m | Restart pod, collect diagnostics |
| **Failed Jobs** | Job/CronJob failure | Collect logs, clean up old failed jobs |
| **Resource Quotas** | >90% usage | Alert before scheduling failures occur |
| **Anomalies** | CRD-driven PromQL rules | Evaluate thresholds, alert on breach |
| **Auto-Scaling** | CPU + multi-signal gating | Scale up/down with HPA awareness, cooldowns |

## Architecture

```
                          +------------------+
                          |   Dashboard UI   |  :8080/
                          +--------+---------+
                                   |
+----------------------------------+----------------------------------+
|                        auto-agent DaemonSet                         |
|                                                                     |
|  Per-Node (all pods):            Leader-Only (one pod):             |
|  - Pod watcher (local node)      - Auto-scaler (30s tick)           |
|  - Node pressure handler         - Anomaly checker (CRD-driven)    |
|  - Log retention cleanup         - Failed job scanner              |
|                                  - Resource quota monitor           |
|                                                                     |
|  Shared:                                                            |
|  - Rate limiter + deduplicator + circuit breaker                    |
|  - Leader election (Lease-based)                                    |
|  - CRD controller (AutoRemediationPolicy)                          |
|  - Config hot-reload (ConfigMap watch)                              |
+---------------------------------------------------------------------+
        |           |           |           |           |
   +----+    +------+    +------+    +------+    +------+
   |Slack|   |  LLM  |   |  S3  |   |GitHub|   |Alert- |
   |     |   |Diagnose|  | /EFS |   |/GitLab|  |manager|
   +-----+   +-------+  +------+   +-------+  +-------+
```

## Quick Start

```bash
# Deploy with Helm
helm install auto-agent charts/auto-agent \
  -n kube-system --create-namespace \
  --set agent.namespaceAllowlist='{default,prod}' \
  --set slack.webhookUrl='https://hooks.slack.com/...' \
  --set agent.mode=observe  # start safe, graduate to fix

# Access the dashboard
kubectl port-forward -n kube-system svc/auto-agent 8080:8080
open http://localhost:8080
```

## Safety Model

Auto-Agent has 5 layers of protection against runaway remediation:

1. **Three modes**: `observe` (alert only) -> `suggest` (alert + recommend) -> `fix` (auto-remediate)
2. **Deduplication**: Workload-level dedup prevents re-processing the same issue within a TTL window
3. **Rate limiting**: Global action limiter caps total actions per 10-minute window
4. **Circuit breaker**: Trips after N actions on the same workload in 1 hour — stops and escalates
5. **CRD policies**: Per-workload `requireApproval`, `maxActionsPerHour`, custom `cooldown`

Additional safeguards:
- HPA coexistence: skips scaling when an HPA exists
- PDB awareness: eviction API respects PodDisruptionBudgets
- StatefulSet protection: skips ordered-lifecycle pods during node drain
- Annotation escape hatch: `auto-agent.io/disable: "true"` excludes any workload

## Configuration

### Environment Variables (via ConfigMap)

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTO_MODE` | `fix` | `observe`, `suggest`, or `fix` |
| `NAMESPACE_ALLOWLIST` | `default` | Comma-separated namespaces to watch |
| `SCALE_CPU_THRESHOLD` | `0.8` | CPU ratio to trigger scale-up |
| `MAX_SCALE_STEP` | `2` | Max replicas to add per scale-up |
| `MAX_REPLICAS` | `50` | Global max replicas cap |
| `MIN_REPLICAS` | `1` | Global min replicas floor |
| `MAX_ACTIONS_PER_10M` | `10` | Rate limiter max actions |
| `COOLDOWN_UP` | `2m` | Min time between scale-ups |
| `COOLDOWN_DOWN` | `10m` | Min time between scale-downs |
| `DEDUP_TTL_SECONDS` | `300` | Deduplication window (5 min) |
| `HPA_COEXISTENCE` | `true` | Skip scaling when HPA exists |
| `LLM_ENABLED` | `false` | Enable LLM diagnosis |
| `METRICS_PROVIDER` | `prometheus` | `prometheus` or `metrics-server` |
| `LOG_STORE` | `efs` | `s3`, `efs`, or `none` |
| `ALERTMANAGER_URL` | _(empty)_ | Alertmanager endpoint for structured alerts |

### Secrets

| Secret Key | Description |
|-----------|-------------|
| `SLACK_WEBHOOK_URL` | Slack incoming webhook |
| `LLM_API_KEY` | LLM API key (OpenAI-compatible) |
| `GIT_TOKEN` | GitHub/GitLab token for GitOps PRs |
| `GITHUB_TOKEN` | GitHub token for issue creation |
| `JIRA_TOKEN` | Jira API token |

### CRD: AutoRemediationPolicy

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
    bumpMemoryPercent: 30
    scale:
      enabled: true
      minReplicas: 3
      maxReplicas: 20
      step: 2
  escalation:
    slackChannel: "#prod-incidents"
    runbookURL: "https://wiki.internal/runbooks/api"
    ticketing:
      provider: jira
      projectOrRepo: OPS
  safety:
    cooldown: "5m"
    maxActionsPerHour: 3
    requireApproval: false
  anomalies:
    - name: high_error_rate
      promql: 'rate(http_errors_total{app="api"}[5m]) > 0.1'
      zscoreThreshold: 2.0
```

## Dashboard

The embedded web UI is served at `:8080/` with no extra deployment. It shows:

- **Stat cards**: Incidents, actions, scaling decisions, uptime
- **Live event feed**: Auto-refreshes every 5 seconds
- **Filters**: All / Incidents / Actions / Scaling / Anomalies
- **Event details**: Severity, namespace, workload, reason, links to logs/PRs/tickets
- **Agent status**: Version, mode, leader/follower, node name

### API Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /` | Dashboard UI |
| `GET /api/status` | Agent info (version, mode, leader, uptime) |
| `GET /api/events?limit=50&type=incident` | Recent events |
| `GET /api/stats` | Event counts by type |
| `GET /healthz` | Liveness probe |
| `GET /readyz` | Readiness probe |
| `GET /metrics` | Prometheus metrics |

## Admission Webhook (Optional)

Auto-Agent includes a validating webhook that prevents bad deployments before they cause incidents:

- Rejects deployments without resource limits
- Rejects deployments without readiness probes
- Blocks known-bad image prefixes
- Warns on `:latest` or untagged images

Enable by providing TLS certs via `WEBHOOK_CERT_FILE` and `WEBHOOK_KEY_FILE`.

## Prometheus Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `auto_agent_incidents_total` | Counter | reason, namespace, workload |
| `auto_agent_actions_total` | Counter | type, namespace, workload |
| `auto_agent_scaling_decisions_total` | Counter | direction, namespace, deployment |
| `auto_agent_dedup_skipped_total` | Counter | reason |
| `auto_agent_rate_limited_total` | Counter | _(none)_ |
| `auto_agent_handler_errors_total` | Counter | handler, error_type |
| `auto_agent_anomalies_detected_total` | Counter | policy, namespace, rule |
| `auto_agent_info` | Gauge | version, mode |

Import `dashboards/auto-agent.json` into Grafana for a pre-built dashboard.

## Development

```bash
make build         # Compile binary
make test          # Run tests with race detector
make test-cover    # Tests with coverage report
make vet           # Static analysis
make lint          # golangci-lint
make docker        # Build Docker image
make helm-install  # Deploy to cluster
make helm-template # Render Helm templates
```

## Project Structure

```
cmd/auto-agent/        Entry point, dependency wiring
internal/
  alertmanager/        Alertmanager API client
  crd/                 CRD controller + policy store
  events/              In-memory event recorder for UI
  httpapi/             HTTP server + REST API + embedded UI
  integrations/        GitOps (GitHub/GitLab PR) + Ticketing (Issues/Jira)
  kube/                Pod/node handlers, scaler, anomalies, jobs, quotas
  leader/              Leader election
  llm/                 LLM diagnosis client
  metrics/             Prometheus query provider
  obs/                 Prometheus metric definitions
  policy/              Configuration + hot-reload
  ratelimit/           Deduplicator, rate limiter, circuit breaker
  slack/               Slack webhook client
  storage/             S3 + filesystem log sink
  webhook/             Admission webhook validator
charts/auto-agent/     Helm chart
dashboards/            Grafana dashboard JSON
.github/workflows/     CI/CD pipeline
```

## License

MIT
