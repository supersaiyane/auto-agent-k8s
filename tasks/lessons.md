# Lessons

Corrections received and the rule each one produced. Rules marked "carried
over" were adopted on 2026-10-07 from `~/MyTechExploration/meter/tasks/lessons.md`
and rewritten for a Go and Kubernetes codebase.

## 2026-10-07: dashes (carried over)
**Rule:** Never use an em dash or an en dash in any output, chat included.
Before finishing a task that writes files, run `make check-writing` (or the
grep it wraps) and paste the result.
**Triggers:** any written output.

## 2026-10-07: reach it the way a person would (carried over)
**Rule:** A change to an HTTP route, the chart or remediation behaviour is not
done until it was run against a kind cluster and reached the way an operator
would: `curl` the route and read the body, `kubectl` the object and read its
state. A passing suite proves the code matches the test, not that it is wired,
permitted or reachable.
**Triggers:** httpapi, slack, charts, any action in internal/kube.

## 2026-10-07: exit codes lie (carried over)
**Rule:** A checker runs alone in its own Bash call, output redirected to a
file, followed by `echo "exit: $?"`, and both are read. A checker and a commit
never share a call. Prove the artifact, not the command: inspect the built
image or the running pod for the change.
**Triggers:** make verify, go test, docker build, helm.

## 2026-10-07: make the checker fail once (carried over)
**Rule:** Before trusting a checker's silence, break the input on purpose and
watch it fail. Reverse the break by inverting the same edit, never with
`git checkout` or `git restore`, which also discard uncommitted work.
**Triggers:** new make target, new test, new lint rule.

## 2026-10-07: test the category, not the member (carried over)
**Rule:** When a test is about a mechanism that applies to a category (every
mutating action, every route), derive the members at run time and assert over
all of them, so a new member cannot escape the test.
**Triggers:** gate tests, auth tests, RBAC inventory.

## 2026-10-07: counts are computed (carried over)
**Rule:** A number derived from the repository is computed in the same command
that writes it down. Never from memory or from an earlier count.
**Triggers:** docs, commit messages, ISSUES rows with counts.

## 2026-10-07: reproduce on baseline (carried over)
**Rule:** Before blaming a change for a failure, reproduce it on the baseline
under the same conditions with only the code different. If an attribution
turns out wrong, correct every place it was written, comments included.
**Triggers:** a test fails after an edit.

## 2026-10-07: main session only (carried over)
**Rule:** Builds, merges and reviews happen in the main session, one task at a
time. Start no background agent or side session unless the owner asks. This
outranks the kernel's "orchestrator never executes".
**Triggers:** about to call Agent or start_session.

## 2026-10-07: no subagents
**Mistake:** I delegated the first repository study to a subagent and proposed a side session for phase 1.
**Correction:** The owner said do not use agents: they take a long time, use many tokens and go stale.
**Rule:** Never call the Agent tool, start_session or a Workflow in this project unless the owner names one in their latest message. Read code directly with targeted `sed -n`/`grep` and do the work in the main session.
**Triggers:** about to delegate, "explore", "review", "study", a phase that feels large.

## 2026-10-07: phase loop
**Rule:** For PLAN-001 the owner set the loop: finish a phase, run `make verify` alone and read it, commit, then start the next phase without asking. Stop and ask only for something destructive or outward facing (push, history rewrite, real cluster).
**Triggers:** a phase finished.
