# PLAN-002: Config reload, weak features, fix ladder, coverage

Created: 2026-10-07
Status: proposed, not started
Builds on: PLAN-001 (done; PR #1)
Rules: `CLAUDE.md` (definition of done, constraints 1 to 11)
New issues: ISS-033 to ISS-040 in `tasks/ISSUES.md`

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
| A3.2 | Chart values `reload.enabled` (default true, still dry-run by mode), `reload.secrets` (default **false**, owner decision), `reload.reloadOn`, `reload.debounce`; RBAC | Config reference test and RBAC test pass |
| A3.2b | **Secret reload usage documentation** (owner requirement): a GUIDE section "Reloading on Secret changes" covering why it is off, the exact value to enable it, the RBAC it adds (Secret get/list/watch in allowlisted namespaces only), that only key hashes are kept and values are never logged, how to scope it with annotations, how to test it in dry-run, and how to turn it off again; CONFIGURATION.md rows for every `reload.*` value with a warning on `reload.secrets` | Docs reviewed against the code; every value appears in the config reference test |
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
| ISS-012 | Escalation chain built but never called | `cmd/auto-agent/main.go` | **Wire it**: critical incidents and failed fixes escalate (PagerDuty, OpsGenie, email) after Slack, deduplicated, redacted. The code exists; leaving it dead is the worst option | **Decided 2026-10-07: wire it** |
| ISS-012 | Learning baselines collected, thresholds never applied | `GetThreshold` has no caller | Use the learned per-workload CPU baseline as the scale threshold once `minSamples` is reached, falling back to the global value; show which was used in the scaling event | Do |
| ISS-012 | Slack buttons never sent; callbacks unwired | `BuildIncidentBlocks` no caller | Becomes the approval channel of Part C | Do (Part C) |
| ISS-012 | `gitops.mode`, `images.mirror.*` read by nothing; no code implements either (checked 2026-10-07, enforced by `TestConfigReference_MatchesCodeAndChart`) | CONFIGURATION.md | **Keep both as flags, off by default, behind explicit warnings, and implement them** (phase 17). `gitops.mode: live`: commit the fix straight to the configured branch instead of opening a PR; warning in values, docs and the startup log that it skips code review, and it still goes through the gate. `images.mirror`: the admission webhook rewrites matching image references to the mirror prefix; warning that it changes pods as they are created, allowlist only. Until implemented, values and docs say "not implemented" | **Decided 2026-10-07: keep, off by default, warn, implement** |
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

## Part E: Every issue, not only network

Part B lists weak spots already found. It is not a complete audit. Two gaps
remain (ISS-039, ISS-040), measured 2026-10-07:

### E.1 Audit every existing detector (ISS-039)

Each detector gets a written review in its phase 12 test file: what it
claims to detect, the exact condition in code, false negatives (missed
cases), false positives (noise), and its fix-ladder rung. The reviews so far
found a bug every time (ISS-023, ISS-029, ISS-034), so this is expected to
find more. Done when every detector called from `main` has a test that
covers its positive case, a healthy control, and each documented miss.

### E.2 Failure classes we do not detect (ISS-040)

Measured by searching detector code for each failure's signal (a mention is
not proof of correct handling; E.1 checks that). 0 means no signal at all.

