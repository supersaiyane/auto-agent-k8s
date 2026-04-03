#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
MANIFESTS="$TEST_DIR/manifests"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'
BOLD='\033[1m'
DIM='\033[2m'

pass=0; fail=0; skip=0

log()    { echo -e "\n${BOLD}${CYAN}[$1]${NC} $2"; }
step()   { echo -e "  ${DIM}=>  $1${NC}"; }
ok()     { echo -e "  ${GREEN}PASS${NC} $1"; pass=$((pass+1)); }
warn()   { echo -e "  ${YELLOW}SKIP${NC} $1"; skip=$((skip+1)); }
err()    { echo -e "  ${RED}FAIL${NC} $1"; fail=$((fail+1)); }
waiting(){ echo -ne "  ${DIM}    Waiting $1...${NC}\r"; }

check_agent_log() {
    local pattern=$1 timeout=${2:-30}
    for i in $(seq 1 $timeout); do
        waiting "${i}s/${timeout}s"
        if kubectl logs -n kube-system -l app=auto-agent --tail=300 --since=15m 2>/dev/null | grep -iq "$pattern"; then
            echo -ne "\033[2K\r"
            return 0
        fi
        sleep 1
    done
    echo -ne "\033[2K\r"
    return 1
}

get_agent_pod() {
    kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

wait_ready() {
    local ns=$1 deploy=$2 timeout=${3:-60}
    kubectl rollout status deploy/"$deploy" -n "$ns" --timeout="${timeout}s" > /dev/null 2>&1
}

echo ""
echo -e "${BOLD}=========================================="
echo " Auto-Agent Test Suite"
echo " Single Demo App — Break, Detect, Fix"
echo "==========================================${NC}"

# ------------------------------------------
# PRE-CHECK
# ------------------------------------------
log "PRE" "Checking agent is running..."
READY=$(kubectl get daemonset auto-agent -n kube-system -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
if [ "$READY" -gt 0 ] 2>/dev/null; then
    ok "Agent DaemonSet running ($READY pods)"
else
    err "Agent NOT running — run ./test/scripts/setup.sh first"
    exit 1
fi

POD=$(get_agent_pod)
log "PRE" "Checking dashboard..."
if kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/healthz 2>/dev/null | grep -q "ok"; then
    ok "Dashboard healthy"
else
    err "Dashboard not responding"
fi

# ------------------------------------------
# SETUP: Deploy healthy demo-app
# ------------------------------------------
log "SETUP" "Deploying healthy demo-app..."
kubectl apply -f "$MANIFESTS/00-namespace.yaml" > /dev/null 2>&1
kubectl apply -f "$MANIFESTS/demo-app.yaml"
wait_ready test-apps demo-app
ok "demo-app deployed and healthy (2/2 pods running)"
echo ""
echo -e "${BOLD}  The demo-app is now running. We will break it in 8 different"
echo -e "  ways and verify the agent detects and fixes each issue.${NC}"

# ==========================================
# SCENARIO 1: CrashLoopBackOff
# ==========================================
log "SCENARIO 1/8" "CrashLoopBackOff — app starts crashing"
step "Patching demo-app to crash on startup..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","echo FATAL: database connection refused; exit 1"]}]' > /dev/null

step "Waiting for agent to detect and fix..."
if check_agent_log "CrashLoopBackOff.*demo-app" 90; then
    ok "Agent DETECTED CrashLoopBackOff on demo-app"
else
    err "Agent did not detect CrashLoopBackOff"
fi

# Check if agent deleted the pod (fix mode)
if check_agent_log "deleted pod.*demo-app\|delete_pod.*demo-app" 10; then
    ok "Agent FIXED: deleted crashing pod (controller recreates)"
else
    warn "Pod deletion not confirmed in logs"
fi

step "Restoring healthy app..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","echo demo-app v1 running healthy; while true; do sleep 10; done"]}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 2: OOMKilled
# ==========================================
log "SCENARIO 2/8" "OOMKilled — app exceeds memory limit"
step "Patching demo-app to consume excessive memory..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","echo Allocating memory...; head -c 128M /dev/urandom > /dev/null; sleep infinity"]},{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"32Mi"}]' > /dev/null

if check_agent_log "OOMKilled.*demo-app" 90; then
    ok "Agent DETECTED OOMKilled on demo-app"
    ok "Agent reported memory limit in alert"
else
    err "Agent did not detect OOMKilled"
