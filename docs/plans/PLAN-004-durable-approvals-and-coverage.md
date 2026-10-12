# PLAN-004: durable approvals and coverage to 95 percent

Status: phases 1 to 5 done 2026-10-12; phase 6 part 1 done 2026-10-12 (B5, every package at 90, total 94.3). Part 2: the last 0.7 percent and the floor to 95.

## Goal

1. **ISS-078 is a bug, not a limitation** (owner, 2026-10-11). An approve to
   fix change that waits for a person must survive a restart and a change of
   leader. Today the queue lives only in the leader's memory: a restart or
   failover drops every pending approval, and the approver's press answers
   "no such approval". The automated fix is then useless exactly when it is
   needed.
2. **ISS-088**: total coverage from 83.0 to at least 95 percent, with tests
   that assert behaviour, not tests that only execute lines.

## How each task is run

Same loop as PLAN-002: one phase at a time in the main session, test first,
`make verify` after every phase, `make e2e` for any change to behaviour,
records in the same commit, merge on the owner's go.

## Part A: durable approvals (ISS-078)

### A.1 What goes wrong today

- `internal/kube/approvals.go`: `Approvals.items` is a map in memory.
- A leader restart, a crash, a `helm upgrade` or a lost lease empties it.
- The Slack message with Approve and Reject buttons stays in the channel;
  pressing it later returns `ErrApprovalUnknown`.
- The finding comes back after its dedup window and queues a new approval
  with a new id, so the approver sees stale buttons that fail and new ones
  that may never be noticed.

### A.2 Design (to be fixed in ADR-003)

- **Store**: one ConfigMap, `auto-agent-approvals`, in the release namespace,
  holding the queue as JSON. Recommended over a new CRD: no new API to
  install, the same pattern as `auto-agent-scope` (ADR-002), and the queue is
  capped at 200 items, far under the 1 MiB ConfigMap limit.
- **Write path**: through `writeAgentSetting`, the one entry point besides
  the gate that may write, which today writes only `auto-agent-scope`.
  ADR-003 widens it to these two named ConfigMaps and nothing else. Every
  write is audited. The mutation guard test is updated to allow exactly
  these two names.
- **Conflict safe**: `retry.RetryOnConflict` on the ConfigMap's
  resourceVersion (constraint 6).
- **Write ahead**: an approval is marked approved and persisted **before**
  `applyMutation` runs. A crash between the two leaves an approved item with
  no result, which is reported, never applied a second time. Applying the
  same change twice is worse than not applying it.
- **Load**: a controller that becomes leader loads the queue before it
  serves any approval; items past their expiry are marked expired at load.
- **Standby**: an approval callback that reaches the standby is proxied to
  the leader, as ADR-001 already does for the API. A test proves it.
- **Nothing secret stored**: the stored mutation holds the change and a
  summary that has already passed redaction; a test fails if a Secret value
  can reach the ConfigMap.
- **RBAC**: `get` and `patch` pinned by `resourceNames` to
  `auto-agent-approvals`; `create` (which cannot be pinned) in the release
  namespace only, as for the scope ConfigMap (constraint 7).

### A.3 Tasks

| ID | Task | Done when |
| --- | --- | --- |
| A1.1 | ADR-003: where approvals live, why a ConfigMap, the widened `writeAgentSetting` | ADR merged and linked from README |
| A1.2 | `mutation` and `pendingApproval` serialize to JSON and back without loss | Round-trip table test over every action type that can be proposed |
| A1.3 | Persist on propose, approve, reject and expire; write ahead before `applyMutation` | Tests: each state change is in the ConfigMap; a crash injected after the write and before the gate never applies twice |
| A1.4 | Load on becoming leader; expire on load; duplicate proposals still collapse onto the stored item | Test: leader A queues, A stops, B loads and approves, the change is applied once |
| A1.5 | Standby proxies approval callbacks to the leader | Test with two servers, one leader |
| A1.6 | Chart Role and generated manifests; `TestMutationsOnlyThroughGate` allows exactly the two ConfigMaps | `helm lint`, `make check-manifests`, guard falsified once with a third name |
| A1.7 | Mutation testing covers the persistence code | Added to `KUBE_TARGETS` in `scripts/mutation.sh`; no survivor |
| A1.8 | e2e on kind: queue an approval in fix mode, delete the leader pod, send a signed Approve to the new leader | `kubectl` shows the change applied once and the ConfigMap item approved; output pasted |
| A1.9 | Records: ISS-078 FIXED, feature table, GUIDE approvals section, CONFIGURATION, checkpoint | `make verify`, `make e2e` |

## Part B: coverage to 95 percent (ISS-088)

### B.1 Where the gap is (measured 2026-10-11)

