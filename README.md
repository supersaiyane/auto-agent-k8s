# Auto-Agent for Kubernetes

## Overview
Auto-Agent is a Kubernetes-native DaemonSet that acts as an intelligent auto-remediation agent for workloads. It:
- Monitors Pods, Nodes, and Deployments for issues.
- Detects and fixes problems like CrashLoopBackOff, OOMKilled, ImagePullBackOff, Node Pressure, and scaling needs.
- Integrates with Prometheus for anomaly detection and SLO-based scaling decisions.
- Stores contextual logs and events in S3 or EFS for auditing and ML automation.
- Escalates unresolved issues to Slack and creates tickets in Jira or GitHub Issues.
- Supports GitOps-safe operations via automatic Pull Requests instead of live-patching.

This project evolved through multiple versions to reach a feature-rich, extensible remediation platform.

---

## Features by Version

### v0.1
- Basic DaemonSet agent watching Pods and scaling Deployments.
- Slack and LLM integration for incident summaries.

### v0.2
- CRD: `AutoRemediationPolicy` for per-app/team rules.
- GitOps-safe scaling and config changes (PR workflow).
- Incident ticket stubs for GitHub and Jira.
- Anomaly detection scaffolding with Prometheus z-score.

### v0.3
- Full CrashLoopBackOff handling: fetch last 50 logs, save bundle to S3/EFS, delete pod.
- ImagePullBackOff handling with mirror image swap scaffold.
- OOMKilled detection with memory bump suggestion.
- Node pressure detection (cordon + evict non-critical pods).
- Advanced scaling logic: CPU + SLO checks.
- Configurable log storage backends.

### v0.4
- Scale-down logic with multi-signal gating (CPU, queue depth, error rate, latency).
- `/metrics` endpoint for Prometheus scrape (self-observability).
- AWS SDK v2 S3 PutObject (IRSA-ready).
- CRD controller for AutoRemediationPolicy with per-namespace cache.
- GitHub/GitLab PR + Jira/GitHub Issue client scaffolding.

---

## Repository Structure

```
charts/auto-agent/
  crds/autoagentpolicies.yaml   # Defines AutoRemediationPolicy CRD
  templates/                    # Helm templates for DaemonSet, RBAC, Secrets
cmd/agent/
  main.go                       # Entry point for agent binary
internal/
  kube/watch.go                 # Watches Pods, Nodes, Deployments
  kube/scale.go                 # Scaling logic
  kube/node.go                  # Node pressure handling
  kube/logs.go                  # Log/event fetching
  policy/controller.go          # CRD controller
  policy/store.go                # In-memory policy store
  prom/query.go                  # Prometheus client for metrics
  gitops/git.go                  # GitOps PR creation client
  tickets/github.go              # GitHub Issues client
  tickets/jira.go                # Jira ticket client
  s3/storage.go                  # S3/EFS log storage
pkg/api/metrics.go               # /metrics endpoint (Prometheus)
values.yaml                      # Helm values
README.md                        # This file
```

---

## How to Use

### 1. Deploy Helm Chart
```bash
helm install auto-agent ./charts/auto-agent -n auto-agent --create-namespace
```

### 2. Configure CRDs
Create `AutoRemediationPolicy` resources for each app/team.

```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata:
  name: api-prod
  namespace: prod
spec:
  targetSelector:
    matchLabels:
      app: api
  actions:
    restartStuckPods: true
    bumpMemoryPercent: 20
    scale:
      enabled: true
      minReplicas: 3
      maxReplicas: 30
  escalation:
    slackChannel: "#prod-incidents"
```

### 3. Set up Integrations
- **S3/EFS**: Provide AWS IRSA role or mount EFS volume.
- **Slack**: Set webhook URL in Secret.
- **GitOps**: Configure repo, branch, valuesFile, and token in values.yaml.
- **Ticketing**: Set provider (GitHub/Jira) and credentials.

