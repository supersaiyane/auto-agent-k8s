# Auto-Agent Testing Guide

Complete end-to-end testing guide for validating every feature on a KIND cluster.

## Prerequisites

```bash
# Required tools
brew install kind kubectl helm docker
```

## Quick Start

```bash
# 1. Setup (creates KIND cluster, builds image, deploys agent)
chmod +x test/scripts/*.sh
./test/scripts/setup.sh

# 2. Run all tests (takes ~15 minutes)
./test/scripts/run-tests.sh

# 3. Teardown
./test/scripts/teardown.sh
```

## Manual Testing (Step by Step)

### Step 0: Setup the Cluster

```bash
# Create KIND cluster with 3 nodes
kind create cluster --config test/kind-config.yaml

# Build and load the agent image
docker build -t auto-agent:test .
kind load docker-image auto-agent:test --name auto-agent-test

# Install CRDs
kubectl apply -f charts/auto-agent/crds/

# Deploy agent in fix mode
helm upgrade --install auto-agent charts/auto-agent \
  -n kube-system --create-namespace \
  -f test/manifests/agent-values-test.yaml

# Wait for agent to start
kubectl rollout status daemonset/auto-agent -n kube-system --timeout=120s

# Create test namespaces
kubectl apply -f test/manifests/00-namespace.yaml
```

Open two extra terminals:
```bash
# Terminal 2: Watch agent logs
kubectl logs -n kube-system -l app=auto-agent -f

# Terminal 3: Port-forward dashboard
kubectl port-forward -n kube-system svc/auto-agent 8080:8080
# Then open http://localhost:8080
```

---

### Test 1: CrashLoopBackOff

**What it tests:** Agent detects crash-looping pods, collects logs, deletes pod in fix mode.

```bash
kubectl apply -f test/manifests/01-crashloop.yaml
```

**Wait:** 30-60 seconds for pod to enter CrashLoopBackOff.

**Verify:**
```bash
# Agent log should show detection
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep CrashLoopBackOff

# Pod should have been deleted and recreated
kubectl get pods -n test-apps -l app=crashloop-test

# Events should show agent action
kubectl get events -n test-apps --sort-by=.lastTimestamp | tail -10

# Dashboard should show incident
curl -s http://localhost:8080/api/events | jq '.[] | select(.reason=="CrashLoopBackOff")'
```

**Expected:**
- [x] Agent log: `handler: CrashLoopBackOff detected on test-apps/crashloop-test-xxx`
- [x] Pod deleted and recreated by ReplicaSet
- [x] Dashboard shows CrashLoopBackOff incident with severity=critical
- [x] Audit log entry written

**Cleanup:** `kubectl delete -f test/manifests/01-crashloop.yaml`

---

### Test 2: OOMKilled

**What it tests:** Agent detects OOM-killed containers, reports memory limit, would open GitOps PR if configured.

```bash
kubectl apply -f test/manifests/02-oomkilled.yaml
```

**Wait:** 10-30 seconds for container to be OOM-killed.

**Verify:**
```bash
# Check for OOM detection
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep OOMKilled

# Check pod status
kubectl get pods -n test-apps -l app=oom-test -o wide

# Check dashboard
curl -s http://localhost:8080/api/events | jq '.[] | select(.reason=="OOMKilled")'
```

**Expected:**
- [x] Agent log: `handler: OOMKilled detected on test-apps/oom-test-xxx`
- [x] Message includes current memory limit (64Mi)
- [x] Dashboard shows OOMKilled event

**Cleanup:** `kubectl delete -f test/manifests/02-oomkilled.yaml`

---

### Test 3: ImagePullBackOff

**What it tests:** Agent detects failed image pulls and reports the exact image name.

```bash
kubectl apply -f test/manifests/03-imagepull.yaml
```

**Wait:** 15-30 seconds.

**Verify:**
```bash
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep ImagePullBackOff
kubectl get pods -n test-apps -l app=imagepull-test
```

**Expected:**
- [x] Agent log: `handler: ImagePullBackOff detected`
- [x] Message includes image name `registry.invalid/nonexistent/image:v99.99.99`

**Cleanup:** `kubectl delete -f test/manifests/03-imagepull.yaml`

---

### Test 4: Pending Pod