| Failure class | Signal found in detectors | Plan | Target rung |
| --- | --- | --- | --- |
| Pods stuck Terminating (finalizers, dead node) | 0 | Pods with `deletionTimestamp` older than grace plus 5 min | R3: force delete after approval |
| Objects or namespaces stuck on finalizers | 0 | `deletionTimestamp` with finalizers older than a window | R1: names the finalizer and its owner |
| Volume mount failures, pods stuck ContainerCreating | 0 | `FailedMount` / `FailedAttachVolume` events; ContainerCreating older than a window | R1 |
| Liveness / readiness probe failing (before CrashLoop) | 0 | `Unhealthy` events rate per pod | R1 (often a slow start: suggests probe changes) |
| Scheduling failures with the reason | 0 (Pending exists without the reason) | `FailedScheduling` event message parsed into the constraint | R1 |
| PodDisruptionBudget blocking evictions or drains | 0 | Eviction 429s counted; PDB with `disruptionsAllowed: 0` for long | R1 |
| Job hit `backoffLimit` | 0 (failed jobs exist, reason not read) | `BackoffLimitExceeded` condition reason in the message | R1 |
| Preemption of pods | 0 | `Preempted` events | R0 / R1 |
| CPU throttling | 0 (only API server throttling exists) | `container_cpu_cfs_throttled_periods_total` ratio from Prometheus | R3: propose a CPU limit raise |
| PVC almost full | 0 | `kubelet_volume_stats_used_bytes / capacity` from Prometheus | R3: propose expansion if the StorageClass allows it |
| Topology spread unsatisfiable | 0 | From `FailedScheduling` parsing | R1 |
| Readiness gates never satisfied | 0 | Pods with unmet `readinessGates` for long | R1 |
| etcd health | 0 | Control plane metrics when exposed (often not on managed clusters) | R0 |
| Deprecated API use | 0 | `apiserver_requested_deprecated_apis` metric | R1 |
| HPA at max replicas | 2 (check precision in E.1) | `currentReplicas == maxReplicas` with high utilisation for long | R3: propose raising max within a ceiling |
| Image pull secret problems, registry rate limits | 1 each (check in E.1) | Distinguish `unauthorized` and `toomanyrequests` in pull errors | R1 |
| Node not ready, disk pressure, kubelet, API server | present (check in E.1) | Audit only | as today |

**Summary as agreed with the owner (2026-10-07), the scope of phase 10:**

| Failure | Detected today? |
| --- | --- |
| Pods stuck Terminating; objects or namespaces stuck on finalizers | No |
| Volume mount failures (pods stuck in ContainerCreating) | No |
| Liveness / readiness probes failing before a crashloop | No |
| Why a pod cannot be scheduled (we see "Pending", not the reason) | No |
| PodDisruptionBudget blocking evictions or drains | No |
| Job hit its retry limit (we see "failed", not why) | No |
| Pod preemption | No |
| CPU throttling | No (only API server throttling) |
| Disk (PVC) almost full | No |
| Topology spread rules that cannot be met; readiness gates never met | No |
| etcd health; use of deprecated APIs | No |
| HPA stuck at max replicas; pull-secret and registry rate-limit errors | Partly; checked in phase 10 |
| Node not ready, node disk pressure, kubelet, API server | Yes; audited in phase 12 |

Work: E.2 is phase 10 (new detectors, test first); E.1 runs inside phase 12 (existing detectors).

## Phases and order

| Phase | Content | Why this order | Estimate (modelled, one engineer) |
| --- | --- | --- | --- |
| 8 | Testability, first half: D1 config struct (one `config.Load()`, no `os.Getenv` in components), D2 injected clock, D7 coverage floor at the current number | The floor stops regressions from day one; config and clock unblock most tests | 2 to 3 days |
| 9 | Testability, second half: D3 injected HTTP clients and no package globals or `init()` (ISS-015), D4 `run()` extracted from `main` with a boot smoke test | Outbound clients and `main` become testable | 2 to 3 days |
| 10 | **Missing failure classes** (Part E.2, ISS-040): one test-first detector per row of the E.2 table, each on its fix-ladder rung | The biggest blind spot: common failures we do not see at all | 5 to 8 days |
| 11 | **Architect review fixes** (added 2026-10-07): one cluster-wide dashboard and a complete UI, deployment manifests generated from the chart, safe scripts, entry point and shutdown, docs with one owner per topic (ISS-047 to ISS-058) | The dashboard and the install path are what every user touches first; ISS-047 and ISS-048 mislead or lose data today | 14 to 23 days |
| 12 | D5 tests for every existing detector with the E.1 audit (ISS-039); ISS-034, ISS-035 fixed on the way | Network and storage checks are the weakest existing code | 6 to 10 days |
| 13 | Part A, config reload (A1 to A3), with tests at 100 percent | The headline feature | 6 to 9 days |
| after 13 | **PLAN-003** (`docs/plans/PLAN-003-terminal.md`): read-only terminal with more commands, agent commands and a "can run / cannot run" panel | Owner decision 2026-10-08: after config reload, before the network work | 2 to 2.5 days |
| 14 | Part B network (ISS-033, ISS-036) | Builds on phase 12 tests | 4 to 6 days |
| 15 | Part C approval queue and ladder moves | Needs reload's rollout verification and the escalation wiring | 6 to 9 days |
| 16 | Part B remaining (escalation, learning, CRD fields, webhook certs, tracing, ISS-025, ISS-038), D6, D8 | Reaches the coverage target | 6 to 10 days |
| 17 | A4 (Argo Rollouts); `gitops.mode: live` and `images.mirror` behind their flags, off by default, with warnings (ISS-012); floor raised to the final target | Optional extras last | 4 to 6 days |

