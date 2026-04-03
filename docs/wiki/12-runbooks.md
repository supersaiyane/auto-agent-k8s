# Operator Runbooks

For each issue type, what the agent does automatically and what requires manual action.

## CrashLoopBackOff

**Agent does**: Collects logs (50 lines), events, persists to storage, deletes pod.
**Manual**: Read the logs to find the root cause. Common causes:
- Missing env vars or config → check ConfigMap/Secret
- DB connection failure → check DB is running and credentials
- Permission error → check RBAC, file permissions, security context
- OOM without explicit OOMKill → check for memory leaks
- Exit code 137 → killed by kernel (OOM) or liveness probe

## OOMKilled

**Agent does**: Logs memory limit, opens GitOps PR to bump by 20-50%.
**Manual**:
- Profile memory usage: `kubectl top pod <name>`
- Check for memory leaks (heap dumps, pprof)
- Is the limit too low or is the app actually leaking?
- Consider: are all replicas OOMing or just one? (shared cache issue?)

## ImagePullBackOff

**Agent does**: Logs image name, deletes pod to retry.
**Manual**:
- Image exists? `docker pull <image>` locally
- Registry credentials: `kubectl get secret -n <ns>` → check imagePullSecrets
- Private registry: create `docker-registry` secret
- Typo in image name/tag? Check deployment spec

## Pending Pod

**Agent does**: Diagnoses reason (Insufficient resources, affinity, PVC).
**Manual by reason**:
- **Insufficient cpu/memory**: Scale node pool or reduce requests
- **node affinity/selector**: Check labels match available nodes
- **PVC**: See PVC Pending runbook below
- **Taint**: Add toleration or remove taint

## Node Pressure

**Agent does**: Cordons node, evicts non-critical pods, uncordons on recovery.
**Manual**:
- **Memory**: Check for pods without limits, memory leaks, log accumulation
- **Disk**: `df -h` on node, clean up container images: `crictl rmi --prune`
- **PID**: `ps aux | wc -l`, look for fork bombs or sidecar spawning

## Stuck Rollout

**Agent does**: Detects ProgressDeadlineExceeded, rolls back to previous revision.
**Manual**: If rollback also fails:
- Check new image exists and pulls successfully
- Check readiness probe — is the endpoint correct?
- Check resource requests — can the cluster schedule the new pods?
- `kubectl describe deploy <name>` for conditions

## Failed Job

**Agent does**: Collects pod logs, creates ticket, cleans up old failed CronJob children.
**Manual**:
- Read job pod logs: `kubectl logs job/<name>`
- Check backoffLimit — is it too low?
- Check command/args — is the binary correct?
- CronJob: check `concurrencyPolicy` and `startingDeadlineSeconds`

## PVC Pending

**Agent does**: Alerts with StorageClass name and size.
**Manual**:
- `kubectl get sc` — does the StorageClass exist?
- Is the provisioner running? (check CSI driver pods)
- Is there capacity? (check cloud provider quotas)
- Access mode issue? (ReadWriteMany not supported by all provisioners)

## Certificate Expired

**Agent does**: Alerts with subject, expiry date, secret name.
**Manual**:
- Using cert-manager? Check Certificate resource and Issuer
- Manual cert: renew and update the Secret
- `kubectl get cert -A` (if cert-manager)
- Check if the cert is for the right domain

## CordonedForgotten

**Agent does**: Alerts when a node is Ready but cordoned (unschedulable) and wasn't cordoned by the agent itself.
**Manual**:
- Was this intentional maintenance? If done, uncordon: `kubectl uncordon <node>`
- Check if a drain operation was interrupted
- The agent uses `auto-agent.io/cordoned` annotation to track nodes it cordoned — it won't alert on those

## ClockSkew

**Agent does**: Alerts when a node's last heartbeat is >5 minutes old but the node claims Ready status.
**Manual**:
- SSH to the node and check NTP: `timedatectl status`
- Restart chrony/ntpd: `systemctl restart chronyd`
- TLS certificates may fail with clock drift — check HTTPS services on the node
- If kubelet restarts, heartbeat resets

## DNS Down/Degraded

**Agent does**: Checks CoreDNS pods in kube-system. Alerts if all pods down (critical) or partially down (warning).
**Manual**:
- `kubectl get pods -n kube-system -l k8s-app=kube-dns`
- Check CoreDNS logs: `kubectl logs -n kube-system -l k8s-app=kube-dns`
- Verify DNS resolves: `kubectl run test --rm -i --image=busybox -- nslookup kubernetes.default`
- Check if CoreDNS ConfigMap has a loop (resolv.conf pointing to itself)

## Ephemeral Storage Full

**Agent does**: Detects pods evicted for ephemeral-storage usage.
**Manual**:
- Check which container filled the volume: `kubectl describe pod <name>` → Last State
- Reduce log output or redirect to external logging
- Increase `ephemeral-storage` limit in pod spec
- Clean up temp files in the container

## Automated Runbook Execution

If a CRD policy has `runbookURL`, the agent fetches and executes it:
```yaml
spec:
  escalation:
    runbookURL: "https://wiki.internal/runbooks/api.json"
```

The agent:
1. Fetches the JSON runbook from the URL
2. Executes each step via the kubectl API (read-only commands only)
3. Checks `expect` field against output (substring match)
4. Reports pass/fail per step in the Slack message
5. Records as `RunbookExecuted` event in the dashboard
