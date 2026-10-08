# Feature Status: What's Running vs What Needs Configuration

This page documents the honest status of every feature: what's actually working out of the box vs what needs credentials/URLs to activate.

## Actually Working (no config needed)

| Feature | Proof | Notes |
|---------|-------|-------|
| **Issue detectors** | Pod handlers in `internal/kube/watcher.go`; leader checks listed in `leaderChecks` (`cmd/auto-agent/run.go`); each audited and tested in `internal/kube/*_audit_test.go` (PLAN-002 phase 12); `TestEveryDetectorHasATest`, `TestEveryLeaderCheckHasATest` | Pod, node, workload, storage, network and security checks, each with its fix-ladder rung |
| **Pod deletion (fix mode)** | `tryFixAction: SUCCESS` in agent logs | Deletes crashing pods, controller recreates them |
| **Fix verification** | FixTracker confirmed 2+ fixes | Verifies deployment healthy after action (checks ReadyReplicas) |
| **Dashboard UI (16 tabs, counted 2026-10-08)** | `internal/httpapi/ui/`; `make ui-test` opens every tab | Events, Audit, Dry run, Fixes, Compliance, Deploys, Baselines, K8s events, Charts, Report, Cluster, Nodes, Cost, Resources, Terminal, Settings |
| **Event persistence** | 258+ events on disk | Survives pod restarts via `events.jsonl` on hostPath volume |
| **Dedup / rate limiter / circuit breaker** | Dedup skipped events visible in logs | Fix scope, dry-run, rate limiter, dedup, circuit breaker, blast radius, quiet hours, CRD approval |
| **Node-local pod informer** | Filtered by `NODE_NAME` env var | Each pod watches only its own node's pods |
| **Leader election** | One pod acquires lease, runs periodic scans | Lease `auto-agent-leader` in the agent's namespace |
| **Config hot-reload** | ConfigMap changes picked up every 30s | No pod restart needed for config changes |
| **Cost estimation** | Working with built-in instance prices | 40+ AWS/GCP/Azure instance types hardcoded |
| **Resource efficiency** | Pod overuse/underuse/no-limits analysis | Based on requests vs limits comparison |
| **kubectl terminal** (PLAN-003) | `internal/httpapi/terminal*.go`, one command table; `terminal_test.go`, `make ui-test` | Read-only: get (20 kinds), describe, logs, events, rollout status and history, auth can-i, agent scope/why/gate/status; writes, exec and secrets refused with the reason; outputs redacted |
| **Watch scope and fix scope** (ADR-002) | `internal/policy/scope.go`, gate check in `internal/kube/gate.go`; tests `internal/policy/scope_test.go`, `TestGate_OutsideFixScopeOnlySuggests`; kind e2e scope case in `scripts/e2e-kind.sh` | Watches every non-system namespace; acts only in the fix scope, inside the Helm ceiling; suggests elsewhere |
| **Settings tab** (fix scope from the dashboard) | `internal/httpapi/scope.go`, `internal/kube/scope_settings.go`, `internal/httpapi/ui/app.js` (`viewSettings`); tests `internal/httpapi/scope_test.go`, `TestSaveFixScope`, `make ui-test` | Changes stored in the `auto-agent-scope` ConfigMap, kept across Helm upgrades, audited |
| **Config reload** (PLAN-002 Part A) | `internal/kube/reload*.go`; tests `reload_refs_test.go`, `reload_test.go`, the gate driver "config reload"; kind e2e reload case | ConfigMaps by default, Secrets opt-in; key level; debounce; one workload at a time with rollback on stall; Stakater annotations; Reloads tab |
| **Namespace selector** | `internal/httpapi/ui/app.js` (`fillNamespaces`, `inNs`); `make ui-test` | Filters every tab; never changes what the agent does |
| **Audit log** | Actions logged to `audit.jsonl` | Persistent on hostPath volume |
| **CRD controller** | Watches AutoRemediationPolicy resources | Policies loaded into in-memory cache |
| **Admission webhook** | Code ready, validates limits/probes | Needs TLS certs to activate (see below) |

## Added in PLAN-002 phase 10 (2026-10-08)

Each runs on the leader, reports through one path that records its rung on
the fix ladder (R0 alert, R1 guided fix, R3 approve to fix once the approval
queue exists), and is tested with a bad case and a healthy control.
Prometheus rows need `METRICS_PROVIDER=prometheus` and stay silent without it.

