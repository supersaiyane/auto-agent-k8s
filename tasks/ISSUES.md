# Issues

Source: read-only review on 2026-10-07 (subagent, recorded in
`docs/checkpoint.md`). File and line references come from that review and
are confirmed as the first step of each fix. Status is OPEN unless stated.

| ID | Severity | Finding | Evidence | Plan task |
| --- | --- | --- | --- | --- |
| ISS-001 | Critical (FIXED 2026-10-07, branch phase0-1-safety) | Scaler updates Deployments in every mode, no mode or dry-run check | `internal/kube/scaler.go:23-139` | P1.2 |
| ISS-002 | Critical (FIXED 2026-10-07, branch phase0-1-safety) | `CleanupEvictedPods` deletes in every mode, no guardrails | `internal/kube/workloads.go:169-185` | P1.2 |
| ISS-003 | Critical (FIXED 2026-10-07, branch phase0-1-safety) | Init-container delete, node cordon/evict/uncordon, rollback and job cleanup check mode but skip guardrails | `handlers_extended.go:35-39`, `nodes.go:33-120`, `workloads.go:79`, `jobs.go:101-142` | P1.2 |
| ISS-004 | Critical | Node informer is unfiltered and not leader gated, so every node's agent can cordon and evict the same node; eviction ignores the allowlist | `internal/kube/watcher.go:72-87`, `nodes.go:89-91` | P2.1 |
| ISS-005 | Critical | Dashboard `/api/*` including `/api/kubectl` has no authentication and reads logs cluster wide | `internal/httpapi/http.go:64-79`, `terminal.go:61` | P3.1 |
| ISS-006 | High | Slack signing secret stored but never verified; `SetCallbacks` never called so buttons do nothing | `internal/slack/slack_actions.go:26-131` | P3.2 |
| ISS-007 | High | Data race: `deps.Policy` reassigned on hot reload while informers read it | `cmd/auto-agent/main.go:268`, `watcher.go:99` | P2.2 |
| ISS-008 | High (FIXED 2026-10-07, branch phase0-1-safety) | Default `AUTO_MODE` is `fix` | `internal/policy/policy.go:56`, `charts/*/values.yaml:10`, `deployment/03-config.yaml:8` | P1.3 |
| ISS-009 | High | RBAC: cluster-wide lease write; ClusterRole always rendered; chart and manifest drift; roles missing APIs the code uses, errors swallowed | `clusterrole.yaml:1,36-38`, `namespace-rbac.yaml:1`, `deployment/02-rbac.yaml:13-15` | P4.1 |
| ISS-010 | High | Image runs as root on unpinned end-of-life bases; minimal securityContext; writable hostPath | `Dockerfile:2,9`, `daemonset.yaml:66-67`, `values.yaml:7,102` | P4.2 |
| ISS-011 | High | Raw pod logs and events sent to external LLM without redaction | `internal/kube/llm.go:59`, `handlers.go:39` | P5.1 |
| ISS-012 | Medium | Docs claim features the code does not deliver: escalation chain unused, learning thresholds unused, `images.mirror` and `gitops.mode` unread | `docs/wiki/23-feature-status.md`, `learning.go:124` | P7.1 |
| ISS-013 | Medium | Full-object `Update` without conflict retry; dry-run log only created at startup | `scaler.go:99`, `nodes.go:37,78`, `workloads.go:159`, `main.go:197` | P2.3 |
| ISS-014 | Medium | Swallowed errors (count to be recomputed by `grep`), ignored `Sscanf` error | `main.go:189`, `workloads.go:183`, `jobs.go:142` | P7.2 |
| ISS-015 | Medium | Hardcoded limits, lease namespace and log path; package level globals; dropped events logged only at V(2) | `main.go:122,170`, `leader.go:35`, `watcher.go:30` | P7.2 |
| ISS-016 | High | Go 1.22 and k8s.io 0.30 are end of life. Confirmed by govulncheck v1.1.4 on 2026-10-07 (measured): 18 vulnerabilities reachable from our code, 14 in the standard library of the local go1.26.1 (fixed in go1.26.6), 2 in `golang.org/x/net` v0.23.0, 1 in `golang.org/x/text` v0.14.0, 1 in `aws-sdk-go-v2/service/s3` v1.56.0; plus 5 in imported packages and 19 in required modules not called | `go.mod`, `make vuln` | P6.2 |
| ISS-017 | Medium | CI has no govulncheck, image scan, SBOM, signing or semver release; lint unpinned; `make lint` cannot fail | `.github/workflows/ci.yaml`, `Makefile:21` | P0.2 done 2026-10-07 (lint pinned, `make lint` can fail); rest P6.1 |
| ISS-018 | Low | No tracing; high cardinality metric labels; `/healthz` always ok; version is a constant | `main.go:36` | P7.3 |
| ISS-019 | Low | No kind e2e (FIXED 2026-10-07: `make e2e`), thin README, placeholder module path | `README.md`, `go.mod` | P7.3 |
| ISS-020 | Low | Git history holds a real Jira tenant URL and project key (no live tokens found) | commit `9da1d52` | OPEN question for owner |
| ISS-021 | Medium | Core package `internal/kube` at 10.0 percent coverage (measured 2026-10-07; 23.6 percent after phase 1, measured the same day); 13 packages have no tests | `go test ./... -cover` | P1.1, every phase |
| ISS-022 | Medium | Dry-run consumes the real blast-radius budget: `SimulateAction` calls `BlastRadius.AllowAction`, so a switch to fix within the hour starts with budget already spent | `internal/kube/dryrun.go:79` | P7.2 |
| ISS-023 | High (FIXED 2026-10-07, branch phase0-1-safety) | Rollback target depended on ReplicaSet list order: when a higher revision appeared the previous one was dropped, so rollback could silently do nothing | `internal/kube/workloads.go` `rollbackDeployment`; test `TestRollbackDeployment_PreviousRevisionIndependentOfListOrder` | P1.2 |
| ISS-024 | Low | Dashes in existing files: 225 lines (51 in Go) measured 2026-10-07. `make check-writing` is a ratchet over changed files until `ALL=1 make check-writing` is clean | repository wide | P7.2 |
| ISS-025 | OPEN question | Housekeeping (evicted pod cleanup, old failed job cleanup) now counts against blast radius, circuit breaker and rate limiter like any fix, one action per namespace or CronJob per run. Safe, but in a cluster with many namespaces it can spend the blast-radius budget real fixes need. Keep, or give housekeeping its own budget? | `internal/kube/workloads.go` `CleanupEvictedPods`, `jobs.go` `cleanupOldFailedJobs` | owner |
