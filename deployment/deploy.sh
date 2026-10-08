#!/bin/bash
# Installs auto-agent from the manifests in this directory, which are
# generated from the Helm chart (make manifests). For a real cluster prefer
# the chart itself; this script is for kind, minikube and quick trials.
#
# It never kills a local process, never installs anything you did not ask
# for, and never overwrites the Secret (PLAN-002 11.4, ISS-048, ISS-049).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$SCRIPT_DIR/lib.sh"

NS=auto-agent
ALLOWLIST_NAMESPACES="test1 test2 chaos" # must match RAW_VALUES in the Makefile
IMAGE="auto-agent:latest"
BUILD=1
COST=""
CPU_PRICE="" MEM_PRICE="" CURRENCY=""
FORWARD=1

usage() {
	cat <<'USAGE'
Usage: deployment/deploy.sh [flags]

  --context NAME          kubectl context to use (default: current context)
  --image REF             use an image already pushed to a registry, e.g.
                          registry.example.com/auto-agent@sha256:...; skips the build
  --no-build              do not build; load the existing auto-agent:latest
  --with-opencost         also install OpenCost with Helm (labelled as installed by this script)
  --with-kubecost         also install Kubecost with Helm (labelled as installed by this script)
  --cost-manual CPU,MEM,CURRENCY
                          manual prices, e.g. 0.05,0.005,USD
  --no-port-forward       do not start a dashboard port-forward on localhost:8080
  -h, --help              this help
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
	--context) KUBE_CONTEXT="$2"; shift 2 ;;
	--image) IMAGE="$2"; BUILD=0; shift 2 ;;
	--no-build) BUILD=0; shift ;;
	--with-opencost) COST=opencost; shift ;;
	--with-kubecost) COST=kubecost; shift ;;
	--cost-manual) COST=manual; IFS=, read -r CPU_PRICE MEM_PRICE CURRENCY <<<"$2"; shift 2 ;;
	--no-port-forward) FORWARD=0; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
	esac
done
export KUBE_CONTEXT="${KUBE_CONTEXT:-}"

# Inputs end up in manifests and JSON patches, so they are checked first.
[[ "$IMAGE" =~ ^[A-Za-z0-9./:@_-]+$ ]] || { echo "invalid --image: $IMAGE" >&2; exit 2; }
if [ "$COST" = manual ]; then
	[[ "$CPU_PRICE" =~ ^[0-9]+(\.[0-9]+)?$ && "$MEM_PRICE" =~ ^[0-9]+(\.[0-9]+)?$ && "$CURRENCY" =~ ^[A-Z]{3}$ ]] ||
		{ echo "--cost-manual wants CPU,MEM,CURRENCY such as 0.05,0.005,USD" >&2; exit 2; }
fi

step() { printf '\n[%s] %s\n' "$1" "$2"; }

step 1/6 "Checking the cluster"
k cluster-info >/dev/null
CTX="${KUBE_CONTEXT:-$(kubectl config current-context)}"
echo "  context $CTX"

step 2/6 "Image"
if [ "$IMAGE" = "auto-agent:latest" ]; then
	if [ "$BUILD" = 1 ]; then
		docker build -t auto-agent:latest "$ROOT_DIR"
	fi
	case "$CTX" in
	kind-*) kind load docker-image auto-agent:latest --name "${CTX#kind-}" ;;
	minikube) minikube image load auto-agent:latest ;;
	*)
		echo "  $CTX is neither kind nor minikube, so a local image cannot reach it." >&2
		echo "  Push the image and pass --image REGISTRY/auto-agent@sha256:..." >&2
		exit 1
		;;
	esac
else
	echo "  using $IMAGE"
fi

step 3/6 "Namespaces, CRD, RBAC and configuration"
k apply -f "$SCRIPT_DIR/00-namespace.yaml"
k apply -f "$SCRIPT_DIR/01-crds.yaml"
for ns in $ALLOWLIST_NAMESPACES; do
	# Write Roles need the namespace; ones this script creates are labelled
	# so teardown --delete-demo-namespaces can find them and nothing else.
	if ! k get namespace "$ns" >/dev/null 2>&1; then
		k create namespace "$ns"
		k label namespace "$ns" auto-agent.io/demo=true
	fi
