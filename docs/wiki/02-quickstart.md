# Quick Start Guide

## Prerequisites

- `kubectl` connected to a Kubernetes cluster
- `docker` for building the agent image
- `helm` (optional, for Helm-based deployment)

## Deploy in 60 Seconds

```bash
# Clone the repo
git clone https://github.com/yourorg/auto-agent-k8s.git
cd auto-agent-k8s

# Deploy (builds image, loads into cluster, applies all manifests)
./deployment/deploy.sh
```

The script will:
1. Build the Docker image
2. Auto-detect your cluster type and load the image
3. Install CRDs, RBAC, ConfigMap, DaemonSet
4. Wait for pods to be ready
5. Open the dashboard in your browser

## Open Dashboard

```
http://localhost:8080
```

If port-forward died:
```bash
kubectl port-forward -n auto-agent svc/auto-agent 8080:8080
```

## See It In Action

Deploy test apps that intentionally fail:

```bash
# Deploy 3 apps across 3 namespaces (9 failing pods)
./deployment/test-apps/deploy-apps.sh
```

This creates:
- **payment-service** (default) — CrashLoop, OOM, ConfigError
- **order-service** (test1) — Init failure, ImagePull, NotReady, 0 endpoints, failed Job
- **inventory-service** (test2) — Pending, restart storm, healthy v1

Watch the dashboard populate with incidents and actions in real-time.

### Trigger Stuck Rollout

```bash
kubectl set image deploy/inventory-api api=registry.invalid/broken:v2 -n test2
```

### Run Full Chaos Test (74 issue types)

```bash
bash deployment/test-apps/chaos-test.sh
```

## Choose Your Mode

Edit `deployment/03-config.yaml`:

```yaml
AUTO_MODE: "observe"    # Alert only — start here
AUTO_MODE: "suggest"    # Alert + recommend actions
AUTO_MODE: "fix"        # Auto-remediate
AUTO_MODE: "dry-run"    # Simulate fixes, show what would happen
```

Apply and restart:
```bash
kubectl apply -f deployment/03-config.yaml
kubectl rollout restart ds/auto-agent -n auto-agent
```

## Connect Integrations

### Slack (recommended first)
```bash
kubectl patch secret auto-agent-secrets -n auto-agent --type merge \
  -p '{"stringData":{"SLACK_WEBHOOK_URL":"https://hooks.slack.com/services/YOUR/WEBHOOK/URL"}}'
kubectl rollout restart ds/auto-agent -n auto-agent
```

### Jira
```bash
kubectl patch cm auto-agent-config -n auto-agent --type merge \
  -p '{"data":{"TICKETS_ENABLED":"true","TICKETS_PROVIDER":"jira","JIRA_BASE_URL":"https://yourorg.atlassian.net","JIRA_PROJECT_KEY":"OPS"}}'
kubectl patch secret auto-agent-secrets -n auto-agent --type merge \
  -p '{"stringData":{"JIRA_TOKEN":"your-api-token","JIRA_EMAIL":"you@company.com"}}'
kubectl rollout restart ds/auto-agent -n auto-agent
```

### Prometheus + Alertmanager (for scaling + alerts)
```bash
# Install via Helm
helm install prometheus prometheus-community/kube-prometheus-stack -n monitoring --create-namespace

# Point agent to it
kubectl patch cm auto-agent-config -n auto-agent --type merge \
  -p '{"data":{"METRICS_PROVIDER":"prometheus","PROMETHEUS_URL":"http://prometheus-kube-prometheus-prometheus.monitoring:9090","ALERTMANAGER_URL":"http://prometheus-kube-prometheus-alertmanager.monitoring:9093"}}'
kubectl rollout restart ds/auto-agent -n auto-agent
```

See [Feature Status](23-feature-status.md) for all integration activation guides.

## Verify Everything Works

```bash
# Check agent status
curl -s http://localhost:8080/api/status | python3 -m json.tool

# Check event counts
curl -s http://localhost:8080/api/stats

# Check fixes
curl -s http://localhost:8080/api/fixes | python3 -m json.tool

# Check cost
curl -s http://localhost:8080/api/cost | python3 -c "import sys,json;d=json.load(sys.stdin);print(f'Total: \${d[\"totalMonthly\"]:.0f}/mo')"
```

## Clean Up

```bash
# Remove test apps
./deployment/test-apps/remove-apps.sh

# Remove agent
./deployment/teardown.sh
```

## Next Steps

- [Configuration Reference](03-configuration.md) — all settings explained
- [Dashboard Guide](06-dashboard.md) — what each tab shows
- [Safety Model](05-safety.md) — how guardrails protect your cluster
- [Feature Status](23-feature-status.md) — what's running vs what needs config
- [Troubleshooting](11-troubleshooting.md) — when things don't work
