#!/bin/bash
set -euo pipefail

echo "=========================================="
echo " Auto-Agent Teardown"
echo "=========================================="

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Removing DaemonSet..."
kubectl delete -f "$SCRIPT_DIR/04-daemonset.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing config..."
kubectl delete -f "$SCRIPT_DIR/03-config.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing RBAC..."
kubectl delete -f "$SCRIPT_DIR/02-rbac.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing CRDs..."
kubectl delete -f "$SCRIPT_DIR/01-crds.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing namespace..."
kubectl delete -f "$SCRIPT_DIR/00-namespace.yaml" --ignore-not-found --timeout=60s 2>/dev/null || true

# Remove Kubecost if installed
if kubectl get ns kubecost > /dev/null 2>&1; then
    echo "Removing Kubecost..."
    helm uninstall kubecost -n kubecost 2>/dev/null || true
    kubectl delete ns kubecost --ignore-not-found --timeout=60s 2>/dev/null || true
fi

# Remove OpenCost if installed
if kubectl get ns opencost > /dev/null 2>&1; then
    echo "Removing OpenCost..."
    helm uninstall opencost -n opencost 2>/dev/null || true
    kubectl delete ns opencost --ignore-not-found --timeout=60s 2>/dev/null || true
fi

# Kill port-forward
lsof -ti:8080 | xargs kill -9 2>/dev/null || true

echo ""
echo "Teardown complete."