| Detector | Reasons reported | Rung | Code | Test |
| --- | --- | --- | --- | --- |
| Pods stuck terminating | `PodStuckTerminating` | R1, R3 later | `internal/kube/podstate.go` | `TestStuckTerminating` |
| Volumes that do not attach or mount | `VolumeAttachFailed`, `VolumeMountFailed` | R1 | `internal/kube/podstate.go` | `TestVolumeFailures` |
| Probes failing before a crashloop | `LivenessProbeFailing`, `ReadinessProbeFailing`, `StartupProbeFailing` | R1 | `internal/kube/podstate.go` | `TestProbeFailures` |
| Why a pod cannot be scheduled (taint, affinity, resources, volume, topology spread) | `Unschedulable` | R1 | `internal/kube/podstate.go`, `scheduling.go` | `TestUnschedulable_ReportedByLeader`, `TestParseSchedulingFailure_OneCasePerCause` |
| Preemption, naming victim and preemptor | `Preempted` | R1 | `internal/kube/podstate.go` | `TestPreemption_NamesVictimAndPreemptor` |
| Readiness gates never met | `ReadinessGateUnmet` | R1 | `internal/kube/podstate.go` | `TestReadinessGates` |
| Namespaces and claims held by finalizers | `NamespaceStuckTerminating`, `PVCStuckTerminating` | R1 | `internal/kube/lifecycle.go` | `TestStuckFinalizers_NamespaceAndPVC` |
| Disruption budgets blocking evictions; refused evictions counted (`auto_agent_evictions_blocked_total`) | `PDBBlocksDisruption` | R1 | `internal/kube/lifecycle.go`, `nodes.go` | `TestDisruptionBudgets`, `TestNodePressure_EvictionRefusedByBudgetIsCounted` |
| Job hit its retry limit or deadline, reason in the alert | `JobFailed` | R1 | `internal/kube/jobs.go` | `TestFailedJob_ReasonInAlert` |
| HPA capped at its maximum for 15 minutes, higher maximum proposed within the policy ceiling | `HPAMaxedOut`, `HPAScalingFailed` | R1, R3 later | `internal/kube/workload_extended.go` | `TestHPA_StuckAtMaxOnlyWhenLimited` |
| Image pull cause: rate limit, unauthorized, not found, network; no retry where it cannot help | `ImagePullBackOff` | R1, or R4 for network | `internal/kube/handlers.go` | `TestImagePull_CauseDecidesRetry` |
| CPU throttling (Prometheus) | `CPUThrottled` | R1, R3 later | `internal/kube/promchecks.go` | `TestCPUThrottling` |
| Claims almost full; expansion offered only when the StorageClass allows it (Prometheus) | `VolumeAlmostFull` | R1, R3 later | `internal/kube/promchecks.go` | `TestVolumeAlmostFull` |
| etcd: no leader, leader churn, database near quota (Prometheus, self-managed control planes) | `EtcdNoLeader`, `EtcdLeaderChurn`, `EtcdDBNearQuota` | R0 | `internal/kube/promchecks.go` | `TestEtcdHealth` |
| Deprecated API use, with the replacement (Prometheus scraping the API server) | `DeprecatedAPIInUse` | R1 | `internal/kube/promchecks.go` | `TestDeprecatedAPIs` |

`TestEveryDetectorHasATest` (`internal/kube/detector_guard_test.go`) fails
when an exported `Check*` detector has no test; the older ones are listed
there until PLAN-002 phase 12 tests them.

## Code Exists: Needs Configuration to Activate

### Messaging & Alerting

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **Slack** | Sends incident alerts with logs and LLM diagnosis. Interactive buttons are not sent yet (`BuildIncidentBlocks` has no caller, ISS-012); the callback endpoint verifies Slack signatures (`SLACK_SIGNING_SECRET`) and replies that no action was taken | Set `SLACK_WEBHOOK_URL` in secrets | Agent detects and fixes silently: visible only in dashboard |
| **Alertmanager** | Sends structured alerts (AutoAgentIncident, AutoAgentCircuitBreaker) | Set `ALERTMANAGER_URL` (e.g., `http://alertmanager:9093`) | No Alertmanager alerts fired: `FireIncident()` returns nil |
| **PagerDuty** | Would trigger PD incidents for critical/warning severity. **Not wired (checked 2026-10-07, ISS-012):** the escalation chain is built in `cmd/auto-agent/run.go` but no handler calls it, so this setting has no effect. | Set `PAGERDUTY_ROUTING_KEY` in secrets | No pages: escalation chain skips PD |
| **OpsGenie** | Would create OG alerts for critical/warning. **Not wired (checked 2026-10-07, ISS-012):** the escalation chain is built in `cmd/auto-agent/run.go` but no handler calls it, so this setting has no effect. | Set `OPSGENIE_API_KEY` in secrets | No OG alerts |
| **Email** | Would send email for critical incidents. **Not wired (checked 2026-10-07, ISS-012):** the escalation chain is built in `cmd/auto-agent/run.go` but no handler calls it, so this setting has no effect. | Set `SMTP_HOST`, `SMTP_FROM`, `ESCALATION_EMAIL_TO` | No emails |

