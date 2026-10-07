#!/bin/bash
# Removes what deploy.sh installed, and only that (PLAN-002 11.4, ISS-049).
# Your AutoRemediationPolicies, cost tools you installed yourself and
# namespaces you created stay unless a flag says otherwise.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

NS=auto-agent
DELETE_POLICIES=0
DELETE_DEMO=0

usage() {
	cat <<'USAGE'
Usage: deployment/teardown.sh [flags]

  --context NAME             kubectl context to use (default: current context)
  --delete-policies          also delete the CRD and with it every AutoRemediationPolicy
  --delete-demo-namespaces   also delete namespaces deploy.sh created (label auto-agent.io/demo=true)
  -h, --help                 this help
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
	--context) KUBE_CONTEXT="$2"; shift 2 ;;
	--delete-policies) DELETE_POLICIES=1; shift ;;
	--delete-demo-namespaces) DELETE_DEMO=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
	esac
done
export KUBE_CONTEXT="${KUBE_CONTEXT:-}"

stop_dashboard_forward

echo "Removing node agents, controller, configuration and RBAC..."
k delete -f "$SCRIPT_DIR/04-agent.yaml" --ignore-not-found
k delete -f "$SCRIPT_DIR/03-config.yaml" --ignore-not-found
k delete secret auto-agent-secrets -n "$NS" --ignore-not-found
k delete -f "$SCRIPT_DIR/02-rbac.yaml" --ignore-not-found

if [ "$DELETE_POLICIES" = 1 ]; then
	echo "Removing the CRD and every AutoRemediationPolicy..."
	k delete -f "$SCRIPT_DIR/01-crds.yaml" --ignore-not-found
else
	echo "Kept the CRD and your AutoRemediationPolicies (--delete-policies removes them)."
fi

# Cost tools only when deploy.sh installed them.
for tool in opencost kubecost; do
	if [ "$(k get namespace "$tool" -o jsonpath='{.metadata.labels.auto-agent\.io/installed-by}' 2>/dev/null)" = "deploy.sh" ]; then
		echo "Removing $tool (installed by deploy.sh)..."
		helm uninstall "$tool" ${KUBE_CONTEXT:+--kube-context "$KUBE_CONTEXT"} -n "$tool"
		k delete namespace "$tool" --wait=false
	fi
done

if [ "$DELETE_DEMO" = 1 ]; then
	for ns in $(k get namespaces -l auto-agent.io/demo=true -o jsonpath='{.items[*].metadata.name}'); do
		echo "Removing demo namespace $ns..."
		k delete namespace "$ns" --wait=false
	done
fi

k delete -f "$SCRIPT_DIR/00-namespace.yaml" --ignore-not-found --wait=false
echo "Teardown complete."
