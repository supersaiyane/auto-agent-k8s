# Changelog

## v1.0.0 — Full Production Release

### Core Engine
- 74 issue detectors across pod, node, workload, storage, network, security, config
- 7 remediation actions: pod delete, node cordon/uncordon, rollback, eviction cleanup, job cleanup, scaling
- 8-layer safety: dry-run, rate limiter, dedup, circuit breaker, blast radius, quiet hours, CRD approval, HPA awareness
- Fix verification: FixTracker confirms workload recovery after action

### Dashboard
- 10-tab embedded UI (Events, K8s Events, Actions, Charts, Report, Cluster, Nodes, Cost, Resources, Terminal)
- Pure SVG/CSS charts (3 pie + 3 bar) — no external dependencies
- Clickable stat boxes filter events by type
- Cross-tab navigation: Report → Events filtered by reason/workload
- Live ticking uptime counter
- kubectl terminal via API (read-only)

### Integrations
- Slack (per-channel + Block Kit interactive buttons)
- LLM diagnosis (OpenAI-compatible)
- GitOps PRs (GitHub + GitLab) with actual file content
- Ticketing (GitHub Issues + Jira) with dedup
- Alertmanager structured alerts
- PagerDuty Events API v2
- OpsGenie Alerts API v2
- Email via SMTP
- Kubecost / OpenCost cost integration

### Intelligence
- Learning mode: baseline CPU collection, auto-tuned thresholds
- Incident correlation: connect incidents to recent deploys
- Cost estimation: per-node, per-namespace, per-workload with 40+ instance type prices
- Compliance reporting: MTTR, remediation rate
- Resource efficiency: overuse/underuse/no-limits per pod

### Operations
- Config hot-reload via ConfigMap watch
- Persistent event storage (events.jsonl on hostPath)
- Persistent audit log (audit.jsonl)
- Agent self-monitoring (Prometheus, Slack, Alertmanager health)
- Log retention cleanup (hourly, configurable days)
- Structured JSON logging option
- Admission webhook (validates limits, probes, image tags)

### Deployment
- Standalone manifests (deployment/ folder)
- Helm chart with CRDs
- Docker Compose for image building
- Auto-install Kubecost/OpenCost based on COST_PROVIDER
- Auto-detect cluster type for image loading

### Testing
- Unit tests (ratelimit, policy, helpers, scaler, watcher, metrics)
- Integration tests with fake.Clientset
- Chaos test suite (22 workloads, 25+ automated checks)
- 3-app test suite across 3 namespaces
