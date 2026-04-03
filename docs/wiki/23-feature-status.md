# Feature Status — What's Running vs What Needs Configuration

This page documents the honest status of every feature — what's actually working out of the box vs what needs credentials/URLs to activate.

## Actually Working (no config needed)

| Feature | Proof | Notes |
|---------|-------|-------|
| **74 issue detectors** | 462+ incidents detected in dashboard | All pod, node, workload, storage, network, security detectors active |
| **Pod deletion (fix mode)** | `tryFixAction: SUCCESS` in agent logs | Deletes crashing pods, controller recreates them |
| **Fix verification** | FixTracker confirmed 2+ fixes | Verifies deployment healthy after action (checks ReadyReplicas) |
| **Dashboard UI (10 tabs)** | Accessible at http://localhost:8080 | Events, K8s Events, Actions, Charts, Report, Cluster, Nodes, Cost, Resources, Terminal |
| **Event persistence** | 258+ events on disk | Survives pod restarts via `events.jsonl` on hostPath volume |
| **Dedup / rate limiter / circuit breaker** | Dedup skipped events visible in logs | 8-layer safety system active |
| **Node-local pod informer** | Filtered by `NODE_NAME` env var | Each pod watches only its own node's pods |
| **Leader election** | One pod acquires lease, runs periodic scans | Lease-based via `kube-system/auto-agent-leader` |
| **Config hot-reload** | ConfigMap changes picked up every 30s | No pod restart needed for config changes |
| **Cost estimation** | Working with built-in instance prices | 40+ AWS/GCP/Azure instance types hardcoded |
| **Resource efficiency** | Pod overuse/underuse/no-limits analysis | Based on requests vs limits comparison |
| **kubectl terminal** | Commands execute via K8s Go client | get, describe, logs, version — read-only |
| **Audit log** | Actions logged to `audit.jsonl` | Persistent on hostPath volume |
| **CRD controller** | Watches AutoRemediationPolicy resources | Policies loaded into in-memory cache |
| **Admission webhook** | Code ready, validates limits/probes | Needs TLS certs to activate (see below) |

## Code Exists — Needs Configuration to Activate

### Messaging & Alerting

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **Slack** | Sends incident alerts with logs, LLM diagnosis, interactive buttons | Set `SLACK_WEBHOOK_URL` in secrets | Agent detects and fixes silently — visible only in dashboard |
| **Alertmanager** | Sends structured alerts (AutoAgentIncident, AutoAgentCircuitBreaker) | Set `ALERTMANAGER_URL` (e.g., `http://alertmanager:9093`) | No Alertmanager alerts fired — `FireIncident()` returns nil |
| **PagerDuty** | Triggers PD incidents for critical/warning severity | Set `PAGERDUTY_ROUTING_KEY` in secrets | No pages — escalation chain skips PD |
| **OpsGenie** | Creates OG alerts for critical/warning | Set `OPSGENIE_API_KEY` in secrets | No OG alerts |
| **Email** | Sends email for critical incidents | Set `SMTP_HOST`, `SMTP_FROM`, `ESCALATION_EMAIL_TO` | No emails |

### Ticketing & GitOps

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **GitHub PRs** | Opens PRs to bump memory on OOMKilled | Set `GIT_TOKEN`, `GITOPS_REPO` | OOM handler logs recommendation but doesn't open PR |
| **GitLab MRs** | Same as GitHub but for GitLab | Set `GIT_TOKEN`, `GITOPS_REPO`, `GITOPS_PROVIDER=gitlab` | No MRs opened |
| **GitHub Issues** | Creates/updates issues for incidents with dedup | Set `TICKETS_ENABLED=true`, `TICKETS_PROVIDER=github`, `GITHUB_TOKEN`, `GITHUB_REPO` | No tickets created |
| **Jira** | Creates Bug issues, adds ADF comments | Set `TICKETS_ENABLED=true`, `TICKETS_PROVIDER=jira`, `JIRA_TOKEN`, `JIRA_BASE_URL`, `JIRA_PROJECT_KEY`, `JIRA_EMAIL` | No Jira tickets |

### Intelligence & Metrics

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **LLM diagnosis** | Sends logs+events to LLM, gets SRE advice | Set `LLM_ENABLED=true`, `LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL` | No AI diagnosis — Slack messages won't have LLM section |
| **Learning mode** | Collects CPU baselines per workload, auto-tunes thresholds | Set `LEARNING_ENABLED=true` + `METRICS_PROVIDER=prometheus` + `PROMETHEUS_URL` | Agent uses global static thresholds (SCALE_CPU_THRESHOLD) |
| **Auto-scaling** | Scales deployments based on CPU + gate signals | Set `METRICS_PROVIDER=prometheus`, `PROMETHEUS_URL` | No scaling — CPU queries return errors with metrics-server |
| **Anomaly detection** | Evaluates CRD PromQL rules | Set `PROMETHEUS_URL` + create AutoRemediationPolicy with anomalies | No anomaly detection — queries fail without Prometheus |

