# Cost Optimization

## How Cost Estimation Works

The agent calculates monthly cost using:

```
Monthly = (CPU_cores × CPU_price/hr + Memory_GiB × Mem_price/hr) × 730 hrs/month
```

### Pricing Sources (priority order)

1. **Kubecost API** — real cluster costs (most accurate)
2. **OpenCost API** — open-source alternative
3. **Instance-type lookup** — reads `node.kubernetes.io/instance-type` label, matches against 40+ built-in prices
4. **Manual rates** — `COST_CPU_PER_HOUR`, `COST_MEM_PER_GIB_HOUR`
5. **Default** — $0.05/vCPU/hr, $0.005/GiB/hr

### Built-in Instance Prices

AWS (us-east-1 on-demand): t3, t3a, m5, m5a, m6i, m6g, m7g, c5, c6g, c6i, c7g, r5, r6g, r6i
GCP: n2-standard, e2-medium/standard, n2d-standard
Azure: Standard_B2s, B2ms, D2s_v3, D4s_v3, D8s_v3, D2as_v4

## Reading the Cost Tab

| Metric | What it means |
|--------|---------------|
| **Total Monthly** | Full cluster node cost |
| **Workload Cost** | Sum of all pod resource requests |
| **Wasted** | Total - Workload = idle capacity you're paying for |
| **Efficiency** | Workload / Total × 100. Under 50% = significant waste |

## Identifying Waste

1. **Node Cost tab**: Look for nodes with low CPU% and Mem%. High cost but low utilization = overpaying
2. **Top Workloads**: Sort by $/mo — are the most expensive workloads actually critical?
3. **Resources tab**: Find pods with `overuse` or `no-limits` — they reserve more than they need

## Right-Sizing Recommendations

The Resources tab classifies each pod:

| Classification | Meaning | Action |
|---|---|---|
| **right-sized** | Limits are 1-5× requests | Good — no action needed |
| **overuse** | No requests, or limits >5× requests | Reduce limits closer to actual usage |
| **underuse** | Very low requests with high limits | Increase requests to prevent throttling |
| **no-limits** | No CPU/memory limits set | Add limits — pod can exhaust node resources |

## Cost Reduction Strategies

1. **Right-size pods**: Use Resources tab advice to adjust requests/limits
2. **Spot/preemptible nodes**: Move non-critical workloads to cheaper nodes
3. **Scale down idle**: Agent's auto-scaler reduces replicas when CPU is low
4. **Clean up evicted pods**: Agent automatically deletes Failed/Evicted pods
5. **Remove unused services**: Check Cluster tab for namespaces with 0 running pods
