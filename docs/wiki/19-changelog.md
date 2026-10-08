# Changelog

Generated from git history on 2026-10-08 by `make changelog`. Counts are
measured with `git rev-list --count --no-merges`; nothing here is written by
hand. The project has no release tags yet.

## Unreleased (branch plan-002 after PR #2)

PLAN-002 phase 11 continued: watch scope and fix scope (ADR-002), the Settings tab, shutdown order, check-config, docs.

12 commits, 2026-10-08 to 2026-10-08. By type: 5 docs, 4 feat, 2 fix, 1 test.

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

