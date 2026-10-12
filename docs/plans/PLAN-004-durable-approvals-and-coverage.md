# PLAN-004: durable approvals and coverage to 95 percent

Status: phases 1 and 2 done 2026-10-12 (ISS-078 fixed, ADR-003; B1). Phase 3 (B2) next.

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