### 4. Monitor and Review
- Metrics: `/metrics` endpoint on agent pod.
- Logs: stored in S3/EFS per namespace/workload/reason/date.
- PRs: in GitHub/GitLab repo.
- Tickets: in Jira/GitHub Issues.

---

## Changes from Previous Version

### v0.4 vs v0.3
- Added scale-down + multi-signal gating.
- `/metrics` endpoint.
- Real AWS S3 SDK integration.
- CRD controller fully functional.
- PR/ticket clients scaffolded.

### v0.3 vs v0.2
- Real issue handlers (CrashLoop, ImagePull, OOMKilled, Node pressure).
- Log storage backends.

### v0.2 vs v0.1
- Introduced CRD for fine-grained policies.
- GitOps PR mode.
- Anomaly detection scaffolding.
- Ticketing stubs.


# Auto Agent (Kubernetes DaemonSet) — v0.4

New in v0.4
- **Scale-down** with long cooldowns and **multi-signal gating** (CPU + one of queue depth / p95 latency / error rate).
- **/metrics** endpoint exposing Prometheus counters (`auto_agent_actions_total`, `auto_agent_incidents_total`).
- **Real AWS SDK v2 S3 PutObject** (IRSA-ready) for log bundles.
- **CRD controller** to watch `AutoRemediationPolicy` and cache per-namespace rules (used by anomaly loop scaffold).
- **GitOps & Tickets clients** (placeholders) for GitHub/GitLab PRs and Jira/GitHub Issues.

> Note: GitHub/GitLab/Jira calls are stubbed for now (URLs returned). If you want, I’ll wire the full REST calls next, including branch creation and idempotent ticket upserts.

## Multi-signal scaling
- Up: `cpu > SCALE_CPU_THRESHOLD` **AND** any of `PROM_QUEUE_DEPTH`, `PROM_ERROR_RATE`, or `PROM_P95_LATENCY` evaluates truthy.
- Down: `cpu < 0.30` **AND** no gates active, with `COOLDOWN_DOWN` enforced.

Configure signals in ConfigMap env (or move them into CRD rules):
```
PROM_QUEUE_DEPTH: 'sum(queue_depth{namespace="prod",deployment="api"})>10'
PROM_ERROR_RATE:  'rate(http_requests_total{namespace="prod",deployment="api",code=~"5.."}[5m])>0.1'
PROM_P95_LATENCY: 'histogram_quantile(0.95, ... ) > 0.5'
```

## /metrics
- `GET /metrics` → Prometheus scrape.
- Counters:
  - `auto_agent_actions_total{type,namespace,workload}`
  - `auto_agent_incidents_total{reason,namespace,workload}`

## S3 (IRSA)
The agent uses **AWS SDK v2** and `config.LoadDefaultConfig()` which picks up **IRSA** credentials in EKS.
Set:
```yaml
logs:
  store: s3
  s3:
    bucket: your-bucket
    prefix: auto-agent/logs
```
Records are saved as JSON at `s3://bucket/prefix/ns/workload/reason/yyyy-mm-dd/pod.json`.

## CRD Controller
`internal/crd/` watches `AutoRemediationPolicy` and caches policies per namespace, ready to drive actions/anomalies. The anomaly loop is scaffolded; wire your PromQLs in the CR spec.

## GitOps PRs & Tickets
Stubs are in `internal/integrations/`. Provide tokens via Secret; calls currently return placeholder URLs. Ask for v0.5 to enable real API calls.

## Build & Install
```bash
make docker && make push
helm upgrade --install auto-agent charts/auto-agent -n kube-system --create-namespace
```

## Next (v0.5)
- Real GitHub/GitLab PR flow (create branch, commit, open PR) + diff of values.
- Real GitHub Issues / Jira REST integration with dedupe on incident key.
- CRD-driven per-app scaling gate queries and thresholds.
- Self-metrics histograms + error counters.
