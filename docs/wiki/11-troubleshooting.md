# Troubleshooting

## Agent Not Starting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `ErrImageNeverPull` | Image not on node | Load via `docker save \| docker exec ctr import` |
| `CrashLoopBackOff` on agent | Config error / missing secret | Check: `kubectl logs -n auto-agent -l app=auto-agent` |
| `permission denied` on logs | readOnlyRootFilesystem + non-root user | Remove securityContext restrictions or mount writable volume |
| `in-cluster config: unable to load` | Not running inside K8s | Agent must run in-cluster (not locally) |

## Agent Not Detecting Issues

| Symptom | Check |
|---------|-------|
| No events in dashboard | Is the namespace in `NAMESPACE_ALLOWLIST`? |
| Pod crashing but no alert | Does pod have `auto-agent.io/disable` annotation? |
| Events appear but no actions | Is `AUTO_MODE` set to `fix`? (not `observe`) |
| Only ImagePullBackOff detected | Cluster can't pull images — fix registry access first |
| Detection delayed >5 min | Pending/NotReady have intentional thresholds (5min/3min) |

## Dashboard Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| Dashboard blank, "Connecting..." | JS syntax error in embedded HTML | Hard refresh `Cmd+Shift+R`, check browser console |
| Port-forward dies | Agent restarted (kills port-forward) | Restart: `kubectl port-forward -n auto-agent svc/auto-agent 8080:8080` |
| Data disappears on restart | Events not persisted | Check `events.jsonl` file on hostPath volume |
| Stats show `-` | API not reachable | Verify: `curl http://localhost:8080/api/status` |
| Charts empty | No events yet | Deploy test apps to generate data |

## Actions Not Taken

| Symptom | Check | Fix |
|---------|-------|-----|
| "Blocked: quiet hours" | `QUIET_HOURS` is set | Wait or clear the env var |
| "Blocked: blast radius" | >5 namespaces affected | Wait 1 hour or increase limit |
| "Blocked: requires approval" | CRD `requireApproval: true` | Set to false or handle manually |
| "Circuit breaker tripped" | >5 actions on same workload | Wait 1 hour, investigate root cause |
| "Rate limited" | >10 actions in 10 min | Wait or increase `MAX_ACTIONS_PER_10M` |
| No action log at all | Mode is `observe` or `suggest` | Set `AUTO_MODE: "fix"` |

## Fix Not Verified

| Symptom | Cause | Fix |
|---------|-------|-----|
| "Pending" forever | FixTracker checks old RS name | Fixed in latest — update agent |
| "Not Fixed" after 15min | Pod didn't recover | Root cause still exists — manual investigation needed |
| Fix verified but pod crashes again | Underlying issue not resolved | The delete only clears backoff; fix the actual bug |

## Leader Election

| Symptom | Check |
|---------|-------|
| Only one pod does scaling/scanning | That's correct — leader-only operations |
| "attempting to acquire leader lease" loops | Normal for non-leader pods |
| Both pods claim leader | Shouldn't happen — check lease in kube-system |

```bash
# Check leader
kubectl get lease auto-agent-leader -n kube-system -o jsonpath='{.spec.holderIdentity}'
```

## Debug Commands

```bash
# Agent logs
kubectl logs -n auto-agent -l app=auto-agent --tail=200 -f

# Check config
kubectl get cm auto-agent-config -n auto-agent -o yaml

# Check events persisted
kubectl exec -n auto-agent -l app=auto-agent -- cat /var/log/auto-agent/events.jsonl | wc -l

# Check audit log
kubectl exec -n auto-agent -l app=auto-agent -- cat /var/log/auto-agent/audit.jsonl

# API health
curl -s http://localhost:8080/api/status | python3 -m json.tool

# Force restart
kubectl rollout restart ds/auto-agent -n auto-agent
```