Total: about 56 to 87 engineer days (modelled), including phase 11. Each phase ends with
`make verify`, `make e2e`, a commit set, and the coverage floor raised to
the measured value.

## Detailed subtasks: phases 8 to 11

Added 2026-10-07. Estimates are modelled.

### Phase 8: testability, first half (2 to 3 days)

| # | Subtask | What changes | Done when |
| --- | --- | --- | --- |
| 8.1 | Config package | New `internal/config` with one `Config` struct and a `Load()` that reads every variable in one place, with defaults and validation | Unit tests for every default, valid and invalid value |
| 8.2 | Config test drives the docs | `TestConfigReference_MatchesCodeAndChart` takes variable names from `config` instead of scanning the code | The test still catches a missing or stale doc row (falsified once) |
| 8.3 | Move env reads into config | Components receive values from `Config`; no `os.Getenv` in `scaler.go`, `security.go`, `retention.go`, `storage.go`, `metrics/provider.go`, `logging`, `main.go` and the rest | A guard test fails if `os.Getenv` appears outside `internal/config` (`cost.go` allowed until 9.3) |
| 8.4 | Wire `agent.logLevel` | Maps to klog verbosity at startup (ISS-032) | Test shows the level changes verbosity |
| 8.5 | Injectable clock | A `now` function in dedup, circuit breaker, blast radius, quiet hours, learning mode, fix tracker | Time branches tested without sleeping (for example a quiet-hours window that crosses midnight) |
| 8.6 | Coverage floor | `.coverage-floor` set to the measured total; `make coverage-check` fails if total coverage drops; part of `make verify` and CI | Deleting a test makes `make verify` fail (falsified once) |
| 8.7 | Per-package floor for safety code | 100 percent required for `gate.go`, `redact`, `ratelimit`, the API auth middleware | A planted untested branch in `gate.go` fails the check |
| 8.8 | Records | ISSUES, STATUS, checkpoint, CONFIGURATION.md, GUIDE section 12 | `make verify` and `make e2e` pass; commit |

### Phase 9: testability, second half (2 to 3 days)

| # | Subtask | What changes | Done when |
| --- | --- | --- | --- |
| 9.1 | Injectable HTTP client: Slack, LLM, Alertmanager | Constructors take an `*http.Client` | `httptest` tests for success, error status, timeout |
| 9.2 | Injectable HTTP client: tickets, GitOps, escalation, Prometheus | Same for GitHub and Jira tickets, GitHub and GitLab PRs, PagerDuty, OpsGenie, email, metrics provider | Tests per client; `TestOutboundClientsRedact` stops swapping `http.DefaultTransport` |
| 9.3 | Remove `cost.go` `init()` | Cost settings move into the API server from `Config` | Cost tab tested for each source (default, manual, Kubecost, OpenCost) |
| 9.4 | Remove package globals | `handlerSem`, `httpapi.SetExtendedDeps`, `storage.GlobalSink` become constructor dependencies (ISS-015) | No mutable package-level state in `internal` (guard test) |
| 9.5 | Extract `run()` from `main` | `main` only loads config and calls `run(ctx, cfg, clients)` | `main.go` under 30 lines |
| 9.6 | Boot smoke test | Starts the whole agent with a fake clientset, waits for ready, checks `/readyz`, shuts down cleanly | Passes under `-race`; `cmd` above 80 percent |
| 9.7 | Leader and CRD tests | Leader election with a fake Lease (acquire, lose, re-acquire); CRD watcher with a fake dynamic client | `leader` and `crd` above 90 percent |
| 9.8 | Floor raised, records | `.coverage-floor` set to the new measured total | `make verify` and `make e2e` pass; commit |

Result, 2026-10-07 (all figures measured with `go test -race -cover`):

- 9.1, 9.2: every outbound client takes an `*http.Client` (`internal/httpx`),
  tested against `httptest` through `internal/httpx/httpxtest`. Package
  coverage: slack 88.9, llm 90.2, alertmanager 82.4, metrics 87.2,
  escalation 92.5, integrations 87.5. Writing these tests found ISS-042:
  escalation sent text to PagerDuty, OpsGenie and email unredacted.
