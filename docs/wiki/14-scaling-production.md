# Scaling for Production

## Running at 100+ Nodes

### Pod Informer Scope
Each DaemonSet pod watches **only its own node's pods** via `NODE_NAME` field selector. At 100 nodes, this means:
- Each pod tracks ~50 pods (not 5000)
- No redundant API server connections
- Memory scales O(pods_per_node), not O(total_pods)

### Leader-Only Operations
Cluster-wide scanning (scaling, jobs, endpoints, quotas, PVCs, certs) runs only on the **leader pod**. One API server connection for all periodic scans.

### API Server Load

| Operation | Frequency | API calls |
|-----------|-----------|-----------|
| Pod informer (per node) | Event-driven | 1 watch connection per node |
| Node informer | Event-driven | 1 watch connection (shared) |
| Scaling loop | 30s | ~N deployments × 2 (list + update) |
| Workload scan | 2min | ~10 list calls across namespaces |
| Security scan | 5min | ~5 list calls |

**Tuning**: Increase scan intervals via periodic loop ticker values in `main.go`. For 500+ node clusters, consider 5min for workload scans.

### Rate Limiter Tuning

| Cluster Size | Recommended `MAX_ACTIONS_PER_10M` |
|---|---|
| Small (<20 nodes) | 10 (default) |
| Medium (20-100 nodes) | 30 |
| Large (100+ nodes) | 50-100 |

### Circuit Breaker Tuning

Default: 5 actions per workload per hour. For clusters with frequent deployments, increase to 10-15.

### Memory Usage

| Component | Per-pod memory |
|-----------|---------------|
| Pod informer cache | ~1MB per 100 pods on node |
| Event recorder | ~2MB (500 events) |
| CRD policy cache | ~100KB |
| Base agent | ~30MB |
| **Total** | **~35-50MB per pod** |

## High Availability

The DaemonSet ensures one agent pod per node. If a node goes down:
- That node's pod watcher stops (expected)
- Leader election transfers to another pod in ~15s
- Leader detects NotReady node from remaining pods
- Self-healing: no manual intervention needed

## Multi-Cluster (future)

Currently single-cluster. For multi-cluster:
- Deploy auto-agent DaemonSet in each cluster
- Point all dashboards to a shared Prometheus/Grafana
- Use separate namespaces/config per cluster
