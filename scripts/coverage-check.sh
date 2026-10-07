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
TOTAL=$(go tool cover -func="$PROFILE" | awk '/^total:/ { gsub("%", "", $3); print $3 }')

STRICT='internal/redact/|internal/ratelimit/|internal/kube/gate\.go|internal/httpapi/http\.go:[0-9]+:[[:space:]]+authorize[[:space:]]'
BELOW=$(go tool cover -func="$PROFILE" | grep -E "$STRICT" | awk '$3 != "100.0%" { print "  " $1 " " $2 " " $3 }' || true)

status=0
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
	echo "coverage-check: total ${TOTAL}% (floor ${FLOOR}%), strict set at 100%"
fi
exit "$status"
