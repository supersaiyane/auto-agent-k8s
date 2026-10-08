#!/bin/sh
# e2e-kind: install the agent in a kind cluster in dry-run mode, start a
# crashlooping pod in an allowlisted namespace, and prove two things:
#   1. the agent detected the crashloop (it appears in the agent log), and
#   2. the pod was not deleted (its UID is unchanged), because dry-run must
#      never write to the cluster (CLAUDE.md constraints 1 and 2).
#
# Needs docker, kind, kubectl and helm. KEEP=1 leaves the cluster running.
set -eu

CLUSTER="${CLUSTER:-auto-agent-e2e}"
IMAGE="auto-agent:e2e"
NS_AGENT="auto-agent"
NS_TEST="default"
WAIT_SECONDS="${WAIT_SECONDS:-240}"
# CHART lets a broken copy of the chart prove the RBAC check can fail.
CHART="${CHART:-charts/auto-agent}"
# MODE=fix exists to prove this test can fail: in fix mode the agent deletes
# the crasher, so the UID check below must report FAIL.
MODE="${MODE:-dry-run}"
# Time allowed after detection for a handler to act before the UID check.
SETTLE_SECONDS="${SETTLE_SECONDS:-30}"
CTX="kind-$CLUSTER"

log() { echo "e2e: $*"; }
fail() { echo "e2e: FAIL: $*"; exit 1; }

