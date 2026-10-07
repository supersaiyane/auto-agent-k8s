#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=========================================="
echo " Auto-Agent Full Deployment"
echo "=========================================="

# 1. Verify cluster
echo ""
echo "[1/6] Verifying cluster..."
kubectl cluster-info > /dev/null 2>&1 || { echo "ERROR: kubectl cannot connect"; exit 1; }
echo "  Cluster OK"
kubectl get nodes --no-headers | awk '{printf "  Node: %s (%s)\n", $1, $2}'

# 2. Build image
echo ""
echo "[2/6] Building auto-agent image..."
cd "$ROOT_DIR"
docker build -t auto-agent:latest .

# 3. Load image into cluster
echo ""
echo "[3/6] Loading image into cluster nodes..."
RUNTIME=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.containerRuntimeVersion}' 2>/dev/null || echo "")
if echo "$RUNTIME" | grep -q "containerd"; then
    while IFS= read -r NODE; do
        [ -z "$NODE" ] && continue
        echo "  Loading into $NODE..."
        docker save auto-agent:latest | docker exec -i "$NODE" ctr -n k8s.io images import - 2>/dev/null || \
            echo "  WARNING: Failed for $NODE"
    done < <(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
elif command -v kind &> /dev/null && kind get clusters 2>/dev/null | grep -q .; then
    kind load docker-image auto-agent:latest --name "$(kind get clusters | head -1)"
elif command -v minikube &> /dev/null; then
    minikube image load auto-agent:latest
else
    echo "  Image built locally. If using remote cluster, push to registry."
fi

# 4. Install cost provider (Kubecost or OpenCost) if configured
echo ""
echo "[4/6] Checking cost provider..."
# Optional: COST_PROVIDER=kubecost|opencost|manual ./deployment/deploy.sh
COST_PROVIDER="${COST_PROVIDER:-}"

install_kubecost() {
    echo "  Installing Kubecost (free tier)..."
    if ! command -v helm &> /dev/null; then
        echo "  ERROR: helm required to install Kubecost"
        return 1
    fi
    # Check if already installed
    if kubectl get ns kubecost > /dev/null 2>&1 && kubectl get pods -n kubecost -l app=cost-analyzer --no-headers 2>/dev/null | grep -q "Running"; then
        echo "  Kubecost already running"
    else
        helm repo add kubecost https://kubecost.github.io/cost-analyzer/ > /dev/null 2>&1 || true
        helm repo update > /dev/null 2>&1
        helm upgrade --install kubecost kubecost/cost-analyzer \
            -n kubecost --create-namespace \
            --set kubecostProductConfigs.clusterName="auto-agent" \
            --set prometheus.server.persistentVolume.enabled=false \
            --set prometheus.alertmanager.enabled=false \
            --set grafana.enabled=false \
            --wait --timeout=300s 2>&1 | sed 's/^/  /'
        echo "  Kubecost installed"
    fi
    # Set the URL in config
    KUBECOST_SVC="http://kubecost-cost-analyzer.kubecost:9090"
    kubectl patch cm auto-agent-config -n auto-agent --type merge \
        -p "{\"data\":{\"KUBECOST_URL\":\"$KUBECOST_SVC\"}}" 2>/dev/null || true
    echo "  KUBECOST_URL=$KUBECOST_SVC"
}

install_opencost() {
    echo "  Installing OpenCost..."
    if ! command -v helm &> /dev/null; then
        echo "  ERROR: helm required to install OpenCost"
        return 1
    fi
    if kubectl get ns opencost > /dev/null 2>&1 && kubectl get pods -n opencost -l app.kubernetes.io/name=opencost --no-headers 2>/dev/null | grep -q "Running"; then
        echo "  OpenCost already running"
    else
        helm repo add opencost https://opencost.github.io/opencost-helm-chart > /dev/null 2>&1 || true
        helm repo update > /dev/null 2>&1
        helm upgrade --install opencost opencost/opencost \
            -n opencost --create-namespace \
            --wait --timeout=300s 2>&1 | sed 's/^/  /'
        echo "  OpenCost installed"
    fi
    OPENCOST_SVC="http://opencost.opencost:9003"
    kubectl patch cm auto-agent-config -n auto-agent --type merge \
        -p "{\"data\":{\"OPENCOST_URL\":\"$OPENCOST_SVC\"}}" 2>/dev/null || true
    echo "  OPENCOST_URL=$OPENCOST_SVC"
}

case "$COST_PROVIDER" in
    kubecost)
        install_kubecost
        ;;
    opencost)
        install_opencost
        ;;
    manual)
        echo "  Using manual pricing"
        read -p "  CPU price ($/vCPU/hour) [0.05]: " CPU_PRICE
        read -p "  Memory price ($/GiB/hour) [0.005]: " MEM_PRICE
        read -p "  Currency [USD]: " CURRENCY
        CPU_PRICE=${CPU_PRICE:-0.05}
        MEM_PRICE=${MEM_PRICE:-0.005}
        CURRENCY=${CURRENCY:-USD}
        # Will patch after applying config
        ;;
    *)
        echo "  Using built-in instance-type pricing (40+ AWS/GCP/Azure types)"
        echo "  Run with COST_PROVIDER=kubecost or COST_PROVIDER=opencost for real costs"
        ;;