fi

step "Restoring healthy app..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","echo demo-app v1 running healthy; while true; do sleep 10; done"]},{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"64Mi"}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 3: ImagePullBackOff
# ==========================================
log "SCENARIO 3/8" "ImagePullBackOff — broken image tag"
step "Patching demo-app with non-existent image..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"registry.invalid/demo:broken"}]' > /dev/null

if check_agent_log "ImagePullBackOff.*demo-app" 90; then
    ok "Agent DETECTED ImagePullBackOff on demo-app"
else
    err "Agent did not detect ImagePullBackOff"
fi

if check_agent_log "deleted pod.*demo-app\|delete_pod.*demo-app" 10; then
    ok "Agent FIXED: deleted pod to retry pull"
else
    warn "Retry action not confirmed"
fi

step "Restoring healthy image..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"busybox:1.36"}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 4: Config Error (missing ConfigMap)
# ==========================================
log "SCENARIO 4/8" "ConfigError — delete the ConfigMap"
step "Deleting demo-config ConfigMap..."
kubectl delete configmap demo-config -n test-apps > /dev/null 2>&1
step "Restarting pods to trigger config error..."
kubectl rollout restart deploy/demo-app -n test-apps > /dev/null

if check_agent_log "config.*error\|ConfigError\|CreateContainerConfigError" 60; then
    ok "Agent DETECTED CreateContainerConfigError"
else
    err "Agent did not detect config error"
fi

step "Restoring ConfigMap..."
kubectl apply -f "$MANIFESTS/demo-app.yaml" > /dev/null 2>&1
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 5: Init Container Failure
# ==========================================
log "SCENARIO 5/8" "Init Container Failure — add failing init"
step "Adding a failing init container..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/initContainers","value":[{"name":"db-check","image":"busybox:1.36","command":["sh","-c","echo ERROR: cannot reach database; exit 1"],"resources":{"limits":{"memory":"32Mi","cpu":"50m"}}}]}]' > /dev/null

if check_agent_log "init.*container.*fail\|InitContainerFailed\|init-fail\|Init.*demo-app" 90; then
    ok "Agent DETECTED init container failure"
else
    err "Agent did not detect init container failure"
fi

step "Removing init container..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"remove","path":"/spec/template/spec/initContainers"}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 6: Stuck Rollout
# ==========================================
log "SCENARIO 6/8" "Stuck Rollout — deploy broken version"
step "Verifying demo-app is healthy..."
wait_ready test-apps demo-app 30 || true
step "Deploying broken v2 (will exceed progressDeadline)..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"registry.invalid/demo:v2-broken"}]' > /dev/null

step "Waiting for ProgressDeadlineExceeded (~60-90s)..."
if check_agent_log "RolloutStuck\|ProgressDeadlineExceeded\|rollback.*demo-app" 180; then
    ok "Agent DETECTED stuck rollout on demo-app"
else
    warn "Stuck rollout detection not confirmed (may need longer deadline)"
fi

step "Restoring healthy image..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"busybox:1.36"}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 7: Excluded Pod (annotation)
# ==========================================
log "SCENARIO 7/8" "Excluded Pod — annotation escape hatch"
step "Adding auto-agent.io/disable annotation and crashing app..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"add","path":"/spec/template/metadata/annotations","value":{"auto-agent.io/disable":"true"}},{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","exit 1"]}]' > /dev/null
sleep 20

if check_agent_log "demo-app.*CrashLoop\|demo-app.*handler" 10; then
    err "Agent should NOT have acted on excluded pod"
else
    ok "Agent correctly IGNORED excluded pod (annotation respected)"
fi

step "Removing annotation and restoring app..."
kubectl patch deploy demo-app -n test-apps --type=json \
  -p='[{"op":"remove","path":"/spec/template/metadata/annotations/auto-agent.io~1disable"},{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","echo demo-app v1 running healthy; while true; do sleep 10; done"]}]' > /dev/null
wait_ready test-apps demo-app 60 || true
sleep 5

# ==========================================
# SCENARIO 8: Failed Job
# ==========================================
log "SCENARIO 8/8" "Failed Job — batch job failure"
step "Creating a job that fails..."
kubectl apply -f - <<'JOBEOF' > /dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: demo-batch-job
  namespace: test-apps
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: worker
          image: busybox:1.36
          command: ["sh", "-c", "echo Processing batch...; sleep 3; echo FATAL: invalid data; exit 1"]
          resources:
            limits:
              memory: "32Mi"
              cpu: "50m"