`go test -coverprofile ./...`: 7690 statements, 6386 covered (83.0
percent). Reaching 95 percent needs 919 more covered statements. The files
with the most uncovered statements, measured in the same run:

| File | Uncovered | Covered |
| --- | --- | --- |
| `internal/kube/watcher.go` | 116 | 17.7% |
| `internal/kube/runbook.go` | 88 | 0.0% |
| `internal/httpapi/http.go` | 88 | 72.5% |
| `internal/kube/handlers.go` | 57 | 72.3% |
| `internal/httpapi/cost.go` | 51 | 73.3% |
| `internal/events/recorder.go` | 44 | 47.0% |
| `internal/integrations/gitops.go` | 42 | 83.1% |
| `internal/httpapi/terminal_read.go` | 41 | 75.4% |
| `internal/kube/handlers_extended.go` | 39 | 31.6% |
| `internal/kube/retention.go` | 37 | 0.0% |
| `internal/kube/learning.go` | 35 | 67.0% |
| `internal/kube/auditlog.go` | 35 | 64.3% |
| `internal/httpapi/terminal_get.go` | 35 | 87.9% |
| `internal/kube/pod_extended.go` | 30 | 43.4% |
| `cmd/auto-agent/run.go` | 28 | 90.2% |

These 15 files hold 766 of the 1304 uncovered statements.

### B.2 Rules for this part

- A test asserts an outcome (a message, a metric, an API call, a refusal).
  A test that only calls a function to colour lines is rejected in review.
- Code with no caller is deleted, not tested (constraint 10). `runbook.go`
  and `retention.go` at 0 percent are checked first: wired and untested, or
  dead.
- Test helpers that ship no code (`internal/httpx/httpxtest`, 24
  statements) are excluded from the total by name in
  `scripts/coverage-check.sh`, and the exclusion is listed in the plan and
  the guide, never hidden.
- The floor in `.coverage-floor` rises at the end of every phase to the
  measured total; it never falls.

### B.3 Tasks

| ID | Task | Done when |
| --- | --- | --- |
| B1 | Dead or wired: `runbook.go`, `retention.go`, `httpxtest` exclusion | Each file either has a caller and a test, or is removed with its config and docs |
| B2 | Informer entry points: `watcher.go`, `handlers.go`, `handlers_extended.go`, `pod_extended.go`, driven through fake informers | Each event type reaches its handler and the right finding; every file at or above 90 |
| B3 | Records and state: `events/recorder.go`, `auditlog.go`, `learning.go`, `fixtracker.go`, `correlation.go` | Each at or above 90; write failures counted |
| B4 | HTTP API: `http.go`, `cost.go`, `terminal_read.go`, `terminal_get.go`, `terminal_describe.go`, `slack_actions.go`, `resources.go` | `internal/httpapi` at or above 95 |
| B5 | The rest: `integrations/gitops.go`, `cmd/auto-agent`, `policy/reload.go`, `escalation`, `netchecks.go`, `netprobe.go`, `scaler.go`, `workloads.go` | Every package at or above 90 |
| B6 | Floor to 95, a per-package floor of 90 in `make coverage-check`, records | `make verify` passes with the floor at the measured total, at least 95.0 |

## Phases and order

| Phase | Content | Why this order |
| --- | --- | --- |
| 1 | Part A (A1.1 to A1.9) | A bug in a shipped feature comes before coverage |
| 2 | B1 | Removing dead code changes the denominator, so it goes first |
| 3 | B2 | The largest gap and the path every finding takes |
| 4 | B3 | |
| 5 | B4 | |
| 6 | B5, B6 | Floor raised to the final number |

## Owner decisions

- A.2: ConfigMap, decided 2026-10-12 (ADR-003).

## Done 2026-10-12: phase 1

All of A1.1 to A1.9. Two changes from the design above, both forced by the
code: a queued change was a closure, so each R3 change is now stored as a
`fixSpec` and rebuilt by `buildFix`; and every queue step reads and writes
the stored copy with a resourceVersion precondition, so no separate load at
the start of a leader term is needed. Run now reads the CronJob at approval
time. Kind e2e: approval `8ade4f33759a5027` queued on one leader, the leader
deleted, approved by a signed Slack press on the new leader, one Job created,
the replay refused. ISS-091 (a doubled refusal message) found on the way.

## Done 2026-10-12: phase 2 (B1)

- `runbook.go` (88 statements, 0 percent) had no caller: deleted, and three
  wiki pages that described runbook automation corrected (ISS-092). The
  CRD field `runbookURL` stays: it is a link in anomaly alerts.
- `retention.go` (37 statements, 0 percent) is called by node agents: it now
  cleans once at start and counts every read and remove failure (ISS-093),
  with tests.
- `internal/httpx/httpxtest` is excluded from the total by name in
  `scripts/coverage-check.sh`, as the guide now says.
