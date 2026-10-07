# shellcheck shell=bash
# Shared by deploy.sh, teardown.sh and test-apps/chaos-test.sh (PLAN-002 11.4).
# Nothing here kills a process it did not start (ISS-049).

PF_PIDFILE="${TMPDIR:-/tmp}/auto-agent-port-forward.pid"

# k runs kubectl, against $KUBE_CONTEXT when it is set.
k() {
	if [ -n "${KUBE_CONTEXT:-}" ]; then
		kubectl --context "$KUBE_CONTEXT" "$@"
	else
		kubectl "$@"
	fi
}

# dashboard_forward starts a port-forward to the dashboard on localhost:8080,
# unless something already answers there. A port held by another process is
# reported and left alone.
dashboard_forward() {
	local ns="${1:-auto-agent}"
	if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
		return 0
	fi
	if lsof -ti:8080 >/dev/null 2>&1; then
		echo "  Port 8080 is used by another process; it is left alone."
		echo "  Pick a free port: kubectl port-forward -n $ns svc/auto-agent 18080:8080"
		return 1
	fi
	k port-forward -n "$ns" svc/auto-agent 8080:8080 >/dev/null 2>&1 &
	echo "$!" >"$PF_PIDFILE"
	sleep 2
	curl -sf http://localhost:8080/healthz >/dev/null 2>&1
}

# stop_dashboard_forward stops only the port-forward dashboard_forward
# started, and only if that PID is still a kubectl port-forward.
stop_dashboard_forward() {
	[ -f "$PF_PIDFILE" ] || return 0
	local pid
	pid=$(cat "$PF_PIDFILE")
	if ps -p "$pid" -o command= 2>/dev/null | grep -q "kubectl.*port-forward"; then
		kill "$pid"
	fi
	rm -f "$PF_PIDFILE"
}
