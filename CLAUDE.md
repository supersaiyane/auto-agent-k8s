# auto-agent-k8s: project rules

These rules are binding for every contributor, human or agent. They sit on top
of the global rules and override them where they conflict. Adapted on
2026-10-07 from the meter project (`~/MyTechExploration/meter/CLAUDE.md`,
`docs/03-CHANGE-CONTROL.md`, `tasks/lessons.md`). The writing rules, the
definition of done and the routing table carry over; the architectural
constraints are this project's own.

## Writing rules

1. **No em dashes and no en dashes anywhere.** Not in code, comments,
   documentation, commit messages, UI copy, chat responses or generated
   output. Use a comma, a colon, parentheses, or restructure the sentence. The
   only acceptable dash is a hyphen inside a compound word or a list bullet.
2. Plain English. No filler, no marketing voice, no hedging.
3. Every number in a document states whether it is measured or modelled, and a
   count derived from the repository is computed in the same command that
   writes it down. Never from memory.
4. Absolute dates only. Write `2026-10-07`, never `tomorrow` or `last week`.

Enforced by `make check-writing` (added in PLAN-001 phase 0), which runs inside
`make verify` and in CI. The literal characters are deliberately not written
in this file so the check does not fail on its own documentation.

## Documentation routing

Every change has exactly one obvious home. Read the change, take the first row
that applies, and stop. If two rows apply, take the higher one. If none apply,
the table is wrong, so fix the table.

| If the change is | It goes in | And you also |
| --- | --- | --- |
| A decision that is expensive to reverse | A new ADR in `docs/adr/` | Link it from `README.md` |
| A change to how a body of work is built | Its plan in `docs/plans/` | Update `tasks/STATUS.md` |
| A defect, risk, gap or audit finding | An `ISS-nnn` row in `tasks/ISSUES.md` | Never track a finding only in chat or a commit body |
| A question awaiting a person | An OPEN row in `tasks/ISSUES.md` | Nothing else |
| Progress on existing work | `tasks/STATUS.md` | Nothing else |
| A feature claim changes (added, removed, really working) | `docs/wiki/23-feature-status.md`, citing the code path | Nothing else |
| A module added, moved or repurposed | `project_map.md` | Nothing else |
| A new or removed exported symbol in `internal/kube` | `context_index.md` | Nothing else |
| Where a session stopped | `docs/checkpoint.md`, with a Resume Vector | Rewrite its "Next action" section |
| A correction received from the owner | `tasks/lessons.md` | Nothing else |

Batch these updates into the same commit as the change that made them stale.
Never one commit per document, never a separate cleanup pass later.

## Definition of done

A change is done when all of the following are true, and each was shown by
running the command and reading its output.

- `go build ./...`, `go vet ./...` and `go test -race ./...` pass.
- Coverage for every changed package is at or above 80 percent.
- `golangci-lint run` (pinned version) and `govulncheck ./...` report nothing.
- `helm lint charts/*` passes when anything under `charts/` changed.
- A change to remediation behaviour was run against a kind cluster and the
  effect (or the absence of an effect in dry-run) was observed with `kubectl`.
  Paste what came back.
- Errors are returned or counted, never discarded with `_ =`. A forbidden or
  not found error from the API server is a metric and a log line, never a
  silent `continue`.
- No secrets, tenant URLs, account identifiers or hardcoded namespaces.
- The routing table above was applied, and `make check-writing` passes.
- A staff engineer would approve it in review.

Reading your own code back is not evidence. Running the check and pasting the
output is evidence. A passing unit suite says nothing about whether the agent
is wired, reachable or permitted by RBAC.

## Architectural constraints

These are not style preferences. Breaking one turns a remediation agent into an
outage generator.

1. **One gate for every mutation.** Every create, update, patch, delete or
   evict goes through the single gate in `internal/kube` that checks mode,
   guardrails (quiet hours, blast radius, circuit breaker) and the rate
   limiter. No other code calls a mutating client method. A test enforces it.
2. **Safe by default.** The default mode is `dry-run`. `fix` is an explicit
   opt-in per cluster, never a shipped default.
3. **One actor per target.** Node actions are taken only by the agent running
   on that node, or only by the leader. Cluster-wide loops run only on the
   leader. Two replicas must never act on the same object.
4. **The namespace allowlist applies to every read and every write**,
   including evictions, polling and the dashboard.
5. **Policy is a snapshot.** Code reads policy through an atomic pointer or the
   hot reloader's getter, never a shared field that can be reassigned.
6. **Writes are conflict safe.** Use `Patch`, the scale subresource or
   `retry.RetryOnConflict`. Never a blind full-object `Update`.
7. **RBAC matches the code.** Every API the code calls is granted in the chart
   and nothing more. Leases are a namespaced Role with `resourceNames`.
8. **No data leaves the cluster unredacted.** Logs and events sent to an LLM,
   Slack or an issue tracker pass through redaction first.
9. **Every inbound endpoint is authenticated.** Dashboard and API by token or
   OIDC, Slack by verified signature.
10. **Docs claim only what code does.** A feature marked working cites the code
    path that implements it and the test that proves it.
11. **Images are non-root, distroless and pinned by digest**, with
    `readOnlyRootFilesystem`, `drop: [ALL]` and seccomp `RuntimeDefault`.

## Working discipline

- Run project tools through their own entry points (`make` targets, `go`),
  never through wrappers that install things on the fly.
- Never read success from an exit code alone or from a piped command. A check
  runs alone, output to a file, then `echo "exit: $?"`, and the number is read.
  A checker and a commit never share one Bash call.
- Before trusting a checker's silence, make it fail once. A check that passes
  over zero inputs looks exactly like a check that passes over correct ones.
- Before blaming a change for a failure, reproduce it on the baseline with only
  the code different.
- Builds, merges and reviews happen in the main session, one task at a time.
  Start no background agent or side session unless the owner asks for one.
- Every finding gets an `ISS-nnn` before it gets a fix, and the fix commit
  names it.

## Commit format

```
<type>: <description>

<body explaining why, not what; names the ISS-nnn it closes>
```

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.
One logical change per commit. No dashes other than hyphens in the message.

## Repository layout

```
cmd/auto-agent/     entry point, dependency wiring, leader-only loops
internal/kube/      detection, the mutation gate, remediation actions
internal/policy/    mode, allowlist, thresholds, hot reload
internal/*/         integrations, HTTP API, leader election, storage
charts/             Helm chart, the deployment of record
deployment/         raw manifests, kept in sync with the chart
docs/wiki/          user facing documentation
docs/plans/         how a body of work is built (PLAN-nnn)
docs/adr/           decisions that are expensive to reverse
docs/checkpoint.md  session continuity log with resume vectors
tasks/              STATUS.md, ISSUES.md, lessons.md
project_map.md      module level map
```