- 9.3, 9.4: met. The globals guard allows only read-only values.
- 9.5: partly met. `main()` is 30 lines; `main.go` is 54 with imports.
- 9.6: partly met. `run()` is at 84.5 percent and the smoke test passes under
  `-race`, but the `cmd` package is at 77.8 because `main()` builds the
  in-cluster config and cannot run outside a pod.
- 9.7: `crd` 98.6. `leader` 88.9; the uncovered lines are the two
  `klog.Fatalf` branches, which exit the process.

### Phase 10: missing failure classes (5 to 8 days)

Each row is one detector, written test first: a bad state plus a healthy
control in a fake cluster, asserting the message, the metric and the rung.

| # | Failure | How we detect it | Rung | Done when |
| --- | --- | --- | --- | --- |
| 10.1 | Pods stuck Terminating | `deletionTimestamp` older than grace period plus 5 min | R3 force delete after approval (R1 until phase 15) | Stuck pod found; pod still within its grace period ignored |
| 10.2 | Objects or namespaces stuck on finalizers | Deleting, with finalizers, past a window | R1: names the finalizer and its owning controller | Namespace and PVC cases |
| 10.3 | Volume mount failures | `FailedMount` / `FailedAttachVolume` events; ContainerCreating past a window | R1 | Message carries the volume and the reason |
| 10.4 | Probes failing before a crashloop | `Unhealthy` event rate per pod | R1: suggests probe timing for slow starts | Liveness and readiness separated |
| 10.5 | Why a pod cannot be scheduled | `FailedScheduling` parsed into the cause: taint, affinity, resources, PVC, topology spread | R1 with the exact constraint | One test per cause |
| 10.6 | PodDisruptionBudget blocking evictions | Eviction refusals counted; PDB at 0 allowed disruptions for long | R1 | Covers our own node-pressure evictions being refused |
| 10.7 | Job hit its retry limit | `BackoffLimitExceeded` reason in the failed-job message | R1 | Reason appears in the alert |
| 10.8 | Pod preemption | `Preempted` events | R0 / R1 | Victim and preemptor named |
| 10.9 | CPU throttling | Throttled-period ratio from Prometheus | R3 propose a higher CPU limit (R1 until phase 15) | No alert and no error without Prometheus |
| 10.10 | PVC almost full | Used vs capacity bytes from Prometheus | R3 propose expansion when the StorageClass allows it | Expansion offered only when allowed |
| 10.11 | Topology spread unsatisfiable | From the 10.5 parser | R1 | Covered by 10.5 tests |
| 10.12 | Readiness gates never met | Unmet `readinessGates` past a window | R1 | Gate name in the message |
| 10.13 | etcd health | Control plane metrics when exposed; skipped on managed clusters | R0 | No noise when the metrics do not exist |
| 10.14 | Deprecated API use | `apiserver_requested_deprecated_apis` metric | R1: names the API and its replacement | One removed and one deprecated API |
| 10.15 | HPA stuck at max replicas | Audit the existing check: at max with high utilisation for long | R3 propose a higher max within a ceiling | Logic reviewed; misses fixed |
| 10.16 | Pull-secret and registry rate-limit errors | Audit the existing check: separate `unauthorized` and `toomanyrequests` | R1 | Two distinct messages |
| 10.17 | Wiring, RBAC, docs | Called from the right loop; new reads granted (for example PodDisruptionBudgets); feature table, guide, configuration reference | RBAC, config and every-detector-has-a-test checks pass; `make e2e` passes |

Result, 2026-10-08 (measured): all 17 rows done. Every new detector reports
through `report()` in `internal/kube/findings.go` with its rung and has a
test with a bad case and a healthy control; `TestEveryDetectorHasATest`
keeps it that way and lists the 20 older detectors that phase 12 must test.
Auditing the existing checks found ISS-043 (unschedulable pods never
reported under the DaemonSet), ISS-044 (image pull retried even on rate
limits), ISS-045 (job failure reason missing) and ISS-046 (false HPA
alerts), all fixed. 10.3 replaced `CheckVolumeAttachments`. Total coverage
44.6 to 51.5 percent. Commits 7990933, e8a227c and the phase 10 completion.

