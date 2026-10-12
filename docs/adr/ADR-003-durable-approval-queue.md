# ADR-003: Store the approval queue in the agent's own ConfigMap

Status: accepted, 2026-10-12 (owner chose the ConfigMap option)
Plan: PLAN-004 phase 1, ISS-078

## Context

Approve to fix (PLAN-002 phase 15) holds an R3 change until a listed
approver presses Approve in Slack. The queue was a map in the leader's
memory. A restart, a crash, a `helm upgrade` or a change of leader emptied
it, and a later press answered "no such approval". The owner ruled this a
bug on 2026-10-11: if pending fixes are dropped, approve to fix is of no
use.

Two things made a plain "save the map" impossible:

1. A queued change was a `mutation` whose `Apply` is a Go closure, which
   cannot be stored.
2. Constraint 1 allows only one writer besides the gate,
   `writeAgentSetting`, and it wrote only the fix scope ConfigMap.

## Decision

**Store.** The queue is JSON under the key `queue.json` in one ConfigMap,
`auto-agent-approvals`, in the agent's namespace. A new `Approval` CRD was
the alternative: it reads better in `kubectl`, but it is one more API to
install and upgrade, and the queue is capped at 200 items, far below the
ConfigMap size limit (writes over 900 KiB are refused).

**A change is stored as data.** Each R3 change is a `fixSpec` (action,
namespace, object name and the one value it sets). `buildFix` turns a spec
into the mutation with its `Apply`. Every propose function goes through
`buildFix`, so a stored approval applies exactly what a fresh one would. A
stored spec that cannot be rebuilt is listed as unreadable and never applied.
Run now reads the CronJob when the approval is applied, as
`kubectl create job --from` does, so nothing large is stored.

**The store is the truth.** Every propose, approve and reject reads the
stored queue, applies one step and writes it back with a merge patch that
carries the read `resourceVersion`. The API server refuses the patch with a
conflict if the queue changed in between, and the step reruns on the fresh
copy (constraint 6). Whichever controller leads therefore acts on the stored
queue; no load step at the start of a leader term is needed, and an old
leader with a stale copy in memory cannot overwrite a newer one.

**Write ahead.** Approve stores the approved state before the change goes
through `applyMutation`. A crash between the two leaves an approved item with
no result, which is never applied again. If the approved state cannot be
stored, nothing is applied. Applying a change twice is worse than not
applying it.

**Failure is visible.** A change that cannot be stored is not queued, and
the finding says so; the next check pass tries again. A failed read or a
failed result write is logged and counted in
`auto_agent_handler_errors_total{handler="approvals",error_type="store"}`.

**Constraint 1.** `writeAgentSetting(name, write)` now takes the ConfigMap's
name and refuses anything but `auto-agent-scope` and `auto-agent-approvals`.
`TestMutationsOnlyThroughGate` accepts a `writeAgentSetting` call only when
its first argument is one of those two constants.

**RBAC (constraint 7).** A Role `auto-agent-approvals` grants `get` and
`patch` on that one ConfigMap by `resourceNames`; `create`, which cannot be
pinned, comes from the existing `auto-agent-scope` Role in the agent's
namespace. The controller's read ClusterRole gains `get` on CronJobs for run
now. Node agents never get a store: `run.go` sets it on controllers only, and
the RBAC scan follows `Approvals.StoreIn` as a controller entry point.

**Standby.** The Slack callback is under `/api/`, which a standby already
forwards to the leader unchanged (ADR-001), signature headers included.

## Consequences

- Pending approvals survive restarts, upgrades and leader changes; the kind
  e2e queues one, deletes the leader and approves on the new one.
- Every queue step costs one read and usually one write of a small
  ConfigMap. Approvals are rare, so this is not a load concern.
- The stored queue holds the change and the finding's one-line summary.
  It stays inside the cluster (constraint 8 covers data that leaves it), but
  anyone who can read ConfigMaps in the agent's namespace can read pending
  approvals.
- Deleting the ConfigMap drops every pending approval, as uninstalling
  would.
