# Integrations

## Slack

**Setup**: Set `SLACK_WEBHOOK_URL` in secrets.

**Features**:
- Incident alerts with logs, events, LLM diagnosis
- Per-channel routing via CRD `slackChannel` field
- Block Kit interactive buttons (Approve / Rollback / Silence)
- Callback endpoint: `POST /api/slack/actions`

**Per-policy channels**:
```yaml
spec:
  escalation:
    slackChannel: "#prod-incidents"   # routes to this channel's webhook
```

## LLM Diagnosis

**Setup**: Set `LLM_ENABLED=true`, `LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL`.

Works with any OpenAI-compatible API (OpenAI, Anthropic, Ollama, vLLM, Azure OpenAI).

**What it does**: Sends pod logs + events to the LLM and gets a concise SRE diagnosis (3-5 sentences). Appended to Slack messages.

**Privacy**: Truncates context to 4KB. Use an internal LLM gateway for sensitive data.

## GitOps (GitHub / GitLab)

**Setup**: Set `GIT_TOKEN`, `GITOPS_REPO`, `GITOPS_PROVIDER` (github/gitlab), `GITOPS_BRANCH`.

**What it does**: On OOMKilled, opens a PR/MR with a kustomize patch to bump memory limits.

**Flow**: OOM detected → generates patch YAML → creates branch → commits file → opens PR → links PR in Slack message.

## Ticketing (GitHub Issues / Jira)

**Setup**: Set `TICKETS_ENABLED=true`, `TICKETS_PROVIDER` (github/jira).

**GitHub Issues**: Set `GITHUB_TOKEN`, `GITHUB_REPO`. Searches by embedded key in body for dedup. Creates or adds comment.

**Jira**: Set `JIRA_TOKEN`, `JIRA_BASE_URL`, `JIRA_PROJECT_KEY`, `JIRA_EMAIL`. JQL search by summary. Creates Bug or adds ADF comment.

## Alertmanager

**Setup**: Set `ALERTMANAGER_URL` (e.g., `http://alertmanager:9093`).

Sends structured alerts for every incident:
```json
{
  "labels": {
    "alertname": "AutoAgentIncident",
    "reason": "CrashLoopBackOff",
    "namespace": "default",
    "severity": "critical"
  }
}
```

Circuit breaker trips fire `AutoAgentCircuitBreaker` alerts.

## Escalation Chain (PagerDuty / OpsGenie / Email)

| Channel | Setup | Triggers on |
|---------|-------|-------------|
| PagerDuty | `PAGERDUTY_ROUTING_KEY` | Critical + Warning |
| OpsGenie | `OPSGENIE_API_KEY` | Critical + Warning |
| Email (SMTP) | `SMTP_HOST`, `SMTP_FROM`, `ESCALATION_EMAIL_TO` | Critical only |

All channels fire in parallel. Severity routing is automatic.

## Kubecost / OpenCost

**Setup**: Set `COST_PROVIDER: "kubecost"` or `"opencost"` in config. deploy.sh auto-installs.

The Cost tab switches from estimated pricing to real cluster costs via the Kubecost/OpenCost API.
