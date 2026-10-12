#!/bin/sh
# coverage-check: fail if total statement coverage drops below .coverage-floor,
# or if any function in the strict set is below 100 percent (PLAN-002 8.6, 8.7).
# The strict set is code that can change the cluster or leak data.
#
# Reads the profile written by `make test` (coverage.out); runs the tests
# itself if it is missing.
set -eu

PROFILE="${PROFILE:-coverage.out}"
if [ ! -f "$PROFILE" ]; then
	go test -count=1 -coverprofile="$PROFILE" ./... >/dev/null
fi

FLOOR=$(grep -v '^#' .coverage-floor | head -1 | tr -d ' ')
# Test helpers ship no code and are left out of the total, by name only
# (PLAN-004 B.2): internal/httpx/httpxtest is imported only by tests.
EXCLUDE='/internal/httpx/httpxtest/'
COUNTED=$(mktemp)
trap 'rm -f "$COUNTED"' EXIT
grep -v "$EXCLUDE" "$PROFILE" > "$COUNTED"
TOTAL=$(go tool cover -func="$COUNTED" | awk '/^total:/ { gsub("%", "", $3); print $3 }')

STRICT='internal/redact/|internal/ratelimit/|internal/kube/gate\.go|internal/httpapi/http\.go:[0-9]+:[[:space:]]+authorize[[:space:]]'
BELOW=$(go tool cover -func="$PROFILE" | grep -E "$STRICT" | awk '$3 != "100.0%" { print "  " $1 " " $2 " " $3 }' || true)

# Every package must also reach PACKAGE_FLOOR (PLAN-004 B6), so a well
# tested package cannot hide an untested one in the total.
PACKAGE_FLOOR=90
LOW_PACKAGES=$(awk -v floor="$PACKAGE_FLOOR" 'NR > 1 {
	split($1, a, ":"); f = a[1]; sub(/\/[^\/]*$/, "", f)
	key = $1; n[key] = $2; if ($3 > 0) hit[key] = 1; pkg[key] = f
} END {
	for (k in n) { t[pkg[k]] += n[k]; if (hit[k]) c[pkg[k]] += n[k] }
	for (p in t) if (100 * c[p] / t[p] < floor) printf "  %s %.1f%%\n", p, 100 * c[p] / t[p]
}' "$COUNTED")

status=0
if [ -n "$LOW_PACKAGES" ]; then
	echo "coverage-check: FAIL, packages below ${PACKAGE_FLOOR}%:"
	echo "$LOW_PACKAGES"
	status=1
fi
if awk -v t="$TOTAL" -v f="$FLOOR" 'BEGIN { exit !(t + 0 < f + 0) }'; then
	echo "coverage-check: FAIL, total ${TOTAL}% is below the floor ${FLOOR}% (.coverage-floor)"
	status=1
fi
if [ -n "$BELOW" ]; then
	echo "coverage-check: FAIL, strict functions below 100%:"
	echo "$BELOW"
	status=1
fi
if [ "$status" -eq 0 ]; then
	echo "coverage-check: total ${TOTAL}% (floor ${FLOOR}%), every package at ${PACKAGE_FLOOR}% or more, strict set at 100%"
fi
exit "$status"
