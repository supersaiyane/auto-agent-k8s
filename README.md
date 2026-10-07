# auto-agent-k8s

A Kubernetes remediation agent. It runs as a DaemonSet, detects unhealthy pods,
workloads and nodes, and (only when you allow it) fixes them: restarts a
crashlooping pod, rolls back a stuck rollout, scales a deployment, cordons and
drains a node under pressure.

It ships in **dry-run**: it detects and records what it would do, and changes
nothing until you set `agent.mode: fix`.

## How it works

```
 pod / node informers ─┐
 leader-only loops ────┼─> detectors ─> applyMutation (internal/kube/gate.go) ─> Kubernetes API
                       │                  1. mode: observe | suggest | dry-run | fix
                       │                  2. guardrails: quiet hours, blast radius,
                       │                     CRD approval, circuit breaker
                       │                  3. rate limiter
                       └─> Slack, Alertmanager, tickets, LLM (redacted, internal/redact)
```

Every write to the cluster goes through one gate. Tests enforce the design,
not just describe it:

| Guarantee | Enforced by |
| --- | --- |
| No write outside the gate | `TestMutationsOnlyThroughGate` (parses the source) |
| No write outside `fix` or past a guardrail | `TestGate_NoMutationOutsideFixOrWhenBlocked` |
| Only the agent on a node acts on that node | `TestNodePressure_OnlyAgentOnThatNodeActs` |
| RBAC grants exactly what the code calls | `TestRBAC_ChartMatchesCode` (renders the chart) |
| Workload writes only in allowlisted namespaces | `TestRBAC_WritesAreNamespacedAndLeasePinned` |
| No client error silently dropped | `TestAPIErrorsAreNotSwallowed` |
| Dashboard API needs a token, reads only allowlisted namespaces | `TestAPI_EveryRouteRequiresToken`, `TestAPI_NoRouteLeaksNonAllowlistedNamespaces` |
| No secret leaves the cluster in LLM, Slack, tickets, PRs or alerts | `TestOutboundClientsRedact` |

## Install (Helm)

```bash
helm upgrade --install auto-agent charts/auto-agent -n kube-system \
  --set "agent.namespaceAllowlist={default,payments}" \
  --set dashboard.token="$(openssl rand -hex 32)"
```

- Every namespace in `agent.namespaceAllowlist` must exist; each gets a write
  Role, and the agent reads and acts nowhere else.
- Start in `dry-run` (the default), watch `/api/dry-run`, then set
  `agent.mode=fix` when the simulated actions look right.
- Without `dashboard.token` the `/api/` endpoints return 503.

Local quick start against a kind cluster: `./deployment/deploy.sh`.

## Develop

```bash
make tools    # pinned golangci-lint and govulncheck into bin/tools
make verify   # build, vet, race tests, lint, govulncheck, dash check, helm lint
make e2e      # kind cluster: dry-run leaves pods alone, API needs a token,
              # kubectl panel is scoped, pod runs non-root, no forbidden reads
```

Project rules (definition of done, constraints) are in [CLAUDE.md](CLAUDE.md);
the work plan is [docs/plans/PLAN-001-safety-hardening.md](docs/plans/PLAN-001-safety-hardening.md)
and open issues are in [tasks/ISSUES.md](tasks/ISSUES.md).

## Integrations

| Integration | Status |
| --- | --- |
| Slack alerts | Working; text is redacted. Interactive buttons are not sent yet |
| Alertmanager | Working; annotations are redacted |
| GitHub / GitLab PRs (OOM memory bump) | Working; PR text is redacted |
| GitHub Issues / Jira tickets | Working; redacted |
| LLM diagnosis | Working, off by default (`llm.enabled`); prompts are redacted |
| Prometheus metrics | Working |
| PagerDuty, OpsGenie, email | **Not wired**: the escalation chain is built but never called (ISS-012) |
| Learning mode threshold tuning | **Not wired**: baselines are collected, thresholds are not applied (ISS-012) |

Details: [docs/wiki/23-feature-status.md](docs/wiki/23-feature-status.md).

## Documentation

Full wiki: [docs/wiki/](docs/wiki/README.md), including
[configuration](docs/wiki/03-configuration.md),
[safety model](docs/wiki/05-safety.md),
[dashboard](docs/wiki/06-dashboard.md),
[security hardening](docs/wiki/15-security-hardening.md) and
[troubleshooting](docs/wiki/11-troubleshooting.md).

## License

MIT
