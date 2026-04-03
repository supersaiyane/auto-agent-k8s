# Metrics & Alerting

## Prometheus Metrics

All metrics prefixed with `auto_agent_`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `auto_agent_incidents_total` | Counter | reason, namespace, workload | Incidents detected |
| `auto_agent_actions_total` | Counter | type, namespace, workload | Remediation actions taken |
| `auto_agent_scaling_decisions_total` | Counter | direction, namespace, deployment | Scale up/down decisions |
| `auto_agent_dedup_skipped_total` | Counter | reason | Events deduplicated |
| `auto_agent_rate_limited_total` | Counter | — | Actions blocked by rate limiter |
| `auto_agent_handler_errors_total` | Counter | handler, error_type | Errors in event handlers |
| `auto_agent_anomalies_detected_total` | Counter | policy, namespace, rule | CRD anomaly detections |
| `auto_agent_llm_requests_total` | Counter | status | LLM diagnosis requests |
| `auto_agent_info` | Gauge | version, mode | Agent build info |

## Scrape Config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: auto-agent
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [auto-agent]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app]
        regex: auto-agent
        action: keep
```

## Grafana Dashboard

Import `dashboards/auto-agent.json` into Grafana. 12 panels:
- Agent info, incidents rate, actions rate
- Incidents by namespace (pie), scaling decisions
- Rate limited counter, dedup skipped
- Handler errors, anomalies detected
- Top workloads by incidents/actions

## Alert Rules

### Agent health
```yaml
- alert: AutoAgentDown
  expr: up{job="auto-agent"} == 0
  for: 5m
  labels: { severity: critical }
  annotations: { summary: "Auto-agent pod is down" }

- alert: AutoAgentHighErrorRate
  expr: rate(auto_agent_handler_errors_total[5m]) > 0.1
  for: 10m
  labels: { severity: warning }
```

### Incident rate
```yaml
- alert: HighIncidentRate
  expr: rate(auto_agent_incidents_total[5m]) > 1
  for: 5m
  labels: { severity: warning }
  annotations: { summary: "More than 1 incident/sec detected" }
```

### Circuit breaker
```yaml
- alert: CircuitBreakerTripped
  expr: increase(auto_agent_handler_errors_total{error_type="circuit_breaker"}[1h]) > 0
  labels: { severity: critical }
  annotations: { summary: "Circuit breaker tripped — investigate workload" }
```

## PromQL Queries

```promql
# Incidents per minute
rate(auto_agent_incidents_total[5m]) * 60

# Top 5 failing workloads
topk(5, sum(auto_agent_incidents_total) by (namespace, workload))

# Action success rate
sum(auto_agent_actions_total{type="verified_fix"}) / sum(auto_agent_actions_total) * 100

# Dedup effectiveness
sum(auto_agent_dedup_skipped_total) / (sum(auto_agent_incidents_total) + sum(auto_agent_dedup_skipped_total)) * 100
```
