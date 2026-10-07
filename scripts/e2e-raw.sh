#!/bin/sh
# Installs the generated raw manifests (deployment/) on a throwaway kind
# cluster, the way deploy.sh applies them, and checks the result: both roles
# roll out, a controller leads, nothing hits a forbidden read, and re-running
# ensure-secret.sh keeps a value an operator patched in (ISS-048, ISS-050).
set -eu

CLUSTER="${CLUSTER:-auto-agent-raw}"
CTX="kind-$CLUSTER"
NS=auto-agent
DIR="$(cd "$(dirname "$0")/.." && pwd)"
RBAC_SETTLE="${RBAC_SETTLE:-90}"

log() { echo "e2e-raw: $*"; }
fail() { echo "e2e-raw: FAIL: $*" >&2; exit 1; }

cleanup() { kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
cleanup
trap cleanup EXIT

log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null

log "running deployment/deploy.sh against $CTX"
k() { kubectl --context "$CTX" "$@"; }
k create namespace keepme >/dev/null # yours: no demo label; teardown must never touch it
bash "$DIR/deployment/deploy.sh" --context "$CTX" --no-port-forward

log "checking ensure-secret.sh never overwrites"
k -n "$NS" patch secret auto-agent-secrets --type merge -p '{"stringData":{"SLACK_WEBHOOK_URL":"https://hooks.example.test/kept"}}' >/dev/null
TOKEN_BEFORE=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.INTERNAL_TOKEN}')
KUBECTL="kubectl --context $CTX" sh "$DIR/deployment/ensure-secret.sh" "$NS"
KEPT=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.SLACK_WEBHOOK_URL}' | base64 -d)
TOKEN_AFTER=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.INTERNAL_TOKEN}')
[ "$KEPT" = "https://hooks.example.test/kept" ] || fail "a re-run lost the patched SLACK_WEBHOOK_URL"
if [ -z "$TOKEN_BEFORE" ] || [ "$TOKEN_BEFORE" != "$TOKEN_AFTER" ]; then
	fail "a re-run changed INTERNAL_TOKEN"
fi

log "waiting ${RBAC_SETTLE}s for the leader loops and node agents to run"
sleep "$RBAC_SETTLE"
LOGS=$(k -n "$NS" logs -l 'app in (auto-agent,auto-agent-controller)' -c agent --tail=-1 --prefix)
echo "$LOGS" | grep -q "acquired leader lease" || fail "no controller acquired the leader lease"
echo "$LOGS" | grep -q "role=node" || fail "no node agent started in the node role"
FORBIDDEN=$(echo "$LOGS" | grep -i "forbidden" || true)
[ -z "$FORBIDDEN" ] || { echo "$FORBIDDEN" | head -10; fail "forbidden API reads: generated RBAC does not match the code"; }

log "checking teardown.sh removes only what deploy.sh installed"
bash "$DIR/deployment/teardown.sh" --context "$CTX"
k get crd autoremediationpolicies.autoagent.io >/dev/null || fail "plain teardown deleted the CRD and every policy"
k get namespace test1 >/dev/null || fail "plain teardown deleted a demo namespace"
bash "$DIR/deployment/teardown.sh" --context "$CTX" --delete-policies --delete-demo-namespaces
k get crd autoremediationpolicies.autoagent.io >/dev/null 2>&1 && fail "--delete-policies kept the CRD"
for ns in test1 test2 chaos; do
	phase=$(k get namespace "$ns" -o jsonpath='{.status.phase}' 2>/dev/null || echo gone)
	[ "$phase" = "Terminating" ] || [ "$phase" = "gone" ] || fail "--delete-demo-namespaces left $ns ($phase)"
done
[ "$(k get namespace keepme -o jsonpath='{.status.phase}')" = "Active" ] || fail "teardown touched a namespace it did not create"

log "PASS: deploy.sh installs both roles, secret kept, RBAC complete, teardown removes only its own"
