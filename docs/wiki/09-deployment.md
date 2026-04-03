# Deployment Guide

## Option 1: Standalone (recommended)

```bash
./deployment/deploy.sh
```

### Manifest Files

| File | What |
|------|------|
| `00-namespace.yaml` | `auto-agent` namespace |
| `01-crds.yaml` | AutoRemediationPolicy CRD |
| `02-rbac.yaml` | ServiceAccount + ClusterRole + ClusterRoleBinding |
| `03-config.yaml` | ConfigMap (all settings) + Secret (tokens) |
| `04-daemonset.yaml` | DaemonSet + NodePort Service (30080) |
| `deploy.sh` | One-shot: build → load → apply → port-forward |
| `teardown.sh` | Clean removal of everything |

### What deploy.sh Does

1. Verifies `kubectl` cluster access
2. Builds Docker image (`auto-agent:latest`)
3. Auto-detects cluster type and loads image (containerd/KIND/Minikube/k3d)
4. Reads `COST_PROVIDER` — installs Kubecost/OpenCost if configured
5. Applies all manifests in order
6. Patches cost provider URL into ConfigMap
7. Waits for DaemonSet rollout
8. Starts port-forward, opens browser

## Option 2: Helm

```bash
helm upgrade --install auto-agent charts/auto-agent \
  -n kube-system --create-namespace \
  -f my-values.yaml
```

See `charts/auto-agent/values.yaml` for all configurable values.

## Option 3: Docker Compose (image build only)

```bash
docker compose up --build    # builds the image
# Then deploy manually or use deploy.sh
```

## RBAC

### ClusterRole (default)
Full cluster access for all resources the agent monitors. Simple but broad.

### Namespace-Scoped (production)
Set `namespacedRBAC.enabled: true` in Helm values. Generates per-namespace Role + RoleBinding for each namespace in the allowlist. Minimal ClusterRole for nodes/leases only.

## Image Loading

The agent image must be available on cluster nodes:

| Cluster Type | How |
|---|---|
| Docker Desktop (containerd) | `docker save | docker exec ctr import` (deploy.sh does this) |
| KIND | `kind load docker-image` |
| Minikube | `minikube image load` |
| k3d | `k3d image import` |
| Remote (EKS/GKE/AKS) | Push to registry, set `image.repository` and `pullPolicy: Always` |

## Upgrade

```bash
# Rebuild image
docker build -t auto-agent:latest .

# Load into nodes
# (same as initial deploy)

# Restart DaemonSet
kubectl rollout restart ds/auto-agent -n auto-agent
```

Events persist to disk (`events.jsonl`) — no data loss on restart.