### Chart values that nothing reads

Checked 2026-10-07 (ISS-012): `gitops.mode` (`GITOPS_MODE`) and
`images.mirror.*` (`IMAGE_MIRROR_*`) are rendered into the ConfigMap, but no
Go code reads them. Setting them changes nothing.

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
| **LLM diagnosis** | Sends logs+events to LLM, gets SRE advice | Set `LLM_ENABLED=true`, `LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL` | No AI diagnosis: Slack messages won't have LLM section |
| **Learning mode** | Collects CPU baselines per workload (see `/api/baselines`). Thresholds are **not** auto-tuned yet: `LearningMode.GetThreshold` (`internal/kube/learning.go`) has no caller (ISS-012) | Set `LEARNING_ENABLED=true` + `METRICS_PROVIDER=prometheus` + `PROMETHEUS_URL` | Agent uses global static thresholds (SCALE_CPU_THRESHOLD) |
| **Auto-scaling** | Scales deployments based on CPU + gate signals | Set `METRICS_PROVIDER=prometheus`, `PROMETHEUS_URL` | No scaling: CPU queries return errors with metrics-server |
| **Anomaly detection** | Evaluates CRD PromQL rules | Set `PROMETHEUS_URL` + create AutoRemediationPolicy with anomalies | No anomaly detection: queries fail without Prometheus |

### Storage & Cost

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **S3 storage** | Persists incident logs to AWS S3 | Set `LOG_STORE=s3`, `LOG_S3_BUCKET`, `LOG_S3_PREFIX` + AWS credentials (IRSA) | Logs go to filesystem `/var/log/auto-agent/` on the node |
| **Kubecost** | Real cluster cost data from Kubecost API | Set `KUBECOST_URL` to Kubecost's API | Uses built-in instance-type price estimates |
| **OpenCost** | Real cluster cost data from OpenCost API | Set `OPENCOST_URL`; `deploy.sh --with-opencost` installs OpenCost | Same as above |

### Security

| Feature | What it does | How to activate | Without it |
|---------|-------------|-----------------|------------|
| **Admission webhook** | Validates deployments have limits, probes, non-blocked images | Set `WEBHOOK_CERT_FILE`, `WEBHOOK_KEY_FILE` + create ValidatingWebhookConfiguration | No admission validation: bad deployments are not prevented, only detected after the fact |
| **Namespace-scoped RBAC** | Per-namespace Role/RoleBinding instead of ClusterRole | Set `namespacedRBAC.enabled=true` in Helm values | ClusterRole grants access to all namespaces (agent only acts on allowlist though) |

## Quick Activation Guide

### Minimum viable production setup
```yaml
# auto-agent-secrets (kubectl patch secret ...):
SLACK_WEBHOOK_URL: "https://hooks.slack.com/services/..."   # alerts
```
That's it: you get Slack alerts for every incident. Everything else is optional.

### Recommended production setup
```yaml
# Config:
AUTO_MODE: "fix"
METRICS_PROVIDER: "prometheus"
PROMETHEUS_URL: "http://prometheus:9090"
ALERTMANAGER_URL: "http://alertmanager:9093"
KUBECOST_URL: "http://kubecost-cost-analyzer.kubecost:9090"
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
KUBECOST_URL: "http://kubecost-cost-analyzer.kubecost:9090"
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
| Slack | Check agent logs: `grep "slack:" logs`: should show POST, not "no webhook" |
| Alertmanager | `curl http://alertmanager:9093/api/v2/alerts`: check for AutoAgentIncident |
| GitOps | Check GitHub/GitLab repo for branches named `auto-agent/oom-*` |
| Tickets | Search Jira/GitHub Issues for `[auto-agent]` in title |
| LLM | Check Slack messages for `_LLM diagnosis_:` section |
| Kubecost | Dashboard Cost tab shows `source: kubecost` instead of `default` |
| Learning | `curl http://localhost:8080/api/baselines`: should show `learning: true` with baseline data |
| Prometheus | Agent logs should NOT show `metrics provider not implemented` |
