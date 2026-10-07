#!/bin/bash
# Removes the demo apps and the demo namespaces (label auto-agent.io/demo=true),
# never a namespace without that label (PLAN-002 11.4, ISS-049).
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Removing test apps..."
for f in app1-payment-service.yaml app2-order-service.yaml app3-inventory-service.yaml chaos-full.yaml; do
	kubectl delete -f "$DIR/$f" --ignore-not-found
done
for ns in $(kubectl get namespaces -l auto-agent.io/demo=true -o jsonpath='{.items[*].metadata.name}'); do
	echo "Removing demo namespace $ns..."
	kubectl delete namespace "$ns" --wait=false
done
echo "Test apps removed."
