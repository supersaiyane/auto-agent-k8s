# Kubernetes That Heals Itself

Autonomous Kubernetes remediation agent — detects 74 failure conditions, fixes them automatically, and verifies recovery.

## Quick Start

```bash
./deployment/deploy.sh              # build + deploy + open dashboard
./deployment/test-apps/deploy-apps.sh   # deploy test apps to see it in action
```

Dashboard: **http://localhost:8080**

## What It Does

- **74 issue detectors** across pod, node, workload, storage, network, security
- **7 remediation actions** — pod delete, node cordon/uncordon, rollback, scaling, cleanup
- **8 safety layers** — dry-run, rate limiter, dedup, circuit breaker, blast radius, quiet hours, CRD policies
- **Fix verification** — confirms workload recovered after action
- **10-tab dashboard** with charts, cluster view, cost estimation, kubectl terminal
- **9 integrations** — Slack, LLM, GitOps PRs, Jira, PagerDuty, OpsGenie, Alertmanager, Kubecost

## Documentation

Full wiki: **[docs/wiki/](docs/wiki/README.md)**

| Guide | Description |
|-------|-------------|
| [Quick Start](docs/wiki/02-quickstart.md) | Deploy in 60 seconds |
| [Configuration](docs/wiki/03-configuration.md) | All settings and modes |
| [74 Detectors](docs/wiki/04-detection.md) | Every issue type with severity and action |
| [Safety Model](docs/wiki/05-safety.md) | 8-layer guardrail system |
| [Dashboard](docs/wiki/06-dashboard.md) | All tabs, charts, filtering |
| [API Reference](docs/wiki/16-api-reference.md) | 17 REST endpoints |
| [Deployment](docs/wiki/09-deployment.md) | Standalone, Helm, Docker Compose |
| [Troubleshooting](docs/wiki/11-troubleshooting.md) | Every error with fix |
| [FAQ](docs/wiki/21-faq.md) | 30+ questions answered |

## License

MIT
