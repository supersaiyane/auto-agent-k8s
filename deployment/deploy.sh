#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=========================================="
echo " Auto-Agent Full Deployment"
echo "=========================================="

# 1. Verify cluster
echo ""
echo "[1/5] Verifying cluster..."
kubectl cluster-info > /dev/null 2>&1 || { echo "ERROR: kubectl cannot connect"; exit 1; }
echo "  Cluster OK"
kubectl get nodes --no-headers | awk '{printf "  Node: %s (%s)\n", $1, $2}'

# 2. Build image
echo ""
echo "[2/5] Building auto-agent image..."
cd "$ROOT_DIR"
docker build -t auto-agent:latest .

# 3. Load image into cluster
echo ""
echo "[3/5] Loading image into cluster nodes..."
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

# 4. Apply manifests
echo ""
echo "[4/5] Deploying auto-agent..."
kubectl apply -f "$SCRIPT_DIR/00-namespace.yaml"
kubectl apply -f "$SCRIPT_DIR/01-crds.yaml"
kubectl apply -f "$SCRIPT_DIR/02-rbac.yaml"
kubectl apply -f "$SCRIPT_DIR/03-config.yaml"
kubectl apply -f "$SCRIPT_DIR/04-daemonset.yaml"

# 5. Wait for ready
echo ""
echo "[5/5] Waiting for agent pods..."
kubectl rollout status daemonset/auto-agent -n auto-agent --timeout=120s

echo ""
kubectl get pods -n auto-agent -o wide

# Port-forward
echo ""
echo "Starting dashboard port-forward..."
lsof -ti:8080 | xargs kill -9 2>/dev/null || true
sleep 1
kubectl port-forward -n auto-agent svc/auto-agent 8080:8080 > /dev/null 2>&1 &
sleep 2

if curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    echo "  Dashboard: http://localhost:8080"
else
    echo "  Port-forward may need a moment. Try: kubectl port-forward -n auto-agent svc/auto-agent 8080:8080"
fi

# Open browser
if command -v open &> /dev/null; then
    open http://localhost:8080
fi

echo ""
echo "=========================================="
echo " Deployment Complete!"
echo "=========================================="
echo ""
echo "  Dashboard:     http://localhost:8080"
echo "  Agent logs:    kubectl logs -n auto-agent -l app=auto-agent -f"
echo "  Agent mode:    $(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.AUTO_MODE}')"
echo "  Watching:      $(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.NAMESPACE_ALLOWLIST}')"
echo ""
echo "  To change config:"
echo "    kubectl edit cm auto-agent-config -n auto-agent"
echo ""
echo "  To tear down:"
echo "    ./deployment/teardown.sh"
echo ""
