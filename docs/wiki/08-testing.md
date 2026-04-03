# Testing Guide

## Test Applications

### 3-App Suite (`deployment/test-apps/`)

| App | Namespace | Failures |
|-----|-----------|----------|
| payment-service | default | CrashLoop, OOM, ConfigError |
| order-service | test1 | Init failure, ImagePull, NotReady, 0 endpoints, failed Job |
| inventory-service | test2 | Pending, restart storm, stuck rollout (manual trigger) |

```bash
./deployment/test-apps/deploy-apps.sh    # deploy all
./deployment/test-apps/remove-apps.sh    # cleanup
```

### Chaos Suite (`deployment/test-apps/chaos-full.yaml`)

22 workloads in `chaos` namespace simulating all failure categories:
- 11 pod-level (CrashLoop, OOM, ImagePull, Config, Init, Pending, NotReady, RestartStorm, RunContainerError, ErrImageNeverPull, excluded)
- 5 workload-level (failed Job, CronJob, 0-endpoint Service, stuck rollout, paused deploy)
- 3 storage (PVC bad StorageClass, volume stuck, ResourceQuota)
- 2 networking (LoadBalancer pending, Ingress bad backend)
- 1 security (expired TLS cert)

### Automated Chaos Test

```bash
bash deployment/test-apps/chaos-test.sh
```

Runs 25+ checks with PASS/FAIL/SKIP reporting:
- Auto-adds `chaos` to namespace allowlist
- Deploys all chaos workloads
- Waits for agent detection (with timeout per check)
- Verifies dashboard APIs have data
- Reports detection rate percentage

## Unit Tests

```bash
go test -race -count=1 ./...
```

Key test suites:
- `internal/ratelimit/` — Deduplicator, ActionLimiter, CircuitBreaker
- `internal/policy/` — LoadFromEnv, validation, defaults
- `internal/kube/` — helpers, scaler cooldown, pressure detection, integration tests with fake clientset
- `internal/events/` — Recorder ring buffer, persistence
- `internal/metrics/` — PromQL sanitization

## Integration Tests

Use `fake.NewSimpleClientset` for K8s API mocking:
- Scale-up with CPU above threshold
- HPA skip when HPA exists
- Max replicas cap
- Cooldown prevents re-scaling
- Scale-down gate logic
- CrashLoop pod deletion in fix mode
- Suggest mode skips deletion
- Rate limiting blocks action
- Dedup blocks repeat events
- Excluded annotation
- Disallowed namespace

## Manual Break-Fix Test

```bash
# Deploy healthy app
kubectl apply -f deployment/test-apps/app1-payment-service.yaml

# Break it
kubectl patch deploy api-server -n default --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","exit 1"]}]'

# Watch agent fix it
kubectl logs -n auto-agent -l app=auto-agent -f

# Fix the underlying issue
kubectl patch deploy api-server -n default --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","sleep infinity"]}]'

# Check dashboard Actions tab for verified fix
curl -s http://localhost:8080/api/fixes | python3 -m json.tool
```
