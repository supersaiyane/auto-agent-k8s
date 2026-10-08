# Security Hardening

## RBAC

Since 2026-10-07 (ISS-009) the chart grants exactly what the code calls, and
`TestRBAC_ChartMatchesCode` (internal/kube) renders the chart and fails on any
missing or unused grant.

- **ClusterRole `auto-agent`**: read-only (`get`/`list`/`watch` on what the
  detectors read) plus `patch` on nodes, which are cluster-scoped and are only
  cordoned by the agent running on that node.
- **Role `auto-agent-write` in each allowlisted namespace**: delete pods,
  create pod evictions, patch/update deployments, delete jobs. No workload
  write exists outside the allowlist. Each allowlisted namespace must exist
  before install.
- **Role `auto-agent-leader` in the agent's namespace**: create leases, and get/update
  only the `auto-agent-leader` lease.
- **Role `auto-agent-config` in the agent namespace**: read its ConfigMap.
- **TLS secrets**: `rbac.readTLSSecrets: true` grants secret list in
  allowlisted namespaces and turns on the certificate expiry check
  (`TLS_CERT_CHECK`). Off by default.

A read the chart does not grant is counted in
`auto_agent_api_errors_total{reason="forbidden"}` and logged once per resource.

## Secret Management

- **Never** put secrets in `values.yaml` or `03-config.yaml` in Git
- Use **External Secrets Operator** or **Sealed Secrets** for production
- Rotate `SLACK_WEBHOOK_URL`, `LLM_API_KEY`, `GIT_TOKEN` regularly
- The agent reads secrets from env vars at startup: rotation requires restart

## Network Policies

Restrict agent network access:
```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: auto-agent
  namespace: auto-agent
spec:
  podSelector:
    matchLabels:
      app: auto-agent
  policyTypes: ["Egress"]
  egress:
    - to:
        - namespaceSelector: {}    # K8s API server
      ports:
        - port: 443
    - to: []                       # Slack, LLM, GitHub (external)
      ports:
        - port: 443
```

## Admission Webhook TLS

If using the admission webhook:
1. Generate TLS cert (self-signed or cert-manager)
2. Set `WEBHOOK_CERT_FILE` and `WEBHOOK_KEY_FILE` env vars
3. Set `webhook.caBundle` in Helm values (base64-encoded CA cert)
4. The webhook validates Deployments/StatefulSets for missing limits and probes

## LLM Data Privacy

The LLM client sends **pod logs and events** to the configured LLM endpoint. This may contain:
- Environment variable names (not values)
- Service names, hostnames, IP addresses
- Error messages with stack traces

**Mitigations**:
- Secrets and personal data are redacted before sending (see "Data leaving the cluster" below)
- Truncates context to 4KB
- Use an internal LLM gateway (vLLM, Ollama) instead of external APIs
- Set `LLM_ENABLED: "false"` to disable entirely

## Audit Trail

All remediation actions are logged to:
- `/var/log/auto-agent/audit.jsonl`: JSONL format, persistent
- Event recorder: queryable via `/api/events`
- Slack messages (if configured)
- Prometheus metrics (`auto_agent_actions_total`)

For compliance: mount the hostPath volume to a persistent storage system and ship audit logs to your SIEM.

## Data leaving the cluster (redaction)

Since 2026-10-07 (ISS-011) every client that sends text off the cluster masks
secrets and personal data first: the LLM prompt, Slack messages and blocks,
GitHub and Jira tickets, GitHub and GitLab pull request titles and
descriptions, and Alertmanager annotations. Redaction lives inside those
clients (`internal/redact`), so new call sites get it without doing anything.

Masked: private key blocks, credentials in URLs (`scheme://user:pass@host`
keeps the host), bearer tokens, `password=`/`token=`/`api_key=` style values,
AWS access key IDs, GitHub, Slack and `sk-` style API keys, JWTs, email
addresses and IPv4 addresses (a following `:port` is kept, since it is usually
the diagnosis).

Not masked: pull request file content (it is the change itself) and log
bundles written to your own S3 or EFS storage (the forensic record).

Redaction is pattern based, so it lowers the risk rather than removing it.
The LLM is off by default (`llm.enabled: false`); leave it off if pod logs may
carry data that must never leave the cluster.

`TestOutboundClientsRedact` in `internal/redact` sends synthetic secrets
through every client and fails if any reaches the wire.
