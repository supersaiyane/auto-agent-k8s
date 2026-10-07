# PLAN-002: Config reload, weak features, fix ladder, coverage

Created: 2026-10-07
Status: proposed, not started
Builds on: PLAN-001 (done; PR #1)
Rules: `CLAUDE.md` (definition of done, constraints 1 to 11)
New issues: ISS-033 to ISS-038 in `tasks/ISSUES.md`

## Goal

1. **Config reload, built in, better than Reloader.** Restart workloads when
   their ConfigMaps or Secrets change, with no third-party dependency, every
   restart going through the mutation gate, and features Reloader does not
   have.
2. **Fix every weak feature** found so far, or remove it, so nothing in the
   chart or the docs claims more than the code does.
3. **A fix ladder for every detector.** Problems that are not safe to fix
   automatically still get a fix: a prepared, one-click, human-approved one.
4. **Test coverage from 26.1 percent to at least 95 percent** (measured
   2026-10-07), with 100 percent on every package that can change the cluster.

## How each task is run

Same as PLAN-001: confirm the evidence, write the failing test and watch it
fail for the right reason, make the smallest change, run `make verify` alone
and read it, run `make e2e` for anything that changes behaviour, update the
records in the same commit, commit naming the ISS. One phase per commit set,
in order, in the main session.

---

## Part A: Config reload (native)

### A.1 What Reloader does, and where we go further

The Reloader column describes Stakater Reloader v1 (the `master` branch of
`supersaiyane/Reloader`, forked from `stakater/Reloader`) as its README
describes it on 2026-10-07. Reloader v2 is developed separately and may add
some of these.

| Capability | Stakater Reloader v1 | auto-agent (this plan) |
| --- | --- | --- |
| Restart on ConfigMap / Secret change | Yes | Yes |
| Annotations `reloader.stakater.com/auto`, `configmap.reloader.stakater.com/reload`, `secret.reloader.stakater.com/reload`, `.../search` + `.../match` | Yes | **Read as is**, so existing workloads migrate with no edits, plus our own `auto-agent.io/reload*` names |
| Deployment, StatefulSet, DaemonSet | Yes | Yes |
| CronJob | Triggers a Job | Updates the CronJob template so the **next** run uses the new config; no surprise extra run |
| Argo Rollouts, OpenShift DeploymentConfig | Yes | Phase A4, only if the CRD is installed (detected at startup) |
| **Dry-run** (record the rollout it would do) | No | **Yes**, the default mode |
| **Guardrails** (quiet hours, blast radius, circuit breaker, rate limit) | No | **Yes**, through `applyMutation` |
| **Namespace allowlist** | Namespace selector flags | **Yes**, same allowlist as everything else, dashboard included |
| **Key-level change detection**: restart only if a key the workload actually uses changed | No: a change to the object restarts | **Yes**: `env.valueFrom` with one key only restarts on that key; `envFrom` on any key |
| **Knows when a restart is not needed**: mounted ConfigMap / Secret volumes update in place (without `subPath`) | No: always restarts | **Yes**, `reloadOn: auto` restarts for env, `envFrom` and `subPath` mounts; plain volume mounts are skipped (opt out with `always`) |
| **Debounce**: several edits within a window give one rollout | Not described | **Yes**, `reload.debounce` (default 10s) |
| **Rollout health check and automatic rollback** if the new pods do not become Ready | Not described | **Yes**: watches the rollout; if it stalls past `progressDeadlineSeconds`, rolls back to the previous ReplicaSet (reuses the PLAN-001 rollback) and alerts |
| **Staged reload across workloads** (one ConfigMap used by many deployments) | Not described | **Yes**: one workload at a time, next only after the previous is healthy; a failure stops the wave |
| **Never logs secret values** | n/a | Only SHA-256 hashes of the referenced keys are kept, in memory; redaction applies to every message |
| **Audit trail**: what changed, which workloads restarted, why | Events and logs | Audit log entry and dashboard "Reloads" tab: object, changed keys (names only), workloads, outcome |
| **Per-workload policy** | Annotations | Annotations plus `AutoRemediationPolicy` fields (`reload.enabled`, `reload.strategy`) |
| Notifications | Slack, Teams, webhook | Slack (redacted), Alertmanager, tickets, existing channels |
| RBAC | Cluster-wide watch on Secrets and ConfigMaps | ConfigMaps: allowlisted namespaces. **Secrets opt-in** (`reload.secrets: true`), allowlisted namespaces only |

### A.2 Design

- **Watch**: a ConfigMap informer, and a Secret informer only when
  `reload.secrets` is on, both limited to allowlisted namespaces, on the
  leader only (one actor per target).
- **Index**: for each workload in the allowlist, the set of (kind, name,
  keys or "all") it references through `env.valueFrom`, `envFrom`, volumes
  and projected volumes, plus names from the reload annotations. Rebuilt on
  workload change.
- **Detect**: on update, hash each referenced key; compare with the stored
  hash. No referenced key changed: nothing happens (logged at V(3)).
- **Decide**: apply `reloadOn` (`auto` or `always`), the annotations, the
  CRD policy, then debounce.
- **Act**: patch the pod template annotation
  `auto-agent.io/config-hash: <hash of referenced keys>` through
  `applyMutation` (`ActionType: config_reload`). The patch is a merge patch
  (constraint 6). Dry-run records it.
- **Verify**: hand the rollout to the existing fix tracker; on stall, roll
  back and alert.
- **RBAC**: ConfigMap get/list/watch and Secret get/list/watch (opt-in) in
  allowlisted namespaces; patch on deployments (exists), statefulsets,
  daemonsets, cronjobs. `TestRBAC_ChartMatchesCode` forces the chart to match.

### A.3 Tasks

| Task | Change | Done when |
| --- | --- | --- |
| A1.1 | Reference index: parse pod specs into (object, keys) references, including `envFrom`, `valueFrom`, volumes, projected, `subPath` | Table test over pod spec shapes; 100 percent coverage of the index |
| A1.2 | Change detector: key hashing, "referenced keys changed?" | Tests: unreferenced key change ignored; referenced key change detected; secret values never appear in any log or message (assert over captured logs) |
| A1.3 | Annotation compatibility: Stakater annotations and `auto-agent.io/reload*`, search and match | Table test per annotation from the Reloader README |
| A2.1 | Reload action through the gate for Deployment, StatefulSet, DaemonSet, CronJob | Added to `mutatingDrivers`, so it is covered in every mode and blocked state; AST guard passes |
| A2.2 | `reloadOn: auto` (skip in-place volume updates), debounce | Tests with a fake clock |
| A2.3 | Rollout verification and automatic rollback | Test: stalled rollout rolls back once and alerts |
| A3.1 | Staged reload for shared objects | Test: second workload waits for the first; a failure stops the wave |
| A3.2 | Chart values `reload.enabled` (default true, still dry-run by mode), `reload.secrets` (default false), `reload.reloadOn`, `reload.debounce`; RBAC; docs in CONFIGURATION.md and GUIDE | Config reference test and RBAC test pass |
| A3.3 | Dashboard "Reloads" tab and `/api/reloads`, allowlist-scoped | Route covered by the auth and namespace sentinel tests automatically |
| A3.4 | e2e: change a ConfigMap; in dry-run the reload is recorded and the pod is untouched; in fix mode the pod is replaced once, even after 3 quick edits | `make e2e` extended and falsified |
| A4.1 | Argo Rollouts support when its CRD exists | Test with a fake dynamic client |

---

## Part B: Weak features and how we fix them

| ID | Weak feature | Evidence | Fix | Decision |
| --- | --- | --- | --- | --- |
| ISS-033 | DNS check only looks at CoreDNS pod readiness; never resolves a name | `checkDNSHealth` | Per-node resolution probe: every agent resolves `kubernetes.default.svc` and a configurable external name each loop, records failures and latency; add CoreDNS SERVFAIL rate and p99 from Prometheus when configured. Per-node results catch NodeLocal DNSCache and node network faults the readiness check cannot | Do |
| ISS-034 | A Service whose selector matches no pods is never reported | `CheckServiceEndpoints` needs `len(Subsets) > 0` | New check: selector against pod labels; also `targetPort` not exposed by any selected container | Do |
| ISS-035 | Endpoints API is deprecated | `CoreV1().Endpoints` reads | Move to `discovery.k8s.io` EndpointSlices; RBAC follows | Do |
| ISS-036 | No network fault coverage beyond the above | n/a | Pod sandbox / CNI events (`FailedCreatePodSandBox`, `NetworkNotReady`); kube-proxy and CNI DaemonSet pod down per node; NetworkPolicy static analysis (Service port with no ingress allowed); opt-in TCP probe to allowlisted Service ClusterIPs; opt-in egress probe; conntrack usage from node-exporter; Ingress TLS secret missing (opt-in secret read) | Do |
| ISS-012 | Escalation chain built but never called | `cmd/auto-agent/main.go` | **Wire it**: critical incidents and failed fixes escalate (PagerDuty, OpsGenie, email) after Slack, deduplicated, redacted. The code exists; leaving it dead is the worst option | Do (recommended; owner can veto) |
| ISS-012 | Learning baselines collected, thresholds never applied | `GetThreshold` has no caller | Use the learned per-workload CPU baseline as the scale threshold once `minSamples` is reached, falling back to the global value; show which was used in the scaling event | Do |
| ISS-012 | Slack buttons never sent; callbacks unwired | `BuildIncidentBlocks` no caller | Becomes the approval channel of Part C | Do (Part C) |
| ISS-012 | `gitops.mode`, `images.mirror.*` read by nothing | CONFIGURATION.md | `gitops.mode: live` has no safe meaning (it would commit to a deploy branch); **remove** both values. Image mirroring belongs to the admission webhook if wanted later | Remove |
| ISS-037 | CRD fields parsed but unused: `restartStuckPods`, `scale.minReplicas`, `scale.step`, `scale.allowHPAOverride`, `safety.cooldown`, `safety.maxActionsPerHour`, `escalation.slackChannel`, `escalation.ticketing` | grep 2026-10-07 | Wire each into the gate or the action it names (per-policy cooldown and action budget into the guardrails, scaling fields into the scaler, Slack channel and ticketing into the notifier). A field that cannot be honoured is removed from the CRD schema | Do |
| ISS-032 | `agent.logLevel` unused | `LogLevel` no reader | Map to klog verbosity at startup | Do |
| ISS-032 | `webhook.enabled` registers a webhook the agent never serves | no cert env | Chart mounts a cert-manager Certificate (or a provided Secret) and sets `WEBHOOK_CERT_FILE` / `WEBHOOK_KEY_FILE`; e2e calls the webhook | Do |
| ISS-032 | `anomalies.pollInterval`, `gitops.valuesFile`, `gitops.author.*` read by nothing | CONFIGURATION.md | Wire `valuesFile` and `author` into the OOM pull request; drop `pollInterval` (the scale loop drives anomalies) | Do |
| ISS-015 | Package globals (`handlerSem`, `httpapi.SetExtendedDeps`, `storage.GlobalSink`) | code | Inject through constructors; needed for Part D too | Do |
| ISS-018 | No tracing; metric labels include workload and node (cardinality) | `obs/metrics.go` | OpenTelemetry spans per incident and action (opt-in exporter); drop `workload` from high-volume counters, keep it on the audit log and events | Do |
| ISS-025 | Housekeeping spends the blast-radius budget | PLAN-001 | Separate, smaller budget for housekeeping actions (evicted pods, old jobs) | Do (recommended) |
| ISS-038 | Recovery verification is untested and only checks pod phase | `fixtracker.go` 0 percent | Verify per action type: restart (container stays Ready N minutes), rollback (rollout complete), scale (replicas available); feed failures to escalation | Do |

---

## Part C: The fix ladder (what cannot be auto-fixed, and how it still gets fixed)

Not every problem is safe to fix without a person: the fix may lose data,
needs intent only the owner has, or the cause is outside the cluster. Instead
of stopping at "alert", every detector gets the highest rung that is safe.

| Rung | Meaning | Who acts |
| --- | --- | --- |
| **R4 Auto-fix** | Applied through the gate in `fix` mode | Agent |
| **R3 Approve-to-fix** | The agent prepares the exact change (a patch or a pull request), shows it in Slack and the dashboard with **Approve** / **Reject**; on approve it applies it through the same gate | Person clicks, agent applies |
| **R2 Pull request** | The fix belongs in Git (manifest, Helm values); the agent opens the PR | Person merges |
| **R1 Guided** | Diagnosis plus the exact `kubectl` commands, prefilled | Person runs them |
| **R0 Alert** | Cause outside the cluster; explain and link the runbook | Person |

### C.1 Every detector on the ladder

| Detector | Today | Target rung | Why not higher |
| --- | --- | --- | --- |
| CrashLoopBackOff, ImagePullBackOff (transient), init failure, NotReady | R4 delete pod | R4 | Safe: controller recreates |
| Stuck rollout | R4 rollback | R4 | Safe: previous ReplicaSet |
| Node pressure | R4 cordon and evict | R4 | Safe within guardrails |
| CPU scaling | R4 scale | R4 | Bounded by min, max, cooldown |
| Evicted pods, old failed jobs | R4 cleanup | R4 (own budget, ISS-025) | |
| ConfigMap / Secret changed (Part A) | none | R4 | Through the gate, with rollback |
| OOMKilled | R2 PR (memory bump) | **R3**: approve applies the memory patch now and opens the PR so Git follows | A live patch without Git drifts from GitOps |
| ImagePullBackOff (bad tag or missing secret) | R4 delete (does not help) | **R3**: offer rollback to the last image that ran | Needs intent: the new tag may be wanted |
| CreateContainerConfigError (missing ConfigMap / Secret key) | alert | **R1** with the exact missing object and key; R3 if a previous version of the object had the key | Creating config needs its real value |
| Pending (unschedulable) | alert | **R1**: names the constraint (taint, affinity, quota, PVC) and the command | Needs capacity or intent |
| Pending PVC, missing StorageClass, volume attach failure | alert | R1 | Storage changes risk data |
| Quota exhausted, LimitRange violation | alert | **R3**: propose a quota increase within a configured ceiling | Spend decision |
| HPA problems | alert | R1 | |
| Missed CronJob, deadline exceeded | alert | R3: "run now" Job | Side effects of the job |
| DaemonSet missing pods | alert | R3: delete the stuck pod on that node | |
| Deployment paused | alert | R3: resume | Someone paused it on purpose |
| No endpoints, selector matches no pods (ISS-034) | partial | R1 with the selector and the closest matching labels | Labels are intent |
| DNS failing (ISS-033) | readiness only | R3: restart the unhealthy CoreDNS pod; R0 if upstream DNS fails | Upstream is outside the cluster |
| kube-proxy / CNI pod down (ISS-036) | none | R3: restart that pod on that node | Node networking blast radius |
| NetworkPolicy blocks a Service | none | R1: shows the policy and the missing rule | Security intent |
| LoadBalancer pending | alert | R0 / R1: cloud quota or controller | Outside the cluster |
| Ingress backend missing or TLS secret missing | alert | R1 | |
| Certificate expiring | alert | R1 (cert-manager renew command) | |
| RBAC denied, webhook blocking | alert | R1 | Security intent |
| Clock skew, container runtime, PID pressure | alert | R4 for PID pressure (cordon), R0 otherwise | Node-level |
| Anomalies | alert | R1 | Needs interpretation |

### C.2 Tasks

| Task | Change | Done when |
| --- | --- | --- |
| C1.1 | Pending-approval queue: an approval holds the exact mutation, expires (default 30 min), and is applied through `applyMutation` (guardrails are checked again at approve time) | Tests: approve applies once; expired or rejected never applies; replayed approval rejected |
| C1.2 | Slack blocks with Approve / Reject wired to the queue (signature check already in place); dashboard "Approvals" tab and `/api/approvals` | Tests over the signed callback; auth and namespace sentinel tests cover the route |
| C1.3 | Who approved is recorded in the audit log | Test |
| C2.x | One task per detector row that moves up the ladder | Each detector's tests assert its rung |

---

## Part D: Coverage, why it is low, and how to reach 95 percent and above

### D.1 Why it is low (measured 2026-10-07: 26.1 percent overall)

| Cause | Where | Effect |
| --- | --- | --- |
| Detectors written without tests | `internal/kube` (24.0 percent); every network and storage check at 0 | Bugs like ISS-029 and ISS-034 shipped unseen |
| 12 packages with no test file | `cmd`, `alertmanager`, `crd`, `escalation`, `integrations`, `leader`, `llm`, `logging`, `obs`, `slack`, `storage`, `webhook` | 0 percent each |
| Configuration read inside functions with `os.Getenv` | `scaler.go`, `security.go`, `cost.go`, `storage.go`, `main.go` | Tests must set process-wide env; branches are skipped |
| Package globals and `init()` | `cost.go` `init`, `storage.GlobalSink`, `httpapi.SetExtendedDeps`, `handlerSem` | Hard to reset between tests |
| HTTP clients not injectable | `slack`, `llm`, `integrations`, `alertmanager`, `escalation` | Only reachable through `http.DefaultTransport` swaps |
| `main` does everything | `cmd/auto-agent/main.go` | 0 percent; wiring bugs only show in e2e |
| Time read directly (`time.Now`) | dedup, breaker, quiet hours, learning | Time-based branches untested |

### D.2 How we get there

| Step | Change | Target |
| --- | --- | --- |
| D1 | **Config struct**: one `config.Load()` reads every variable (driven by the same list as CONFIGURATION.md); components receive values, never call `os.Getenv` | Env-dependent branches testable with plain structs |
| D2 | **Injected clock** (`Now func() time.Time`) for dedup, breaker, quiet hours, learning, debounce | Time branches tested without sleeping |
| D3 | **Injected HTTP client** for every outbound client; remove globals and `init()` (ISS-015) | Each client tested with `httptest` |
| D4 | **`run(ctx, cfg, clients) error`** extracted from `main`; `main` only parses and calls it | `cmd` tested with fakes; a smoke test boots the whole agent against a fake clientset |
| D5 | **One table-driven test file per detector**: seed a fake cluster with the bad state and a healthy control; assert the message, the metric, the rung and (for R4) the gated mutation. Category test: a detector called from `main` without a test fails `TestEveryDetectorHasATest` (AST walk of the loop calls) | `internal/kube` 95 percent |
| D6 | Tests for the 12 untested packages | Each 95 percent |
| D7 | **Coverage gate in `make verify` and CI**: total coverage may not drop below the stored floor (`.coverage-floor`), and the floor rises with each phase; 100 percent required for `gate.go`, `policy`, `redact`, `ratelimit`, `httpapi` auth, the reload and approval packages | Regression impossible without editing the floor in review |
| D8 | **Mutation testing** on the safety-critical packages (gate, guardrails, redact, approvals): a surviving mutant fails the run | Coverage that actually asserts, not just executes |

"Near 100 percent" here means at least **95 percent of statements overall**
and **100 percent on code that can change the cluster or leak data**. The
remaining few percent (process exit paths in `run`, unreachable error
branches from `json.Marshal` of plain maps) are listed in `.coverage-floor`
with a reason each, so every uncovered line is a decision, not an accident.

---

## Phases and order

| Phase | Content | Why this order | Estimate (modelled, one engineer) |
| --- | --- | --- | --- |
| 8 | D1 to D4 (testability refactor), D7 gate at the current number | Every later phase needs it; the floor stops regressions immediately | 4 to 6 days |
| 9 | D5 for existing detectors; ISS-034, ISS-035 fixed on the way | Network and storage checks are the weakest | 5 to 8 days |
| 10 | Part A, config reload (A1 to A3), with tests at 100 percent | The headline feature | 6 to 9 days |
| 11 | Part B network (ISS-033, ISS-036) | Builds on phase 9 tests | 4 to 6 days |
| 12 | Part C approval queue and ladder moves | Needs reload's rollout verification and the escalation wiring | 6 to 9 days |
| 13 | Part B remaining (escalation, learning, CRD fields, webhook certs, tracing, ISS-025, ISS-038), D6, D8 | Reaches the coverage target | 6 to 10 days |
| 14 | A4 (Argo Rollouts), floor raised to the final target | Optional extras last | 2 to 3 days |

Total: about 33 to 51 engineer days (modelled). Each phase ends with
`make verify`, `make e2e`, a commit set, and the coverage floor raised to
the measured value.

## Open questions for the owner

- Part B: wire the escalation chain (recommended) or delete it?
- Part B: remove `gitops.mode` and `images.mirror.*` (recommended)?
- Part A: should `reload.secrets` stay off by default (recommended: yes, it
  needs Secret read access)?
- Part C: approval expiry (proposed 30 minutes) and who may approve (any
  Slack user in the channel, or a configured group)?
