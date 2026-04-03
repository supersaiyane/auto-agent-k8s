# Comparison with Other Tools

## Feature Matrix

| Feature | Auto-Agent | Kubecost | PagerDuty | Datadog | kube-monkey |
|---------|-----------|----------|-----------|---------|-------------|
| **Issue detection** | 74 types | Cost only | Alert routing | APM + infra | None |
| **Auto-remediation** | Yes (7 actions) | No | No (alert only) | Limited | Chaos only |
| **Cost estimation** | Yes + Kubecost | Yes (core) | No | Yes | No |
| **Dashboard** | Embedded | Web app | Web app | Web app | None |
| **CRD policies** | Yes | No | No | No | Yes |
| **Safety layers** | 8 layers | N/A | Escalation | N/A | None |
| **LLM diagnosis** | Yes | No | AIOps ($$$) | AI features ($$$) | No |
| **GitOps PRs** | Yes | No | No | No | No |
| **In-cluster** | DaemonSet | Deployment | SaaS | Agent | Deployment |
| **Cost** | Free/OSS | Free tier + paid | $$$$ | $$$$ | Free/OSS |
| **Fix verification** | Yes (FixTracker) | No | No | No | No |

## When to Use What

| Use Case | Best Tool |
|----------|-----------|
| "I want automatic K8s issue detection + fixing" | **Auto-Agent** |
| "I need detailed cloud cost allocation" | **Kubecost** (or Auto-Agent + Kubecost) |
| "I need on-call alerting with escalation" | **PagerDuty** (Auto-Agent feeds alerts to PD) |
| "I need full APM + traces + logs" | **Datadog** (complement with Auto-Agent for remediation) |
| "I want to test resilience (chaos engineering)" | **kube-monkey / LitmusChaos** |
| "I need everything in one tool, free" | **Auto-Agent** |

## Auto-Agent + Other Tools

Auto-Agent is designed to **complement** existing tooling:

- **+ Prometheus/Grafana**: Auto-Agent exposes metrics, imports Grafana dashboard, queries Prometheus for scaling
- **+ Kubecost/OpenCost**: Real cost data in the Cost tab via API integration
- **+ PagerDuty/OpsGenie**: Auto-Agent sends incidents to PD for on-call routing
- **+ Slack**: Real-time alerts with interactive buttons
- **+ GitHub/GitLab**: Opens PRs for resource changes
- **+ Jira**: Creates tickets for tracking

## What Auto-Agent Does NOT Do

- **APM / tracing** — use Datadog, New Relic, or Jaeger
- **Log aggregation** — use ELK, Loki, or Datadog Logs
- **Chaos engineering** — use LitmusChaos or kube-monkey
- **Detailed cost allocation** — use Kubecost for showback/chargeback
- **Cluster provisioning** — use Terraform, Pulumi, or Cluster API
- **CI/CD** — use ArgoCD, Flux, or GitHub Actions
