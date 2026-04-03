# Auto-Agent for Kubernetes — Overview

## What is Auto-Agent?

Auto-Agent is an autonomous Kubernetes remediation system that runs as a DaemonSet on every node in your cluster. It continuously monitors pods, nodes, deployments, storage, networking, and security — detecting 74 distinct failure conditions and automatically fixing them.

Think of it as an **always-on SRE** that watches your cluster 24/7, catches problems before they page you, and fixes what it can while escalating what it can't.

## Problem It Solves

| Without Auto-Agent | With Auto-Agent |
|---|---|
| Pod crashes at 3am → PagerDuty fires → engineer wakes up → investigates → deletes pod → goes back to sleep | Pod crashes at 3am → agent detects in 30s → deletes pod → controller recreates → verifies healthy → logs it → you read the report in the morning |
| OOM kills happen silently → app degrades → users complain | OOM detected → memory limit logged → GitOps PR opened to bump limits → ticket created → Slack alert with LLM diagnosis |
| Node runs out of disk → pods evicted → cascade failure | Disk pressure detected → node cordoned → non-critical pods evicted gracefully → uncordoned when pressure resolves |
| Someone deploys bad image → stuck rollout → manual rollback | ProgressDeadlineExceeded detected → auto-rollback to previous revision → Slack notification |

## Architecture

```
                    ┌─────────────────────────┐
                    │    Dashboard UI :8080    │
                    │  Events │ Charts │ Cost  │
                    │  Report │ Terminal │ ... │
                    └────────────┬────────────┘
                                 │
┌────────────────────────────────┴────────────────────────────────┐
│                     auto-agent DaemonSet                        │
│                                                                 │
│  Per-Node (all pods):              Leader-Only (one pod):       │
│  ├─ Pod watcher (local node)       ├─ Auto-scaler (30s)        │
│  ├─ Node pressure handler          ├─ Anomaly checker (CRD)    │
│  └─ Log retention cleanup          ├─ Failed job scanner       │
│                                    ├─ Stuck rollout detector   │
│  Shared:                           ├─ PVC/DNS/cert checker     │
│  ├─ Rate limiter + deduplicator    ├─ Resource quota monitor   │
│  ├─ Circuit breaker                ├─ Fix verification         │
│  ├─ Blast radius tracker           ├─ Baseline collection      │
│  ├─ CRD policy controller         └─ Self-monitoring           │
│  ├─ Config hot-reload                                          │
│  └─ Event persistence                                          │
└─────────────────────────────────────────────────────────────────┘
        │          │          │          │          │
   ┌────┴──┐ ┌────┴──┐ ┌────┴──┐ ┌────┴──┐ ┌────┴──┐
   │ Slack │ │  LLM  │ │S3/EFS │ │GitHub │ │Alert- │
   │       │ │Diagnos│ │ Logs  │ │/GitLab│ │manager│
   └───────┘ └───────┘ └───────┘ │/Jira  │ │/PD/OG │
                                 └───────┘ └───────┘
```

## Feature Summary

| Category | Count | Capabilities |
|----------|-------|-------------|
| **Detection** | 74 | Every K8s issue across pod, node, workload, storage, network, security, config |
| **Actions** | 7 | Pod delete, node cordon/uncordon, rollback, eviction cleanup, job cleanup, scaling |
| **Safety** | 8 layers | Dry-run, rate limiter, dedup, circuit breaker, blast radius, quiet hours, CRD approval, HPA awareness |
| **Integrations** | 9 | Slack, LLM, GitHub/GitLab PRs, GitHub Issues/Jira, Alertmanager, PagerDuty, OpsGenie, Email |
| **Intelligence** | 4 | Learning mode, incident correlation, cost estimation, compliance reporting |
| **Dashboard** | 10 tabs | Events, K8s Events, Actions, Charts, Report, Cluster, Nodes, Cost, Resources, Terminal |
| **APIs** | 17 | Full REST API for all data |

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.22 |
| K8s Client | client-go v0.30 |
| Container | Alpine 3.19 |
| Charts | Helm 3 |
| Metrics | Prometheus client_golang |
| Storage | AWS SDK v2 (S3) / filesystem |
| CI/CD | GitHub Actions |

## Project Stats

- **65 Go files** | **10,800+ lines** | **18 packages**
- **74 issue detectors** | **7 remediation actions**
- **17 REST API endpoints** | **10 dashboard tabs**
- **8 safety layers** | **9 integrations**
