# Project map: auto-agent-k8s

Autonomous Kubernetes remediation agent (Go, client-go), deployed as a DaemonSet.

| Path | Responsibility |
|------|----------------|
| cmd/auto-agent/main.go | Entry point; wires kube.Deps (~:209), leader-only polling loops (~:250-308), ConfigMap hot-reload |
| internal/kube/ | Detection + remediation: gate.go (applyMutation, the only path that writes to the cluster; enforced by mutation_guard_test.go), watcher.go (pod/node informers), handlers.go (tryFixAction, wrapper over the gate), guardrails.go, scaler.go, nodes.go, workloads.go, jobs.go, llm.go |
| internal/policy/ | Mode (observe/suggest/dry-run/fix), allowlist, thresholds |
| internal/ratelimit/ | Action rate limiter |
| internal/leader/ | Lease-based leader election (kube-system) |
| internal/httpapi/ | Dashboard UI + /api/* (incl. kubectl terminal), /metrics, /healthz |
| internal/slack/, alertmanager/, integrations/, escalation/ | Notifications, alert ingest, Jira/GitHub issues & PRs, escalation chain |
| internal/crd/, webhook/ | AutoRemediationPolicy CRD, admission webhook |
| internal/storage/, logging/, obs/, metrics/, events/ | Log sinks (S3/EFS), logging, observability |
| charts/ | Helm chart (DaemonSet, RBAC, values) |
| deployment/ | Raw manifests (RBAC, config) |
| dashboards/ | Grafana dashboards |
| docs/wiki/ | Wiki docs (23-feature-status.md) |
| scripts/ | check-writing.sh (dash ratchet), e2e-kind.sh (kind dry-run e2e) |
| Makefile | build, test, tools (pinned), lint, vuln, check-writing, helm-lint, verify, verify-full, e2e |
| .github/workflows/ci.yaml | CI: build, test, lint, helm lint, docker push |