esac

# 5. Apply manifests
echo ""
echo "[5/6] Deploying auto-agent..."
kubectl apply -f "$SCRIPT_DIR/00-namespace.yaml"
kubectl apply -f "$SCRIPT_DIR/01-crds.yaml"
# 02-rbac.yaml holds a write Role per allowlisted namespace (make manifests);
# a Role needs its namespace to exist first.
for ns in default test1 test2; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
done
kubectl apply -f "$SCRIPT_DIR/02-rbac.yaml"
kubectl apply -f "$SCRIPT_DIR/03-config.yaml"
# The Secret is created once and never applied over (ISS-048).
sh "$SCRIPT_DIR/ensure-secret.sh" auto-agent
# Node agents (DaemonSet) and the controller (Deployment), ADR-001.
kubectl apply -f "$SCRIPT_DIR/04-agent.yaml"

# Patch config with cost provider settings
case "$COST_PROVIDER" in
    kubecost)
        kubectl patch cm auto-agent-config -n auto-agent --type merge \
            -p '{"data":{"KUBECOST_URL":"http://kubecost-cost-analyzer.kubecost:9090"}}' > /dev/null 2>&1
        ;;
    opencost)
        kubectl patch cm auto-agent-config -n auto-agent --type merge \
            -p '{"data":{"OPENCOST_URL":"http://opencost.opencost:9003"}}' > /dev/null 2>&1
        ;;
    manual)
        kubectl patch cm auto-agent-config -n auto-agent --type merge \
            -p "{\"data\":{\"COST_CPU_PER_HOUR\":\"$CPU_PRICE\",\"COST_MEM_PER_GIB_HOUR\":\"$MEM_PRICE\",\"COST_CURRENCY\":\"$CURRENCY\"}}" > /dev/null 2>&1
        ;;
esac

# 6. Wait for ready
echo ""
echo "[6/6] Waiting for agent pods..."
kubectl rollout status daemonset/auto-agent -n auto-agent --timeout=120s
kubectl rollout status deployment/auto-agent-controller -n auto-agent --timeout=120s

echo ""
kubectl get pods -n auto-agent -o wide

# Dashboard: served by the controllers behind a ClusterIP Service (ADR-001).
echo ""
echo "Dashboard access..."
lsof -ti:8080 | xargs kill -9 2>/dev/null || true
sleep 1
kubectl port-forward -n auto-agent svc/auto-agent 8080:8080 > /dev/null 2>&1 &
sleep 2
DASHBOARD_URL=""
if curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    DASHBOARD_URL="http://localhost:8080"
fi
if [ -n "$DASHBOARD_URL" ]; then
    echo "  Dashboard: $DASHBOARD_URL"
else
    echo "  Dashboard not reachable yet. Try:"
    echo "    kubectl port-forward -n auto-agent svc/auto-agent 8080:8080"
fi

echo ""
echo "=========================================="
echo " Deployment Complete!"
echo "=========================================="
echo ""
echo "  Dashboard:     http://localhost:8080 (port-forward to svc/auto-agent)"
echo "  Token:         kubectl get secret auto-agent-secrets -n auto-agent -o jsonpath='{.data.DASHBOARD_TOKEN}' | base64 -d"
echo "  Node agents:   kubectl logs -n auto-agent -l app=auto-agent -f"
echo "  Controller:    kubectl logs -n auto-agent -l app=auto-agent-controller -f"
echo "  Agent mode:    $(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.AUTO_MODE}')"
echo "  Watching:      $(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.NAMESPACE_ALLOWLIST}')"
echo "  Cost provider: $(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.COST_PROVIDER}' 2>/dev/null || echo 'default')"
echo ""
echo "  To change config:"
echo "    kubectl edit cm auto-agent-config -n auto-agent"
echo ""
echo "  To tear down:"
echo "    ./deployment/teardown.sh"
echo ""
