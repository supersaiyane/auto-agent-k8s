# All 74 Issue Detectors

Auto-Agent detects every category of Kubernetes failure. Each detector has a severity, detection method, timing, and remediation action.

## Pod-Level Issues (18)

| # | Issue | Severity | Detection | Timing | Action (fix mode) |
|---|-------|----------|-----------|--------|-------------------|
| 1 | CrashLoopBackOff | Critical | Container waiting reason | Event-driven, <30s | Delete pod (controller recreates) |
| 2 | OOMKilled | Critical | LastTerminationState.Terminated.Reason | Event-driven | Open GitOps PR to bump memory limit |
| 3 | ImagePullBackOff | Critical | Container waiting reason | Event-driven | Delete pod to retry pull |
| 4 | ErrImagePull | Critical | Container waiting reason | Event-driven | Delete pod to retry pull |
| 5 | CreateContainerConfigError | Critical | Container waiting reason | Event-driven | Alert — missing ConfigMap/Secret |
| 6 | Init:Error | Warning | InitContainerStatuses waiting | Event-driven | Delete pod to retry init |
| 7 | Init:CrashLoopBackOff | Warning | InitContainerStatuses waiting | Event-driven | Delete pod to retry init |
| 8 | Pending (unschedulable) | Warning | Pod phase Pending > 5min | Event-driven | Alert with diagnosis (resources/affinity/PVC) |
| 9 | NotReady (probe failing) | Warning | Running but !Ready > 3min | Event-driven | Delete pod to restart |
| 10 | Restart storm | Warning | RestartCount >= 5, not yet BackOff | Event-driven | Alert — heading toward CrashLoop |
| 11 | RunContainerError | Critical | Container waiting reason | Event-driven | Alert — bad entrypoint/binary |
| 12 | ContainerCannotRun | Critical | Container waiting reason | Event-driven | Alert — permission/security context |
| 13 | PostStartHookError | Warning | Container waiting reason | Event-driven | Alert — lifecycle hook failing |
| 14 | InvalidImageName | Critical | Container waiting reason | Event-driven | Alert — malformed image reference |
| 15 | ErrImageNeverPull | Warning | Container waiting reason | Event-driven | Alert — image not pre-loaded |
| 16 | DeadlineExceeded | Warning | Pod phase Failed, reason DeadlineExceeded | 2min scan | Alert — activeDeadlineSeconds exceeded |
| 17 | EphemeralStorageFull | Warning | Pod phase Failed, evicted for ephemeral-storage | 2min scan | Alert — container filling emptyDir |
| 18 | Evicted pods | Info | Pod phase Failed, reason Evicted | 2min scan | Clean up (delete evicted pods) |

## Node-Level Issues (8)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 19 | MemoryPressure | Critical | Node condition change | Event-driven | Cordon + evict non-critical pods |
| 20 | DiskPressure | Critical | Node condition change | Event-driven | Cordon + evict non-critical pods |
| 21 | NodeNotReady | Critical | Node Ready condition False | 2min scan (leader) | Alert from leader (works when node's agent is down) |
| 22 | PIDPressure | Critical | Node condition | 2min scan | Alert — too many processes |
| 23 | NetworkUnavailable | Critical | Node condition | 2min scan | Alert — CNI plugin issue |
| 24 | ContainerRuntimeDown | Critical | Ready=False, reason KubeletNotReady/ContainerRuntimeNotReady | 2min scan | Alert — kubelet/containerd crash |
| 25 | CordonedForgotten | Info | Node Unschedulable but Ready (not by us) | 2min scan | Alert — forgotten maintenance cordon |
| 26 | ClockSkew | Warning | Last heartbeat >5min old but node claims Ready | 2min scan | Alert — NTP sync issue |

## Workload-Level Issues (10)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 27 | ProgressDeadlineExceeded | Critical | Deployment condition | 2min scan | Auto-rollback to previous revision |
| 28 | Failed Job | Warning | Job condition Failed=True | 2min scan | Alert + clean up old CronJob children |
| 29 | Service 0 endpoints | Critical | Endpoints object empty | 2min scan | Alert — traffic blackhole |
| 30 | StatefulSet stuck | Warning | ReadyReplicas < desired | 2min scan | Alert — ordered startup issue |
| 31 | DaemonSet missing pods | Warning | DesiredScheduled > NumberReady | 2min scan | Alert — taint/resource issue |
| 32 | HPA maxed out | Warning | CurrentReplicas >= MaxReplicas | 2min scan | Alert — at scaling ceiling |
| 33 | HPA scaling failed | Warning | ScalingActive condition False | 2min scan | Alert — metrics unavailable |
| 34 | CronJob missed schedule | Warning | Missed schedule events | 2min scan | Alert — concurrency blocking |
| 35 | Deployment paused | Info | Spec.Paused = true | 2min scan | Alert — forgotten pause |
| 36 | ReplicaSet failure | Critical | ReplicaFailure condition | 2min scan | Alert — quota/admission blocking |

## Storage Issues (7)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 37 | PVC Pending | Warning | PVC phase Pending > 5min | 5min scan | Alert with StorageClass info |
| 38 | PVC Lost | Critical | PVC phase Lost | 5min scan | Alert — data at risk |
| 39 | StorageClass not found | Critical | PVC references non-existent SC | 5min scan | Alert |
| 40 | Volume attach stuck | Critical | Pod events with AttachVolume/Multi-Attach | 5min scan | Alert — wrong AZ or multi-attach |
| 41 | Ephemeral storage full | Warning | Pod evicted for ephemeral-storage | 2min scan | Alert |

## Networking Issues (7)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 42 | DNS down | Critical | All CoreDNS pods not ready | 5min scan | Alert — cluster DNS failure |
| 43 | DNS degraded | Warning | Some CoreDNS pods not ready | 5min scan | Alert |
| 44 | LoadBalancer pending | Warning | LB service no external IP > 5min | 5min scan | Alert — cloud LB issue |
| 45 | Ingress backend missing | Critical | Ingress backend service doesn't exist | 5min scan | Alert |
| 46 | Ingress no backends | Critical | Ingress backend has 0 endpoints | 5min scan | Alert — 502/503 for users |

## Security & Config Issues (12)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 47 | TLS cert expired | Critical | Parse tls.crt from TLS secrets | 5min scan | Alert |
| 48 | TLS cert expiring (<30d) | Warning | Parse tls.crt expiry | 5min scan | Alert |
| 49 | RBAC denied | Warning | Events with "forbidden" | 5min scan | Alert |
| 50 | LimitRange violation | Warning | Events with FailedCreate + LimitRange | 5min scan | Alert |
| 51 | API server throttled | Warning | Events with TooManyRequests | 5min scan | Alert |
| 52 | Webhook blocking | Warning | Events with admission denied | 5min scan | Alert |
| 53 | ResourceQuota exhausted | Warning | Quota > 90% usage | 5min scan | Alert before pods fail to schedule |

## Additional Detection (via CRD)

| # | Issue | Severity | Detection | Timing | Action |
|---|-------|----------|-----------|--------|--------|
| 54-74 | Custom anomalies | Configurable | PromQL queries in AutoRemediationPolicy | 30s scan | Alert when threshold breached |
