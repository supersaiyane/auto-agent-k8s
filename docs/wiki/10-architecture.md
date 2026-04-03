# Architecture Deep Dive

## Package Structure

```
cmd/auto-agent/main.go          Entry point, dependency wiring, periodic loops
internal/
  alertmanager/client.go         Alertmanager API client
  crd/controller.go              CRD informer + parser
  crd/store.go                   In-memory CRD policy cache
  escalation/escalation.go       PagerDuty, OpsGenie, Email
  events/recorder.go             File-backed event ring buffer
  httpapi/http.go                HTTP server + REST API
  httpapi/terminal.go            kubectl-over-API
  httpapi/cost.go                Cost estimation + Kubecost/OpenCost
  httpapi/resources.go           Pod sizing analysis
  httpapi/slack_actions.go       Slack interactive callback
  httpapi/api_extended.go        Compliance, baselines, deploys, fixes
  httpapi/ui/index.html          Embedded dashboard (go:embed)
  integrations/gitops.go         GitHub/GitLab PR client
  integrations/tickets.go        GitHub Issues/Jira client
  kube/watcher.go                Pod + node informers with node-local filtering
  kube/handlers.go               CrashLoop, OOM, ImagePull, NotReady, Pending
  kube/handlers_extended.go      Init failure, ConfigError, RestartStorm
  kube/pod_extended.go           RunContainerError, DeadlineExceeded, etc.
  kube/nodes.go                  Node pressure + uncordon
  kube/node_extended.go          PIDPressure, NetworkUnavailable, ClockSkew
  kube/nodecheck.go              NodeNotReady from leader
  kube/scaler.go                 Auto-scaling with HPA awareness
  kube/anomalies.go              CRD-driven PromQL evaluation
  kube/workloads.go              Stuck rollout, evicted cleanup, endpoints
  kube/workload_extended.go      StatefulSet, DaemonSet, HPA, CronJob, Paused
  kube/jobs.go                   Failed Job/CronJob detection + cleanup
  kube/quotas.go                 ResourceQuota exhaustion
  kube/pvc.go                    PVC Pending detection
  kube/storage_network.go        PVC Lost, DNS, Ingress, LoadBalancer
  kube/security.go               Cert expiry, RBAC denied, webhook blocking
  kube/auditlog.go               Persistent audit log + blast radius + quiet hours
  kube/guardrails.go             Unified guardrail check (all 8 layers)
  kube/fixtracker.go             Verify actions actually fixed the problem
  kube/correlation.go            Deploy tracker + incident correlation + cost
  kube/compliance.go             MTTR, remediation rate, compliance reports
  kube/learning.go               Baseline collection, threshold auto-tuning
  kube/dryrun.go                 Dry-run simulation logging
  kube/runbook.go                Runbook fetch + execution
  kube/gitops_content.go         Memory bump patch generation
  kube/policycheck.go            CRD per-policy enforcement
  kube/selfcheck.go              Agent self-monitoring
  kube/retention.go              Log file cleanup
  kube/deps.go                   Dependency injection struct
  kube/helpers.go                Shared utilities
  leader/leader.go               Lease-based leader election
  llm/llm.go                    LLM API client
  logging/json.go               Structured JSON logging
  metrics/provider.go            Prometheus query provider
  obs/metrics.go                 Prometheus metric definitions
  policy/policy.go               Configuration from env vars
  policy/reload.go               ConfigMap hot-reload
  ratelimit/ratelimit.go         Deduplicator + ActionLimiter
  ratelimit/circuit.go           CircuitBreaker
  slack/slack.go                 Slack webhook + Block Kit
  storage/storage.go             Filesystem sink
  storage/s3sdk.go               AWS S3 sink
  webhook/validator.go           Admission webhook
```

## Event Flow

```
Pod status change (informer)
    │
    ▼
handlePodUpdate()
    ├── namespace allowed?
    ├── excluded annotation?
    ├── dedup check (workload-level)
    │
    ▼
runHandler() ── semaphore (max 20) + 60s timeout
    │
    ▼
handleCrashLoop() / handleOOM() / etc.
    ├── collect logs + events
    ├── persist to storage
    ├── tryFixAction()
    │       ├── mode check (observe/suggest/fix/dry-run)
    │       ├── checkGuardrails() (quiet/blast/CRD/breaker)
    │       ├── rate limiter
    │       ├── execute action
    │       ├── audit log
    │       └── FixTracker.RecordAction() → pending verification
    ├── LLM diagnosis
    ├── Slack post
    ├── Alertmanager fire
    ├── ticket create
    └── event recorder
```

## Leader vs Per-Node

| Per-Node (all DaemonSet pods) | Leader-Only (one pod) |
|---|---|
| Pod watcher (local node only via NODE_NAME filter) | Auto-scaler (30s) |
| Node pressure handler (cordon/evict) | Anomaly checker (CRD PromQL) |
| Log retention cleanup | Failed job scanner (2min) |
| Config hot-reload | Stuck rollout detector (2min) |
| HTTP server + dashboard | Evicted pod cleanup (2min) |
| | Service endpoint check (2min) |
| | Node health check (2min) |
| | PVC/DNS/cert/RBAC scanner (5min) |
| | Fix verification (30s) |
| | Baseline collection (5min) |
| | Deploy tracker (2min) |
| | Self-monitoring (3min) |

## Data Persistence

| File | Contents | Survives restart |
|------|----------|-----------------|
| `/var/log/auto-agent/events.jsonl` | Dashboard events | Yes (hostPath volume) |
| `/var/log/auto-agent/audit.jsonl` | Action audit log | Yes |
| `/var/log/auto-agent/baselines.json` | Learning mode baselines | Yes |
| `/var/log/auto-agent/<ns>/<workload>/...` | Incident log bundles | Yes |