cleanup() {
	if [ "${KEEP:-0}" != "1" ]; then
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	log "creating kind cluster $CLUSTER"
	kind create cluster --name "$CLUSTER" --wait 120s
fi

log "building and loading $IMAGE"
docker build -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "installing chart in $MODE mode"
helm upgrade --install auto-agent "$CHART" --kube-context "$CTX" -n "$NS_AGENT" --create-namespace \
	--set image.repository=auto-agent --set image.tag=e2e --set image.pullPolicy=Never \
	--set "agent.mode=$MODE" --set "agent.fixNamespaces={$NS_TEST}" \
	--set dashboard.token=e2e-token \
	--set "env[0].name=JOB_INTERVAL" --set "env[0].value=30s" \
	--set "env[1].name=QUOTA_INTERVAL" --set "env[1].value=40s" \
	--wait --timeout 180s

log "starting a crashlooping pod in $NS_TEST"
kubectl --context "$CTX" -n "$NS_TEST" delete pod crasher --ignore-not-found --wait
kubectl --context "$CTX" -n "$NS_TEST" run crasher --image=busybox:1.36 --restart=Always -- sh -c 'echo boom; exit 1'
UID_BEFORE=$(kubectl --context "$CTX" -n "$NS_TEST" get pod crasher -o jsonpath='{.metadata.uid}')
log "crasher uid $UID_BEFORE"

log "waiting up to ${WAIT_SECONDS}s for the agent to detect the crashloop"
deadline=$(( $(date +%s) + WAIT_SECONDS ))
detected=0
while [ "$(date +%s)" -lt "$deadline" ]; do
	if kubectl --context "$CTX" -n "$NS_AGENT" logs -l app=auto-agent --tail=-1 2>/dev/null | grep -q "crasher"; then
		detected=1
		break
	fi
	sleep 10
done

kubectl --context "$CTX" -n "$NS_AGENT" logs -l app=auto-agent --tail=-1 | grep -E "crasher|dry-run|DRY" | tail -20 || true
[ "$detected" = "1" ] || fail "agent never logged the crasher pod within ${WAIT_SECONDS}s"

log "detected; waiting ${SETTLE_SECONDS}s for any action to land"
sleep "$SETTLE_SECONDS"

UID_AFTER=$(kubectl --context "$CTX" -n "$NS_TEST" get pod crasher -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "missing")
kubectl --context "$CTX" -n "$NS_TEST" get pod crasher -o wide || true
[ "$UID_BEFORE" = "$UID_AFTER" ] || fail "crasher was replaced or deleted in $MODE mode (uid $UID_BEFORE -> $UID_AFTER)"

log "crashloop detected, pod untouched in $MODE mode"

log "checking the dashboard API the way an operator reaches it (port-forward + curl)"
# ADR-001: the API lives on the controllers; node agents serve only probes.
CONTROLLERS=$(kubectl --context "$CTX" -n "$NS_AGENT" get pod -l app=auto-agent-controller -o jsonpath='{.items[*].metadata.name}')
AGENT_POD=${CONTROLLERS%% *}
NODE_POD=$(kubectl --context "$CTX" -n "$NS_AGENT" get pod -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')
[ -n "$AGENT_POD" ] || fail "no controller pod"
[ -n "$NODE_POD" ] || fail "no node agent pod"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$AGENT_POD" 18080:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
NO_TOKEN=$(code http://127.0.0.1:18080/api/status)
WITH_TOKEN=$(code -H 'Authorization: Bearer e2e-token' http://127.0.0.1:18080/api/status)
HEALTH=$(code http://127.0.0.1:18080/healthz)
KUBECTL=$(curl -s -H 'Authorization: Bearer e2e-token' -X POST -d '{"command":"get pods -n kube-system"}' http://127.0.0.1:18080/api/kubectl)
# Phase 15: the approval queue is served, and with no approvers it is empty;
# the dashboard can never approve.
APPROVALS=$(curl -s -H 'Authorization: Bearer e2e-token' http://127.0.0.1:18080/api/approvals)
APPROVE_POST=$(code -H 'Authorization: Bearer e2e-token' -X POST http://127.0.0.1:18080/api/approvals?id=x)
kill "$PF" 2>/dev/null || true
log "/api/approvals -> $APPROVALS; POST -> $APPROVE_POST"
[ "$APPROVALS" = "[]" ] || fail "/api/approvals with no approvers returned $APPROVALS, want []"
[ "$APPROVE_POST" = "405" ] || fail "POST /api/approvals returned $APPROVE_POST, want 405"
log "/api/status no token=$NO_TOKEN, with token=$WITH_TOKEN; /healthz=$HEALTH"
log "kubectl -n kube-system -> $KUBECTL"
[ "$NO_TOKEN" = "401" ] || fail "/api/status without token returned $NO_TOKEN, want 401"
[ "$WITH_TOKEN" = "200" ] || fail "/api/status with token returned $WITH_TOKEN, want 200"
[ "$HEALTH" = "200" ] || fail "/healthz returned $HEALTH, want 200"
echo "$KUBECTL" | grep -q "outside the watch scope" || fail "kubectl endpoint read kube-system"

for pod in "$AGENT_POD" "$NODE_POD"; do
	CHECK=$(kubectl --context "$CTX" -n "$NS_AGENT" exec "$pod" -c agent -- /auto-agent check-config 2>&1) \
		|| { echo "$CHECK" | tail -5; fail "check-config in $pod found keys the agent does not read (ISS-056)"; }
	echo "$CHECK" | grep -q "DASHBOARD_TOKEN=(set, redacted)" || [ "$pod" = "$NODE_POD" ] \
		|| fail "check-config in $pod did not redact the dashboard token"
	if echo "$CHECK" | grep -q "e2e-token"; then fail "check-config in $pod printed a secret"; fi
done
log "check-config: every key the chart sets is read, secrets redacted"

kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$NODE_POD" 18081:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
NODE_API=$(code -H 'Authorization: Bearer e2e-token' http://127.0.0.1:18081/api/status)
kill "$PF" 2>/dev/null || true
[ "$NODE_API" = "404" ] || fail "a node agent served /api/status ($NODE_API); only controllers may"

log "checking every controller shows the node agent's finding (ADR-001, PLAN-002 11.1)"
port=18090
for c in $CONTROLLERS; do
	kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$c" "$port:8080" >/dev/null 2>&1 &
	PF=$!
	sleep 3
	seen=0
	for _ in 1 2 3 4 5 6; do
		if curl -s -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$port/api/events?limit=500" | grep -q "crasher"; then
			seen=1
			break
		fi
		sleep 5
	done
	kill "$PF" 2>/dev/null || true
	[ "$seen" = "1" ] || fail "controller $c does not show the crasher finding from the node agent"
	log "controller $c shows the crasher finding"
	port=$((port + 1))
done

log "checking the history survives two leader changes in a row (ISS-059)"
# Deleting the leader also starts a replacement pod, which may win the lease;
# it must already hold the history. Two rounds make that case likely.
for round in 1 2; do
	LEADER=$(kubectl --context "$CTX" -n "$NS_AGENT" get lease auto-agent-leader -o jsonpath='{.spec.holderIdentity}')
	[ -n "$LEADER" ] || fail "no lease holder"
	kubectl --context "$CTX" -n "$NS_AGENT" delete pod "$LEADER" --wait=false >/dev/null
	deadline=$(( $(date +%s) + 90 ))
	NEW_LEADER="$LEADER"
	while [ "$(date +%s)" -lt "$deadline" ]; do
		NEW_LEADER=$(kubectl --context "$CTX" -n "$NS_AGENT" get lease auto-agent-leader -o jsonpath='{.spec.holderIdentity}')
		[ -n "$NEW_LEADER" ] && [ "$NEW_LEADER" != "$LEADER" ] && break
		sleep 3
	done
	[ "$NEW_LEADER" != "$LEADER" ] || fail "round $round: no controller took over after $LEADER was deleted"
	kubectl --context "$CTX" -n "$NS_AGENT" wait --for=condition=Ready "pod/$NEW_LEADER" --timeout=90s >/dev/null
	log "round $round: leader moved from $LEADER to $NEW_LEADER"
	kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$NEW_LEADER" 18099:8080 >/dev/null 2>&1 &
	PF=$!
	sleep 3
	HISTORY=$(curl -s -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:18099/api/events?limit=500")
	kill "$PF" 2>/dev/null || true
	echo "$HISTORY" | grep -q "crasher" || fail "round $round: the new leader $NEW_LEADER lost the crasher finding"
	log "round $round: new leader $NEW_LEADER still shows the crasher finding"
	# Let the replacement controller start and backfill before the next round.
	kubectl --context "$CTX" -n "$NS_AGENT" rollout status deployment/auto-agent-controller --timeout=120s >/dev/null
done
AGENT_POD="$NEW_LEADER"

log "checking the fix scope set from the dashboard (ADR-002, PLAN-002 11.8)"
kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$AGENT_POD" 18100:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
SCOPE_URL=http://127.0.0.1:18100/api/scope
put_scope() { curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer e2e-token' -X PUT -d "$1" "$SCOPE_URL"; }
scope_json() { curl -s -H 'Authorization: Bearer e2e-token' "$SCOPE_URL"; }
scope_data() { kubectl --context "$CTX" -n "$NS_AGENT" get configmap auto-agent-scope -o jsonpath='{.data}' 2>/dev/null || echo missing; }
# every agent pod logs the scope it applies; wait until all show want
wait_scope_log() {
	want="$1"
	deadline=$(( $(date +%s) + 60 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		ok=1
		for pod in $(kubectl --context "$CTX" -n "$NS_AGENT" get pod -l 'app in (auto-agent,auto-agent-controller)' -o jsonpath='{.items[*].metadata.name}'); do
			last=$(kubectl --context "$CTX" -n "$NS_AGENT" logs "$pod" -c agent --tail=-1 | grep "policy: fix scope is now" | tail -1)
			case "$last" in *"$want"*) ;; *) ok=0 ;; esac
		done
		[ "$ok" = "1" ] && return 0
		sleep 3
	done
	return 1
}
scope_json | grep -q '"fixScope":\["default"\]' || fail "/api/scope does not start from the Helm list: $(scope_json)"
CODE=$(put_scope '{"fixNamespaces":["kube-public"],"confirm":["kube-public"]}')
[ "$CODE" = "400" ] || fail "enabling a namespace outside the ceiling returned $CODE, want 400"
CODE=$(put_scope '{"fixNamespaces":[],"confirm":[]}')
[ "$CODE" = "204" ] || fail "narrowing the fix scope returned $CODE, want 204"
scope_data | grep -q '"fixNamespaces":""' || fail "auto-agent-scope does not hold the empty choice: $(scope_data)"
wait_scope_log "is now [] (dashboard choice: true)" || fail "not every controller and node agent applied the dashboard choice"
log "every controller and node agent applied the empty fix scope"
helm upgrade auto-agent "$CHART" --kube-context "$CTX" -n "$NS_AGENT" --reuse-values --wait --timeout 180s >/dev/null
scope_data | grep -q '"fixNamespaces":""' || fail "a Helm upgrade changed the dashboard choice: $(scope_data)"
scope_json | grep -q '"choice":\[\]' || fail "after a Helm upgrade /api/scope lost the choice: $(scope_json)"
log "a Helm upgrade kept the dashboard choice"
curl -s -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:18100/api/events?limit=500&type=audit" | grep -q 'set_fix_scope' \
	|| fail "the scope change is not in the audit log"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer e2e-token' -X DELETE "$SCOPE_URL")
[ "$CODE" = "204" ] || fail "returning to the Helm values returned $CODE, want 204"
if scope_data | grep -q fixNamespaces; then fail "returning to Helm left a choice: $(scope_data)"; fi
wait_scope_log "is now [default] (dashboard choice: false)" || fail "not every agent returned to the Helm list"
kill "$PF" 2>/dev/null || true
log "scope: ceiling enforced, choice applied by every agent, kept across a Helm upgrade, audited, cleared"

log "checking config reload (PLAN-002 Part A): dry-run records, fix mode restarts once"
k() { kubectl --context "$CTX" "$@"; }
k -n "$NS_TEST" create configmap reload-demo --from-literal=level=info >/dev/null
k -n "$NS_TEST" create deployment reload-demo --image=busybox:1.36 -- sh -c 'sleep 3600' >/dev/null
k -n "$NS_TEST" set env deployment/reload-demo --from=configmap/reload-demo --keys=level >/dev/null
k -n "$NS_TEST" rollout status deployment/reload-demo --timeout=120s >/dev/null
edit3() { for v in a b c; do k -n "$NS_TEST" patch configmap reload-demo --type merge -p "{\"data\":{\"level\":\"$1-$v\"}}" >/dev/null; sleep 1; done; }
rs_count() { k -n "$NS_TEST" get rs -l app=reload-demo --no-headers 2>/dev/null | wc -l | tr -d ' '; }
POD_BEFORE=$(k -n "$NS_TEST" get pod -l app=reload-demo -o jsonpath='{.items[0].metadata.uid}')
RS_BEFORE=$(rs_count)
kubectl --context "$CTX" -n "$NS_AGENT" port-forward "pod/$AGENT_POD" 18110:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
reloads() { curl -s -H 'Authorization: Bearer e2e-token' http://127.0.0.1:18110/api/reloads; }
edit3 dry
sleep 25
DRY=$(reloads)
echo "$DRY" | grep -q '"workload":"deployment/reload-demo","result":"simulated"' || fail "dry-run did not record the reload: $DRY"
[ "$(echo "$DRY" | grep -o '"object":"configmap/reload-demo"' | wc -l | tr -d ' ')" = "1" ] || fail "three quick edits gave more than one reload: $DRY"
[ "$(k -n "$NS_TEST" get pod -l app=reload-demo -o jsonpath='{.items[0].metadata.uid}')" = "$POD_BEFORE" ] || fail "dry-run restarted the pod"
log "dry-run: one simulated reload for three edits, pod untouched"
k -n "$NS_AGENT" patch configmap auto-agent-config --type merge -p '{"data":{"AUTO_MODE":"fix"}}' >/dev/null
sleep 10
edit3 fix
sleep 30
FIX=$(reloads)
kill "$PF" 2>/dev/null || true
k -n "$NS_AGENT" patch configmap auto-agent-config --type merge -p '{"data":{"AUTO_MODE":"dry-run"}}' >/dev/null
echo "$FIX" | grep -q '"workload":"deployment/reload-demo","result":"restarted"' || fail "fix mode did not restart the workload: $FIX"
[ "$(rs_count)" = "$((RS_BEFORE + 1))" ] || fail "fix mode rolled the Deployment $(( $(rs_count) - RS_BEFORE )) times for three quick edits, want 1"
log "fix mode: three quick edits, one restart"

log "checking the pods run non-root and RBAC covers every detector (ISS-009, ISS-010)"
for pod in "$AGENT_POD" "$NODE_POD"; do
	RUN_AS=$(kubectl --context "$CTX" -n "$NS_AGENT" get pod "$pod" -o jsonpath='{.spec.securityContext.runAsUser}')
	[ "$RUN_AS" = "65532" ] || fail "pod $pod runAsUser is '$RUN_AS', want 65532"
done
# JOB_INTERVAL=30s and QUOTA_INTERVAL=40s are set at install, so this wait
# covers at least two passes of every leader loop.
RBAC_SETTLE="${RBAC_SETTLE:-90}"
log "waiting ${RBAC_SETTLE}s for the leader loops to run every detector once"
sleep "$RBAC_SETTLE"
AGENT_LOGS=$(kubectl --context "$CTX" -n "$NS_AGENT" logs -l 'app in (auto-agent,auto-agent-controller)' -c agent --tail=-1 --prefix)
echo "$AGENT_LOGS" | grep -q "acquired leader lease" || fail "no controller acquired the leader lease"
echo "$AGENT_LOGS" | grep -q "netprobe: dns=true" || fail "node agents did not start the network probes (PLAN-002 phase 14)"
if echo "$AGENT_LOGS" | grep -q "DNSResolutionFailed"; then fail "a node agent could not resolve the API server's Service name on a healthy cluster"; fi
FORBIDDEN=$(echo "$AGENT_LOGS" | grep -i "forbidden" || true)
[ -z "$FORBIDDEN" ] || { echo "$FORBIDDEN" | head -10; fail "agent hit forbidden API reads: RBAC does not match the code"; }

log "PASS: dry-run untouched, API requires token, kubectl scoped, fix scope from the dashboard, config reload, network probes, node findings on every controller, history kept across two leader changes, non-root, RBAC complete"
