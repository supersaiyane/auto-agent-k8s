#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
TEST_DIR="$ROOT_DIR/test"

echo "=========================================="
echo " Auto-Agent Test Setup"
echo "=========================================="

# 1. Verify kubectl works
echo ""
echo "[1/5] Verifying cluster access..."
if ! kubectl cluster-info > /dev/null 2>&1; then
    echo "ERROR: kubectl cannot connect to cluster"
    echo "  Make sure your kubeconfig is set and cluster is reachable"
    exit 1
fi
echo "  Cluster: $(kubectl cluster-info 2>/dev/null | head -1)"
echo "  Nodes:"
kubectl get nodes -o wide --no-headers | awk '{printf "    %s  %s  %s\n", $1, $2, $6}'

# 2. Build the agent image
echo ""
echo "[2/5] Building auto-agent Docker image..."
cd "$ROOT_DIR"
docker build -t auto-agent:test .

# 3. Load image into cluster
echo ""
echo "[3/5] Loading image into cluster..."
# Detect cluster type and load accordingly
if command -v kind &> /dev/null && kind get clusters 2>/dev/null | grep -q .; then
    CLUSTER_NAME=$(kind get clusters 2>/dev/null | head -1)
    echo "  Detected KIND cluster: $CLUSTER_NAME"
    kind load docker-image auto-agent:test --name "$CLUSTER_NAME"
elif command -v minikube &> /dev/null && minikube status 2>/dev/null | grep -q "Running"; then
    echo "  Detected Minikube — loading via minikube image load"
    minikube image load auto-agent:test
elif command -v k3d &> /dev/null && k3d cluster list 2>/dev/null | grep -q .; then
    CLUSTER_NAME=$(k3d cluster list -o json 2>/dev/null | grep -o '"name":"[^"]*"' | head -1 | cut -d'"' -f4)
    echo "  Detected k3d cluster: $CLUSTER_NAME"
    k3d image import auto-agent:test -c "$CLUSTER_NAME"
else
    echo "  WARNING: Could not detect cluster type (KIND/Minikube/k3d)"
    echo "  If using a remote cluster, push auto-agent:test to a registry"
    echo "  and update test/manifests/agent-values-test.yaml with:"
    echo "    image.repository: <your-registry>/auto-agent"
    echo "    image.pullPolicy: Always"
    echo ""
    echo "  For Docker Desktop Kubernetes, the image is already available."
fi

# 4. Install CRDs
echo ""
echo "[4/5] Installing CRDs..."
kubectl apply -f "$ROOT_DIR/charts/auto-agent/crds/"

# 5. Deploy agent via Helm
echo ""
echo "[5/5] Deploying auto-agent via Helm..."
if ! command -v helm &> /dev/null; then
    echo "  ERROR: helm not found. Install helm first:"
    echo "    brew install helm  (macOS)"
    echo "    curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash"
    exit 1
fi

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

# Show agent status
echo ""
echo "Agent pods:"
kubectl get pods -n kube-system -l app=auto-agent -o wide

echo ""
echo "=========================================="
echo " Setup Complete!"
echo "=========================================="
echo ""
echo "Dashboard:"
echo "  kubectl port-forward -n kube-system svc/auto-agent 8080:8080"
echo "  Then open http://localhost:8080"
echo ""
echo "Agent logs:"
echo "  kubectl logs -n kube-system -l app=auto-agent -f"
echo ""
echo "Run tests:"
echo "  ./test/scripts/run-tests.sh"
echo ""
