#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
TEST_DIR="$ROOT_DIR/test"

echo "=========================================="
echo " Auto-Agent KIND Test Setup"
echo "=========================================="

# 1. Create KIND cluster
echo ""
echo "[1/5] Creating KIND cluster..."
if kind get clusters 2>/dev/null | grep -q "auto-agent-test"; then
    echo "  Cluster already exists, skipping creation"
else
    kind create cluster --config "$TEST_DIR/kind-config.yaml"
fi
kubectl cluster-info --context kind-auto-agent-test

# 2. Build the agent image
echo ""
echo "[2/5] Building auto-agent Docker image..."
cd "$ROOT_DIR"
docker build -t auto-agent:test .

# 3. Load image into KIND
echo ""
echo "[3/5] Loading image into KIND cluster..."
kind load docker-image auto-agent:test --name auto-agent-test

# 4. Install CRDs
echo ""
echo "[4/5] Installing CRDs..."
kubectl apply -f "$ROOT_DIR/charts/auto-agent/crds/"

# 5. Deploy agent via Helm
echo ""
echo "[5/5] Deploying auto-agent via Helm..."
helm upgrade --install auto-agent "$ROOT_DIR/charts/auto-agent" \
    -n kube-system --create-namespace \
    -f "$TEST_DIR/manifests/agent-values-test.yaml"

# Wait for agent to be ready
echo ""
echo "Waiting for auto-agent pods to be ready..."
kubectl rollout status daemonset/auto-agent -n kube-system --timeout=120s

# Create test namespaces
echo ""
echo "Creating test namespaces..."
kubectl apply -f "$TEST_DIR/manifests/00-namespace.yaml"

echo ""
echo "=========================================="
echo " Setup Complete!"
echo "=========================================="
echo ""
echo "Dashboard: http://localhost:8080  (after port-forward)"
echo "  kubectl port-forward -n kube-system svc/auto-agent 8080:8080"
echo ""
echo "Agent logs:"
echo "  kubectl logs -n kube-system -l app=auto-agent -f"
echo ""
echo "Run tests:"
echo "  ./test/scripts/run-tests.sh"
echo ""