- ISS-091 (doubled refusal text) fixed.
- Measured with `make test` and `make coverage-check` on 2026-10-12: 84.8
  percent over every package, 85.0 percent counted. Floor 83.4 to 85.0.

## Done 2026-10-12: phase 3 (B2)

- One table test drives `handlePodUpdate` through every pod state it routes
  (init container, crash loop, image pull, OOM, config error, restart storm,
  four extra waiting reasons, NotReady and Pending on both sides of their
  windows, unwatched namespace, healthy), each sent twice to prove dedup.
  NotReady deletes the pod in fix mode; Alertmanager receives incidents.
- `runHandler`: a full pool drops the event, a free slot runs it with a
  deadline and releases the slot. `StartWatchers`: a pod update and a node
  pressure change reach their handlers through real informers on the fake
  clientset.
- The router reads the clock from `deps` (D2) for its NotReady and Pending
  windows, named `notReadyAfter` and `pendingAfter`.
- `checkBreaker` and `podAge` had no caller: deleted (ISS-094).
- Falsified once: inverting the Pending window and skipping the ConfigError
  dedup failed three cases.
- Measured 2026-10-12: watcher.go 98.6, handlers.go 92.2,
  handlers_extended.go 93.9, pod_extended.go 94.2 percent; total 87.8
  percent counted (87.6 over every package). Floor 85.0 to 87.8.

## Done 2026-10-12: phase 4 (B3)

- Event history, audit log and learned baselines: tested across a restart
  (history and ids continue, the audit log appends, baselines load back),
  with the ring buffer bound and unreadable lines counted.
- Every failure to open, read or write them is now a warning and counted
  (ISS-097). The baselines were saved after creating a fixed directory
  instead of the configured path's; fixed.
- `AllBaselines` and `GetBaseline` return copies: `/api/baselines` encoded
  live pointers outside the lock (ISS-096).
- Unused, deleted with the claims that described them (ISS-095):
  `CorrelateIncident`, `CostEstimate`, `GetThreshold`, `RecordRestarts`,
  `FixTracker.Stats`, and three baseline fields nothing wrote.
- Fix verification falls back to pods when there is no Deployment, and a
  refused read is counted; tested.
- Falsified once: the fixed save directory and the live pointers each fail
  their test.
- Measured 2026-10-12: recorder.go 90.6, auditlog.go 92.2, learning.go 90.4,
  fixtracker.go 96.6, correlation.go 96.4 percent; total 89.4 counted (89.1
  over every package). Floor 87.8 to 89.4.

## Done 2026-10-12: phase 5 (B4)

- Dashboard API views tested by what they return: events (limit, type),
  stats, nodes (roles, pressure, status, memory), cluster events (scope,
  refused namespace), cluster and namespace overviews, resources (every
  efficiency class), cost (computed, Kubecost, OpenCost, fallback), the
  extended views with real trackers, every Slack refusal, and every view
  with no cluster client.
- Terminal: every `get` kind with `-A -o wide`, a label and a field
  selector; every command with a failing API; rollout status and history
  at every stage; logs and events targets; describe when events cannot be
  read.
- Fixed (ISS-098): `writeJSON` counts a failed encode; an unanswered cost
  source is counted and the fallback is labelled `computed`, not with the
  source's name.
- Removed (ISS-100): the unwired Rollback and Silence buttons, their
  callbacks and helper; the wiki now describes Approve and Reject only and
  names `slack.channelWebhooks` correctly.
- Left for the owner (ISS-099): node pod counts and cost utilisation read
  pods in every namespace.
- Falsified once: the fallback label and the encode counter each fail
  their test. Redaction of nested Block Kit lists keeps its direct test.
- Measured 2026-10-12: `internal/httpapi` 82.7 to 95.3 percent; total
  92.3 counted (92.0 over every package). Floor 89.4 to 92.3.

## Done 2026-10-12: phase 6 part 1 (B5)

- Every package at or above 90 percent (measured): alertmanager, escalation,
  slack, metrics and cmd/auto-agent were below it.
- Failure sweeps: every GitOps and ticket call failing at each step, with
  unparseable answers and unbuildable requests; every SMTP refusal; the
  policy reload's numeric keys, validation and scope informer; the scaler's
  and the network checks' read failures.
- `main` is now `realMain`, which returns an exit code, so deferred work
  runs and every exit is tested.
- Bugs found and fixed: ISS-101 (webhook never shut down), ISS-102 (GitOps
  commits on a guess), ISS-103 (scaler fought an HPA it could not see),
  ISS-104 (duplicate tickets on a failed search), ISS-105 (OOM bump read
  "512M" as 0 and proposed a 0Mi limit).
- Measured 2026-10-12 with `make verify`: total 94.3 percent counted (94.0
  over every package). Floor 92.3 to 94.3.
