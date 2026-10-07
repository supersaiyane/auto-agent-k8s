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

log "building and loading auto-agent:latest"
docker build -t auto-agent:latest "$DIR" >/dev/null
kind load docker-image auto-agent:latest --name "$CLUSTER" >/dev/null

log "applying deployment/ as deploy.sh does"
k() { kubectl --context "$CTX" "$@"; }
k apply -f "$DIR/deployment/00-namespace.yaml" >/dev/null
k apply -f "$DIR/deployment/01-crds.yaml" >/dev/null
for ns in default test1 test2; do
	k create namespace "$ns" --dry-run=client -o yaml | k apply -f - >/dev/null
done
k apply -f "$DIR/deployment/02-rbac.yaml" >/dev/null
k apply -f "$DIR/deployment/03-config.yaml" >/dev/null
KUBECTL="kubectl --context $CTX" sh "$DIR/deployment/ensure-secret.sh" "$NS"
k apply -f "$DIR/deployment/04-agent.yaml" >/dev/null

k -n "$NS" rollout status daemonset/auto-agent --timeout=180s
k -n "$NS" rollout status deployment/auto-agent-controller --timeout=180s

log "checking ensure-secret.sh never overwrites"
k -n "$NS" patch secret auto-agent-secrets --type merge -p '{"stringData":{"SLACK_WEBHOOK_URL":"https://hooks.example.test/kept"}}' >/dev/null
TOKEN_BEFORE=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.INTERNAL_TOKEN}')
KUBECTL="kubectl --context $CTX" sh "$DIR/deployment/ensure-secret.sh" "$NS"
KEPT=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.SLACK_WEBHOOK_URL}' | base64 -d)
TOKEN_AFTER=$(k -n "$NS" get secret auto-agent-secrets -o jsonpath='{.data.INTERNAL_TOKEN}')
[ "$KEPT" = "https://hooks.example.test/kept" ] || fail "a re-run lost the patched SLACK_WEBHOOK_URL"
[ -n "$TOKEN_BEFORE" ] && [ "$TOKEN_BEFORE" = "$TOKEN_AFTER" ] || fail "a re-run changed INTERNAL_TOKEN"

log "waiting ${RBAC_SETTLE}s for the leader loops and node agents to run"
sleep "$RBAC_SETTLE"
LOGS=$(k -n "$NS" logs -l 'app in (auto-agent,auto-agent-controller)' -c agent --tail=-1 --prefix)
echo "$LOGS" | grep -q "acquired leader lease" || fail "no controller acquired the leader lease"
echo "$LOGS" | grep -q "role=node" || fail "no node agent started in the node role"
FORBIDDEN=$(echo "$LOGS" | grep -i "forbidden" || true)
[ -z "$FORBIDDEN" ] || { echo "$FORBIDDEN" | head -10; fail "forbidden API reads: generated RBAC does not match the code"; }

log "PASS: generated manifests install both roles, secret kept, RBAC complete"
