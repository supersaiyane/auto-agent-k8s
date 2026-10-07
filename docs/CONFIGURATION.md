# Configuration reference

Every setting the agent reads, with its Helm value, allowed values, default
and effect. Built from the code on 2026-10-07.

All variables are read in one place, `config.Load` in `internal/config`
(no other code calls `os.Getenv`; `TestNoEnvReadsOutsideConfig` enforces it).
`TestConfigReference_MatchesCodeAndChart` (in `internal/config`) keeps this
file honest: it fails if `config.Load` reads a variable that is not listed here,
if a variable listed here is no longer read, or if the chart sets a variable
that is neither read nor listed under
[Set by the chart but not read](#set-by-the-chart-but-not-read).

## How configuration reaches the agent

1. You set **Helm values** (`values.yaml` or `--set`).
2. The chart writes them into the **ConfigMap** `auto-agent-config` and the
   **Secret** `auto-agent-secrets` (both in the agent's namespace).
3. The DaemonSet loads both as **environment variables**. Anything in the
   chart's `env:` list is added on top and wins over the ConfigMap.
4. The agent reads the variables at startup. It has **no command-line flags**.

**Live reload.** These ConfigMap keys take effect within seconds, without a
restart: `AUTO_MODE`, `SCALE_CPU_THRESHOLD`, `MAX_SCALE_STEP`, `MAX_REPLICAS`,
`MIN_REPLICAS`, `COOLDOWN_UP`, `COOLDOWN_DOWN`, `NAMESPACE_ALLOWLIST`. An
invalid `AUTO_MODE` is ignored and the old mode stays. Every other key needs a
pod restart (`kubectl -n kube-system rollout restart ds/auto-agent`). Adding a
namespace to the allowlist also needs `helm upgrade`, which creates its write
Role.

**Reading the tables.** "env only" means there is no Helm value; set it with
the chart's `env:` list:

```yaml
env:
  - name: AUDIT_LOG_PATH
    value: /var/log/auto-agent/audit.jsonl
```

"Secret" means the chart stores it in `auto-agent-secrets`.

---

## Mode and scope

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `AUTO_MODE` | `agent.mode` | `observe`, `suggest`, `dry-run`, `fix` | `dry-run` | `observe`: alert only. `suggest`: alert and describe the fix. `dry-run`: record the exact fix in the dry-run log. `fix`: apply it through the gate. Invalid at startup means `observe`; invalid on reload is ignored. Live reload. | `internal/policy/policy.go` |
| `NAMESPACE_ALLOWLIST` | `agent.namespaceAllowlist` | comma-separated namespace names | `default` | The only namespaces the agent reads and acts in, dashboard included. Each must exist at install (it gets a write Role). Live reload. | `internal/policy/policy.go` |
| `EXCLUDED_ANNOTATION` | `agent.excludedAnnotation` | annotation key | `auto-agent.io/disable` | Pods carrying this annotation are ignored | `internal/policy/policy.go` |
| `DEDUP_TTL_SECONDS` | `agent.dedupTtlSeconds` | positive integer | `300` | The same problem on the same workload is reported once per window | `internal/policy/policy.go` |

## Guardrails

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `MAX_ACTIONS_PER_10M` | `agent.maxActionsPer10m` | positive integer | `10` | Global rate limit: actions per 10 minutes, all workloads. Needs a restart | `internal/policy/policy.go` |
| `BLAST_RADIUS_MAX_NAMESPACES` | `guardrails.blastRadiusMaxNamespaces` | positive integer | `5` | Distinct namespaces acted on per hour before further actions are blocked | `cmd/auto-agent/run.go` |
| `CIRCUIT_BREAKER_THRESHOLD` | `guardrails.circuitBreakerThreshold` | positive integer | `5` | Actions per workload per hour before the breaker trips and alerts | `cmd/auto-agent/run.go` |
| `QUIET_HOURS` | `guardrails.quietHours` | comma-separated `HH:MM-HH:MM` in UTC; windows may cross midnight | empty (none) | No actions inside these windows; detection and alerts continue | `cmd/auto-agent/run.go` |

## Scaling

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `SCALE_CPU_THRESHOLD` | `agent.cpuThreshold` | decimal, e.g. `0.8` | `0.8` | Average CPU utilisation above which to scale up. Scale-down happens below `0.3`. Live reload | `internal/policy/policy.go` |
| `SCALE_WINDOW` | `agent.scaleWindow` | duration, e.g. `5m` | `5m` | Time window for the CPU average | `internal/policy/policy.go` |
| `MAX_SCALE_STEP` | `agent.maxScaleStep` | positive integer | `2` | Replicas added per scale-up (scale-down removes 1). Live reload | `internal/policy/policy.go` |
| `MIN_REPLICAS` | `agent.minReplicas` | positive integer | `1` | Floor for scale-down. Live reload | `internal/policy/policy.go` |
| `MAX_REPLICAS` | `agent.maxReplicas` | positive integer | `50` | Cap for scale-up (a CRD policy can override it per workload). Live reload | `internal/policy/policy.go` |
| `COOLDOWN_UP` | `agent.cooldownUp` | duration | `2m` | Minimum time between scale-ups of one deployment. Live reload | `internal/policy/policy.go` |
| `COOLDOWN_DOWN` | `agent.cooldownDown` | duration | `10m` | Minimum time between scale-downs. Live reload | `internal/policy/policy.go` |
| `HPA_COEXISTENCE` | `agent.hpaCoexistence` | `true`, `false` | `true` | Skip deployments that already have a HorizontalPodAutoscaler | `internal/policy/policy.go` |
| `PROM_QUEUE_DEPTH` | `scalingGates.queueDepthQuery` | PromQL | empty | If any gate is set, scale-up needs CPU over the threshold **and** a gate returning > 0; empty gates: CPU alone decides | `internal/kube/scaler.go` |
| `PROM_ERROR_RATE` | `scalingGates.errorRateQuery` | PromQL | empty | As above | `internal/kube/scaler.go` |
| `PROM_P95_LATENCY` | `scalingGates.p95LatencyQuery` | PromQL | empty | As above | `internal/kube/scaler.go` |

## Metrics source

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `METRICS_PROVIDER` | `metricsProvider.type` | `prometheus`, `metrics-server` | code: `metrics-server`; chart: `prometheus` | Where CPU usage comes from | `internal/metrics/provider.go` |
| `PROMETHEUS_URL` | `metricsProvider.prometheusUrl` | URL | chart: in-cluster `prometheus-server.monitoring` | Prometheus endpoint (also checked by the self check) | `internal/metrics/provider.go`, `internal/kube/selfcheck.go` |

## Loop timing

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `SCALE_INTERVAL` | `loops.scaleInterval` | Go duration, e.g. `30s` | `30s` | Leader loop: scaling, anomalies, fix verification | `cmd/auto-agent/run.go` |
| `JOB_INTERVAL` | `loops.jobInterval` | Go duration | `2m` | Leader loop: jobs, rollouts, workloads, node checks | `cmd/auto-agent/run.go` |
| `QUOTA_INTERVAL` | `loops.quotaInterval` | Go duration | `5m` | Leader loop: quotas, baselines, storage, network, security, webhooks, RBAC | `cmd/auto-agent/run.go` |
| `HEALTH_INTERVAL` | `loops.healthInterval` | Go duration | `3m` | Self check on every pod | `cmd/auto-agent/run.go` |

## Notifications and diagnosis

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `SLACK_WEBHOOK_URL` | `slack.webhookUrl` (Secret) | Slack incoming webhook URL | empty (Slack off) | Incident messages, redacted | `cmd/auto-agent/run.go`, `internal/kube/selfcheck.go` |
| `SLACK_TIMEOUT_SEC` | `agent.slackTimeoutSec` | positive integer | `5` | Timeout for Slack calls | `internal/policy/policy.go` |
| `SLACK_SIGNING_SECRET` | `slack.signingSecret` (Secret) | Slack app signing secret | empty | Verifies Slack button callbacks; without it they get 503 | `internal/httpapi/http.go` |
| `ALERTMANAGER_URL` | `alertmanager.url` | URL | empty (off) | Sends `AutoAgentIncident` and `AutoAgentCircuitBreaker` alerts, annotations redacted | `cmd/auto-agent/run.go`, `internal/kube/selfcheck.go` |
| `LLM_ENABLED` | `llm.enabled` | `true`, `false` | `false` | Adds an LLM diagnosis to incidents; prompts are redacted | `internal/policy/policy.go` |
| `LLM_API_URL` | `llm.apiUrl` | OpenAI-compatible chat completions URL | chart: `https://llm-gateway.internal/v1/chat/completions` | LLM endpoint | `cmd/auto-agent/run.go` |
| `LLM_API_KEY` | none (Secret key, set it in the Secret) | API key | empty (LLM off) | LLM credential; the LLM is used only when enabled, URL and key are all set | `cmd/auto-agent/run.go` |
| `LLM_MODEL` | `llm.model` | model name | chart: `gpt-4o-mini` | Model to ask | `cmd/auto-agent/run.go` |
| `LLM_TIMEOUT_SEC` | `agent.llmTimeoutSec` | positive integer | `10` | Timeout for LLM calls | `internal/policy/policy.go` |

## Tickets and GitOps

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `TICKETS_ENABLED` | `tickets.enabled` | `true`, anything else is off | `false` | Create or update one ticket per incident key | `cmd/auto-agent/run.go` |
| `TICKETS_PROVIDER` | `tickets.provider` | `github`, `jira` | chart: `github` | Ticket backend; any other value means no tickets | `cmd/auto-agent/run.go` |
| `GITHUB_REPO` | `tickets.github.repo` | `owner/repo` | empty | Repository for GitHub issues | `cmd/auto-agent/run.go` |
| `GITHUB_TOKEN` | none (Secret key) | GitHub token | empty | Credential for GitHub issues | `cmd/auto-agent/run.go` |
| `JIRA_BASE_URL` | `tickets.jira.baseUrl` | URL | empty | Jira site | `cmd/auto-agent/run.go` |
| `JIRA_PROJECT_KEY` | `tickets.jira.projectKey` | project key, e.g. `OPS` | empty | Jira project | `cmd/auto-agent/run.go` |
| `JIRA_TOKEN` | none (Secret key) | Jira API token | empty | Jira credential | `cmd/auto-agent/run.go` |
| `JIRA_EMAIL` | none (Secret key) | email | empty | Jira user for the token | `cmd/auto-agent/run.go` |
| `GIT_TOKEN` | none (Secret key) | GitHub or GitLab token | empty (PRs off) | Credential for OOM memory-bump pull requests | `cmd/auto-agent/run.go` |
| `GITOPS_REPO` | `gitops.repo` | `owner/repo` or GitLab project | chart: placeholder | Repository for those pull requests; PRs need both token and repo | `cmd/auto-agent/run.go` |
| `GITOPS_BRANCH` | `gitops.branch` | branch name | chart: `main` | Base branch for the pull requests | `cmd/auto-agent/run.go` |
| `GITOPS_PROVIDER` | `gitops.provider` | `github`, `gitlab` | `github` (anything but `gitlab`) | Pull request host | `cmd/auto-agent/run.go` |

## Logs, audit and retention

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `LOG_STORE` | `logs.store` | `efs`, `s3`, `none` | filesystem when unset or unknown | Where incident log bundles go. `s3` without a bucket, or when S3 fails, falls back to `/var/log/auto-agent/s3mirror` | `internal/storage/storage.go`, `internal/kube/retention.go` |
| `LOG_EFS_PATH` | none (chart always passes `/var/log/auto-agent`) | container path | `/var/log/auto-agent` | Filesystem sink and retention directory. Must stay under `/var/log/auto-agent`, the only writable path. The host directory is `logs.efs.path` | `internal/storage/storage.go`, `internal/kube/retention.go` |
| `LOG_S3_BUCKET` | `logs.s3.bucket` | bucket name | empty | S3 bucket for log bundles | `internal/storage/storage.go` |
| `LOG_S3_PREFIX` | `logs.s3.prefix` | key prefix | empty | Key prefix in the bucket | `internal/storage/storage.go` |
| `LOG_RETENTION_DAYS` | `logs.retentionDays` | positive integer | `7` | Hourly cleanup deletes filesystem log bundles older than this | `internal/kube/retention.go` |
| `AUDIT_LOG_PATH` | env only | path under `/var/log/auto-agent` | `/var/log/auto-agent/audit.jsonl` | Audit log of every action, block and failure (JSON lines) | `cmd/auto-agent/run.go` |
| `LOG_FORMAT` | `logging.format` | `json`, anything else is off | `text` | **Almost no effect yet**: `json` only prints one startup line; the JSON logger is never used, so normal logs stay in klog format (ISS-032) | `internal/config/config.go` |
| `LOG_LEVEL` | `agent.logLevel` | `error`, `warn`, `info` (klog -v 0), `debug` (-v 4), `trace` (-v 6), or a number 0 to 10 | `info` | Sets klog verbosity at startup; anything else keeps the default and logs a warning | `internal/config/config.go` |

## Cost tab

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `KUBECOST_URL` | `cost.kubecostUrl` | URL | empty | Use Kubecost for prices (source `kubecost`) | `internal/httpapi/cost.go` |
| `OPENCOST_URL` | `cost.opencostUrl` | URL | empty | Use OpenCost when there is no Kubecost (source `opencost`) | `internal/httpapi/cost.go` |
| `COST_CPU_PER_HOUR` | `cost.cpuPerHour` | decimal | `0.05` | Price per core-hour; setting it marks the source `manual` | `internal/httpapi/cost.go` |
| `COST_MEM_PER_GIB_HOUR` | `cost.memPerGiBHour` | decimal | `0.005` | Price per GiB-hour | `internal/httpapi/cost.go` |
| `COST_CURRENCY` | `cost.currency` | currency code | `USD` | Currency label | `internal/httpapi/cost.go` |
| `COST_INSTANCE_PRICES` | `cost.instancePrices` | `type=price,...`, e.g. `t3.medium=0.0416` | empty | Per instance-type hourly prices for node cost | `internal/httpapi/cost.go` |

## Security, dashboard and leader election

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `DASHBOARD_TOKEN` | `dashboard.token` (Secret) | any string; use `openssl rand -hex 32` | empty | Bearer token for every `/api/` route. Empty means `/api/` returns 503 | `internal/httpapi/http.go` |
| `TLS_CERT_CHECK` | `rbac.readTLSSecrets` | `true`, anything else is off | `false` | Runs the TLS certificate expiry check; the same value grants the secret-list RBAC it needs | `internal/kube/security.go` |
| `LEADER_LEASE_NAMESPACE` | `leaderElection.namespace` | namespace | `kube-system` | Where the `auto-agent-leader` Lease lives; the chart's lease Role follows it | `cmd/auto-agent/run.go` |

## Admission webhook

The agent serves the webhook (port 8443) only when **both** certificate
variables are set. `webhook.enabled` in the chart creates the webhook
registration and Service but does **not** set them; mount a certificate and
set both with `env:` (ISS-032).

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `WEBHOOK_CERT_FILE` | env only | path to a mounted TLS certificate | empty (webhook off) | Server certificate | `cmd/auto-agent/run.go` |
| `WEBHOOK_KEY_FILE` | env only | path to the matching key | empty (webhook off) | Server key | `cmd/auto-agent/run.go` |
| `WEBHOOK_REQUIRE_LIMITS` | `webhook.requireLimits` | `true`, `false` | on unless `false` | Reject workloads without resource limits | `cmd/auto-agent/run.go` |
| `WEBHOOK_REQUIRE_READINESS` | `webhook.requireReadiness` | `true`, `false` | on unless `false` | Reject workloads without readiness probes | `cmd/auto-agent/run.go` |
| `WEBHOOK_BLOCKED_IMAGES` | `webhook.blockedImages` | comma-separated list of images | empty | Reject workloads using these images | `cmd/auto-agent/run.go` |

## Escalation (not wired)

These are read when the agent starts, but the escalation chain they configure
is never called, so **none of them has any effect yet** (ISS-012).

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `PAGERDUTY_ROUTING_KEY` | `escalation.pagerdutyRoutingKey` (Secret) | routing key | empty | Would page PagerDuty | `internal/escalation/escalation.go` |
| `OPSGENIE_API_KEY` | `escalation.opsgenieApiKey` (Secret) | API key | empty | Would alert OpsGenie | `internal/escalation/escalation.go` |
| `SMTP_HOST` | `escalation.smtpHost` | host | empty | Would send email | `internal/escalation/escalation.go` |
| `SMTP_PORT` | `escalation.smtpPort` | port | chart: `587` | SMTP port | `internal/escalation/escalation.go` |
| `SMTP_USER` | `escalation.smtpUser` | user | empty | SMTP user | `internal/escalation/escalation.go` |
| `SMTP_PASS` | `escalation.smtpPass` (Secret) | password | empty | SMTP password | `internal/escalation/escalation.go` |
| `SMTP_FROM` | `escalation.smtpFrom` | email | empty | Sender | `internal/escalation/escalation.go` |
| `ESCALATION_EMAIL_TO` | `escalation.emailTo` | email | empty | Recipient | `internal/escalation/escalation.go` |

## Learning mode

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `LEARNING_ENABLED` | `learning.enabled` | `true`, anything else is off | `false` | Collects per-workload CPU baselines (shown at `/api/baselines`). Thresholds are **not** tuned from them yet (ISS-012) | `cmd/auto-agent/run.go` |
| `LEARNING_PERIOD_DAYS` | `learning.periodDays` | positive integer | `14` | How long baselines are learned for | `cmd/auto-agent/run.go` |

## Set by Kubernetes (downward API)

The chart sets these from the pod itself. Do not override them.

| Variable | Helm value | Allowed values | Default | Effect | Read in |
| --- | --- | --- | --- | --- | --- |
| `NODE_NAME` | none (`spec.nodeName`) | node name | empty | The pod informer and node actions cover only this node; empty disables node actions | `cmd/auto-agent/run.go`, `internal/kube/watcher.go` |
| `POD_NAME` | none (`metadata.name`) | pod name | empty | Leader election identity, dashboard top bar | `cmd/auto-agent/run.go`, `internal/leader/leader.go` |
| `POD_NAMESPACE` | none (`metadata.namespace`) | namespace | empty | Namespace of the ConfigMap that is hot reloaded | `cmd/auto-agent/run.go` |

---

## Set by the chart but not read

The chart writes these into the ConfigMap, but no code reads them, so
**changing them does nothing** (ISS-012, ISS-032).

> **Warning: `gitops.mode` and `images.mirror.*` are not implemented.** By
> owner decision (2026-10-07) they stay as flags, **off by default**, and will
> be built in PLAN-002 phase 17. When built, `gitops.mode: live` commits fixes
> straight to the branch and **skips code review**; `images.mirror` makes the
> admission webhook **rewrite images as pods are created**. Both will log a
> warning at startup when turned on.

| Variable | Helm value |
| --- | --- |
| `ANOMALIES_POLL_INTERVAL` | `anomalies.pollInterval` |
| `GITOPS_MODE` | `gitops.mode` |
| `GITOPS_VALUES_FILE` | `gitops.valuesFile` |
| `GITOPS_AUTHOR_NAME` | `gitops.author.name` |
| `GITOPS_AUTHOR_EMAIL` | `gitops.author.email` |
| `IMAGE_MIRROR_ENABLED` | `images.mirror.enabled` |
| `IMAGE_MIRROR_PREFIX` | `images.mirror.prefix` |
| `IMAGE_MIRROR_ALLOWLIST` | `images.mirror.allowList` |

## Chart values that shape the deployment

These do not become variables; they change what the chart renders.

| Helm value | Default | Effect |
| --- | --- | --- |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/supersaiyane/auto-agent-k8s`, `1.0.0`, `IfNotPresent` | Agent image |
| `namespace` | `kube-system` | Namespace the agent, ConfigMap and Secret live in |
| `priorityClassName` | `system-node-critical` | Keeps the agent scheduled under node pressure |
| `initImage` | `busybox:1.36` pinned by digest | Init container that chowns the log directory |
| `tolerations` | tolerate everything | Run on every node, control plane included |
| `resources` | requests 50m / 128Mi, limits 300m / 384Mi | Agent container resources |
| `env` | `[]` | Extra environment variables; win over the ConfigMap |
| `logs.efs.path` | `/var/log/auto-agent` | Host directory mounted at `/var/log/auto-agent` |
| `networkPolicy.enabled` | `true` | Render the NetworkPolicy |
| `networkPolicy.allowFromNamespaces` | `["monitoring"]` | Namespaces allowed to reach port 8080 |
| `rbac.readTLSSecrets` | `false` | Also grants secret list in allowlisted namespaces (see `TLS_CERT_CHECK`) |
| `webhook.enabled` | `false` | Render the webhook registration and Service |
| `webhook.failurePolicy` | `Ignore` | `Ignore` or `Fail` when the webhook is unreachable |
| `webhook.caBundle` | empty | Base64 CA bundle for the webhook registration |
