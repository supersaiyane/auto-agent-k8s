# Checkpoint

| Date/Time | Task | Status | What Changed | Resume Vector |
|-----------|------|--------|-------------|---------------|
| 2026-10-07 | Pull latest + project improvement review | Done | Fast-forwarded master to origin/master (README). No code changes. Review findings delivered in chat; follow-up proposed: gate all mutations behind mode+guardrails. | internal/kube/handlers.go:221 (tryFixAction), internal/kube/guardrails.go:9, internal/kube/scaler.go:23-139, internal/kube/workloads.go:79,169-185, internal/kube/nodes.go:33-120, internal/kube/watcher.go:72-87, cmd/auto-agent/main.go:268 |
| 2026-10-07 | Plan + project rules adapted from meter | Done | Added CLAUDE.md, docs/plans/PLAN-001-safety-hardening.md, tasks/ISSUES.md (ISS-001 to ISS-021), tasks/STATUS.md, tasks/lessons.md. No code changed. | docs/plans/PLAN-001-safety-hardening.md (Phase 0, P0.1), Makefile:21, tasks/ISSUES.md |
| 2026-10-07 | PLAN-001 phases 0 and 1 | Done, not committed (lint, e2e and falsified e2e passed 2026-10-07) | Added internal/kube/gate.go (applyMutation), gate_test.go, mutation_guard_test.go, scripts/check-writing.sh, scripts/e2e-kind.sh, Makefile targets. Gated scaler, evicted cleanup, job cleanup, rollback, init container, node cordon/evict/uncordon. Default mode dry-run. Fixed ISS-001, 002, 003, 008, 023. | internal/kube/gate.go:42, internal/kube/gate_test.go, internal/kube/mutation_guard_test.go, Makefile (tools, verify, e2e), scripts/e2e-kind.sh |
| 2026-10-07 | PLAN-001 phase 2 | Done | Own-node node actions and allowlisted eviction (ISS-004); policy read through Deps.Policy() snapshot (ISS-007); reload accepts dry-run (ISS-026); merge patches and retried rollback, guard rejects unretried Update (ISS-013, ISS-027). | internal/kube/deps.go (Policies, NodeName), internal/kube/nodes.go:17, internal/policy/reload.go, internal/kube/mutation_guard_test.go |
| 2026-10-07 | PLAN-001 phase 3 | Done | Token auth on /api/ (route table + authorize middleware), kubectl allowlist, Slack signature, chart Secret keys and NetworkPolicy, UI token prompt, e2e curl checks. Fixed ISS-005, ISS-006; logged ISS-028. | internal/httpapi/http.go (authorize, apiRouteTable), internal/httpapi/terminal.go (checkKubectlScope), internal/httpapi/slack_actions.go (verifySlackSignature), charts/auto-agent/templates/networkpolicy.yaml |

## Next action

1. PLAN-001 phase 4: P4.1 RBAC inventory from source (client calls, informers, leases, CRD, metrics.k8s.io) matched to charts/auto-agent/templates/clusterrole.yaml and namespace-rbac.yaml; namespaced lease Role with resourceNames; forbidden errors counted. P4.2 image and securityContext hardening (Dockerfile, daemonset.yaml).
2. Owner decisions still open: ISS-025, ISS-020, P7.1.
