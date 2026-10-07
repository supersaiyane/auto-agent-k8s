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
| `02-rbac.yaml` | One ServiceAccount, ClusterRole and write Roles per role (node, controller) |
| `03-config.yaml` | ConfigMap (all settings) |
| `04-agent.yaml` | Node agent DaemonSet, controller Deployment, ClusterIP Service, NetworkPolicies |
| `ensure-secret.sh` | Creates the Secret once with generated tokens; never overwrites it |

Files `01` to `04` are generated from the chart by `make manifests`; `make
verify` fails if they drift.
| `deploy.sh` | One-shot: build → load → apply → port-forward |
| `teardown.sh` | Clean removal of everything |

### What deploy.sh does

1. Checks cluster access (`--context NAME` picks a kubectl context).
2. Builds `auto-agent:latest` and loads it into kind or minikube. Any other
   cluster needs a pushed image: `--image REGISTRY/auto-agent@sha256:...`.
3. Applies the namespace, CRD, RBAC and ConfigMap. Allowlisted namespaces
   (`test1`, `test2`, `chaos`) it has to create are labelled
   `auto-agent.io/demo=true`.
4. Creates the Secret once with generated tokens (`ensure-secret.sh`); a
   re-run keeps every value you patched in.
5. Installs OpenCost or Kubecost only with `--with-opencost` or
   `--with-kubecost`, or sets manual prices with `--cost-manual 0.05,0.005,USD`.
6. Applies the node agents and the controller and waits for both.
7. Starts a dashboard port-forward on `localhost:8080` unless the port is
   taken (`--no-port-forward` skips it). It never stops a process it did not
   start.

`teardown.sh` removes what deploy.sh installed. It keeps the CRD and your
policies unless you pass `--delete-policies`, removes OpenCost or Kubecost
only if deploy.sh installed them, and removes demo namespaces only with
`--delete-demo-namespaces`. `make e2e-raw` runs both scripts on a throwaway
kind cluster and checks exactly that.

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

Events persist to disk (`events.jsonl`): no data loss on restart.
