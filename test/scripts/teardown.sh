#!/bin/bash
set -euo pipefail

echo "=========================================="
echo " Auto-Agent Test Teardown"
echo "=========================================="

echo ""
echo "Cleaning up test workloads..."
kubectl delete ns test-apps test-apps-2 --ignore-not-found --timeout=60s

echo ""
echo "Uninstalling auto-agent..."
helm uninstall auto-agent -n kube-system 2>/dev/null || true

echo ""
echo "Deleting CRDs..."
kubectl delete crd autoremediationpolicies.autoagent.io --ignore-not-found

echo ""
echo "Deleting KIND cluster..."
kind delete cluster --name auto-agent-test

echo ""
echo "Teardown complete."
