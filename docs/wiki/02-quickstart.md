# Quick Start Guide

## Prerequisites

- `kubectl` connected to a Kubernetes cluster
- `docker` for building the agent image
- `helm` (optional, for Helm-based deployment)

## Deploy in 60 Seconds

```bash
# Clone the repo
git clone https://github.com/yourorg/auto-agent-k8s.git
cd auto-agent-k8s

# Deploy (builds image, loads into cluster, applies all manifests)
./deployment/deploy.sh
```

The script will:
1. Build the Docker image
2. Auto-detect your cluster type and load the image
3. Install CRDs, RBAC, ConfigMap, DaemonSet
4. Wait for pods to be ready
5. Open the dashboard in your browser

## Open Dashboard

```
http://localhost:8080
```

If port-forward died:
```bash
kubectl port-forward -n auto-agent svc/auto-agent 8080:8080
```

## See It In Action

Deploy test apps that intentionally fail:

```bash
# Deploy 3 apps across 3 namespaces (9 failing pods)
./deployment/test-apps/deploy-apps.sh
```

This creates:
- **payment-service** (default) — CrashLoop, OOM, ConfigError
- **order-service** (test1) — Init failure, ImagePull, NotReady, 0 endpoints, failed Job
- **inventory-service** (test2) — Pending, restart storm, healthy v1

Watch the dashboard populate with incidents and actions in real-time.

### Trigger Stuck Rollout

```bash
kubectl set image deploy/inventory-api api=registry.invalid/broken:v2 -n test2
```

### Run Full Chaos Test (74 issue types)

```bash
bash deployment/test-apps/chaos-test.sh
```

## Choose Your Mode

Edit `deployment/03-config.yaml`:

```yaml
AUTO_MODE: "observe"    # Alert only — start here
AUTO_MODE: "suggest"    # Alert + recommend actions
AUTO_MODE: "fix"        # Auto-remediate
AUTO_MODE: "dry-run"    # Simulate fixes, show what would happen
```

Apply and restart:
```bash
kubectl apply -f deployment/03-config.yaml
kubectl rollout restart ds/auto-agent -n auto-agent
```

## Clean Up

```bash
# Remove test apps
./deployment/test-apps/remove-apps.sh

# Remove agent
./deployment/teardown.sh
```

## Next Steps

- [Configuration Reference](03-configuration.md) — all settings explained
- [Dashboard Guide](06-dashboard.md) — what each tab shows
- [Safety Model](05-safety.md) — how guardrails protect your cluster
