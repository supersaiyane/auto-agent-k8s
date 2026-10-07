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
NS_AGENT="kube-system"
NS_TEST="default"
WAIT_SECONDS="${WAIT_SECONDS:-240}"
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
helm upgrade --install auto-agent charts/auto-agent --kube-context "$CTX" \
	--set image.repository=auto-agent --set image.tag=e2e --set image.pullPolicy=Never \
	--set "agent.mode=$MODE" --set "agent.namespaceAllowlist={$NS_TEST}" \
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

log "PASS: crashloop detected, pod untouched in $MODE mode"
