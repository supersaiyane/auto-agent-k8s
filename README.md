# auto-agent-k8s

[![CI](https://github.com/supersaiyane/auto-agent-k8s/actions/workflows/ci.yaml/badge.svg?branch=master)](https://github.com/supersaiyane/auto-agent-k8s/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/tag/supersaiyane/auto-agent-k8s?sort=semver&label=release&color=blueviolet)](https://github.com/supersaiyane/auto-agent-k8s/tags)
[![License: MIT](https://img.shields.io/badge/license-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Kubernetes](https://img.shields.io/badge/kubernetes-agent-326CE5?logo=kubernetes&logoColor=white)](docs/GUIDE.md)
[![Helm](https://img.shields.io/badge/helm%20chart-1.0.0-0F1689?logo=helm&logoColor=white)](charts/auto-agent)
[![Coverage](https://img.shields.io/badge/coverage-95%25-brightgreen)](scripts/coverage-check.sh)
[![Mutation tested](https://img.shields.io/badge/mutation%20tested-gremlins-orange)](Makefile)
[![Image](https://img.shields.io/badge/image-signed%20%2B%20SBOM-success?logo=sigstore&logoColor=white)](.github/workflows/ci.yaml)
[![Default mode](https://img.shields.io/badge/default%20mode-dry--run-informational)](docs/CONFIGURATION.md)

A Kubernetes remediation agent. It detects unhealthy pods, workloads, nodes,
storage and networking, and (only where and when you allow it) fixes them:
restarts a crashlooping pod, rolls back a stuck rollout, scales a deployment,
cordons and drains a node under pressure, reloads workloads after a config
change.

It ships in **dry-run**: it detects and records what it would do, and changes
nothing until you set `agent.mode: fix`. Even then it acts only in the
namespaces you name (the fix scope).

## Start here

Everything is in **[docs/GUIDE.md](docs/GUIDE.md)**, the complete guide. Jump
to what you need:

| I want to... | Go to |
| --- | --- |
| Understand what this is (new to Kubernetes is fine) | [What it is](docs/GUIDE.md#1-what-it-is-in-plain-words), [Concepts](docs/GUIDE.md#2-concepts-you-need) |
| Try it on my laptop in 10 minutes | [Try it locally](docs/GUIDE.md#3-try-it-locally-in-10-minutes) |
| Deploy it to a real cluster | [Deploy](docs/GUIDE.md#4-deploy-to-a-real-cluster), [Configuration](docs/GUIDE.md#5-configuration) |
| Use the dashboard and the kubectl terminal | [Dashboard](docs/GUIDE.md#6-the-dashboard) |
| Call the API or scrape metrics | [HTTP API and metrics](docs/GUIDE.md#7-the-http-api) |
| Run it day to day (modes, pausing, per-workload policies) | [Day-2 operations](docs/GUIDE.md#8-day-2-operations) |
| Fix a problem with it | [Troubleshooting](docs/GUIDE.md#9-troubleshooting) |
| Review the design | [Architecture](docs/GUIDE.md#10-architecture), [Security model](docs/GUIDE.md#11-security-model), [ADR-001: node and controller roles](docs/adr/ADR-001-node-and-controller-roles.md), [ADR-002: watch and fix scope](docs/adr/ADR-002-watch-and-fix-scope.md), [ADR-003: durable approval queue](docs/adr/ADR-003-durable-approval-queue.md) |
| Look up any setting (every variable, its options and default) | [Configuration reference](docs/CONFIGURATION.md) |
| Change the code | [Developing](docs/GUIDE.md#12-developing), [CLAUDE.md](CLAUDE.md) |

### Quick start (kind)

```bash
kind create cluster
./deployment/deploy.sh                    # build, deploy, restart on the new image, print the dashboard address
./deployment/test-apps/deploy-apps.sh     # sample broken apps to watch it work
```

## How it works

It runs as two parts (ADR-001):

- a **node agent** (DaemonSet) on every node, which watches the pods on its
  own node and is the only one that acts on that node;
- a **controller** (Deployment, leader elected), which runs the cluster-wide
  checks, the dashboard and API, the approval queue and config reload.

```
 node pod informers ───┐
 leader-only loops ────┼─> detectors ─> applyMutation (internal/kube/gate.go) ─> Kubernetes API
                       │                  1. mode: observe | suggest | dry-run | fix
                       │                  2. fix scope (ADR-002)
                       │                  3. guardrails: quiet hours, blast radius,
                       │                     circuit breaker, CRD policy, approval
                       │                  4. rate limiter (housekeeping has its own)
                       └─> Slack, Alertmanager, PagerDuty, OpsGenie, email,
                           tickets, GitOps, LLM (all redacted, internal/redact)
```

Reads follow the **watch scope** (every non-system namespace by default) and
writes follow the **fix scope** (`agent.fixNamespaces`, empty by default, so
nowhere). Outside the fix scope the agent only suggests.

Every write to the cluster goes through one gate. Tests enforce the design,
not just describe it:

| Guarantee | Enforced by |
| --- | --- |
| No write outside the gate | `TestMutationsOnlyThroughGate` (parses the source) |
| No write outside `fix` or past a guardrail | `TestGate_NoMutationOutsideFixOrWhenBlocked` |
| No write outside the fix scope | `TestGate_OutsideFixScopeOnlySuggests` |
| Only the agent on a node acts on that node | `TestNodePressure_OnlyAgentOnThatNodeActs` |
| RBAC grants exactly what the code calls | `TestRBAC_ChartMatchesCode` (renders the chart) |
| Workload writes only through namespaced Roles | `TestRBAC_WritesAreNamespacedAndLeasePinned` |
| No client error silently dropped | `TestAPIErrorsAreNotSwallowed` |
| Dashboard API needs a token and reads only watched namespaces | `TestAPI_EveryRouteRequiresToken`, `TestAPI_NoRouteLeaksNonAllowlistedNamespaces`, `TestNodeFigures_ReadOnlyWatchedNamespaces` |
| No secret leaves the cluster in LLM, Slack, tickets, PRs or alerts | `TestOutboundClientsRedact` |

## Install (Helm)

```bash
helm upgrade --install auto-agent charts/auto-agent -n auto-agent --create-namespace \
  --set "agent.fixNamespaces={default,payments}" \
  --set dashboard.token="$(openssl rand -hex 32)"
```

- The agent watches every namespace except the system ones and acts only in
  `agent.fixNamespaces` (ADR-002). Each of those must exist and gets a write
  Role. The Settings tab can narrow or widen the fix scope later, but only
  inside `agent.fixCeiling`.
- Start in `dry-run` (the default), watch the Dry run tab or `/api/dry-run`,
  then set `agent.mode=fix` when the simulated actions look right.
- Without `dashboard.token` the `/api/` endpoints return 503.

Full walkthrough, production values file and upgrade steps:
[docs/GUIDE.md, section 4](docs/GUIDE.md#4-deploy-to-a-real-cluster).

## Develop

```bash
make tools      # pinned golangci-lint, govulncheck and gremlins into bin/tools
make verify     # build, vet, race tests, coverage gate, lint, govulncheck,
                # writing check, helm lint, generated manifests in sync
make e2e        # dashboard in headless Chrome, then a kind cluster: dry-run
                # leaves pods alone, API needs a token, scopes hold, non-root
make mutation   # mutation testing of the gate, guardrails, approvals,
                # redaction and rate limiting
make lint-shell # shellcheck over the scripts (install it: brew install shellcheck)
```

Project rules (definition of done, constraints) are in [CLAUDE.md](CLAUDE.md).
How each body of work was built is in [docs/plans/](docs/plans/), progress is
in [tasks/STATUS.md](tasks/STATUS.md) and every finding is in
[tasks/ISSUES.md](tasks/ISSUES.md).

## Features

Built from the code (handlers, detectors, the gate, the chart), checked on
2026-10-12. Every detector has a test (`TestEveryDetectorHasATest` keeps it
that way). Code paths and tests for each row:
[docs/wiki/23-feature-status.md](docs/wiki/23-feature-status.md).

### Detection

| Area | What it detects | Notes |
| --- | --- | --- |
| Pods (per node, real time) | CrashLoopBackOff, OOMKilled, ImagePullBackOff / ErrImagePull, CreateContainerConfigError, init container failures, NotReady, Pending, restart storms, RunContainerError, ContainerCannotRun, InvalidImageName, ErrImageNeverPull, PostStartHookError | Pod informer on each node |
| Pod states (leader) | Stuck terminating, volume failures, probe failures, unschedulable, preemption, readiness gates | `CheckPodStates` |
| Workloads (leader) | Stuck rollouts, failed Jobs, missed CronJobs, deadline exceeded, StatefulSet stuck, DaemonSet missing pods, paused Deployments, ReplicaSet failures, HPA issues, services with no endpoints, pending PVCs, ephemeral storage full, Deployment best-practice scan | Interval: `JOB_INTERVAL` |
| Nodes | Memory, disk and PID pressure, network unavailable, cordoned and forgotten, clock skew, container runtime issues | Pressure handled only by the agent on that node |
| Network | Per-node DNS, Service and egress probes, sandbox and CNI failures, NetworkPolicy blocks, conntrack and CoreDNS (Prometheus), Ingress TLS secrets | `netprobe.go`, `netchecks.go` |
| Storage and cluster | Missing StorageClass, volume attachment issues, claims almost full, LoadBalancer pending, Ingress backend missing, quota exhaustion, LimitRange violations, webhooks blocking, RBAC denials, API server throttling, etcd health, deprecated APIs, TLS cert expiry | Prometheus checks need `PROMETHEUS_URL`; cert expiry needs `rbac.readTLSSecrets` |
| Anomalies and learning | CPU anomalies against learned baselines; a learned threshold replaces the global one for scale-up after 50 samples | `learning.go` (`scaleUpThreshold`) |

Incidents name the Deployment (`deployment/<name>`), not its ReplicaSet, so
one app keeps one name across rollouts.

### Remediation (only in `fix` mode, in the fix scope, all through the gate)

| Action | Triggered by |
| --- | --- |
| Delete pod (restart) | CrashLoopBackOff, ImagePullBackOff, init container failure, NotReady |
| Roll back Deployment | Stuck rollout (ProgressDeadlineExceeded) |
| Scale Deployment up / down | CPU above / below threshold (skipped if an HPA exists) |
| Cordon node, evict pods, uncordon | Node pressure starts / clears (skips critical and StatefulSet pods) |
| Expand a PVC | Claim almost full, only when the StorageClass allows it, after approval |
| Reload workloads | A referenced ConfigMap (or, opt in, Secret) changed; one workload at a time, rolled back on stall |
| Clean up evicted pods, delete old failed Jobs | Housekeeping, on its own budget so it never spends the fix budget |
| Open a GitHub / GitLab PR (or a live commit) to raise memory | OOMKilled |
| Verify the fix worked | After every action |

Risky actions (R3) wait for a named approver, who approves from Slack. The
queue is stored in the `auto-agent-approvals` ConfigMap, so a restart or a
leader change does not lose it (ADR-003).

### Safety

| Feature | Status |
| --- | --- |
| Modes: observe, suggest, dry-run, fix | Working; dry-run is the default |
| Single mutation gate for every cluster write | Working; enforced by a source-parsing test |
| Watch scope and fix scope (ADR-002) | Working on every read and write, dashboard included |
| Guardrails: quiet hours, blast radius, circuit breaker, CRD policies | Working on every action; limits configurable |
| Rate limiters (fixes and housekeeping) | Working; fail closed if missing |
| Deduplication | Working |
| Dry-run log of simulated actions | Working; does not spend the real budget |
| One actor per node, leader-only cluster loops | Working |
| Conflict-safe writes | Working: merge patches, retries on conflict |
| Policy and config hot reload | Working, including switching to dry-run |

### Security

| Feature | Status |
| --- | --- |
| Dashboard API bearer token | Working; 503 if no token is set |
| Slack callback signature check, named approvers | Working |
| Least-privilege RBAC, checked against the code | Working; writes only in `agent.fixCeiling` (or one ClusterRole with `rbac.fixAnywhere`) |
| Forbidden API reads counted and logged | Working (`auto_agent_api_errors_total`) |
| Secret and PII redaction on everything sent out | Working: LLM, Slack, escalation, tickets, PRs, alerts, terminal output |
| NetworkPolicy | Working (allows `monitoring` by default) |
| Hardened container | Working: distroless, non-root, read-only root, pinned images |
| Admission webhook (limits, probes, blocked images; optional image mirror) | Working, off by default |

### Integrations

| Integration | Status |
| --- | --- |
| Slack alerts and approval buttons | Working |
| Alertmanager | Working |
| PagerDuty, OpsGenie, email | Working: critical findings and refused fixes |
| GitHub Issues / Jira tickets | Working |
| GitHub / GitLab PRs, or `gitops.mode: live` | Working (memory bump on OOM) |
| LLM diagnosis | Working, off by default (`llm.enabled`) |
| Prometheus / metrics-server CPU, Kubecost / OpenCost | Working |
| S3 / EFS log bundles, audit log | Working |

### Dashboard and API

| Feature | Status |
| --- | --- |
| Embedded web dashboard (events, fixes, cluster, nodes, resources, cost, dry run, approvals, reloads, compliance, baselines, settings) | Working |
| Read-only kubectl terminal (get, describe, logs, events, rollout status, auth can-i, agent why/gate/status) | Working; watched namespaces only, output redacted |
| Settings tab: change the fix scope inside the ceiling | Working; audited |
| `/metrics`, `/healthz`, `/readyz` | Working; readiness waits for informer sync |
| AutoRemediationPolicy CRD | Working; every field takes effect |

### Engineering and tooling

| Feature | Status |
| --- | --- |
| `make verify` | Working; fails on any lint or govulncheck finding |
| Test coverage | 95.0 percent of statements, measured 2026-10-12 (7364 of 7750); every package at 90 or more and code that can change the cluster or leak data at 100, enforced by `scripts/coverage-check.sh` |
| `make mutation` | Working; runs in CI |
| `make e2e` on kind | Working; runs in CI |
| CI: verify, e2e, mutation, image scan (Trivy), SBOM, cosign signing, semver tags | Working on every push and `v*` tag |

## Upcoming features

Not built yet. Each will get a plan in [docs/plans/](docs/plans/) before work
starts.

1. **Incident correlation.** Group findings with one cause into one incident,
   for example a node under memory pressure and the pods failing on it, so one
   problem sends one alert and gets one fix.
2. **OIDC sign-in for the dashboard.** Per-person sign-in instead of one
   shared bearer token, so the audit log names who did what and changing the
   fix scope can be limited to named people.
3. **Automatic ResourceQuota raises behind an owner-set ceiling.** Today
   quota exhaustion is a guided fix: the agent says what to raise and a person
   does it (ISS-077). The plan is to let it raise a quota through the gate,
   never past a ceiling the owner sets in the chart.

## Documentation

Full wiki: [docs/wiki/](docs/wiki/README.md), including
[configuration](docs/wiki/03-configuration.md),
[safety model](docs/wiki/05-safety.md),
[dashboard](docs/wiki/06-dashboard.md),
[security hardening](docs/wiki/15-security-hardening.md),
[troubleshooting](docs/wiki/11-troubleshooting.md) and the
[changelog](docs/wiki/19-changelog.md).

## License

MIT