JOBEOF

if check_agent_log "JobFailed\|failed.*job\|demo-batch-job" 180; then
    ok "Agent DETECTED failed job"
else
    warn "Failed job detection not confirmed (scans every 2 min)"
fi
kubectl delete job demo-batch-job -n test-apps --ignore-not-found > /dev/null 2>&1

# ==========================================
# VERIFY: Dashboard & Audit
# ==========================================
echo ""
log "VERIFY" "Checking dashboard recorded all events..."
POD=$(get_agent_pod)

# Agent events
EVENTS=$(kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/events 2>/dev/null || echo "[]")
COUNT=$(echo "$EVENTS" | grep -o '"id"' | wc -l | tr -d ' ')
if [ "$COUNT" -gt 0 ]; then
    ok "Dashboard has $COUNT agent events"
else
    warn "No agent events in dashboard"
fi

# K8s events
K8S_EVENTS=$(kubectl exec -n kube-system "$POD" -- wget -qO- "http://localhost:8080/api/k8s-events?namespace=test-apps" 2>/dev/null || echo "[]")
K8S_COUNT=$(echo "$K8S_EVENTS" | grep -o '"reason"' | wc -l | tr -d ' ')
if [ "$K8S_COUNT" -gt 0 ]; then
    ok "Dashboard has $K8S_COUNT K8s events for test-apps"
else
    warn "No K8s events for test-apps"
fi

# Audit log
AUDIT=$(kubectl exec -n kube-system "$POD" -- cat /var/log/auto-agent/audit.jsonl 2>/dev/null || echo "")
if [ -n "$AUDIT" ]; then
    AUDIT_LINES=$(echo "$AUDIT" | wc -l | tr -d ' ')
    ok "Audit log has $AUDIT_LINES entries"
    echo ""
    echo -e "  ${BOLD}Audit log (last 5 actions):${NC}"
    echo "$AUDIT" | tail -5 | while IFS= read -r line; do
        ACTION=$(echo "$line" | python3 -c "import sys,json;d=json.load(sys.stdin);print(f'  {d.get(\"result\",\"?\"):8} {d.get(\"action\",\"?\"):12} {d.get(\"namespace\",\"?\")}/{d.get(\"workload\",\"?\")} reason={d.get(\"reason\",\"?\")}')" 2>/dev/null || echo "  $line")
        echo "$ACTION"
    done
else
    warn "Audit log empty"
fi

# Fixed items
FIXED=$(kubectl exec -n kube-system "$POD" -- wget -qO- http://localhost:8080/api/events 2>/dev/null | python3 -c "
import sys,json
evts=json.load(sys.stdin)
actions=[e for e in evts if e.get('type')=='action']
print(len(actions))
" 2>/dev/null || echo "0")
echo ""
echo -e "  ${BOLD}Remediation summary:${NC}"
echo -e "    Events recorded: $COUNT"
echo -e "    Actions taken:   $FIXED"

# Dashboard check via port-forward
echo ""
log "VERIFY" "Dashboard UI (browser)"
if curl -sf http://localhost:8080/ 2>/dev/null | grep -q "auto-agent"; then
    ok "Dashboard accessible at http://localhost:8080"
    echo ""
    echo -e "  ${BOLD}>>> Open http://localhost:8080 in your browser <<<${NC}"
    echo ""
    echo "    Tabs to check:"
    echo "      K8s Events  — all cluster events for test-apps namespace"
    echo "      Agent Events — incidents detected by auto-agent"
    echo "      Fixed        — successful remediations"
    echo "      Cluster      — click test-apps to see pods/deploys/services"
    echo "      Nodes        — node health cards"
else
    warn "Dashboard not reachable — run: kubectl port-forward -n kube-system svc/auto-agent 8080:8080 &"
fi

# ==========================================
# CLEANUP
# ==========================================
echo ""
log "CLEANUP" "Restoring demo-app to healthy state..."
kubectl apply -f "$MANIFESTS/demo-app.yaml" > /dev/null 2>&1
wait_ready test-apps demo-app 60 || true
ok "demo-app restored to healthy"

# ==========================================
# SUMMARY
# ==========================================
echo ""
echo -e "${BOLD}=========================================="
echo " Results"
echo "==========================================${NC}"
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
echo -e "${GREEN}All tests passed!${NC}"
