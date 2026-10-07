# PLAN-001: Safety hardening

Created: 2026-10-07
Status: proposed, not started
Issues: `tasks/ISSUES.md` ISS-001 to ISS-021
Rules: `CLAUDE.md` (definition of done, architectural constraints)

## Goal

Make it impossible for the agent to change the cluster unless it is in `fix`
mode, inside the guardrails, acting as the single owner of the target, and
permitted by RBAC that matches the code. Then make the docs, CI and image tell
the truth about that.

## How each task is run

1. Confirm the evidence lines in the ISS row still point at the defect. If
   they moved, correct the ISS row first.
2. Write the failing test, run it, and watch it fail for the right reason.
3. Make the smallest change that passes it.
4. Run the definition of done from `CLAUDE.md`. Each check runs alone, its
   output read, never inferred from an exit code.
5. Update `tasks/ISSUES.md`, `tasks/STATUS.md`, `docs/checkpoint.md` and
   `project_map.md` in the same commit, which names the ISS it closes.

One task at a time, in the main session, in this order. A phase is done when
every task in it is done and its exit check passed.

## Phase 0: Guardrails for the work itself

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P0.1 | `make check-writing`: fails on em or en dashes in `*.go`, `*.md`, `*.yaml`, `*.tpl`. Match the UTF-8 bytes, not `grep -P`, which macOS grep rejects (seen 2026-10-07: it exited non-zero and looked like "no matches") | (rule) | Falsified once by adding a dash, then passes on a clean tree |
| P0.2 | `make lint` stops ending in `\|\| true`; pin golangci-lint; add `make vuln` (govulncheck); add `make verify` = build, vet, `test -race`, lint, vuln, check-writing, `helm lint` | ISS-017 (part) | `make verify` runs every step and a planted vet error makes it fail |
| P0.3 | `make kind-up` / `make e2e`: kind cluster, chart installed in dry-run, a crashlooping test pod | ISS-019 (part) | `kubectl get pods -A` shows the agent running and its log shows the detection |

Exit check: `make verify` output pasted into the checkpoint, baseline
coverage per package recorded as measured.

## Phase 1: One gate for every mutation (constraints 1, 2)

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P1.1 | Test harness: fake clientset with a reactor that records every create, update, patch, delete and eviction; table of policy modes and guardrail states | ISS-021 (part) | Harness test asserts zero mutations in observe, suggest and dry-run for one existing safe path |
| P1.2 | Route scaler, evicted-pod cleanup, init-container delete, node cordon/evict/uncordon, rollback and job cleanup through `tryFixAction` (`internal/kube/handlers.go:221`) | ISS-001, 002, 003 | Table test: every action makes zero mutations outside `fix` or when any guardrail blocks; removing any one gate makes a test fail |
| P1.3 | Default `AUTO_MODE` becomes `dry-run` in code, chart and manifest | ISS-008 | Test on policy defaults; `helm template` shows `dry-run` |
| P1.4 | Static guard: a test that fails if a mutating client call appears outside the gate file | (constraint 1) | Falsified by adding a direct `Delete` elsewhere |

Exit check: kind e2e in dry-run, crashlooping pod, `kubectl get events`
shows no deletion and the dry-run log shows the simulated action.

## Phase 2: One actor per target, no races (constraints 3, 5, 6)

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P2.1 | Node pressure handled only when `node.Name == NODE_NAME` (or leader only); eviction filtered by allowlist | ISS-004 | Test with two simulated agents shows one cordon; kind e2e with 2 worker nodes shows one actor in logs |
| P2.2 | Policy held in `atomic.Pointer[Policy]`, read via a getter everywhere | ISS-007 | `go test -race ./...` clean with a test that reloads policy while events flow |
| P2.3 | Replace full-object `Update` with `Patch` or scale subresource plus `retry.RetryOnConflict`; create dry-run log on mode switch, not only at startup | ISS-013 | Conflict test using a reactor that returns 409 once |

## Phase 3: Authenticated inbound surface (constraints 4, 9)

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P3.1 | Bearer token (from a Secret) or OIDC on `/api/*`; kubectl endpoint restricted to allowlisted namespaces and read verbs; NetworkPolicy in chart | ISS-005 | `curl` without token returns 401 against the kind stack, pasted; request for a non allowlisted namespace returns 403 |
| P3.2 | Verify `X-Slack-Signature` with timestamp window; wire `SetCallbacks`, or remove the buttons and say so in docs | ISS-006 | Test with a known signing secret: valid, tampered and replayed requests |

## Phase 4: RBAC and image hardening (constraints 7, 11)

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P4.1 | Inventory every API the code calls; chart role grants exactly that; leases as namespaced Role with `resourceNames`; ClusterRole conditional on `namespacedRBAC.enabled`; delete or regenerate `deployment/` from the chart; forbidden errors counted in a metric | ISS-009 | `kubectl auth can-i --list --as=system:serviceaccount:...` pasted and matched to the inventory; no forbidden errors in a kind run |
| P4.2 | Distroless nonroot image pinned by digest; supported Go builder; `runAsNonRoot`, `readOnlyRootFilesystem`, `drop: [ALL]`, seccomp `RuntimeDefault`; review hostPath and `system-node-critical` | ISS-010 | `docker inspect` shows non-root user; pod runs in kind with the new securityContext |

## Phase 5: Data leaving the cluster (constraint 8)

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P5.1 | Redaction step for tokens, keys, emails and IPs before LLM, Slack and issue tracker calls; config flag to disable LLM; documented in wiki | ISS-011 | Golden tests with synthetic secrets show them masked |

## Phase 6: CI and dependencies

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P6.1 | CI runs `make verify`, Trivy image scan, SBOM, cosign signing, semver tags; docker job needs helm lint; kind e2e job | ISS-017 | A pipeline run on a branch shows every job, link recorded |
| P6.2 | Bump Go to a supported release and k8s.io, x/net, x/oauth2; run govulncheck before and after | ISS-016 | govulncheck output before and after pasted |

## Phase 7: Truth and cleanup

| Task | Change | Closes | Done when |
| --- | --- | --- | --- |
| P7.1 | Feature status page cites code path and test per feature; escalation, learning thresholds, `images.mirror`, `gitops.mode` either wired or removed | ISS-012 | Every "working" row has a path and a test name that exist (checked by grep) |
| P7.2 | Remove `_ =` on errors that matter; config for blast radius, breaker, lease namespace, log path; replace package globals with injected deps; dropped events become a metric | ISS-014, 015 | `grep` count of discarded errors recomputed and recorded before and after |
| P7.3 | `/healthz` checks informer sync; version via ldflags; reduce metric label cardinality; real module path; README with architecture and quick start | ISS-018, 019 | `curl /healthz` fails when informers are not synced, pasted |

## Open questions for the owner

- ISS-020: rewrite git history to remove the Jira tenant URL, or accept it?
- P2.1: own node only, or leader only, for node actions?
- P3.1: token or OIDC for the dashboard?
- P7.1: wire escalation and learning, or remove them?

## Order and risk

Phases 0 to 2 are the safety core and should land before the agent runs in
`fix` mode on any real cluster. Phases 3 to 5 close exposure. Phases 6 and 7
make the result stay true. Effort is not estimated here; it is recorded per
task in `tasks/STATUS.md` as measured once each task finishes.
