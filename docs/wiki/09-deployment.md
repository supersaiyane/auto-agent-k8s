# Deployment with the raw manifests

The Helm chart is the deployment of record: install and upgrade with it as
described in [GUIDE section 4](../GUIDE.md#4-deploy-to-a-real-cluster). This
page owns the other path, `deployment/deploy.sh`, which applies manifests
generated from that chart and is meant for kind, minikube and quick trials.

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

| `deploy.sh` | One-shot: build, load, apply, port-forward |
| `teardown.sh` | Removes only what deploy.sh installed |

Files `01` to `04` are generated from the chart by `make manifests`; `make
verify` fails if they drift.

### What deploy.sh does

1. Checks cluster access (`--context NAME` picks a kubectl context).
2. Builds `auto-agent:latest` and loads it into kind or minikube. Any other
   cluster needs a pushed image: `--image REGISTRY/auto-agent@sha256:...`.
3. Applies the namespace, CRD, RBAC and ConfigMap. The demo fix namespaces
   (`test1`, `test2`, `chaos`) it has to create are labelled
   `auto-agent.io/demo=true`; every other namespace is watched only.
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

## Image loading

| Cluster | How |
|---|---|
| kind | `deploy.sh` runs `kind load docker-image` |
| minikube | `deploy.sh` runs `minikube image load` |
| anything else | Push the image and pass `--image REGISTRY/auto-agent@sha256:...` |

## Upgrade

Re-run `deploy.sh` with the new image. It re-applies the generated manifests
and keeps the Secret, the dashboard's fix scope choice (`auto-agent-scope`)
and the event history (`events.jsonl` on the controller).
