#!/bin/sh
# Mutation testing on the safety-critical code (PLAN-002 D8). gremlins
# changes one operator at a time (a comparison, a negation, && and ||) and
# runs the package's tests; a mutant the tests do not catch is a gap in what
# the tests assert. Any surviving mutant fails this run.
#
# Covered: the whole of internal/redact and internal/ratelimit, and in
# internal/kube the mutation gate, its guardrails, the approval queue and
# the policy checks. gremlins has no include list, so every other kube
# source file is excluded by name.
set -eu

GREMLINS="${GREMLINS:-bin/tools/gremlins}"
[ -x "$GREMLINS" ] || { echo "gremlins missing: run 'make tools'"; exit 1; }

KUBE_TARGETS="gate.go guardrails.go approvals.go policycheck.go"

run() {
	pkg="$1"
	shift
	out=$(mktemp)
	echo "mutation: $pkg"
	# gremlins does not fail on its own threshold, so the counts are read
	# here. The timeout coefficient is high because each mutant rebuilds the
	# package's test binary, which takes far longer than the tests.
	"$GREMLINS" unleash --invert-logical --workers "${MUTATION_WORKERS:-4}" --timeout-coefficient 20 \
		-S ltc "$@" "$pkg" >"$out" 2>&1 || { cat "$out"; echo "mutation: gremlins failed on $pkg"; exit 1; }
	killed=$(sed -n 's/^Killed: \([0-9]*\),.*/\1/p' "$out")
	# Lived and timed out mutants slipped past the tests; a mutant on a line
	# no test runs is no better.
	survived=$(grep -cE '^ *(LIVED|TIMED OUT|NOT COVERED) ' "$out" || true)
	grep -E "^Killed:|^Timed out:|^Test efficacy" "$out"
	if [ "$survived" -gt 0 ]; then
		grep -E '^ *(LIVED|TIMED OUT|NOT COVERED) ' "$out"
		echo "mutation: FAIL in $pkg: $survived mutant(s) survived, so the tests do not assert that behaviour"
		exit 1
	fi
	# A run that killed nothing tested nothing: a broken setup looks like a pass.
	if [ -z "$killed" ] || [ "$killed" -eq 0 ]; then
		cat "$out"
		echo "mutation: FAIL in $pkg: no mutant was killed, so the run tested nothing"
		exit 1
	fi
}

run ./internal/redact
run ./internal/ratelimit

excludes=""
for f in internal/kube/*.go; do
	name=$(basename "$f")
	case "$name" in *_test.go) continue ;; esac
	keep=0
	for t in $KUBE_TARGETS; do
		[ "$name" = "$t" ] && keep=1
	done
	[ "$keep" = 1 ] && continue
	# gremlins matches -E against the path relative to the package, so the
	# file name is anchored on its own.
	excludes="$excludes -E (^|/)${name}\$"
done
# shellcheck disable=SC2086 # one flag per excluded file
run ./internal/kube $excludes

echo "mutation: PASS"