Rows 10.1, 10.9 and 10.10 reach their final rung when the approval queue
lands in phase 15; until then they stop one rung lower and say so in the
alert.

### Phase 11: architect review fixes (14 to 23 days)

Added 2026-10-07 from an architect review of the dashboard, `docs/wiki`,
`deployment/` (manifests and scripts) and `cmd/auto-agent`. Phase 10 was
finished first, on 2026-10-08, at the owner's request.

**Owner decision 2026-10-08 (ISS-058): option A.** Order: phase 10 done,
then 11.1 (ADR-001 first), then 11.2 to 11.7.

11.1 progress, 2026-10-08: ADR-001 written; event forwarding and ingest
(d915b93); `AGENT_ROLE` (4a5f9aa); standby proxy (8901104); chart with a
controller Deployment and node DaemonSet, e2e showing a node finding on
every controller (b7ef047); one ServiceAccount per role, checked against a
call graph from each role's entry points, and every raw manifest generated
from the chart with a drift check and `make e2e-raw` (part of 11.3 pulled
forward, because the split broke the hand-written raw files). ISS-059 is
fixed by the leader copying its event log to the standby. 11.1 is done.

11.2 done 2026-10-08: gate decisions become audit events and compliance is
computed from the event log (ISS-061); security headers (ISS-051); the UI
rewritten as separate HTML, CSS and JS with no inline code, five new views,
rungs, filters, refresh control and deep links (ISS-053, ISS-060);
`make ui-test` runs it in headless Chrome and is part of `make e2e`.

11.4 done 2026-10-08: deploy.sh and teardown.sh rewritten with flags, no
process killing, opt-in cost tools and ownership labels; demo apps in
labelled namespaces `test1`, `test2`, `chaos` (the raw allowlist); `make
e2e-raw` runs both scripts on kind; shellcheck in CI (ISS-049, ISS-052).
11.8 done 2026-10-08 (c3e88c4, 826d32f): watch scope and fix scope
(ADR-002, ISS-062); `/api/scope` and the Settings tab; header namespace
selector; ISS-064 found and fixed. 11.5 done (ed4ddab): ordered shutdown,
leader-only notices, `check-config`, kubeconfig fallback, API limits
(ISS-054, ISS-055, ISS-056). 11.6 done (bace559): one owner per topic,
generated changelog, docs check (ISS-057). 11.3 done: the chart defaults to
the release namespace, `image.digest`, the helm test fixed (ISS-050;
ISS-065 opened for the rest). 11.7: records, coverage floor raised to the
measured total. **Phase 11 is complete.** Next: phase 12.
Estimates are modelled.

**11.1 needs an owner decision first (ISS-058).** The dashboard is wrong
by design today (ISS-047): each DaemonSet pod has its own event log, and
the Service spreads requests across them.

| Option | How | For | Against |
| --- | --- | --- | --- |
| **A (recommended)** | One binary, two roles (`AGENT_ROLE=node` or `controller`). Node agents (DaemonSet) watch their own node's pods, take own-node actions and send findings to the controller over an authenticated internal endpoint. The controller (Deployment, two replicas, leader elected) runs the cluster loops, owns the event store, serves the dashboard, API and Slack actions; the Service selects controller pods only and the standby proxies to the leader | One view, one Slack voice, the cluster loops leave the node pods, API load no longer grows with node count, matches how ISS-043 was fixed | Two workloads to deploy; an internal endpoint to secure |
| B | Keep the DaemonSet; non-leader pods proxy `/api` to the leader found through the Lease | Small change | Node findings still live on each pod unless pushed; every node still runs the cluster informers |
| C | Shared store (CRs, ConfigMaps or a database) | Survives restarts | Write load on etcd or a new dependency; rejected |

