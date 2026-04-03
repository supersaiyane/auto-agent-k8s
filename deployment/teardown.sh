#!/bin/bash
set -euo pipefail

echo "=========================================="
echo " Auto-Agent Teardown"
echo "=========================================="

echo "Removing DaemonSet..."
kubectl delete -f "$(dirname "$0")/04-daemonset.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing config..."
kubectl delete -f "$(dirname "$0")/03-config.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing RBAC..."
kubectl delete -f "$(dirname "$0")/02-rbac.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing CRDs..."
kubectl delete -f "$(dirname "$0")/01-crds.yaml" --ignore-not-found 2>/dev/null || true

echo "Removing namespace..."
kubectl delete -f "$(dirname "$0")/00-namespace.yaml" --ignore-not-found --timeout=60s 2>/dev/null || true

# Kill port-forward
lsof -ti:8080 | xargs kill -9 2>/dev/null || true

echo ""
echo "Teardown complete."
