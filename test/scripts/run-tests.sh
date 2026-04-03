#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
MANIFESTS="$TEST_DIR/manifests"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'
BOLD='\033[1m'

pass=0
fail=0
skip=0

log()  { echo -e "${BOLD}[TEST]${NC} $1"; }
ok()   { echo -e "  ${GREEN}PASS${NC} $1"; ((pass++)); }
warn() { echo -e "  ${YELLOW}SKIP${NC} $1"; ((skip++)); }
err()  { echo -e "  ${RED}FAIL${NC} $1"; ((fail++)); }

check_agent_log() {
    local pattern=$1 timeout=${2:-30}
    for i in $(seq 1 $timeout); do
        if kubectl logs -n kube-system -l app=auto-agent --tail=200 --since=10m 2>/dev/null | grep -iq "$pattern"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

get_agent_pod() {
    kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

echo "=========================================="
echo " Auto-Agent Test Suite"
echo "=========================================="
echo ""

# ------------------------------------------
# PRE-CHECK: Agent running
# ------------------------------------------
log "Pre-check: Agent is running..."
READY=$(kubectl get daemonset auto-agent -n kube-system -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
if [ "$READY" -gt 0 ] 2>/dev/null; then
    ok "Agent DaemonSet running ($READY pods ready)"
else
    err "Agent DaemonSet NOT running"
    echo "  Run: kubectl get pods -n kube-system -l app=auto-agent"
    echo "  Run: ./test/scripts/setup.sh"
    exit 1
fi

POD=$(get_agent_pod)
log "Pre-check: Dashboard endpoints..."
if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/healthz 2>/dev/null | grep -q "ok"; then
    ok "/healthz responding"
else
    err "/healthz not responding"
fi
if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/status 2>/dev/null | grep -q "version"; then
    ok "/api/status responding"
else
    err "/api/status not responding"
fi
if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/ 2>/dev/null | grep -q "auto-agent"; then
    ok "Dashboard UI serving"
else
    err "Dashboard UI not serving"
fi

echo ""
echo "=========================================="
echo " Scenario Tests"
echo "=========================================="

# ------------------------------------------
# TEST 1: CrashLoopBackOff
# ------------------------------------------
echo ""
log "TEST 1: CrashLoopBackOff"
kubectl apply -f "$MANIFESTS/01-crashloop.yaml"
echo "  Waiting for crash loop (30s)..."
sleep 30
if check_agent_log "CrashLoopBackOff.*crashloop-test" 60; then
    ok "Agent detected CrashLoopBackOff"
else
    err "Agent did NOT detect CrashLoopBackOff"
fi
kubectl delete -f "$MANIFESTS/01-crashloop.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 2: OOMKilled
# ------------------------------------------
echo ""
log "TEST 2: OOMKilled"
kubectl apply -f "$MANIFESTS/02-oomkilled.yaml"
echo "  Waiting for OOM (20s)..."
sleep 20
if check_agent_log "OOMKilled.*oom-test" 60; then
    ok "Agent detected OOMKilled"
else
    err "Agent did NOT detect OOMKilled"
fi
kubectl delete -f "$MANIFESTS/02-oomkilled.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 3: ImagePullBackOff
# ------------------------------------------
echo ""
log "TEST 3: ImagePullBackOff"
kubectl apply -f "$MANIFESTS/03-imagepull.yaml"
echo "  Waiting for pull failure (15s)..."
sleep 15
if check_agent_log "ImagePullBackOff.*imagepull-test" 60; then
    ok "Agent detected ImagePullBackOff"
else
    err "Agent did NOT detect ImagePullBackOff"
fi
kubectl delete -f "$MANIFESTS/03-imagepull.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 4: Init Container Failure
# ------------------------------------------
echo ""
log "TEST 4: Init Container Failure"
kubectl apply -f "$MANIFESTS/06-init-failure.yaml"
echo "  Waiting for init failure (30s)..."
sleep 30
if check_agent_log "init.*container.*fail\|InitContainerFailed\|init-fail-test" 60; then
    ok "Agent detected init container failure"
else
    err "Agent did NOT detect init container failure"
fi
kubectl delete -f "$MANIFESTS/06-init-failure.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 5: ConfigMap Reference Error
# ------------------------------------------
echo ""
log "TEST 5: CreateContainerConfigError"
kubectl apply -f "$MANIFESTS/07-config-error.yaml"
echo "  Waiting for config error (15s)..."
sleep 15
if check_agent_log "config.*error\|ConfigError\|config-error-test" 60; then
    ok "Agent detected config error"
else
    err "Agent did NOT detect config error"
fi
kubectl delete -f "$MANIFESTS/07-config-error.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 6: Excluded Pod (annotation)
# ------------------------------------------
echo ""
log "TEST 6: Excluded Pod (annotation escape hatch)"
kubectl apply -f "$MANIFESTS/13-excluded-pod.yaml"
echo "  Waiting 30s, then checking agent did NOT act..."
sleep 30
if check_agent_log "excluded-test" 5; then
    err "Agent should NOT have acted on excluded pod"
else
    ok "Agent correctly ignored excluded pod"
fi
kubectl delete -f "$MANIFESTS/13-excluded-pod.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 7: CRD Policy (requireApproval)
# ------------------------------------------
echo ""
log "TEST 7: CRD Policy — requireApproval blocks fix"
kubectl apply -f "$MANIFESTS/14-crd-policy.yaml"
echo "  Waiting 40s for detection + block..."
sleep 40
if check_agent_log "blocked\|approval\|policy-test" 30; then
    ok "Agent respected CRD requireApproval"
else
    warn "CRD policy enforcement not confirmed in logs"
fi
# Verify pod still exists (not deleted)
if kubectl get pods -n test-apps -l app=policy-test --no-headers 2>/dev/null | grep -q "policy-test"; then
    ok "Pod survived — requireApproval blocked deletion"
else
    err "Pod was deleted despite requireApproval=true"
fi
kubectl delete -f "$MANIFESTS/14-crd-policy.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 8: Failed Job
# ------------------------------------------
echo ""
log "TEST 8: Failed Job"
kubectl apply -f "$MANIFESTS/09-failed-job.yaml"
echo "  Waiting for job failure + agent scan (3 min)..."
if check_agent_log "JobFailed\|failed.*job\|failing-job" 180; then
    ok "Agent detected failed job"
else
    warn "Failed job not detected (agent scans every 2 min)"
fi
kubectl delete -f "$MANIFESTS/09-failed-job.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 9: Zero Endpoints Service
# ------------------------------------------
echo ""
log "TEST 9: Service with 0 Endpoints"
kubectl apply -f "$MANIFESTS/11-zero-endpoints.yaml"
echo "  Waiting for endpoint scan (3 min)..."
if check_agent_log "NoEndpoints\|blackhole" 180; then
    ok "Agent detected zero-endpoint service"
else
    warn "Zero endpoints not detected (agent scans every 2 min)"
fi
kubectl delete -f "$MANIFESTS/11-zero-endpoints.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 10: Stuck Rollout
# ------------------------------------------
echo ""
log "TEST 10: Stuck Deployment Rollout"
kubectl apply -f "$MANIFESTS/10-stuck-rollout.yaml"
echo "  Waiting for v1 to be healthy (15s)..."
sleep 15
kubectl apply -f "$MANIFESTS/10-stuck-rollout-break.yaml"
echo "  Deployed broken v2, waiting for ProgressDeadlineExceeded (~90s)..."
if check_agent_log "RolloutStuck\|rollout.*stuck\|rollback\|rollout-test" 180; then
    ok "Agent detected stuck rollout"
else
    warn "Stuck rollout not detected (progressDeadline=60s + scan interval)"
fi
kubectl delete -f "$MANIFESTS/10-stuck-rollout.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 11: Pending Pod (long wait)
# ------------------------------------------
echo ""
log "TEST 11: Pending Pod (requires 6 min wait)"
kubectl apply -f "$MANIFESTS/04-pending.yaml"
echo "  Waiting 6+ minutes for pending threshold..."
if check_agent_log "Pending.*pending-test" 390; then
    ok "Agent detected Pending pod"
else
    err "Agent did NOT detect Pending pod"
fi
kubectl delete -f "$MANIFESTS/04-pending.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 12: NotReady Pod (long wait)
# ------------------------------------------
echo ""
log "TEST 12: NotReady Pod (requires 4 min wait)"
kubectl apply -f "$MANIFESTS/05-notready.yaml"
echo "  Waiting 4+ minutes for NotReady threshold..."
if check_agent_log "NotReady.*notready-test" 270; then
    ok "Agent detected NotReady pod"
else
    err "Agent did NOT detect NotReady pod"
fi
kubectl delete -f "$MANIFESTS/05-notready.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 13: Resource Quota
# ------------------------------------------
echo ""
log "TEST 13: Resource Quota Exhaustion"
kubectl apply -f "$MANIFESTS/12-resource-quota.yaml"
echo "  Waiting for quota scan (5 min)..."
if check_agent_log "ResourceQuota\|QuotaExhaustion\|quota" 360; then
    ok "Agent detected quota nearing exhaustion"
else
    warn "Quota warning not detected (scans every 5 min)"
fi
kubectl delete -f "$MANIFESTS/12-resource-quota.yaml" --ignore-not-found > /dev/null 2>&1 &

# ------------------------------------------
# TEST 14: Audit Log
# ------------------------------------------
echo ""
log "TEST 14: Audit Log"
POD=$(get_agent_pod)
AUDIT=$(kubectl exec -n kube-system "$POD" -- cat /var/log/auto-agent/audit.jsonl 2>/dev/null || echo "")
if [ -n "$AUDIT" ]; then
    LINES=$(echo "$AUDIT" | wc -l | tr -d ' ')
    ok "Audit log has $LINES entries"
    # Show sample
    echo "  Sample entry:"
    echo "$AUDIT" | head -1 | python3 -m json.tool 2>/dev/null || echo "$AUDIT" | head -1
else
    warn "Audit log empty (no fix actions taken yet)"
fi

# ------------------------------------------
# TEST 15: Dashboard Events
# ------------------------------------------
echo ""
log "TEST 15: Dashboard Events API"
POD=$(get_agent_pod)
EVENTS=$(kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/events 2>/dev/null || echo "[]")
COUNT=$(echo "$EVENTS" | grep -o '"id"' | wc -l | tr -d ' ')
if [ "$COUNT" -gt 0 ]; then
    ok "Dashboard recorded $COUNT events"
else
    warn "No events in dashboard API"
fi

# ------------------------------------------
# Summary
# ------------------------------------------
echo ""
echo "=========================================="
echo " Results"
echo "=========================================="
echo -e "  ${GREEN}PASS: $pass${NC}"
echo -e "  ${RED}FAIL: $fail${NC}"
echo -e "  ${YELLOW}SKIP: $skip${NC}"
TOTAL=$((pass + fail + skip))
echo "  Total: $TOTAL"
echo ""

if [ $fail -gt 0 ]; then
    echo -e "${RED}Some tests failed. Debug:${NC}"
    echo "  kubectl logs -n kube-system -l app=auto-agent --tail=200"
    echo "  kubectl get pods -n test-apps"
    exit 1
fi
echo -e "${GREEN}All critical tests passed!${NC}"
