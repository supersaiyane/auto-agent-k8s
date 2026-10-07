# auto-agent-k8s: the complete guide

This guide takes you from "what is this?" to running it in production and
changing its code. Every statement here was checked against the code on
2026-10-07. Where a feature is configurable but does nothing yet, the guide
says so.

**Read what you need:**

| You are | Read |
| --- | --- |
| New to Kubernetes or to this project | [1](#1-what-it-is-in-plain-words), [2](#2-concepts-you-need), [3](#3-try-it-locally-in-10-minutes) |
| Operating it on a cluster | [4](#4-deploy-to-a-real-cluster), [5](#5-configuration), [6](#6-the-dashboard), [7](#7-the-http-api), [8](#8-day-2-operations), [9](#9-troubleshooting) |
| Reviewing or extending it | [10](#10-architecture), [11](#11-security-model), [12](#12-developing) |

---

## 1. What it is, in plain words

Kubernetes runs your applications in **pods**. Pods fail: a program crashes
on start, an image name is wrong, a node runs out of memory. Normally a
person gets paged, looks at the pod, and runs a fix such as restarting it or
rolling back a bad release.

auto-agent-k8s is a program that runs inside your cluster and does that
first look for you. It:

1. **Watches** pods, workloads and nodes for known failure patterns.
2. **Explains** what it found: it collects logs and events, can ask an LLM
   for a diagnosis, and posts to Slack, Alertmanager or a ticket.
3. **Fixes** a small set of well understood problems, but **only if you
   allow it**. Out of the box it is in **dry-run**: it writes down what it
   *would* do and touches nothing.

Every fix passes safety checks first: quiet hours, limits on how many
namespaces or how often it may act, per-workload circuit breakers, and a
rate limit.

---

## 2. Concepts you need

| Term | Meaning here |
| --- | --- |
| **DaemonSet** | One copy of the agent runs on every node. Each copy watches the pods on its own node. |
| **Leader** | One copy is elected leader (a Kubernetes Lease named `auto-agent-leader`). Only the leader runs the cluster-wide checks, so they are not done N times. |
| **Mode** | How much the agent may do: `observe` (alert only), `suggest` (alert and say what it would do), `dry-run` (record the exact action it would take, the default), `fix` (take the action). |
| **Allowlist** | The namespaces the agent may look at and act in (`agent.namespaceAllowlist`). It ignores everything else, dashboard included. |
| **Gate** | The single function every cluster change goes through (`applyMutation` in `internal/kube/gate.go`). It checks mode, guardrails and the rate limit. |
| **Guardrails** | Quiet hours, blast radius (max distinct namespaces acted on per hour), circuit breaker (max actions per workload per hour), and CRD `requireApproval`. |
| **Detector** | A check for one failure pattern, for example CrashLoopBackOff or a stuck rollout. |
| **Remediation** | The fix for a detected problem, for example deleting a crashlooping pod so it restarts. |
| **AutoRemediationPolicy** | Optional custom resource to tune behaviour for workloads matching labels. See [8.5](#85-per-workload-policies-crd). |

---

## 3. Try it locally in 10 minutes

You need Docker, [kind](https://kind.sigs.k8s.io/), kubectl and Helm.

### Option A: the one-command demo

```bash
kind create cluster
./deployment/deploy.sh
./deployment/test-apps/deploy-apps.sh   # sample broken apps to watch it work
```

`deploy.sh` builds the image, loads it into kind, applies the manifests in
`deployment/` (namespace `auto-agent`, allowlist `default,test1,test2`) and
waits for the pods. It prints the dashboard address when it is done.

### Option B: Helm, the way you would on a real cluster

```bash
kind create cluster
docker build -t auto-agent:dev .
kind load docker-image auto-agent:dev

TOKEN=$(openssl rand -hex 32)
helm upgrade --install auto-agent charts/auto-agent -n kube-system \
  --set image.repository=auto-agent --set image.tag=dev --set image.pullPolicy=Never \
  --set "agent.namespaceAllowlist={default}" \
  --set dashboard.token="$TOKEN"

kubectl -n kube-system rollout status ds/auto-agent
```

Make something break and watch the agent notice:

```bash
kubectl run crasher --image=busybox:1.36 --restart=Always -- sh -c 'echo boom; exit 1'
kubectl -n kube-system logs -l app=auto-agent -c agent -f | grep crasher
```

You should see `CrashLoopBackOff detected on default/crasher` within a
minute. The pod is **not** deleted, because the agent is in dry-run. See what
it would have done:

```bash
kubectl -n kube-system port-forward svc/auto-agent 8080:8080 &
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/dry-run
```

Open `http://localhost:8080` in a browser for the dashboard; paste the token
when asked.

Clean up: `kind delete cluster`.

### Option C: the automated end-to-end test

```bash
make e2e
```

This creates its own kind cluster, installs the chart in dry-run, breaks a
pod, and checks that the agent saw it, did not touch it, that the API needs
the token, that the kubectl panel is scoped, that the pod runs non-root, and
that no API read was forbidden. It deletes the cluster afterwards
(`KEEP=1 make e2e` keeps it).

---

## 4. Deploy to a real cluster

### 4.1 Before you install

1. **Choose the namespaces** the agent may watch and act in. Each one must
   already exist: the chart creates a write Role in each.
2. **Create a dashboard token** and store it safely:
   `openssl rand -hex 32`. Without it the dashboard API returns 503.
3. **Decide where Prometheus lives.** The NetworkPolicy only lets the
   namespaces in `networkPolicy.allowFromNamespaces` (default `monitoring`)
   reach port 8080. Add your ingress controller's namespace too if Slack
   callbacks come in through it.
4. **Image:** CI publishes `ghcr.io/supersaiyane/auto-agent-k8s` (tags: git
   sha, `latest` on master, semver on `v*` tags), signed with cosign.

### 4.2 Install in dry-run (always start here)

Write your settings in a values file rather than on the command line:

```yaml
# my-values.yaml
agent:
  mode: dry-run
  namespaceAllowlist: ["payments", "orders"]
dashboard:
  token: "<from your secret store>"
networkPolicy:
  allowFromNamespaces: ["monitoring", "ingress-nginx"]
slack:
  webhookUrl: "https://hooks.slack.com/services/..."
```

```bash
helm upgrade --install auto-agent charts/auto-agent -n kube-system -f my-values.yaml
kubectl -n kube-system rollout status ds/auto-agent
```

Secrets (`dashboard.token`, `slack.webhookUrl`, `slack.signingSecret`) end
up in the `auto-agent-secrets` Secret. In production, manage that Secret with
External Secrets or Sealed Secrets and keep the values out of Git.

### 4.3 Watch it for a while

Let it run in dry-run for a few days. Check:

- **What it would do:** dashboard Report and Actions tabs, or
  `GET /api/dry-run`.
- **That nothing is blocked by RBAC:**
  `auto_agent_api_errors_total{reason="forbidden"}` should stay at 0. A
  forbidden read also logs one warning naming the resource.
- **That alerts reach you:** Slack / Alertmanager.

### 4.4 Turn on fixing

When the simulated actions look right:

```bash
helm upgrade auto-agent charts/auto-agent -n kube-system -f my-values.yaml --set agent.mode=fix
```

or, without a rollout, edit the ConfigMap (the agent reloads it live):

```bash
kubectl -n kube-system patch configmap auto-agent-config --type merge -p '{"data":{"AUTO_MODE":"fix"}}'
```

To stop all actions immediately, set `AUTO_MODE` back to `dry-run` the same
way.

### 4.5 Upgrade and uninstall

```bash
helm upgrade auto-agent charts/auto-agent -n kube-system -f my-values.yaml
helm uninstall auto-agent -n kube-system
```

Uninstall leaves the `auto-agent-leader` Lease and any nodes the agent
cordoned (they carry the annotation `auto-agent.io/cordoned=true`). Check
with `kubectl get nodes` and uncordon by hand if needed.

---

## 5. Configuration

This section covers the settings most people change. **Every** setting (all
81 environment variables the agent reads, their Helm values, allowed values,
defaults and effect, plus the chart values that do nothing yet) is in
[docs/CONFIGURATION.md](CONFIGURATION.md), which a test keeps in step with
the code and the chart.

All settings are Helm values. The chart turns them into the
`auto-agent-config` ConfigMap (live-reloaded for the mode, allowlist and
scaling settings) and the `auto-agent-secrets` Secret.

### 5.1 Core

| Value | Default | What it does |
| --- | --- | --- |
| `agent.mode` | `dry-run` | `observe`, `suggest`, `dry-run` or `fix` |
| `agent.namespaceAllowlist` | `["default"]` | Namespaces to watch and act in; each must exist |
| `agent.excludedAnnotation` | `auto-agent.io/disable` | Put this annotation on a pod to make the agent ignore it |
| `agent.maxActionsPer10m` | `10` | Global rate limit |
| `agent.dedupTtlSeconds` | `300` | Same problem on the same workload is reported once per window |
| `namespace` | `kube-system` | Where the agent itself runs |

### 5.2 Guardrails

| Value | Default | What it does |
| --- | --- | --- |
| `guardrails.blastRadiusMaxNamespaces` | `5` | Max distinct namespaces acted on per hour |
| `guardrails.circuitBreakerThreshold` | `5` | Max actions per workload per hour before the breaker trips |
| `guardrails.quietHours` | empty | UTC windows with no actions, e.g. `02:00-06:00,22:00-23:00` |

### 5.3 Scaling

| Value | Default | What it does |
| --- | --- | --- |
| `agent.cpuThreshold` | `0.8` | Average CPU utilisation above which to scale up |
| `agent.maxScaleStep` | `2` | Replicas added per scale-up |
| `agent.minReplicas` / `maxReplicas` | `1` / `50` | Floor and cap |
| `agent.cooldownUp` / `cooldownDown` | `2m` / `10m` | Minimum time between scale events |
| `agent.hpaCoexistence` | `true` | Skip deployments that already have an HPA |
| `metricsProvider.type` | `prometheus` | `prometheus` or `metrics-server` |
| `metricsProvider.prometheusUrl` | in-cluster default | Where to query CPU |

Scale-down happens when average CPU utilisation is below 0.3.

### 5.4 Notifications and integrations

| Value | Default | What it does |
| --- | --- | --- |
| `slack.webhookUrl` | empty | Incident messages |
| `slack.signingSecret` | empty | Required for the Slack callback endpoint (it is rejected without it) |
| `llm.enabled`, `llm.apiUrl`, `llm.model` | off | LLM diagnosis in alerts; prompts are redacted |
| `tickets.enabled`, `tickets.provider` (`github` / `jira`) | off | Create or update a ticket per incident |
| `gitops.provider`, `gitops.repo`, `gitops.valuesFile` | github | Where OOM memory-bump PRs go |
| `alertmanager.url` | empty | Send structured alerts to Alertmanager |
| `logs.store` | `efs` | `efs` (host directory), `s3` or `none` for log bundles |
| `logs.efs.path` | `/var/log/auto-agent` | Host directory for log bundles and the audit log |

Tokens for GitHub, GitLab, Jira and the LLM go in the Secret
(`GIT_TOKEN`, `GITHUB_TOKEN`, `JIRA_TOKEN`, `JIRA_EMAIL`, `LLM_API_KEY`).

**Configurable but not wired yet** (setting them changes nothing,
ISS-012): `escalation.*` (PagerDuty, OpsGenie, email), `gitops.mode`,
`images.mirror.*`, and learning-mode threshold tuning.

### 5.5 Security and RBAC

| Value | Default | What it does |
| --- | --- | --- |
| `dashboard.token` | empty | Bearer token for `/api/`; empty means the API returns 503 |
| `networkPolicy.enabled` | `true` | Restrict who can reach port 8080 |
| `networkPolicy.allowFromNamespaces` | `["monitoring"]` | Namespaces allowed in |
| `rbac.readTLSSecrets` | `false` | Grants secret list in allowlisted namespaces and turns on the certificate expiry check |
| `leaderElection.namespace` | `kube-system` | Where the leader Lease lives (the Role follows it) |
| `webhook.enabled` | `false` | Admission webhook that rejects workloads without limits or probes |

### 5.6 Timing (advanced)

`loops.scaleInterval` (30s), `loops.jobInterval` (2m, workload checks),
`loops.quotaInterval` (5m, storage, network, security checks),
`loops.healthInterval` (3m, self check). Scale-up can also require PromQL
gates (`scalingGates.*`); see [CONFIGURATION.md](CONFIGURATION.md#scaling).

---

## 6. The dashboard

### 6.1 Open it

```bash
kubectl -n kube-system port-forward svc/auto-agent 8080:8080
```

Open `http://localhost:8080`. The page asks for the dashboard token once per
browser tab and keeps it in that tab's session storage only. A wrong token
shows as empty panels; reload the tab to be asked again.

With `deploy.sh` (local demo) the dashboard is on a NodePort:
`http://localhost:30080`.

### 6.2 Tabs

| Tab | What you see |
| --- | --- |
| **Events** | Every incident and action, newest first, with filters |
| **Actions** | Actions taken (or simulated) and whether the workload recovered |
| **Charts** | Incidents and actions over time |
| **Cluster** | Allowlisted namespaces: pods, deployments, services, jobs, health |
| **Nodes** | Nodes, conditions, pod counts |
| **Resources** | Requests, limits and usage per allowlisted namespace |
| **Cost** | Estimated cost per node, namespace and workload (allowlisted namespaces) |
| **Report** | Summary report, including the dry-run log |
| **Terminal** | A read-only kubectl: `get`, `describe`, `logs`, `top`, `version`, `help` |

The top bar shows version, mode, whether this pod is the leader, and the node.

### 6.3 The Terminal tab

```
get pods -n payments
describe deploy api -n payments
logs api-7d9f -n payments --tail 100
top pods -n payments
get nodes
```

Only allowlisted namespaces are readable; `-A` and other namespaces are
refused. Nodes and namespaces are always readable. Nothing in the terminal
can change the cluster.

---

## 7. The HTTP API

Every `/api/` call needs `Authorization: Bearer <token>`. Port 8080.

| Endpoint | Returns |
| --- | --- |
| `GET /api/status` | Version, mode, leader, node |
| `GET /api/events?limit=200&type=incident` | Recorded incidents and actions |
| `GET /api/stats` | Counters for the overview |
| `GET /api/fixes` | Actions and whether recovery was verified |
| `GET /api/dry-run` | What the agent would have done |
| `GET /api/cluster` | Per-namespace overview (allowlisted) |
| `GET /api/namespace/<ns>` | Detail of one allowlisted namespace (403 otherwise) |
| `GET /api/nodes` | Nodes |
| `GET /api/k8s-events?namespace=<ns>` | Kubernetes events (allowlisted) |
| `GET /api/resources`, `GET /api/resources/<ns>` | Resource usage |
| `GET /api/cost` | Cost estimate |
| `GET /api/compliance`, `/api/baselines`, `/api/deploys` | Compliance checks, learned baselines, recent deploys |
| `POST /api/kubectl` `{"command":"get pods -n default"}` | Terminal output |
| `POST /api/slack/actions` | Slack callback (Slack signature, not the token) |

No token: 401. No token configured on the server: 503.

```bash
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/status
curl -s -H "Authorization: Bearer $TOKEN" -X POST -d '{"command":"get pods -n default"}' localhost:8080/api/kubectl
```

`/healthz`, `/readyz` and `/metrics` need no token.

### 7.1 Metrics

`GET /metrics` (Prometheus format):

| Metric | Meaning |
| --- | --- |
| `auto_agent_incidents_total` | Problems detected, by reason, namespace, workload |
| `auto_agent_actions_total` | Actions applied, by type |
| `auto_agent_rate_limited_total` | Actions refused by the rate limiter |
| `auto_agent_dedup_skipped_total` | Duplicate reports suppressed |
| `auto_agent_handler_errors_total` | Handler failures, by handler and error type |
| `auto_agent_api_errors_total` | Failed API reads, by resource and reason (`forbidden` means RBAC is missing a grant) |
| `auto_agent_scaling_decisions_total` | Scale up / down decisions |
| `auto_agent_anomalies_detected_total` | CPU anomalies |
| `auto_agent_llm_requests_total` | LLM calls |
| `auto_agent_info` | Version and mode |

---

## 8. Day-2 operations

### 8.1 Change the mode without a restart

```bash
kubectl -n kube-system patch configmap auto-agent-config --type merge -p '{"data":{"AUTO_MODE":"dry-run"}}'
```

Takes effect within seconds; an invalid value is ignored with a warning and
the old mode stays.

### 8.2 Exclude one workload

Annotate its pods (in the Deployment's pod template):

```yaml
metadata:
  annotations:
    auto-agent.io/disable: "true"
```

### 8.3 Pause during a maintenance window

Set `guardrails.quietHours` (section 5.2), or switch to `dry-run` (8.1) and back.

### 8.4 Read the audit trail

Every action, blocked action and failure is written to
`/var/log/auto-agent/audit.jsonl` on the node (host path `logs.efs.path`), and
shows in the dashboard Events tab.

### 8.5 Per-workload policies (CRD)

```yaml
apiVersion: autoagent.io/v1alpha1
kind: AutoRemediationPolicy
metadata:
  name: payments-api
  namespace: payments
spec:
  targetSelector:
    matchLabels:
      app: api
  actions:
    bumpMemoryPercent: 30          # OOM memory-bump PRs raise memory by 30 percent
    scale:
      enabled: true
      maxReplicas: 20              # overrides agent.maxReplicas for these pods
  safety:
    requireApproval: true          # block every automated action for these pods
  escalation:
    runbookURL: https://wiki.example.com/runbooks/payments-api
  anomalies:
    - name: high_error_rate
      promql: 'rate(http_errors_total{app="api"}[5m])'
      zscoreThreshold: 2.0
      minSamples: 12
```

Fields that take effect (checked 2026-10-07): `targetSelector.matchLabels`,
`actions.bumpMemoryPercent`, `actions.scale.enabled` and `maxReplicas`,
`safety.requireApproval`, `anomalies`, `escalation.runbookURL` (added to
anomaly alerts). Parsed but **not used yet**: `actions.restartStuckPods`,
`scale.minReplicas`, `scale.step`, `scale.allowHPAOverride`,
`safety.cooldown`, `safety.maxActionsPerHour`, `escalation.slackChannel`,
`escalation.ticketing`.

### 8.6 Add a namespace

Create the namespace, add it to `agent.namespaceAllowlist`, and
`helm upgrade`. The upgrade creates its write Role.

---

## 9. Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `helm install` fails: namespace not found | Every allowlisted namespace must exist first; create it |
| Dashboard panels are empty | Wrong or missing token. Reload the tab to re-enter it; check `curl /api/status` returns 200 |
| `/api/...` returns 503 | `dashboard.token` is not set |
| Problems detected but nothing is fixed | Check the mode (`/api/status`). In `fix`, look for `gate: BLOCKED` in the logs: quiet hours, blast radius, circuit breaker or `requireApproval` stopped it, or `RATE LIMITED` |
| Node pressure ignored | Node actions are taken only by the agent pod on that node; check it runs there (`kubectl -n kube-system get pods -o wide`) and that `NODE_NAME` is set |
| `auto_agent_api_errors_total{reason="forbidden"}` rising | The chart is missing a grant for that resource. The warning log names it. Please open an issue; `TestRBAC_ChartMatchesCode` should have caught it |
| Kubectl panel says "not in the namespace allowlist" | Expected for namespaces outside the allowlist and for `-A` |
| Slack buttons answer "no action was taken" | Buttons are not wired yet (ISS-012) |
| No leader | Look for `acquired leader lease` in the logs and check the Lease in `leaderElection.namespace` |

Useful commands:

```bash
kubectl -n kube-system logs -l app=auto-agent -c agent --tail=200
kubectl -n kube-system get lease auto-agent-leader -o yaml
kubectl -n kube-system get configmap auto-agent-config -o yaml
```

---

## 10. Architecture

### 10.1 Components

```
                      +-------------------- each node -------------------+
 pods on this node -->| pod informer --> handlers (crashloop, OOM, ...)   |
 this node ---------->| node informer --> node pressure (own node only)   |
                      |                                                   |
                      |   leader only: loops every 30s / 2m / 5m          |
                      |   (scaling, jobs, rollouts, storage, network ...) |
                      |                        |                          |
                      |                        v                          |
                      |   applyMutation: mode -> guardrails -> rate limit |
                      |                        |                          |
                      +------------------------|--------------------------+
                                               v
                                       Kubernetes API (patch / delete / evict)

   alerts, tickets, PRs, LLM prompts --> internal/redact --> Slack, Jira, GitHub, LLM, Alertmanager
   dashboard + API (:8080, token)   --> allowlisted namespaces only
```

| Package | Responsibility |
| --- | --- |
| `cmd/auto-agent` | Wiring, leader loops, config from env |
| `internal/kube` | Detectors, handlers, the mutation gate, guardrails, dry-run log, audit log |
| `internal/policy` | Mode, allowlist, thresholds; ConfigMap hot reload with immutable snapshots |
| `internal/crd` | AutoRemediationPolicy watcher and matching |
| `internal/leader` | Lease-based leader election |
| `internal/httpapi` | Dashboard, API, kubectl panel, Slack callback, auth |
| `internal/redact` | Masks secrets and personal data in everything sent out |
| `internal/obs` | Prometheus metrics, API error counting |
| `internal/slack`, `alertmanager`, `integrations`, `llm` | Outbound clients |
| `charts/auto-agent` | The deployment of record |

### 10.2 Design rules

These are in [CLAUDE.md](../CLAUDE.md) and enforced by tests:

1. One gate for every write (`TestMutationsOnlyThroughGate`).
2. Safe by default: dry-run.
3. One actor per target: node actions only on the agent's own node; cluster
   loops only on the leader.
4. The allowlist applies to every read and write, dashboard included.
5. Policy is an immutable snapshot (`Deps.Policy()`).
6. Writes are conflict safe: merge patches, or re-read and retry.
7. RBAC matches the code exactly (`TestRBAC_ChartMatchesCode`).
8. Nothing leaves the cluster unredacted (`TestOutboundClientsRedact`).
9. Every inbound endpoint is authenticated.
10. Docs claim only what the code does.
11. Non-root, distroless, pinned images.

### 10.3 What happens when a pod crashloops (in `fix` mode)

1. The pod informer on that node sees the container waiting with reason
   `CrashLoopBackOff`.
2. The pod's namespace is checked against the allowlist and its annotations
   against `auto-agent.io/disable`; duplicates within the dedup window stop here.
3. Logs and events are collected and saved as a log bundle; the LLM is asked
   for a diagnosis if enabled.
4. `tryFixAction` hands "delete this pod" to the gate. The gate checks the
   mode, then quiet hours, blast radius, CRD approval and the circuit breaker,
   then the rate limiter.
5. The pod is deleted; its controller recreates it. The action is written to
   the audit log, counted in `auto_agent_actions_total`, and recorded for
   recovery verification.
6. A redacted message goes to Slack and Alertmanager; a ticket is opened if
   configured.

In `dry-run`, step 5 is replaced by an entry in the dry-run log.

---

## 11. Security model

| Concern | How it is handled |
| --- | --- |
| Who can see the dashboard | Bearer token on every `/api/` route, constant-time compare; NetworkPolicy on port 8080 |
| What the agent can change | Writes only via namespaced Roles in allowlisted namespaces, plus node patch; no secrets by default |
| Leader lock abuse | Lease `get`/`update` pinned to `auto-agent-leader` |
| Forged Slack actions | Slack v0 signature with a 5 minute replay window |
| Data leaving the cluster | Redaction of keys, tokens, URL credentials, JWTs, emails and IPs in LLM, Slack, tickets, PR text and alerts; LLM off by default |
| The container | Distroless, uid 65532, read-only root, no capabilities, seccomp RuntimeDefault, digest-pinned images |
| Supply chain | govulncheck in `make verify`; CI scans the image with Trivy, attaches an SBOM and signs it with cosign |

More: [docs/wiki/15-security-hardening.md](wiki/15-security-hardening.md).

---

## 12. Developing

### 12.1 Commands

| Command | What it does |
| --- | --- |
| `make tools` | Installs pinned golangci-lint and govulncheck into `bin/tools` |
| `make build` | Builds `bin/auto-agent` with the version from `git describe` |
| `make test` | All tests with the race detector |
| `make lint` | go vet and golangci-lint on changed code |
| `make vuln` | govulncheck |
| `make check-writing` | Fails on em or en dashes anywhere (project writing rule) |
| `make helm-lint` | Lints the chart |
| `make verify` | All of the above; the definition of done |
| `make e2e` | Full kind test (section 3, option C) |
| `make manifests` | Regenerates `deployment/02-rbac.yaml` from the chart |
| `make docker` | Builds the image with the version baked in |

### 12.2 Project rules

Read [CLAUDE.md](../CLAUDE.md) before changing code: the definition of done,
the architectural constraints, and where each kind of change is recorded.
Work is planned in [docs/plans/](plans/), tracked in
[tasks/ISSUES.md](../tasks/ISSUES.md) and [tasks/STATUS.md](../tasks/STATUS.md).

### 12.3 Adding a detector

1. Write a test first, in `internal/kube`, that seeds a fake cluster and
   asserts what the detector reports.
2. Implement it. If it reads a new kind of resource, add it to
   `typedAccessors` in `rbac_test.go` and grant it in
   `charts/auto-agent/templates/clusterrole.yaml`; the RBAC test fails until
   both agree.
3. Handle API errors with `countAPIError(err, "<resource>", ns)`; never
   discard them (`TestAPIErrorsAreNotSwallowed`).
4. If it fixes something, hand the write to `tryFixAction` or
   `applyMutation`, and add it to `mutatingDrivers` in `gate_test.go` so it is
   tested in every mode and blocked state.
5. Call it from the right leader loop in `cmd/auto-agent/main.go`.
6. `make verify`, then `make e2e`.

### 12.4 Known gaps

See [tasks/ISSUES.md](../tasks/ISSUES.md). The main open items: escalation
and learning tuning are not wired (ISS-012), many packages have no tests yet
(ISS-021), and the new CI pipeline has not run yet (ISS-017).
