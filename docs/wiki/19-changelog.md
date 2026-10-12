# Changelog

Generated from git history on 2026-10-12 by `make changelog`. Counts are
measured with `git rev-list --count --no-merges`; nothing here is written by
hand. Releases are the git tags `v*`.

## v1.0.0 (2026-10-12)

Everything after PR #2: watch scope and fix scope (ADR-002), the Settings tab, the read-only terminal (PLAN-003), network checks, config reload, escalation, live GitOps, the image mirror, durable approvals (ADR-003), 95 percent coverage with mutation testing (PLAN-004), and every issue closed.

54 commits, 2026-10-08 to 2026-10-12. By type: 20 docs, 15 feat, 11 fix, 8 test.

- `d68b9a7` docs: PLAN-003 read-only terminal, after PLAN-002 phase 13
- `c3e88c4` feat: separate watch scope and fix scope (PLAN-002 11.8 step 1)
- `826d32f` feat: Settings tab and fix scope from the dashboard (PLAN-002 11.8)
- `ed4ddab` fix: ordered shutdown, leader-only notices, check-config (PLAN-002 11.5)
- `bace559` docs: one owner per topic, generated changelog, docs check (PLAN-002 11.6)
- `6aafe5b` fix: one namespace and a pinnable image for the chart (PLAN-002 11.3)
- `de32f7c` docs: phase 11 complete, coverage floor 56.4 (PLAN-002 11.7)
- `985cf61` test: audit and test every detector and leader loop (PLAN-002 phase 12)
- `fd94273` docs: checkpoint after PR #3 merged
- `ff6f96b` docs: project map checked after PR #3
- `762c7bd` feat: native config reload (PLAN-002 phase 13, Part A)
- `aefe6e5` feat: read-only terminal from one command table (PLAN-003)
- `cd60b90` feat: network checks from every node and the leader (PLAN-002 phase 14)
- `040ce5c` feat: approve to fix, rung R3 (PLAN-002 phase 15)
- `17c0e51` fix: bind the HTTP port before reporting ready
- `48dcab3` feat: wire escalation and tighten fix verification (PLAN-002 phase 16)
- `ff30698` feat: every AutoRemediationPolicy field takes effect (PLAN-002 phase 16)
- `0025475` test: read the helm test result from its phase, not its logs
- `06e77ba` feat: learned CPU baselines set the scale-up threshold (PLAN-002 phase 16)
- `43e2ee1` feat: OOM pull requests patch gitops.valuesFile and use gitops.author
- `0645d0b` test: cover storage, logging, obs and webhook (PLAN-002 phase 16, D6)
- `2e89921` docs: checkpoint after PR #5 merged and PR #6 opened
- `f2a5200` feat: the admission webhook gets a certificate and serves it (PLAN-002 phase 16)
- `d141d2a` feat: opt-in tracing and bounded metric labels (PLAN-002 phase 16)
- `9a137a8` docs: checkpoint with phase 16 webhook, tracing and D8 progress
- `0e8ec4b` test: mutation testing on the safety-critical code (PLAN-002 phase 16, D8)
- `1393961` docs: checkpoint, phase 16 complete on PR #7
- `f2b8877` feat: gitops.mode live commits the OOM fix through the gate (PLAN-002 phase 17)
- `d4581ae` feat: image mirror, Argo Rollouts reload, final floor (PLAN-002 phase 17)
- `ecce315` docs: checkpoint, PLAN-002 complete (PR #8 merged)
- `d5c3aaf` docs: project map checked after PR #8
- `c6dd155` docs: close ISS-021 and ISS-058, both done
- `ee4920b` fix: terminal gets its full width; the UI is never served stale
- `9a2755c` test: the browser test checks the command list under Settings
- `40d47a1` docs: close ISS-032; STATUS header matches where the project is
- `e1cd990` docs: PLAN-004 for durable approvals and 95 percent coverage
- `cc56090` fix: approvals survive a restart and a change of leader
- `74c5e50` fix: remove dead runbook code, make log retention visible and tested
- `f9b8012` docs: project map and checkpoint after PLAN-004 phases 1 and 2
- `d64d271` test: the pod and node informer paths are tested end to end
- `01502d0` fix: the agent's own records are tested, failures counted, dead code removed
- `b433789` test: the dashboard API at 95 percent; hidden API failures counted
- `fe69af8` docs: project map and checkpoint after PLAN-004 phase 5
- `625042f` fix: five bugs found by the phase 6 failure sweeps; every package at 90
- `e1d4f8a` test: coverage 95 percent with every package at 90; PLAN-004 complete
- `aede63a` docs: project map and checkpoint after PLAN-004
- `285e86a` fix: deploy.sh restarts the agent after loading a local image
- `976c8e8` feat: housekeeping has its own budget
- `ccbeb30` fix: node figures read pods only in watched namespaces
- `bc1ce10` docs: ISS-020 accepted; every issue closed
- `67d9999` docs: project map and checkpoint after PR 19
- `f6e2772` fix: incidents name the Deployment, not its ReplicaSet
- `a7f163c` docs: project map and checkpoint for ISS-108
- `34faafd` docs: tidy stale docs and rewrite the README for v1.0.0

## PR #2: PLAN-002 phases 8 to 11 (merged 2026-10-08)

Config in one place, coverage gates, native detectors with the fix ladder, node and controller roles (ADR-001), dashboard rebuilt, safe deployment scripts.

32 commits, 2026-10-07 to 2026-10-08. By type: 14 docs, 9 feat, 5 fix, 2 refactor, 1 test, 1 chore.

- `87c0330` docs: PLAN-002, native config reload, weak features, fix ladder, coverage
- `2401398` docs: project map lists the plans folder
- `1fc839c` docs: record PLAN-002 owner decisions; warn on unimplemented flags
- `79a1590` docs: PLAN-002 Part E, audit every detector and add missing failure classes
- `cd36f77` docs: PLAN-002 phases renumbered; failure-class table becomes phase 10
- `7683ea6` docs: PLAN-002 subtask tables for phases 8, 9 and 10
- `1c0fd55` refactor: read every setting in internal/config; no env reads elsewhere
- `70f8577` test: inject a clock into every time-based component
- `f6baa8e` chore: coverage floor in make verify; 100 percent on safety-critical code
- `ba7e6b0` docs: record PLAN-002 phase 8
- `8c5cd3c` refactor: inject HTTP clients, remove globals, extract run()
- `8577ffb` fix: redact incidents before every escalation channel
- `fc73dd6` docs: record PLAN-002 phase 9
- `7990933` feat: leader pod checks with named causes and fix rungs
- `e8a227c` feat: finalizer, disruption budget, job, HPA and pull-cause checks
- `19710a3` docs: PLAN-002 phase 11 from an architect review
- `0e2d752` docs: project map for the phase 10 kube files
- `30c3b00` feat: Prometheus checks and a test for every detector; phase 10 done
- `d915b93` feat: ADR-001 and event forwarding from node agents to the controller
- `4a5f9aa` feat: AGENT_ROLE splits the agent into node and controller
- `8901104` feat: standby controller proxies the API and ingest to the leader
- `b7ef047` feat: chart deploys a controller Deployment and node agents
- `6428d6a` docs: record PLAN-002 11.1 progress
- `cca7d8f` feat: one ServiceAccount per role; raw manifests generated from the chart
- `4585d66` fix: keep the event history across a controller leader change
- `ea308c1` fix: audit events for every gate decision; compliance from the event log; security headers
- `be1b043` feat: rebuilt dashboard and history kept by a fresh controller
- `a5f96bf` fix: deploy and teardown scripts touch only what they own
- `55711e8` docs: checkpoint after PLAN-002 11.4
- `8fcefc5` docs: ADR-002 watch scope and fix scope with a Settings tab
- `ccf32b6` docs: checkpoint after ADR-002
- `581c215` fix: shellcheck findings from the first CI run

## PR #1: PLAN-001 safety hardening (merged 2026-10-07)

One mutation gate, dry-run by default, RBAC matching the code, redaction, authenticated endpoints.

15 commits, 2026-10-07 to 2026-10-07. By type: 8 fix, 5 docs, 2 chore.

- `3c3d99c` chore: add verify, pinned lint and vuln tools, dash check and kind e2e
- `4082923` fix: route every cluster write through one mutation gate
- `98bb754` fix: default AUTO_MODE to dry-run; add project rules and PLAN-001
- `408c8ca` fix: one actor per node, policy snapshots, conflict-safe writes
- `1f1e6d0` fix: require a token on the dashboard API and verify Slack signatures
- `2f25d06` fix: RBAC that matches the code, counted API errors, non-root image
- `4c2b5b3` fix: redact secrets and personal data before anything leaves the cluster
- `fd6a1ac` chore: upgrade dependencies, pin go1.26.6, gate CI on make verify and e2e
- `5b992ae` fix: dashboard allowlist, dry-run budget, configurable limits, honest docs
- `817cab4` docs: add the full feature table to the README
- `d291bc9` docs: checkpoint and project map for the README feature table
- `6f9023c` fix: pass the container log path, not the host path, as LOG_EFS_PATH
- `25576ff` docs: add a complete guide from first look to production and development
- `fd8326a` docs: complete configuration reference, kept in step by a test
- `b72e21b` docs: checkpoint for PR #1

## Original history

The agent as first written, before the plans. Claims made in that period (detector counts, a "production release") were not measured and are not repeated here.

45 commits, 2025-08-09 to 2026-04-28. By type: 22 feat, 12 fix, 3 docs, 3 chore, 2 test.