done
k apply -f "$SCRIPT_DIR/02-rbac.yaml"
k apply -f "$SCRIPT_DIR/03-config.yaml"
KUBECTL="kubectl${KUBE_CONTEXT:+ --context $KUBE_CONTEXT}" sh "$SCRIPT_DIR/ensure-secret.sh" "$NS"

step 4/6 "Cost source"
install_cost_tool() { # name repo chart service-url key
	command -v helm >/dev/null || { echo "  helm is required for --with-$1" >&2; exit 1; }
	helm repo add "$1" "$2" >/dev/null
	helm repo update >/dev/null
	helm upgrade --install "$1" "$1/$3" ${KUBE_CONTEXT:+--kube-context "$KUBE_CONTEXT"} -n "$1" --create-namespace --wait --timeout=300s
	k label namespace "$1" auto-agent.io/installed-by=deploy.sh --overwrite
	k patch configmap auto-agent-config -n "$NS" --type merge -p "{\"data\":{\"$5\":\"$4\"}}"
}
case "$COST" in
opencost) install_cost_tool opencost https://opencost.github.io/opencost-helm-chart opencost http://opencost.opencost:9003 OPENCOST_URL ;;
kubecost) install_cost_tool kubecost https://kubecost.github.io/cost-analyzer/ cost-analyzer http://kubecost-cost-analyzer.kubecost:9090 KUBECOST_URL ;;
manual)
	k patch configmap auto-agent-config -n "$NS" --type merge \
		-p "{\"data\":{\"COST_CPU_PER_HOUR\":\"$CPU_PRICE\",\"COST_MEM_PER_GIB_HOUR\":\"$MEM_PRICE\",\"COST_CURRENCY\":\"$CURRENCY\"}}"
	;;
*) echo "  built-in instance prices (pass --with-opencost, --with-kubecost or --cost-manual for others)" ;;
esac

step 5/6 "Node agents and controller (ADR-001)"
if [ "$IMAGE" = "auto-agent:latest" ]; then
	k apply -f "$SCRIPT_DIR/04-agent.yaml"
else
	sed "s#image: \"auto-agent:latest\"#image: \"$IMAGE\"#" "$SCRIPT_DIR/04-agent.yaml" | k apply -f -
fi
k rollout status daemonset/auto-agent -n "$NS" --timeout=180s
k rollout status deployment/auto-agent-controller -n "$NS" --timeout=180s
k get pods -n "$NS" -o wide

step 6/6 "Dashboard"
if [ "$FORWARD" = 1 ] && dashboard_forward "$NS"; then
	echo "  http://localhost:8080 (port-forward; stop it with teardown.sh or: kill \$(cat $PF_PIDFILE))"
else
	echo "  kubectl port-forward -n $NS svc/auto-agent 8080:8080"
fi
cat <<DONE

Done.
  Token:        kubectl get secret auto-agent-secrets -n $NS -o jsonpath='{.data.DASHBOARD_TOKEN}' | base64 -d
  Node agents:  kubectl logs -n $NS -l app=auto-agent -f
  Controller:   kubectl logs -n $NS -l app=auto-agent-controller -f
  Mode:         $(k get configmap auto-agent-config -n "$NS" -o jsonpath='{.data.AUTO_MODE}')
  Watching:     $(k get configmap auto-agent-config -n "$NS" -o jsonpath='{.data.WATCH_NAMESPACES}') (empty: every non-system namespace)
  Fixing in:    $(k get configmap auto-agent-config -n "$NS" -o jsonpath='{.data.FIX_NAMESPACES}')
  Change it:    kubectl edit configmap auto-agent-config -n $NS   (reloads without a restart)
  Remove it:    deployment/teardown.sh
DONE
