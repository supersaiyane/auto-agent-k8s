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
