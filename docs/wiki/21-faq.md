# FAQ

## General

**Q: What happens if the agent itself crashes?**
A: The DaemonSet controller restarts it. Events persist to disk (`events.jsonl`). Leader election transfers in ~15s. No data loss.

**Q: Can I run it in one namespace only?**
A: Yes. Set `NAMESPACE_ALLOWLIST: "my-namespace"`. The agent ignores everything else.

**Q: Does it work with Istio/Linkerd service mesh?**
A: Yes. The agent watches pod status via the K8s API, not network traffic. Sidecar containers are included in the container status checks.

**Q: Does it support OpenShift?**
A: It should work on any K8s-compatible cluster. OpenShift may need additional RBAC/SCC configuration.

**Q: What K8s versions are supported?**
A: Built against client-go v0.30 (K8s 1.30). Should work on 1.26+.

## Detection

**Q: Why is the agent not fixing my pods?**
A: Check in order: (1) Is `AUTO_MODE` set to `fix`? (2) Is the namespace in `NAMESPACE_ALLOWLIST`? (3) Does the pod have `auto-agent.io/disable` annotation? (4) Check `kubectl logs -n auto-agent -l app=auto-agent` for "BLOCKED" messages.

**Q: Why does it detect everything as ImagePullBackOff?**
A: Your cluster can't pull the image (registry unreachable, no credentials). Fix the underlying image pull issue first.

**Q: Why does Pending detection take 5 minutes?**
A: Intentional threshold. Many pods are Pending briefly during normal scheduling. 5 minutes filters out transient scheduling delays.

**Q: Can I change the detection thresholds?**
A: Some are configurable (CPU threshold, cooldowns). Others are hardcoded (3min NotReady, 5min Pending, 5 restarts for storm). Configurable thresholds are in the env vars.

## Safety

**Q: Can the agent make things worse?**
A: The 8-layer safety system prevents cascading failures. Circuit breaker stops after 5 actions on the same workload. Blast radius limits to 5 namespaces/hour. Start in `observe` mode to build confidence.

**Q: How do I disable the agent for one deployment?**
A: Add annotation: `auto-agent.io/disable: "true"` to the pod template.

**Q: What if I want human approval before fixes?**
A: Create a CRD policy with `requireApproval: true`. The agent will detect and alert but not act.

**Q: Does it respect PodDisruptionBudgets?**
A: Yes. Node pressure eviction uses the Eviction API which returns 429 if PDB would be violated.

## Dashboard

**Q: Why is the dashboard blank?**
A: Usually a stale port-forward. Kill it and restart: `kubectl port-forward -n auto-agent svc/auto-agent 8080:8080`. Hard refresh browser with `Cmd+Shift+R`.

**Q: How do I access the dashboard from outside the cluster?**
A: The Service is NodePort on 30080. Or use an Ingress/LoadBalancer.

**Q: Does the dashboard data survive pod restarts?**
A: Yes. Events are persisted to `events.jsonl` on the hostPath volume and reloaded on startup.

## Cost

**Q: How accurate is the cost estimation?**
A: With built-in pricing: approximate (within 20%). With Kubecost/OpenCost: highly accurate (real cloud billing data). Set `COST_PROVIDER` to improve accuracy.

**Q: Does it include storage and network costs?**
A: Currently compute only (CPU + memory). Kubecost integration includes PV costs.

## Scaling

**Q: Does auto-scaling work without Prometheus?**
A: No. Scaling requires `METRICS_PROVIDER: "prometheus"` with a Prometheus instance that has container CPU metrics.

**Q: Will it fight with my HPA?**
A: No. When `HPA_COEXISTENCE: "true"` (default), the agent checks for HPAs and skips scaling on any deployment that has one.

## Performance

**Q: How much CPU/memory does the agent use?**
A: ~50m CPU, ~128Mi memory per pod. Configured in the DaemonSet resource requests.

**Q: Does it add load to the API server?**
A: Minimal. Pod watching is event-driven (not polling). Periodic scans run every 2-5 minutes and use list calls with field selectors.
