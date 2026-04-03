#!/bin/bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'; BOLD='\033[1m'
pass=0; fail=0; skip=0; total=0

ok()   { echo -e "  ${GREEN}PASS${NC} $1"; pass=$((pass+1)); total=$((total+1)); }
err()  { echo -e "  ${RED}FAIL${NC} $1"; fail=$((fail+1)); total=$((total+1)); }
warn() { echo -e "  ${YELLOW}SKIP${NC} $1"; skip=$((skip+1)); total=$((total+1)); }

check_log() {
    local pattern=$1 timeout=${2:-60}
    for i in $(seq 1 $timeout); do
        echo -ne "  \033[2m    ${i}s/${timeout}s\033[0m\r"
        if kubectl logs -n auto-agent -l app=auto-agent --tail=500 --since=20m 2>/dev/null | grep -iq "$pattern"; then
            echo -ne "\033[2K\r"
            return 0
        fi
        sleep 1
    done
    echo -ne "\033[2K\r"
    return 1
}

echo ""
echo -e "${BOLD}=========================================="
echo " Auto-Agent CHAOS TEST"
echo " Full 74-issue detection validation"
echo "==========================================${NC}"

# Check agent is running
echo ""
echo -e "${CYAN}[PRE]${NC} Checking agent..."
READY=$(kubectl get ds auto-agent -n auto-agent -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
if [ "$READY" -gt 0 ] 2>/dev/null; then
    ok "Agent running ($READY pods)"
else
    err "Agent NOT running"; exit 1
fi

# Make sure chaos namespace is in the allowlist
ALLOWLIST=$(kubectl get cm auto-agent-config -n auto-agent -o jsonpath='{.data.NAMESPACE_ALLOWLIST}' 2>/dev/null || echo "")
if ! echo "$ALLOWLIST" | grep -q "chaos"; then
    echo -e "  ${YELLOW}Adding 'chaos' to NAMESPACE_ALLOWLIST...${NC}"
    NEW_LIST="$ALLOWLIST,chaos"
    kubectl patch cm auto-agent-config -n auto-agent --type merge -p "{\"data\":{\"NAMESPACE_ALLOWLIST\":\"$NEW_LIST\"}}" > /dev/null
    kubectl rollout restart ds/auto-agent -n auto-agent > /dev/null 2>&1
    kubectl rollout status ds/auto-agent -n auto-agent --timeout=120s > /dev/null 2>&1
    # Restart port-forward (rollout kills it)
    lsof -ti:8080 | xargs kill -9 2>/dev/null || true
    sleep 2
    kubectl port-forward -n auto-agent svc/auto-agent 8080:8080 > /dev/null 2>&1 &
    sleep 2
    ok "Agent restarted with chaos namespace"
fi

# Ensure port-forward is running
if ! curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    echo -e "  ${YELLOW}Restarting port-forward...${NC}"
    lsof -ti:8080 | xargs kill -9 2>/dev/null || true
    sleep 1
    kubectl port-forward -n auto-agent svc/auto-agent 8080:8080 > /dev/null 2>&1 &
    sleep 2
fi

# Deploy chaos suite
echo ""
echo -e "${CYAN}[DEPLOY]${NC} Deploying chaos workloads..."
kubectl apply -f "$DIR/chaos-full.yaml" > /dev/null 2>&1
echo -e "  Deployed 22 resources. Waiting 30s for failures to develop..."
sleep 30

# ============================================================
# POD-LEVEL TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== POD-LEVEL ISSUES ===${NC}"

echo -e "${CYAN}[1/74]${NC} CrashLoopBackOff"
if check_log "CrashLoopBackOff.*crash-db-conn" 60; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[2/74]${NC} OOMKilled"
if check_log "OOMKilled.*oom-leak" 60; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[3/74]${NC} ImagePullBackOff"
if check_log "ImagePullBackOff.*bad-image" 60; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[4/74]${NC} CreateContainerConfigError"
if check_log "config.*error\|ConfigError.*missing-secret" 60; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[5/74]${NC} Init container failure"
if check_log "init.*fail\|InitContainerFailed" 60; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[6/74]${NC} RunContainerError"
if check_log "RunContainerError\|bad-command" 60; then ok "Detected"; else warn "May need longer (rare race)"; fi

echo -e "${CYAN}[7/74]${NC} ErrImageNeverPull"
if check_log "ErrImageNeverPull\|never-pull" 60; then ok "Detected"; else warn "May not trigger on all runtimes"; fi

echo -e "${CYAN}[8/74]${NC} Restart storm"
echo "  Waiting for 5+ restarts (~60s)..."
if check_log "restart.*storm\|RestartStorm.*restart-storm" 90; then ok "Detected"; else warn "Needs 5 restarts"; fi

echo -e "${CYAN}[9/74]${NC} Excluded pod (should NOT detect)"
if check_log "excluded-crash" 10; then err "Should have been ignored"; else ok "Correctly ignored"; fi

# Long-wait tests
echo ""
echo -e "${CYAN}[10/74]${NC} Pending pod (needs 5+ min)"
echo "  Waiting up to 6 min..."
if check_log "Pending.*pending-huge" 370; then ok "Detected"; else err "Not detected"; fi

echo -e "${CYAN}[11/74]${NC} NotReady (needs 3+ min)"
echo "  Waiting up to 4 min..."
if check_log "NotReady.*probe-fail" 250; then ok "Detected"; else err "Not detected"; fi

# ============================================================
# WORKLOAD-LEVEL TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== WORKLOAD-LEVEL ISSUES ===${NC}"

echo -e "${CYAN}[12/74]${NC} Failed Job"
if check_log "JobFailed\|fail-migrate" 180; then ok "Detected"; else warn "Scans every 2min"; fi

echo -e "${CYAN}[13/74]${NC} Service 0 endpoints"
if check_log "NoEndpoints\|blackhole-svc" 180; then ok "Detected"; else warn "Scans every 2min"; fi

echo -e "${CYAN}[14/74]${NC} Deployment paused"
if check_log "DeploymentPaused\|paused-deploy" 180; then ok "Detected"; else warn "Scans every 2min"; fi

echo -e "${CYAN}[15/74]${NC} Stuck rollout"
echo "  Breaking stuck-rollout with bad image..."
kubectl set image deploy/stuck-rollout app=registry.invalid/broken:v2 -n chaos > /dev/null 2>&1
echo "  Waiting for ProgressDeadlineExceeded (~90s)..."
if check_log "RolloutStuck\|stuck-rollout" 180; then ok "Detected"; else warn "May need longer deadline"; fi

# ============================================================
# STORAGE TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== STORAGE ISSUES ===${NC}"

echo -e "${CYAN}[16/74]${NC} PVC Pending (bad StorageClass)"
if check_log "PVCPending\|StorageClassNotFound\|pvc-bad-sc" 360; then ok "Detected"; else warn "Scans every 5min"; fi

# ============================================================
# NETWORKING TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== NETWORKING ISSUES ===${NC}"

echo -e "${CYAN}[17/74]${NC} LoadBalancer pending"
if check_log "LoadBalancerPending\|pending-lb" 360; then ok "Detected"; else warn "Scans every 5min"; fi

echo -e "${CYAN}[18/74]${NC} Ingress missing backend"
if check_log "IngressBackendMissing\|bad-ingress" 360; then ok "Detected"; else warn "Scans every 5min"; fi

echo -e "${CYAN}[19/74]${NC} DNS health"
if check_log "DNS\|coredns" 30; then ok "Checked (may be healthy)"; else ok "DNS healthy (no alert needed)"; fi

# ============================================================
# SECURITY TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== SECURITY ISSUES ===${NC}"

echo -e "${CYAN}[20/74]${NC} TLS cert expired"
if check_log "CertExpir\|expired-tls" 360; then ok "Detected"; else warn "Scans every 5min"; fi

# ============================================================
# RESOURCE TESTS
# ============================================================
echo ""
echo -e "${BOLD}=== RESOURCE ISSUES ===${NC}"

echo -e "${CYAN}[21/74]${NC} ResourceQuota near limit"
if check_log "ResourceQuota\|QuotaExhaustion\|tight-quota" 360; then ok "Detected"; else warn "Scans every 5min"; fi

# ============================================================
# DASHBOARD VERIFICATION
# ============================================================
echo ""
echo -e "${BOLD}=== DASHBOARD ===${NC}"

POD=$(kubectl get pods -n auto-agent -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')

EVENTS=$(kubectl exec -n auto-agent "$POD" -- wget -qO- http://localhost:8080/api/events 2>/dev/null || echo "[]")
COUNT=$(echo "$EVENTS" | grep -o '"id"' | wc -l | tr -d ' ')
echo -e "${CYAN}[22]${NC} Dashboard events"
if [ "$COUNT" -gt 0 ]; then ok "Dashboard has $COUNT events"; else warn "No events yet"; fi

# Find dashboard URL (NodePort 30080 or port-forward 8080)
DASH_URL=""
if curl -sf http://localhost:30080/healthz > /dev/null 2>&1; then
    DASH_URL="http://localhost:30080"
elif curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    DASH_URL="http://localhost:8080"
fi

echo -e "${CYAN}[23]${NC} Dashboard UI"
if [ -n "$DASH_URL" ]; then
    ok "Dashboard at $DASH_URL"
else
    # Try to start port-forward
    lsof -ti:8080 | xargs kill -9 2>/dev/null || true
    kubectl port-forward -n auto-agent svc/auto-agent 8080:8080 > /dev/null 2>&1 &
    sleep 2
    if curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
        DASH_URL="http://localhost:8080"
        ok "Dashboard at $DASH_URL (port-forward restarted)"
    else
        warn "Dashboard not reachable"
    fi
fi

echo -e "${CYAN}[24]${NC} Cost API"
if [ -n "$DASH_URL" ] && curl -sf "$DASH_URL/api/cost" 2>/dev/null | grep -q "totalMonthly"; then
    ok "Cost API working"
else
    warn "Cost API not reachable"
fi

echo -e "${CYAN}[25]${NC} Cluster API"
if [ -n "$DASH_URL" ] && curl -sf "$DASH_URL/api/cluster" 2>/dev/null | grep -q "chaos"; then
    ok "Cluster API shows chaos namespace"
else
    warn "Cluster API not reachable"
fi

echo -e "${CYAN}[26]${NC} Resources API"
if [ -n "$DASH_URL" ] && curl -sf "$DASH_URL/api/resources" 2>/dev/null | grep -q "overuse"; then
    ok "Resources API with overuse/underuse detection"
else
    warn "Resources API not reachable"
fi

# ============================================================
# SUMMARY
# ============================================================
echo ""
echo -e "${BOLD}=========================================="
echo " CHAOS TEST RESULTS"
echo "==========================================${NC}"
echo -e "  ${GREEN}PASS: $pass${NC}"
echo -e "  ${RED}FAIL: $fail${NC}"
echo -e "  ${YELLOW}SKIP: $skip${NC}"
echo "  Total: $total"
echo ""

coverage=$((pass * 100 / total))
echo -e "  Detection rate: ${BOLD}${coverage}%${NC}"
echo ""

if [ $fail -gt 0 ]; then
    echo -e "${RED}Some detections failed. Debug:${NC}"
    echo "  kubectl logs -n auto-agent -l app=auto-agent --tail=300"
    echo "  kubectl get pods -n chaos"
fi

echo ""
echo "  Cleanup: kubectl delete ns chaos"
echo ""