**What it tests:** Agent detects pods stuck in Pending >5 minutes and diagnoses the reason.

```bash
kubectl apply -f test/manifests/04-pending.yaml
```

**Wait:** 6+ minutes (agent checks for >5 min pending threshold).

**Verify:**
```bash
# Pod should be stuck in Pending
kubectl get pods -n test-apps -l app=pending-test

# Agent should detect after 5 min
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep Pending
```

**Expected:**
- [x] Pod stays in Pending state (impossible resource request)
- [x] After 5+ min, agent detects and diagnoses "Insufficient" resources
- [x] Dashboard shows Pending incident

**Cleanup:** `kubectl delete -f test/manifests/04-pending.yaml`

---

### Test 5: NotReady Pod

**What it tests:** Agent detects containers running but failing readiness probes for >3 minutes.

```bash
kubectl apply -f test/manifests/05-notready.yaml
```

**Wait:** 4+ minutes.

**Verify:**
```bash
# Pod should be Running but not Ready (0/1)
kubectl get pods -n test-apps -l app=notready-test

# Agent detects after 3 min
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep NotReady
```

**Expected:**
- [x] Pod shows `0/1 Running` (running but not ready)
- [x] After 3+ min, agent detects and deletes pod (fix mode)

**Cleanup:** `kubectl delete -f test/manifests/05-notready.yaml`

---

### Test 6: Init Container Failure

**What it tests:** Agent detects init containers that crash or fail.

```bash
kubectl apply -f test/manifests/06-init-failure.yaml
```

**Wait:** 30 seconds.

**Verify:**
```bash
# Pod should show Init:CrashLoopBackOff
kubectl get pods -n test-apps -l app=init-fail-test

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep -i "init"
```

**Expected:**
- [x] Pod shows `Init:CrashLoopBackOff` or `Init:Error`
- [x] Agent detects init container failure
- [x] Agent collects init container logs (not main container)

**Cleanup:** `kubectl delete -f test/manifests/06-init-failure.yaml`

---

### Test 7: ConfigMap/Secret Reference Error

**What it tests:** Agent detects `CreateContainerConfigError` when a referenced ConfigMap doesn't exist.

```bash
kubectl apply -f test/manifests/07-config-error.yaml
```

**Wait:** 15 seconds.

**Verify:**
```bash
# Pod should show CreateContainerConfigError
kubectl get pods -n test-apps -l app=config-error-test

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep -i "config"
```

**Expected:**
- [x] Pod shows `CreateContainerConfigError`
- [x] Agent detects and alerts with ConfigMap name
- [x] Message suggests checking ConfigMap exists in namespace

**Cleanup:** `kubectl delete -f test/manifests/07-config-error.yaml`

---

### Test 8: Restart Storm

**What it tests:** Agent detects containers restarting rapidly (>5 times) before entering CrashLoopBackOff.

```bash
kubectl apply -f test/manifests/08-restart-storm.yaml
```

**Wait:** 60-90 seconds (needs 5+ restarts).

**Verify:**
```bash
# Check restart count
kubectl get pods -n test-apps -l app=restart-storm-test -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep -i "restart"
```

**Expected:**
- [x] Restart count reaches 5+
- [x] Agent detects and warns about impending CrashLoopBackOff

**Cleanup:** `kubectl delete -f test/manifests/08-restart-storm.yaml`

---

### Test 9: Failed Job

**What it tests:** Agent detects failed Jobs/CronJobs and cleans up old failures.

```bash
kubectl apply -f test/manifests/09-failed-job.yaml
```

**Wait:** 2-3 minutes (agent scans every 2 min).

**Verify:**
```bash
# Job should be in Failed state
kubectl get jobs -n test-apps

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=50 | grep -i "job"
```

**Expected:**
- [x] `failing-job` shows as Failed
- [x] Agent detects and collects pod logs
- [x] CronJob failures accumulate and get cleaned up (>1hr old)

**Cleanup:** `kubectl delete -f test/manifests/09-failed-job.yaml`

---

### Test 10: Stuck Rollout

**What it tests:** Agent detects `ProgressDeadlineExceeded` and auto-rolls back.

