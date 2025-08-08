# Architecture Review & Decision Log

## Purpose
Design an extensible Kubernetes-native agent for automated remediation, integrating with observability, GitOps, and ticketing workflows.

---

## High-Level Architecture
- **DaemonSet**: Runs on every node for local pod/node observation and remediation.
- **CRD**: `AutoRemediationPolicy` for per-app/team actions, thresholds, and escalation rules.
- **Controllers**:
  - Pod/Node watcher: Detects CrashLoopBackOff, ImagePullBackOff, OOMKilled, Node pressure.
  - Deployment scaler: Adjusts replicas based on CPU, latency, queue depth, error rate.
  - CRD controller: Syncs policies into memory for fast lookup.
- **Integrations**:
  - Slack: Incident alerts + LLM suggestions.
  - GitOps: PR-based safe changes.
  - Ticketing: Jira/GitHub Issues for incident tracking.
  - Storage: S3/EFS for logs/events bundles.
  - Prometheus: Metrics scraping for anomaly detection and agent health.
  - LLM: Root cause suggestions from logs/events.

---

## Tooling Decisions

### Kubernetes Client
- **client-go** for watching Pods, Nodes, Deployments.
- **dynamic client** for CRD resources.

### Storage
- **AWS S3 via AWS SDK v2**: Chosen for portability and IRSA integration.
- **EFS**: Optional for clusters without object storage.

### Metrics & Observability
- **Prometheus client_golang**: `/metrics` endpoint for agent metrics.
- **Prometheus HTTP API**: For querying CPU, latency, error rates, queue depth.

### GitOps
- PR workflow avoids live patching to maintain Git as source of truth.
- Supports GitHub/GitLab via API tokens.

### Ticketing
- **Jira REST API v3** for enterprise environments.
- **GitHub Issues API** for GitOps-native orgs.

### LLM Integration
- Abstract interface to call any hosted LLM API (e.g., OpenAI, Anthropic) for automated incident analysis.

---

## Design Decisions

1. **DaemonSet over Deployment**:
   - Pros: Local node state access, faster event reaction.
   - Cons: Higher resource footprint.

2. **CRD for policies**:
   - Pros: Native K8s UX, fine-grained control per team/app.
   - Cons: Requires CRD lifecycle management.

3. **GitOps-safe mode**:
   - Avoids config drift by proposing PRs rather than live-patching manifests.

4. **Multi-signal scaling**:
   - Gating scale actions on multiple metrics reduces false positives.

5. **Log storage**:
   - Persisting logs/events enables offline analysis and training data for ML.

6. **Extensible integrations**:
   - Slack, ticketing, and LLM decoupled via interfaces for future provider swaps.

---

## Future Enhancements
- Full anomaly detection baselines with seasonality.
- Auto-rollback of failed remediation.
- ChatOps commands for approving/triggering remediations.
- Multi-cluster/global view.
