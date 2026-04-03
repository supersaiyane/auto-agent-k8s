# Dashboard & UI Guide

The dashboard is embedded in the agent binary — no separate deployment needed. Access at `http://localhost:8080` via port-forward.

## Layout

### Top Bar
- **Version** badge, **mode** badge (observe/suggest/fix/dry-run), **leader/follower** badge, **node name**

### Stat Boxes (clickable)
Five boxes across the top. **Clicking any box filters the Events tab** to show only that type:

| Box | Shows | Click → |
|-----|-------|---------|
| Total Events | All events count | Unfiltered events |
| Incidents | Incident count (red) | Only incidents |
| Actions | Action count (green) | Only actions |
| Scaling | Scaling count (purple) | Only scaling decisions |
| Uptime | Live HH:MM:SS ticker | - |

### Tabs

| Tab | What it shows |
|-----|---------------|
| **Events** | Filtered event feed — filtered by stat box selection |
| **K8s Events** | Raw Kubernetes events with namespace + Warning/Normal filter |
| **Actions** | Verified fixes, pending verification, not fixed — with clickable sub-filters |
| **Charts** | 3 pie charts + 3 bar charts — visual cluster health |
| **Report** | Incident report: by service, by issue type — clickable rows jump to filtered events |
| **Cluster** | Namespace overview → click to drill into pods/deploys/services |
| **Nodes** | Node health cards with CPU, memory, pod count, pressure |
| **Cost** | Monthly cost: per-node, per-namespace, per-workload |
| **Resources** | Pod sizing: right-sized / overuse / underuse / no-limits with advice |
| **Terminal** | kubectl commands via API (get, describe, logs, version) |

## Charts Tab

### Pie Charts
- **Events by Type** — incidents (red) / actions (green) / scaling (purple) / info (blue)
- **Remediation Status** — fixed (green) / pending (yellow) / failed (red)
- **Pod Sizing** — right-sized / overuse / underuse / no-limits

### Bar Charts
- **Top 10 Issues by Reason** — CrashLoopBackOff, ImagePullBackOff, etc. ranked
- **Events by Namespace** — which namespaces have the most issues
- **Monthly Cost by Namespace** — dollar spend visualization

## Report Tab

### Summary Boxes (clickable)
Click any box to filter the section below:
- **Total Incidents** → shows service table + issue breakdown
- **Actions Taken** → shows service table only
- **Verified Fixed** → shows fix list
- **Pending** → shows pending verification list
- **Not Fixed** → shows failed remediation list

### Cross-Tab Navigation
- Click an **issue type pill** (e.g., CrashLoopBackOff x30) → jumps to Events tab showing only that reason
- Click a **service row** → jumps to Events tab showing only that workload's events
- **Back button** appears to return to the unfiltered view

## Actions Tab

Sub-filters:
| Box | Shows when clicked |
|-----|-------------------|
| All | Everything |
| Verified Fixed | Only confirmed fixes (green border) |
| Pending | Waiting for verification (yellow border) |
| Not Fixed | Failed remediations (red border) |

## Terminal Tab

Type kubectl commands directly:
```
$ kubectl get pods -n default
$ kubectl describe deploy api-server -n default
$ kubectl logs api-server-xxx -n default --tail 20
$ kubectl get all -n kube-system
$ kubectl get nodes
$ kubectl version
$ help
```

Read-only only — no create/delete/apply from the terminal.

## API Reference

See [API Reference](16-api-reference.md) for all 17 endpoints.
