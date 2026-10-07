#!/bin/sh
# Creates the agent's Secret once and never overwrites it (ISS-048). Values
# an operator patched in later survive every re-run. Generates the dashboard
# token and the node-to-controller token (ADR-001) when they are missing.
#
# Usage: ensure-secret.sh [namespace]
set -eu

NS="${1:-auto-agent}"
NAME="auto-agent-secrets"
KUBECTL="${KUBECTL:-kubectl}" # e.g. "kubectl --context kind-x"

token() {
	# 32 random bytes as hex, without needing openssl in the image.
	head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
}

if ! $KUBECTL get secret "$NAME" -n "$NS" >/dev/null 2>&1; then
	$KUBECTL create secret generic "$NAME" -n "$NS" \
		--from-literal=DASHBOARD_TOKEN="$(token)" \
		--from-literal=INTERNAL_TOKEN="$(token)" \
		--from-literal=SLACK_WEBHOOK_URL= \
		--from-literal=SLACK_SIGNING_SECRET= \
		--from-literal=LLM_API_KEY= \
		--from-literal=GIT_TOKEN= \
		--from-literal=GITHUB_TOKEN= \
		--from-literal=JIRA_TOKEN= \
		--from-literal=JIRA_EMAIL= \
		--from-literal=PAGERDUTY_ROUTING_KEY= \
		--from-literal=OPSGENIE_API_KEY= \
		--from-literal=SMTP_PASS=
	echo "created secret $NS/$NAME with generated DASHBOARD_TOKEN and INTERNAL_TOKEN"
	exit 0
fi

# An older Secret, from before the controller existed, lacks INTERNAL_TOKEN.
if [ -z "$($KUBECTL get secret "$NAME" -n "$NS" -o jsonpath='{.data.INTERNAL_TOKEN}')" ]; then
	$KUBECTL patch secret "$NAME" -n "$NS" --type merge \
		-p "{\"stringData\":{\"INTERNAL_TOKEN\":\"$(token)\"}}"
	echo "added a generated INTERNAL_TOKEN to $NS/$NAME"
	exit 0
fi

echo "secret $NS/$NAME exists; left unchanged"