### Storage & Cost

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **S3 storage** | Persists incident logs to AWS S3 | Set `LOG_STORE=s3`, `LOG_S3_BUCKET`, `LOG_S3_PREFIX` + AWS credentials (IRSA) | Logs go to filesystem `/var/log/auto-agent/` on the node |
| **Kubecost** | Real cluster cost data from Kubecost API | Set `COST_PROVIDER=kubecost` — deploy.sh auto-installs | Uses built-in instance-type price estimates |
| **OpenCost** | Real cluster cost data from OpenCost API | Set `COST_PROVIDER=opencost` — deploy.sh auto-installs | Same as above |

### Security

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **Admission webhook** | Validates deployments have limits, probes, non-blocked images | Set `WEBHOOK_CERT_FILE`, `WEBHOOK_KEY_FILE` + create ValidatingWebhookConfiguration | No admission validation — bad deployments are not prevented, only detected after the fact |
| **Namespace-scoped RBAC** | Per-namespace Role/RoleBinding instead of ClusterRole | Set `namespacedRBAC.enabled=true` in Helm values | ClusterRole grants access to all namespaces (agent only acts on allowlist though) |

## Quick Activation Guide

### Minimum viable production setup
```yaml
# deployment/03-config.yaml secrets:
SLACK_WEBHOOK_URL: "https://hooks.slack.com/services/..."   # alerts
```
That's it — you get Slack alerts for every incident. Everything else is optional.

### Recommended production setup
```yaml
# Config:
AUTO_MODE: "fix"
METRICS_PROVIDER: "prometheus"
PROMETHEUS_URL: "http://prometheus:9090"
ALERTMANAGER_URL: "http://alertmanager:9093"
COST_PROVIDER: "kubecost"
LLM_ENABLED: "true"
LEARNING_ENABLED: "true"

# Secrets:
SLACK_WEBHOOK_URL: "https://hooks.slack.com/..."
LLM_API_URL: "https://api.openai.com/v1/chat/completions"
LLM_API_KEY: "sk-..."
PAGERDUTY_ROUTING_KEY: "R..."
```

### Full setup (everything activated)
```yaml
# Config:
AUTO_MODE: "fix"
METRICS_PROVIDER: "prometheus"
PROMETHEUS_URL: "http://prometheus:9090"
ALERTMANAGER_URL: "http://alertmanager:9093"
COST_PROVIDER: "kubecost"
LLM_ENABLED: "true"
LEARNING_ENABLED: "true"
TICKETS_ENABLED: "true"
TICKETS_PROVIDER: "jira"

# Secrets:
SLACK_WEBHOOK_URL: "https://hooks.slack.com/..."
LLM_API_URL: "https://api.openai.com/v1/chat/completions"
LLM_API_KEY: "sk-..."
GIT_TOKEN: "ghp_..."
GITOPS_REPO: "yourorg/helm-env"
GITHUB_TOKEN: "ghp_..."
GITHUB_REPO: "yourorg/api-service"
JIRA_TOKEN: "..."
JIRA_BASE_URL: "https://yourorg.atlassian.net"
JIRA_PROJECT_KEY: "OPS"
JIRA_EMAIL: "bot@yourorg.com"
PAGERDUTY_ROUTING_KEY: "R..."
OPSGENIE_API_KEY: "..."
SMTP_HOST: "smtp.gmail.com"
SMTP_FROM: "auto-agent@yourorg.com"
ESCALATION_EMAIL_TO: "oncall@yourorg.com"
```

## How to Verify an Integration is Working

| Integration | Verify command |
|-------------|---------------|
| Slack | Check agent logs: `grep "slack:" logs` — should show POST, not "no webhook" |
| Alertmanager | `curl http://alertmanager:9093/api/v2/alerts` — check for AutoAgentIncident |
| GitOps | Check GitHub/GitLab repo for branches named `auto-agent/oom-*` |
| Tickets | Search Jira/GitHub Issues for `[auto-agent]` in title |
| LLM | Check Slack messages for `_LLM diagnosis_:` section |
| Kubecost | Dashboard Cost tab shows `source: kubecost` instead of `default` |
| Learning | `curl http://localhost:8080/api/baselines` — should show `learning: true` with baseline data |
| Prometheus | Agent logs should NOT show `metrics provider not implemented` |
