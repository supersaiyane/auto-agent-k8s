# Security Hardening

## RBAC

### Default: ClusterRole
Grants access to pods, deployments, nodes, etc. across all namespaces. Simple but broad.

### Production: Namespace-Scoped
Set `namespacedRBAC.enabled: true` in Helm values:
- Generates `Role` + `RoleBinding` per namespace in the allowlist
- Minimal `ClusterRole` for nodes, namespaces, and leases only
- Agent can only access resources in watched namespaces

### Principle of Least Privilege
The agent needs:
- **Read** on most resources (pods, deployments, events, services, etc.)
- **Delete** on pods only (remediation)
- **Create** on pod evictions (node drain)
- **Update/Patch** on nodes (cordon/uncordon) and deployments (scaling/rollback)
- **List/Watch** on HPAs, PDBs (awareness only)

## Secret Management

- **Never** put secrets in `values.yaml` or `03-config.yaml` in Git
- Use **External Secrets Operator** or **Sealed Secrets** for production
- Rotate `SLACK_WEBHOOK_URL`, `LLM_API_KEY`, `GIT_TOKEN` regularly
- The agent reads secrets from env vars at startup — rotation requires restart

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
- Truncates context to 4KB
- Use an internal LLM gateway (vLLM, Ollama) instead of external APIs
- Set `LLM_ENABLED: "false"` to disable entirely

## Audit Trail

All remediation actions are logged to:
- `/var/log/auto-agent/audit.jsonl` — JSONL format, persistent
- Event recorder — queryable via `/api/events`
- Slack messages (if configured)
- Prometheus metrics (`auto_agent_actions_total`)

For compliance: mount the hostPath volume to a persistent storage system and ship audit logs to your SIEM.
