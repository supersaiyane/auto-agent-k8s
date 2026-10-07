# Context index

Exported symbols in `internal/kube` that other code depends on. Update on any
new or removed exported symbol (CLAUDE.md routing table). Started 2026-10-07.

| Symbol | File | Purpose |
| --- | --- | --- |
| `Deps` | `internal/kube/deps.go` | Shared dependencies for every handler and loop |
| `Deps.Policies` | `internal/kube/deps.go` | Source of the current policy snapshot (hot reloader in production) |
| `Deps.Policy()` | `internal/kube/deps.go` | Current immutable policy snapshot; replaces the removed `Deps.Policy` field (ISS-007) |
| `Deps.NodeName` | `internal/kube/deps.go` | Node this agent runs on; node actions only for this node (ISS-004) |
| `PolicySource` | `internal/kube/deps.go` | Interface with `Get() *policy.Policy` |
| `EvaluateAndScale`, `CleanupEvictedPods`, `CheckFailedJobs`, `CheckStuckRollouts` | `internal/kube/scaler.go`, `workloads.go`, `jobs.go` | Leader-only loops called from `cmd/auto-agent/main.go` |
| `StartWatchers` | `internal/kube/watcher.go` | Pod and node informers |
| `NewDryRunLog`, `SimulateAction` | `internal/kube/dryrun.go` | Dry-run record of what the gate would have done |

Unexported but central: `applyMutation` (`internal/kube/gate.go`), the only
path that writes to the cluster, and `tryFixAction` (`handlers.go`), its
wrapper for remediations that need recovery verification.
