# auto-agent-k8s

A Kubernetes remediation agent. It runs as a DaemonSet, detects unhealthy pods,
workloads and nodes, and (only when you allow it) fixes them: restarts a
crashlooping pod, rolls back a stuck rollout, scales a deployment, cordons and
drains a node under pressure.

It ships in **dry-run**: it detects and records what it would do, and changes
nothing until you set `agent.mode: fix`.

## Start here

Everything is in **[docs/GUIDE.md](docs/GUIDE.md)**, the complete guide. Jump
to what you need:

| I want to... | Go to |
| --- | --- |
| Understand what this is (new to Kubernetes is fine) | [What it is](docs/GUIDE.md#1-what-it-is-in-plain-words), [Concepts](docs/GUIDE.md#2-concepts-you-need) |
| Try it on my laptop in 10 minutes | [Try it locally](docs/GUIDE.md#3-try-it-locally-in-10-minutes) |
| Deploy it to a real cluster | [Deploy](docs/GUIDE.md#4-deploy-to-a-real-cluster), [Configuration](docs/GUIDE.md#5-configuration) |
| Use the dashboard and the kubectl panel | [Dashboard](docs/GUIDE.md#6-the-dashboard) |
| Call the API or scrape metrics | [HTTP API and metrics](docs/GUIDE.md#7-the-http-api) |
| Run it day to day (modes, pausing, per-workload policies) | [Day-2 operations](docs/GUIDE.md#8-day-2-operations) |
| Fix a problem with it | [Troubleshooting](docs/GUIDE.md#9-troubleshooting) |
| Review the design | [Architecture](docs/GUIDE.md#10-architecture), [Security model](docs/GUIDE.md#11-security-model) |
| Look up any setting (every variable, its options and default) | [Configuration reference](docs/CONFIGURATION.md) |
| Change the code | [Developing](docs/GUIDE.md#12-developing), [CLAUDE.md](CLAUDE.md) |

### Quick start (kind)

```bash
kind create cluster
./deployment/deploy.sh                    # build, deploy, print the dashboard address
./deployment/test-apps/deploy-apps.sh     # sample broken apps to watch it work
```

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

Full walkthrough, production values file and upgrade steps:
[docs/GUIDE.md, section 4](docs/GUIDE.md#4-deploy-to-a-real-cluster).

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

## Features

Built from the code (handlers, detectors, the gate, the chart), checked on
2026-10-07. "Working" means the code path runs; the safety, security and
tooling rows are also covered by tests or the kind e2e. Most detectors are not
yet unit tested (ISS-021).

### Detection

| Area | What it detects | Notes |
| --- | --- | --- |
| Pods (per node, real time) | CrashLoopBackOff, OOMKilled, ImagePullBackOff / ErrImagePull, CreateContainerConfigError, init container failures, NotReady (failing readiness), Pending, restart storms, RunContainerError, ContainerCannotRun, InvalidImageName, ErrImageNeverPull, PostStartHookError | Pod informer on each node |
| Workloads (leader, every 2 min) | Stuck rollouts, failed Jobs, missed CronJobs, deadline exceeded, StatefulSet stuck, DaemonSet missing pods, paused Deployments, ReplicaSet failures, HPA issues, services with no endpoints, pending PVCs, ephemeral storage full, Deployment best-practice scan | Interval: `JOB_INTERVAL` |
| Nodes | Memory/disk pressure, PID pressure, network unavailable, node health, cordoned and forgotten, clock skew, container runtime issues | Pressure handled only by the agent on that node |
| Storage and network (every 5 min) | Missing StorageClass, volume attachment issues, DNS health, LoadBalancer pending, Ingress backend missing | Only a real NotFound raises these, never a permission error |
| Cluster and security (every 5 min) | Resource quota exhaustion, LimitRange violations, webhooks blocking, RBAC denials, API server throttling, TLS cert expiry | Cert expiry only with `rbac.readTLSSecrets` |
| Anomalies | CPU anomalies against learned baselines | Baselines collected; thresholds are not auto-tuned |

### Remediation (only in `fix` mode, all through the gate)

| Action | Triggered by |
| --- | --- |
| Delete pod (restart) | CrashLoopBackOff, ImagePullBackOff, init container failure, NotReady |
| Roll back Deployment | Stuck rollout (ProgressDeadlineExceeded) |
| Scale Deployment up / down | CPU above / below threshold (skipped if an HPA exists) |
| Cordon node, evict pods, uncordon | Node pressure starts / clears (allowlisted namespaces only, skips critical and StatefulSet pods) |
| Clean up evicted / failed pods | Leader loop, per namespace |
| Delete old failed Jobs | Failed CronJob runs older than 1 hour |
| Open GitHub / GitLab PR to raise memory | OOMKilled |
| Verify the fix worked | After every action |

### Safety

| Feature | Status |
| --- | --- |
| Modes: observe, suggest, dry-run, fix | Working; dry-run is the default |
| Single mutation gate for every cluster write | Working; enforced by a source-parsing test |
| Guardrails: quiet hours, blast radius, circuit breaker, CRD approval policies | Working on every action; limits configurable |
| Rate limiter | Working; fails closed if missing |
| Deduplication | Working |
| Dry-run log of simulated actions | Working; does not spend the real budget |
| One actor per node, leader-only cluster loops | Working |
| Namespace allowlist on every read and write | Working, dashboard included |
| Conflict-safe writes | Working: merge patches, rollback retries on conflict |
| Policy hot reload from ConfigMap | Working, including switching to dry-run |

### Security

| Feature | Status |
| --- | --- |
| Dashboard API bearer token | Working; 503 if no token is set |
| Slack callback signature check | Working |
| Least-privilege RBAC, checked against the code | Working; writes only in allowlisted namespaces |
| Forbidden API reads counted and logged | Working (`auto_agent_api_errors_total`) |
| Secret and PII redaction on everything sent out | Working: LLM, Slack, tickets, PRs, alerts |
| NetworkPolicy | Working (allows `monitoring` by default) |
| Hardened container | Working: distroless, non-root, read-only root, pinned images |

### Integrations

| Integration | Status |
| --- | --- |
| Slack alerts | Working; interactive buttons are not sent yet |
| Alertmanager | Working |
| GitHub Issues / Jira tickets | Working |
| GitHub / GitLab PRs | Working (memory bump on OOM) |
| LLM diagnosis | Working, off by default (`llm.enabled`) |
| Prometheus / metrics-server CPU | Working |
| S3 / EFS log bundles, audit log | Working |
| PagerDuty, OpsGenie, email | **Not wired**: escalation chain is built but never called (ISS-012) |
| `gitops.mode`, `images.mirror` chart values | **Not wired**: nothing reads them (ISS-012) |

### Dashboard and API

| Feature | Status |
| --- | --- |
| Embedded web dashboard (events, fixes, cluster, nodes, resources, cost, dry-run, compliance, baselines) | Working |
| Read-only kubectl panel (get, describe, logs, top) | Working; allowlisted namespaces only |
| `/metrics`, `/healthz`, `/readyz` | Working; readiness waits for informer sync |
| AutoRemediationPolicy CRD | Working |
| Admission webhook | Working, off by default |

### Engineering and tooling

| Feature | Status |
| --- | --- |
| `make verify` (build, vet, race tests, lint, govulncheck, dash check, helm lint) | Working |
| `make e2e` on kind (dry-run, token, scoping, non-root, RBAC) | Working |
| CI: verify, e2e, image scan, SBOM, cosign signing, semver tags | Written; not yet run (needs a push) |
| Known vulnerabilities (govulncheck) | 0 as of 2026-10-07 |

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
