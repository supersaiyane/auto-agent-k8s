# Project map: auto-agent-k8s

Last checked against the code: 2026-10-08, after PR #4 merged and PLAN-002 phase 16 part 1 (PR #5 open): adds internal/kube approvals.go, approval_fixes.go, policycheck.go, internal/httpapi approvals.go, cmd/auto-agent approvals.go; the chart's helm test pod is admitted by the controller NetworkPolicy.

Autonomous Kubernetes remediation agent (Go, client-go), deployed as a DaemonSet.

| Path | Responsibility |
|------|----------------|
| cmd/auto-agent/main.go | Entry point: subcommands `version` and `check-config`, config, klog level, in-cluster or kubeconfig clients with `KUBE_API_QPS`, signal context, then `run()` |
| cmd/auto-agent/run.go | `run(ctx, conf, Clients, RunOptions)`: builds every dependency (the `agent` struct), leader election, HTTP server, watchers, leader-only check groups (`leaderChecks`), leader-only Slack notices, ordered shutdown (ISS-054); tests in `run_test.go`, `main_test.go` |
| internal/kube/ | Detection + remediation: gate.go (applyMutation, the only path that changes workloads; enforced by mutation_guard_test.go), scope_settings.go (SaveFixScope, the one write to the agent's own settings ConfigMap), namespaces.go (watch scope lister), netprobe.go (node network probes) + netchecks.go (leader network checks; PLAN-002 phase 14), approvals.go (R3 approval queue) + approval_fixes.go (the changes it can apply; PLAN-002 phase 15), policycheck.go (AutoRemediationPolicy refusals and the hourly PolicyBudget, used by the gate, dry-run and `agent gate`; ISS-037), gate.go also escalates critical findings and refused fixes (ISS-080), reload_refs.go + reload_policy.go + reload.go + reload_watch.go (config reload: references, Stakater annotations, waves through the gate, informers; PLAN-002 Part A), *_audit_test.go (phase 12 written audit and tests per detector), watcher.go (pod/node informers), handlers.go (tryFixAction, wrapper over the gate), guardrails.go, scaler.go, nodes.go, workloads.go, jobs.go, llm.go |
| internal/kube/podstate.go, scheduling.go, lifecycle.go, promchecks.go, findings.go | PLAN-002 phase 10 leader checks: `CheckPodStates` (stuck terminating, volume failures, probes, unschedulable with parsed scheduler causes, preemption, readiness gates), `CheckStuckFinalizers`, `CheckDisruptionBudgets`, `CheckResourcePressure` and `CheckControlPlane` (Prometheus); `detector_guard_test.go` requires a test per detector; `findings.go` holds the fix-ladder `Rung` and the one `report()` path |
| internal/config/ | The only place environment variables are read: `Load(getenv)` returns `Config` (81 variables, measured); `Names()` lists them for the docs test; `KlogVerbosity` maps LOG_LEVEL |
| internal/policy/ | Mode (observe/suggest/dry-run/fix), watch scope and fix scope (scope.go, ADR-002), thresholds; HotReloader (ConfigMap reload, immutable snapshots), Static source for tests |
| internal/ratelimit/ | Action rate limiter |
| internal/leader/ | Lease-based leader election (kube-system) |
| internal/httpapi/ | Dashboard UI + /api/* (incl. kubectl terminal), /metrics, /healthz; `approvals.go` serves /api/approvals and the Slack Approve and Reject buttons (phase 15), `ingest.go` takes node agents' events, `proxy.go` sends a standby controller's API and ingest requests to the leader (ADR-001); `scope.go` serves `/api/scope` for the Settings tab (ADR-002); `terminal*.go` is the read-only terminal, one command table (PLAN-003); `ui/` is index.html, app.css and app.js (no inline code; strict CSP in `securityHeaders`), tested in headless Chrome by `ui_browser_test.go` (build tag `browser`) and `scripts/ui-test.mjs` |
| internal/redact/ | Masks secrets and personal data; called inside every outbound client (llm, slack, alertmanager, tickets, PR text). outbound_test.go proves no client leaks |
| internal/slack/, alertmanager/, integrations/, escalation/ | Notifications, alert ingest, Jira/GitHub issues & PRs, escalation chain (redacted per channel, ISS-042); constructors take an `*http.Client` |
| internal/events/ | Event log (`Recorder`, controller), `Forwarder` (node agents to the controller ingest), `Tee` and replica forwarder (leader copies its log to the standby, ISS-059); all `Sink` (ADR-001) |
| internal/httpx/ | `Client(hc, timeout)`: the injected HTTP client rule; `httpxtest/` is the shared `httptest` server that records requests |
| internal/crd/, webhook/ | AutoRemediationPolicy CRD, admission webhook |
| internal/obs/ | Prometheus metrics; CountAPIError (forbidden reads become a metric and one warning) |
| internal/storage/, logging/, metrics/, events/ | Log sinks (S3/EFS), logging, observability |
| charts/ | Helm chart, the deployment of record (ADR-001): controller.yaml (Deployment, 2 replicas, AGENT_ROLE=controller), daemonset.yaml (node agents, AGENT_ROLE=node), one ServiceAccount and ClusterRole per role, roles.yaml (write Roles per role per allowlisted namespace, lease Role for the controller), Service and webhook select controllers, NetworkPolicy per role, generated INTERNAL_TOKEN; RBAC checked per role by `TestRBAC_EachRoleMatchesItsCode` |
| deployment/ | Raw manifests generated from the chart by `make manifests` (01 CRD, 02 RBAC per role, 03 ConfigMap, 04 node DaemonSet, controller Deployment, Service, NetworkPolicies; allowlist test1,test2,chaos); `make verify` fails on drift. `ensure-secret.sh` creates the Secret once; `deploy.sh` and `teardown.sh` (flags, `--help`) share `lib.sh`; `test-apps/` demo apps in labelled namespaces |
| dashboards/ | Grafana dashboards |
| docs/CONFIGURATION.md | Every setting: env var, Helm value, allowed values, default, effect, reading file; checked by internal/policy/configref_test.go |
| docs/plans/ | PLAN-001 (safety hardening, done, PR #1); PLAN-002 (config reload, weak features, fix ladder, coverage; owner decisions recorded 2026-10-07, phase 8 next); PLAN-003 (read-only terminal, after PLAN-002 phase 13) |
| docs/adr/ | ADR-001 (node and controller roles), ADR-002 (watch scope and fix scope, Settings tab; accepted 2026-10-08, not built yet: PLAN-002 11.8) |
| docs/GUIDE.md | Complete guide by reader level: concepts, local try-out, deploy, configuration, dashboard, API, operations, troubleshooting, architecture, security, developing |
| README.md | Entry point: how it works, guarantees with their tests, install, develop, full feature table (kept in step with docs/wiki/23-feature-status.md) |
| docs/wiki/ | Wiki docs (23-feature-status.md) |
| scripts/ | check-writing.sh (dash ratchet), coverage-check.sh (floor and strict set), e2e-kind.sh (Helm install on kind: dry-run, API, node findings on every controller, RBAC), e2e-raw.sh (deploy.sh and teardown.sh on kind), ui-test.mjs (dashboard in headless Chrome over the DevTools protocol) |
| Makefile | build, test, tools (pinned), lint, vuln, check-writing, helm-lint, verify, verify-full, e2e |
| .github/workflows/ci.yaml | CI: build, test, lint, helm lint, docker push |
