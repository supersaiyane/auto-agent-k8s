#!/bin/bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Removing test apps..."
kubectl delete -f "$DIR/app1-payment-service.yaml" --ignore-not-found 2>/dev/null || true
kubectl delete -f "$DIR/app2-order-service.yaml" --ignore-not-found 2>/dev/null || true
kubectl delete -f "$DIR/app3-inventory-service.yaml" --ignore-not-found 2>/dev/null || true
kubectl delete ns test1 test2 --ignore-not-found --timeout=30s 2>/dev/null || true

echo "Test apps removed."
