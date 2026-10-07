# PLAN-003: A more capable read-only terminal

Status: planned 2026-10-08. Starts after PLAN-002 phase 13 (native config
reload); PLAN-002 phases 14 to 17 follow it. Owner decision 2026-10-08.
Estimates are modelled.

## Goal

The dashboard's Terminal tab answers more troubleshooting questions without
leaving the browser, stays strictly read-only, and states on screen what it
can run, what it cannot, and why.

## Rules (unchanged by this plan)

- **Read-only.** Every change to the cluster goes through the mutation gate,
  its guardrails and the audit trail; the terminal never writes.
- **Watch scope only** (ADR-002): namespaced reads must be inside the watch
  scope; cluster-scoped reads (nodes, namespaces) are allowed.
- **No secrets.** Secret objects are never read; ConfigMaps show key names,
  not values; every output passes through redaction.
- **RBAC matches the code.** Each new read is granted to the controller and
  nothing more; `TestRBAC_EachRoleMatchesItsCode` enforces it.
- **One source of truth.** The server's command table decides what runs and
  produces `help`, the on-screen panel and the docs, so they cannot drift.

## Never added

| Command | Why |
| --- | --- |
| `delete`, `apply`, `edit`, `patch`, `scale`, `rollout restart`, `cordon`, `drain` | Writes outside the mutation gate, its guardrails and its audit trail. Fixes come from the agent, and from approval buttons in PLAN-002 phase 15 |
| `exec`, `attach`, `cp`, `debug`, `port-forward`, `proxy` | Shell or network access into workloads for anyone holding the dashboard token |
| `get secrets`, `describe secret`, ConfigMap values | Credentials |
| Anything outside the watch scope | Namespace rule (ADR-002) |

## Phases

### Phase 1: command table, help API and the on-screen panel (0.5 day)

| # | Task | Done when |
| --- | --- | --- |
| 1.1 | One command table in `internal/httpapi/terminal.go`: verb, resources, flags, scope rule, description; parsing, `help` and refusals read from it | Unit test: every table entry runs, every entry in the never-added list is refused with its reason |
| 1.2 | `GET /api/kubectl/help` returns the table: can run, cannot run (with why), rules | API test |
| 1.3 | Terminal tab side panel: "You can run", "Not available, and why", "Rules", rendered from 1.2 | Browser test checks the panel against the API |
| 1.4 | `get pods -A` lists every namespace in the watch scope instead of being refused (ISS-063) | Test with three watched namespaces and one outside the scope |

### Phase 2: Tier 1 commands (1 day)

| # | Task | Done when |
| --- | --- | --- |
| 2.1 | `get` for statefulsets, daemonsets, replicasets, cronjobs, hpa, pdb, pvc, ingresses, endpointslices, resourcequotas, networkpolicies, autoremediationpolicies | One test per kind with a fake clientset; RBAC test passes with exactly the new grants |
| 2.2 | `describe` for statefulset, job, service, pvc, hpa, ingress | One test per kind |
| 2.3 | `-o wide`, `-l <selector>`, `--field-selector <selector>` | Tests for each flag |
| 2.4 | `logs --previous`, `--since <duration>`, `logs deploy/<name>` (picks a pod of the deployment) | Tests, including a deployment with no running pod |
| 2.5 | `events --for <kind>/<name>`, newest last | Test |
| 2.6 | `rollout status` and `rollout history` for deployments and statefulsets (read-only) | Tests for a progressing, a stuck and a complete rollout |
| 2.7 | `auth can-i <verb> <resource> [-n <ns>]` for the agent's own permissions (SelfSubjectAccessReview) | Test; RBAC grants `create` on selfsubjectaccessreviews only |

### Phase 3: Tier 2 agent commands (0.5 day)

| # | Task | Done when |
| --- | --- | --- |
| 3.1 | `agent scope`: watch scope, fix scope, fix ceiling, fix-anywhere mode | Test against a policy snapshot |
| 3.2 | `agent why <pod> -n <ns>`: every finding, gate decision and fix for the pod, from the event log | Test with seeded events |
| 3.3 | `agent gate <ns>`: would a fix pass now (mode, quiet hours, blast radius, rate budget, circuit breaker), without acting | Test for each blocking reason |
| 3.4 | `agent status`, `agent version`: role, leader, mode, version | Test |

### Phase 4: records (0.25 day)

| # | Task | Done when |
| --- | --- | --- |
| 4.1 | GUIDE section 6.3 rewritten from the command table; feature table updated; Settings tab links to the panel | `make check-writing` passes |
| 4.2 | Coverage floor raised to the measured total; `make verify`, `make e2e` | Commit |

Total: about 2 to 2.5 days (modelled).
