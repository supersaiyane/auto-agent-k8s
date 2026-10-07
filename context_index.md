# Context index

Exported symbols in `internal/kube` that other code depends on. Update on any
new or removed exported symbol (CLAUDE.md routing table). Started 2026-10-07.

| Symbol | File | Purpose |
| --- | --- | --- |
| `Deps` | `internal/kube/deps.go` | Shared dependencies for every handler and loop |
| `Deps.Policies` | `internal/kube/deps.go` | Source of the current policy snapshot (hot reloader in production) |
| `Deps.Policy()` | `internal/kube/deps.go` | Current immutable policy snapshot; replaces the removed `Deps.Policy` field (ISS-007) |
| `Deps.NodeName` | `internal/kube/deps.go` | Node this agent runs on; node actions only for this node (ISS-004) |
| `Deps.ScalingGates`, `Deps.TLSCertCheck`, `Deps.Endpoints`, `SelfCheckEndpoints` | `internal/kube/deps.go` | Settings that used to be read from the environment inside kube (PLAN-002 8.3) |
| `StartLogRetention(ctx, store, days)` | `internal/kube/retention.go` | Hourly log bundle cleanup; takes config instead of reading env |
| `PolicySource` | `internal/kube/deps.go` | Interface with `Get() *policy.Policy` |
| `EvaluateAndScale`, `CleanupEvictedPods`, `CheckFailedJobs`, `CheckStuckRollouts` | `internal/kube/scaler.go`, `workloads.go`, `jobs.go` | Leader-only loops called from `cmd/auto-agent/run.go` |
| `StartWatchers` | `internal/kube/watcher.go` | Pod and node informers; bounds concurrent handlers with `Deps` slots (no package semaphore, PLAN-002 9.4) |
| `Deps.HTTPClient` | `internal/kube/deps.go` | Injected client for runbook fetches, runbook HTTP steps and self-check probes; nil means a default client (PLAN-002 9.1) |
| `FetchRunbook(ctx, hc, url)` | `internal/kube/runbook.go` | Fetches a runbook with the injected client |
| `CheckPodStates` | `internal/kube/podstate.go` | Leader pass over every allowlisted pod: stuck terminating, volume failures, probes, unschedulable, preemption, readiness gates (PLAN-002 10.1 to 10.12); replaced `CheckVolumeAttachments` |
| `CheckStuckFinalizers`, `CheckDisruptionBudgets` | `internal/kube/lifecycle.go` | Leader checks: namespaces and claims held by finalizers (10.2), budgets blocking evictions (10.6) |
| `Rung`, `RungAlert` to `RungAuto` | `internal/kube/findings.go` | Fix ladder rung carried by every phase 10 finding; reported through the unexported `report()` |
| `Deps.Now` | `internal/kube/deps.go` | Injected clock for detector time windows; nil means time.Now |
| `NewDryRunLog`, `SimulateAction` | `internal/kube/dryrun.go` | Dry-run record of what the gate would have done |

Unexported but central: `applyMutation` (`internal/kube/gate.go`), the only
path that writes to the cluster, and `tryFixAction` (`handlers.go`), its
wrapper for remediations that need recovery verification.
