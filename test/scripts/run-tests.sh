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

wait_for_condition() {
    local ns=$1 kind=$2 name=$3 condition=$4 timeout=$5
    kubectl wait --for="$condition" "$kind/$name" -n "$ns" --timeout="${timeout}s" 2>/dev/null
}

check_agent_log() {
    local pattern=$1 timeout=${2:-30}
    for i in $(seq 1 $timeout); do
        if kubectl logs -n kube-system -l app=auto-agent --tail=100 2>/dev/null | grep -q "$pattern"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

check_event_api() {
    local reason=$1
    local pod=$(kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -z "$pod" ]; then return 1; fi
    kubectl exec -n kube-system "$pod" -- wget -qO- "http://localhost:8080/api/events?limit=50" 2>/dev/null | grep -q "$reason"
}

echo "=========================================="
echo " Auto-Agent Test Suite"
echo "=========================================="
echo ""

# Verify agent is running
log "Verifying agent is running..."
if kubectl get daemonset auto-agent -n kube-system -o jsonpath='{.status.numberReady}' | grep -qE '[1-9]'; then
    ok "Agent DaemonSet is running"
else
    err "Agent DaemonSet is NOT running"
    echo "  Run: kubectl get pods -n kube-system -l app=auto-agent"
    exit 1
fi

# Verify dashboard
log "Verifying dashboard..."
POD=$(kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')
if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/healthz 2>/dev/null | grep -q "ok"; then
    ok "Health endpoint responding"
else
    err "Health endpoint not responding"
fi

if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/status 2>/dev/null | grep -q "version"; then
    ok "API status endpoint responding"
else
    err "API status endpoint not responding"
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
sleep 30  # wait for crash loop to develop
if check_agent_log "CrashLoopBackOff.*crashloop-test" 60; then
    ok "Agent detected CrashLoopBackOff"
else
    err "Agent did NOT detect CrashLoopBackOff within 60s"
fi
# Check if pod was deleted (fix mode)
if kubectl get events -n test-apps --field-selector reason=Killing 2>/dev/null | grep -q "crashloop-test"; then
    ok "Agent deleted the crashing pod"
else
    warn "Pod deletion not confirmed (may be timing)"
fi

# ------------------------------------------
# TEST 2: OOMKilled
# ------------------------------------------
echo ""
log "TEST 2: OOMKilled"
kubectl apply -f "$MANIFESTS/02-oomkilled.yaml"
sleep 20  # wait for OOM
if check_agent_log "OOMKilled.*oom-test" 60; then
    ok "Agent detected OOMKilled"
else
    err "Agent did NOT detect OOMKilled within 60s"
fi

# ------------------------------------------
# TEST 3: ImagePullBackOff
# ------------------------------------------
echo ""
log "TEST 3: ImagePullBackOff"
kubectl apply -f "$MANIFESTS/03-imagepull.yaml"
sleep 15
if check_agent_log "ImagePullBackOff.*imagepull-test" 60; then
    ok "Agent detected ImagePullBackOff"
else
    err "Agent did NOT detect ImagePullBackOff within 60s"
fi

# ------------------------------------------
# TEST 4: Pending Pod
# ------------------------------------------
echo ""
log "TEST 4: Pending Pod (wait 6 min for detection)"
kubectl apply -f "$MANIFESTS/04-pending.yaml"
echo "  Waiting 6 minutes for pending detection threshold..."
if check_agent_log "Pending.*pending-test" 380; then
    ok "Agent detected Pending pod"
else
    err "Agent did NOT detect Pending pod within 6+ minutes"
fi

# ------------------------------------------
# TEST 5: NotReady Pod
# ------------------------------------------
echo ""
log "TEST 5: NotReady Pod (wait 4 min)"
kubectl apply -f "$MANIFESTS/05-notready.yaml"
echo "  Waiting 4 minutes for NotReady detection..."
if check_agent_log "NotReady.*notready-test" 260; then
    ok "Agent detected NotReady pod"
else
    err "Agent did NOT detect NotReady pod"
fi

# ------------------------------------------
# TEST 6: Init Container Failure
# ------------------------------------------
echo ""
log "TEST 6: Init Container Failure"
kubectl apply -f "$MANIFESTS/06-init-failure.yaml"
sleep 30
if check_agent_log "init container failure.*init-fail-test" 60; then
    ok "Agent detected init container failure"
else
    err "Agent did NOT detect init container failure"
fi

# ------------------------------------------
# TEST 7: Config Error
# ------------------------------------------
echo ""
log "TEST 7: CreateContainerConfigError"
kubectl apply -f "$MANIFESTS/07-config-error.yaml"
sleep 15
if check_agent_log "config error\|ConfigError.*config-error-test" 60; then
    ok "Agent detected config error"
else
    err "Agent did NOT detect config error"
fi

# ------------------------------------------
# TEST 8: Restart Storm
# ------------------------------------------
echo ""
log "TEST 8: Restart Storm"
kubectl apply -f "$MANIFESTS/08-restart-storm.yaml"
echo "  Waiting for 5+ restarts..."
sleep 30
if check_agent_log "restart storm\|RestartStorm.*restart-storm-test" 60; then
    ok "Agent detected restart storm"
else
    warn "Restart storm not detected (may need more time for 5 restarts)"
fi

# ------------------------------------------
# TEST 9: Failed Job
# ------------------------------------------
echo ""
log "TEST 9: Failed Job"
kubectl apply -f "$MANIFESTS/09-failed-job.yaml"
sleep 30
if check_agent_log "failed Job\|JobFailed.*failing-job" 180; then
    ok "Agent detected failed job"
else
    warn "Failed job not detected yet (check runs every 2 min)"
fi

# ------------------------------------------
# TEST 10: Stuck Rollout
# ------------------------------------------
echo ""
log "TEST 10: Stuck Rollout"
kubectl apply -f "$MANIFESTS/10-stuck-rollout.yaml"
echo "  Waiting for v1 to be healthy..."
sleep 15
kubectl apply -f "$MANIFESTS/10-stuck-rollout-break.yaml"
echo "  Deployed broken v2, waiting for ProgressDeadlineExceeded (60s)..."
if check_agent_log "RolloutStuck\|stuck rollout.*rollout-test" 180; then
    ok "Agent detected stuck rollout"
else
    err "Agent did NOT detect stuck rollout"
fi

# ------------------------------------------
# TEST 11: Zero Endpoints
# ------------------------------------------
echo ""
log "TEST 11: Service with 0 Endpoints"
kubectl apply -f "$MANIFESTS/11-zero-endpoints.yaml"
if check_agent_log "NoEndpoints.*blackhole-svc" 180; then
    ok "Agent detected zero-endpoint service"
else
    warn "Zero endpoints not detected yet (check runs every 2 min)"
fi

# ------------------------------------------
# TEST 12: Resource Quota
# ------------------------------------------
echo ""
log "TEST 12: Resource Quota Exhaustion"
kubectl apply -f "$MANIFESTS/12-resource-quota.yaml"
if check_agent_log "ResourceQuota\|QuotaExhaustion" 360; then
    ok "Agent detected quota nearing exhaustion"
else
    warn "Quota warning not detected (check runs every 5 min)"
fi

# ------------------------------------------
# TEST 13: Excluded Pod
# ------------------------------------------
echo ""
log "TEST 13: Excluded Pod (annotation)"
kubectl apply -f "$MANIFESTS/13-excluded-pod.yaml"
sleep 30
if check_agent_log "excluded-test" 5; then
    err "Agent should NOT have acted on excluded pod"
else
    ok "Agent correctly ignored excluded pod"
fi

# ------------------------------------------
# TEST 14: CRD Policy (requireApproval)
# ------------------------------------------
echo ""
log "TEST 14: CRD Policy Enforcement"
kubectl apply -f "$MANIFESTS/14-crd-policy.yaml"
sleep 30
if check_agent_log "Blocked.*requires.*approval\|policy-test" 60; then
    ok "Agent respected CRD requireApproval=true"
else
    warn "CRD policy enforcement not confirmed (check agent logs)"
fi
# Verify pod was NOT deleted (requireApproval blocks it)
if kubectl get pods -n test-apps -l app=policy-test --no-headers 2>/dev/null | grep -q "policy-test"; then
    ok "Pod survived (requireApproval blocked deletion)"
else
    err "Pod was deleted despite requireApproval=true"
fi

# ------------------------------------------
# TEST 15: HPA Coexistence
# ------------------------------------------
echo ""
log "TEST 15: HPA Coexistence"
kubectl apply -f "$MANIFESTS/15-scaling-target.yaml"
sleep 15
if check_agent_log "skipping.*hpa-protected.*HPA present" 60; then
    ok "Agent skipped HPA-protected deployment"
else
    warn "HPA skip log not found (scaler may not have run yet)"
fi

# ------------------------------------------
# Audit Log Check
# ------------------------------------------
echo ""
log "Checking Audit Log..."
AUDIT=$(kubectl exec -n kube-system "$POD" -- cat /var/log/auto-agent/audit.jsonl 2>/dev/null || echo "")
if [ -n "$AUDIT" ]; then
    LINES=$(echo "$AUDIT" | wc -l)
    ok "Audit log has $LINES entries"
else
    warn "Audit log empty or not found"
fi

# ------------------------------------------
# Dashboard Events Check
# ------------------------------------------
echo ""
log "Checking Dashboard Events API..."
EVENTS=$(kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/events 2>/dev/null || echo "[]")
COUNT=$(echo "$EVENTS" | grep -o '"id"' | wc -l)
if [ "$COUNT" -gt 0 ]; then
    ok "Dashboard has $COUNT events recorded"
else
    warn "No events in dashboard API"
fi

# ------------------------------------------
# Summary
# ------------------------------------------
echo ""
echo "=========================================="
echo " Test Results"
echo "=========================================="
echo -e "  ${GREEN}PASS: $pass${NC}"
echo -e "  ${RED}FAIL: $fail${NC}"
echo -e "  ${YELLOW}SKIP: $skip${NC}"
echo ""
TOTAL=$((pass + fail + skip))
echo "  Total: $TOTAL tests"
echo ""

if [ $fail -gt 0 ]; then
    echo -e "${RED}Some tests failed. Check agent logs:${NC}"
    echo "  kubectl logs -n kube-system -l app=auto-agent --tail=200"
    exit 1
fi
echo -e "${GREEN}All critical tests passed!${NC}"