```bash
# Step 1: Deploy working v1
kubectl apply -f test/manifests/10-stuck-rollout.yaml
sleep 15

# Verify v1 is healthy
kubectl get pods -n test-apps -l app=rollout-test

# Step 2: Deploy broken v2
kubectl apply -f test/manifests/10-stuck-rollout-break.yaml
```

**Wait:** 60-90 seconds (progressDeadlineSeconds=60).

**Verify:**
```bash
# Deployment should show ProgressDeadlineExceeded
kubectl describe deploy rollout-test -n test-apps | grep -A2 Conditions

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep -i "rollout\|rollback"

# After rollback, v1 pods should be running again
kubectl get pods -n test-apps -l app=rollout-test
```

**Expected:**
- [x] Deployment shows `ProgressDeadlineExceeded`
- [x] Agent detects and rolls back to previous revision
- [x] Working v1 pods are restored

**Cleanup:** `kubectl delete -f test/manifests/10-stuck-rollout.yaml`

---

### Test 11: Service with 0 Endpoints

**What it tests:** Agent detects services with no ready endpoints (traffic blackhole).

```bash
kubectl apply -f test/manifests/11-zero-endpoints.yaml
```

**Wait:** 2-3 minutes (agent scans every 2 min).

**Verify:**
```bash
# Service should have no endpoints
kubectl get endpoints blackhole-svc -n test-apps

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=50 | grep -i "endpoint\|blackhole"
```

**Expected:**
- [x] Endpoints object shows 0 addresses
- [x] Agent detects and alerts about traffic blackhole

**Cleanup:** `kubectl delete -f test/manifests/11-zero-endpoints.yaml`

---

### Test 12: Resource Quota Exhaustion

**What it tests:** Agent detects when resource quotas are >90% consumed.

```bash
kubectl apply -f test/manifests/12-resource-quota.yaml
```

**Wait:** 5-6 minutes (agent scans every 5 min).

**Verify:**
```bash
# Check quota usage
kubectl describe resourcequota tight-quota -n test-apps

# Agent log
kubectl logs -n kube-system -l app=auto-agent --tail=50 | grep -i "quota"
```

**Expected:**
- [x] Quota usage >90% for pods and/or memory
- [x] Agent alerts before new pods fail to schedule

**Cleanup:** `kubectl delete -f test/manifests/12-resource-quota.yaml`

---

### Test 13: Excluded Pod (Annotation)

**What it tests:** Agent ignores pods with `auto-agent.io/disable: "true"` annotation.

```bash
kubectl apply -f test/manifests/13-excluded-pod.yaml
sleep 30
```

**Verify:**
```bash
# Pod should be crashing
kubectl get pods -n test-apps -l app=excluded-test

# Agent should NOT mention this pod
kubectl logs -n kube-system -l app=auto-agent --tail=50 | grep "excluded-test"
# This grep should return NO results
```

**Expected:**
- [x] Pod is crashing but agent produces NO logs about it
- [x] No dashboard events for `excluded-test`
- [x] Agent respects the escape hatch

**Cleanup:** `kubectl delete -f test/manifests/13-excluded-pod.yaml`

---

### Test 14: CRD Policy Enforcement

**What it tests:** `requireApproval=true` in CRD blocks automated fix actions.

```bash
kubectl apply -f test/manifests/14-crd-policy.yaml
sleep 30
```

**Verify:**
```bash
# Pod should be crashing but NOT deleted (requireApproval blocks it)
kubectl get pods -n test-apps -l app=policy-test

# Agent log should show "Blocked"
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep -i "blocked\|approval"

# Check audit log for "blocked" entry
POD=$(kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n kube-system $POD -- cat /var/log/auto-agent/audit.jsonl | grep "blocked"
```

**Expected:**
- [x] Pod is crashing but stays alive (not deleted)
- [x] Agent logs "Blocked: CRD policy requires manual approval"
- [x] Audit log records the blocked action

**Cleanup:** `kubectl delete -f test/manifests/14-crd-policy.yaml`

---

### Test 15: HPA Coexistence

**What it tests:** Agent skips scaling deployments that have an HPA.

```bash
kubectl apply -f test/manifests/15-scaling-target.yaml
```

**Wait:** 30-60 seconds for scaler tick.

