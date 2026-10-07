# Status

Last updated: 2026-10-07

## Active plan

`docs/plans/PLAN-001-safety-hardening.md`: phases 0 and 1 in progress on
branch `phase0-1-safety` (not committed, not pushed).

## Phase 0

- P0.1 done: `make check-writing` (ratchet over changed files, `ALL=1` for
  the whole repo). Failed on 5 real dashes in touched files, passes after.
- P0.2 code done: `make tools` (pinned golangci-lint v1.64.8, govulncheck
  v1.1.4), `make lint` can now fail, `make vuln`, `make helm-lint`,
  `make verify`, `make verify-full`; CI lint pinned. Run 2026-10-07 with owner approval: `make tools` ok;
  `make lint` found 1 errcheck issue in new code (nodes.go Slack post), fixed,
  then clean; `make vuln` exits non-zero with 18 reachable vulnerabilities
  (ISS-016), so `vuln` stays out of `verify` until P6.2.
- P0.3 done 2026-10-07 (owner approved): `make e2e` passed. Falsified with
  `MODE=fix`: the gate logged `APPLIED delete_pod default/pod/crasher` and the
  test failed as it must; dry-run then passed with a 30s settle wait.

## Phase 1

- P1.1 done: `gate_test.go` fake clientset write recorder, 9 drivers x
  (fix control, 8 blocked states, dry-run log).
- P1.2 done: every write goes through `applyMutation` (`internal/kube/gate.go`).
  ISS-001, 002, 003 fixed; ISS-023 found and fixed.
- P1.3 done: default mode `dry-run` in code, chart, manifest. ISS-008.
- P1.4 done: `TestMutationsOnlyThroughGate` (AST, 11 call sites measured
  2026-10-07); falsified with a planted delete.
- Exit check done 2026-10-07: kind e2e in dry-run, crasher detected, uid unchanged.

## Evidence (measured 2026-10-07)

`go build ./...` ok, `go vet ./...` ok, `go test -race ./...` ok,
`helm lint` ok, `make check-writing` ok, `make lint` ok, `make e2e` ok;
`make vuln` fails on ISS-016 as expected. `internal/kube` coverage 10.0 to
23.6 percent; changed functions 75 to 100 percent.

## Done

- 2026-10-07: review of the repository, findings recorded as ISS-001 to
  ISS-021 in `tasks/ISSUES.md`; project rules adopted in `CLAUDE.md`.
