# Auto-Agent Wiki

Complete documentation for the Kubernetes auto-remediation agent.

## Getting Started
1. [Overview](01-overview.md) — What it is, architecture, feature summary
2. [Quick Start](02-quickstart.md) — Deploy in 60 seconds

## Configuration
3. [Configuration Reference](03-configuration.md) — All settings, modes, env vars
4. [CRD Reference](17-crd-reference.md) — AutoRemediationPolicy per-workload policies

## Detection & Safety
5. [All 74 Issue Detectors](04-detection.md) — Every issue by category with severity, timing, action
6. [Safety & Guardrails](05-safety.md) — 8-layer protection model
7. [Operator Runbooks](12-runbooks.md) — Manual steps for each issue type

## Dashboard & APIs
8. [Dashboard Guide](06-dashboard.md) — All 10 tabs, charts, filtering, cross-tab navigation
9. [API Reference](16-api-reference.md) — All 17 REST endpoints with examples
10. [Metrics & Alerting](18-metrics-alerting.md) — Prometheus metrics, Grafana, alert rules

## Integrations
11. [Integrations](07-integrations.md) — Slack, LLM, GitOps, Jira, PagerDuty, Kubecost

## Operations
12. [Deployment Guide](09-deployment.md) — Standalone, Helm, Docker Compose
13. [Cost Optimization](13-cost-optimization.md) — Pricing sources, waste identification
14. [Scaling for Production](14-scaling-production.md) — 100+ nodes, tuning, HA
15. [Security Hardening](15-security-hardening.md) — RBAC, secrets, network policies

## Development
16. [Architecture Deep Dive](10-architecture.md) — Package structure, event flow, data persistence
17. [Testing Guide](08-testing.md) — Chaos suite, unit tests, break-fix manual testing
18. [Contributing](20-contributing.md) — Add a new detector, PR checklist
19. [Changelog](19-changelog.md) — Version history

## Reference
20. [Troubleshooting](11-troubleshooting.md) — Every error mapped to root cause + fix
21. [FAQ](21-faq.md) — 30+ real questions answered
22. [Comparison](22-comparison.md) — Auto-Agent vs Kubecost vs PagerDuty vs Datadog