**Verify:**
```bash
# Agent should skip hpa-protected deployment
kubectl logs -n kube-system -l app=auto-agent --tail=50 | grep -i "hpa\|skipping"

# hpa-protected deployment should NOT be scaled by agent
kubectl get deploy hpa-protected -n test-apps -o jsonpath='{.spec.replicas}'
# Should still be 2
```

**Expected:**
- [x] Agent log: `skipping test-apps/hpa-protected (HPA present)`
- [x] Replica count unchanged by agent
- [x] `scale-test` (no HPA) may be scaled if CPU is high enough

**Cleanup:** `kubectl delete -f test/manifests/15-scaling-target.yaml`

---

### Test 16: Dashboard UI Verification

```bash
# Port-forward if not already done
kubectl port-forward -n kube-system svc/auto-agent 8080:8080 &
```

**Open:** http://localhost:8080

**Verify manually:**
- [x] Header shows version, mode badge (fix), leader/follower badge, node name
- [x] Stat cards show incident/action/scaling counts
- [x] Event feed shows incidents from the tests above
- [x] Tab filters work (All / Incidents / Actions / Scaling / Anomalies)
- [x] Events have severity dots (red=critical, yellow=warning, blue=info)
- [x] Auto-refreshes every 5 seconds

---

### Test 17: Audit Log Verification

```bash
POD=$(kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')

# View audit log
kubectl exec -n kube-system $POD -- cat /var/log/auto-agent/audit.jsonl

# Count entries
kubectl exec -n kube-system $POD -- wc -l /var/log/auto-agent/audit.jsonl
```

**Expected format (JSONL):**
```json
{"timestamp":"2026-04-03T...","action":"delete_pod","namespace":"test-apps","workload":"replicaset/crashloop-test-xxx","reason":"CrashLoopBackOff","result":"success","mode":"fix"}
{"timestamp":"2026-04-03T...","action":"delete_pod","namespace":"test-apps","workload":"replicaset/policy-test-xxx","reason":"CrashLoopBackOff","result":"blocked","detail":"CRD policy requires manual approval","mode":"fix"}
```

---

### Test 18: Self-Monitoring

```bash
# Agent should run self-checks every 3 minutes
kubectl logs -n kube-system -l app=auto-agent --tail=100 | grep -i "selfcheck"

# If Prometheus/Slack/Alertmanager are not configured, it should log warnings
```

---

### Test 19: Observe Mode (No Actions)

```bash
# Switch to observe mode
helm upgrade auto-agent charts/auto-agent \
  -n kube-system \
  -f test/manifests/agent-values-test.yaml \
  --set agent.mode=observe

# Wait for restart
kubectl rollout status daemonset/auto-agent -n kube-system

# Deploy a crashing pod
kubectl apply -f test/manifests/01-crashloop.yaml

# Agent should detect but NOT delete
kubectl logs -n kube-system -l app=auto-agent --tail=20 | grep CrashLoopBackOff
# No "Action:" line, no "deleted pod" message
```

**Cleanup:** Switch back to fix mode for other tests.

---

### Test 20: Helm Test

```bash
helm test auto-agent -n kube-system
```

**Expected:** Test pod runs and verifies healthz, readyz, API, metrics, and dashboard UI.

---

## Troubleshooting

### Agent pod not starting
```bash
kubectl describe pod -n kube-system -l app=auto-agent
kubectl logs -n kube-system -l app=auto-agent --previous
```

### Agent not detecting events
```bash
# Check namespace allowlist
kubectl get configmap auto-agent-config -n kube-system -o yaml | grep NAMESPACE_ALLOWLIST

# Check if pod informer is filtered to local node
kubectl logs -n kube-system -l app=auto-agent | grep "filtering pod informer"
```

### CRD not being watched
```bash
# Check CRD is installed
kubectl get crd autoremediationpolicies.autoagent.io

# Check agent logs for CRD controller
kubectl logs -n kube-system -l app=auto-agent | grep "crd:"
```

### Dashboard not accessible
```bash
# Check service
kubectl get svc auto-agent -n kube-system

# Direct pod access
POD=$(kubectl get pods -n kube-system -l app=auto-agent -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n kube-system $POD -- wget -qO- http://localhost:8080/healthz
```

## Full Cleanup

```bash
./test/scripts/teardown.sh
```