| # | Subtask | What changes | Done when |
| --- | --- | --- | --- |
| 11.1 | One cluster-wide view (ISS-047, ISS-058) | ADR-001 records the owner's choice; for option A: `AGENT_ROLE`, controller Deployment and node DaemonSet in the chart, findings forwarded node to controller with a shared token, events and stats served by the controller only | e2e: a finding from a node agent and one from a leader loop both appear on every request, through the Service, from any pod |
| 11.2 | Dashboard complete (ISS-051, ISS-053) | Views for compliance, baselines, deploys, dry-run log and the audit log; rung column and filters (namespace, severity, reason, time); search; pause and refresh rate; deep links per tab; server-sent events or one batched status call instead of polling every endpoint; accessible markup (labels, keyboard, contrast); HTML, CSS and JS split into embedded files; Content-Security-Policy, nosniff and frame-ancestors headers | A headless browser test in `make e2e` opens every tab with a token, finds no console error and no unescaped field; header test in `httpapi` |
| 11.3 | One deployment of record (ISS-048, ISS-050, ISS-051) | Every file in `deployment/` generated by `make manifests` from the chart; CI fails if regenerating changes anything; the Secret is created once by the script and never applied over; Service is ClusterIP; one namespace name everywhere (`auto-agent`, including `helm-install`); image by value with a digest; raw defaults equal chart defaults; `COST_PROVIDER` removed or implemented | `make manifests` then `git diff --exit-code` passes in CI; re-running the install keeps a patched Secret (e2e) |
| 11.4 | Scripts that only touch what they own (ISS-049, ISS-052) | `deploy.sh` and `teardown.sh` rewritten: flags instead of prompts (`--namespace`, `--allowlist`, `--image`, `--with-opencost`), no `kill` of local processes, no orphan port-forward, third-party installs only behind a flag, ownership labels so teardown removes only what deploy created, CRD removal only with `--delete-policies`, failures stop the script; test apps in their own labelled namespaces, never `default`; `shellcheck` on every script in `make verify` and CI | shellcheck clean; e2e runs deploy, re-deploy and teardown on kind and finds no residue and nothing foreign removed |
| 11.5 | Entry point and shutdown (ISS-054, ISS-055, ISS-056) | Shutdown order: stop intake, wait for handlers and loops (bounded), then close recorder and audit log; one start and stop notice per rollout (leader or controller only); `flag.Set` error handled; QPS and burst in config; kubeconfig fallback for local runs; `auto-agent version` and `auto-agent check-config` (effective config, redacted, unknown keys named); `run()` split into functions under 50 lines | Test: an action in flight at cancel is in the audit log; test: unknown key reported; `cmd` at 85 percent |
| 11.6 | Docs with one owner per topic (ISS-057) | Each wiki page either owns its topic or is a short page linking to the owner (`GUIDE.md`, `CONFIGURATION.md`); changelog rewritten from git history with computed counts; a docs check (extending the configuration reference test) fails when a page names a setting, API route or Helm value that does not exist; dashboard page rewritten with the new views | The docs check is falsified once; `make check-writing` passes |
| 11.8 | Watch scope and fix scope (ADR-002, owner decision 2026-10-08, option 3) | Watch every namespace except the system ones; fix only inside a Helm ceiling (`agent.fixCeiling`, or `rbac.fixAnywhere: true` for a write ClusterRole); the initial fix list from Helm, then changeable from a new **Settings** tab that explains what, how and why, warns in fix-anywhere mode, confirms each change and records it as an audit event; a top-bar namespace dropdown filters every tab; the gate refuses actions outside the fix scope; constraint 4 reworded | Tests: reads outside the watch scope refused; the UI cannot enable a namespace outside the ceiling; a Helm upgrade keeps the UI choice; the browser test switches namespaces and edits the fix scope; `make e2e` |
| 11.7 | Records | ADR-001, feature table, GUIDE, CONFIGURATION, STATUS, checkpoint, project map | `make verify` and `make e2e` pass; commit set |

## Owner decisions (2026-10-07)

| Question | Decision | What it means for the work |
| --- | --- | --- |
| Escalation chain | **Wire it** | Phase 16: PagerDuty, OpsGenie and email fire for critical incidents and failed fixes, after Slack, deduplicated and redacted |
| `gitops.mode`, `images.mirror` | **Keep, off by default, under a warning** | Phase 17: implement both behind their flags. Until then, values and docs mark them "not implemented"; once built, each logs a warning at startup when enabled and its docs carry a warning box |
| Secret watching for reload | **Off by default; document its usage clearly** | Task A3.2b: a dedicated guide section on enabling, scoping, testing and disabling it, and what access it grants |
| Approvals | **30 minutes; a named group only** | Task C1.1: approvals expire after 30 minutes (configurable); `approvals.groups` lists who may approve; with no group configured, approvals are disabled and R3 fixes stay suggestions; every approval records who clicked |
