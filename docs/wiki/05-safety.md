# Safety & Guardrails

Auto-Agent has 8 layers of protection preventing runaway remediation. Every action passes through ALL layers before execution.

## The 8 Safety Layers

```
Incident detected
    │
    ▼
[1] Mode check ─── observe/suggest? → alert only, no action
    │ fix
    ▼
[2] Quiet hours ── in maintenance window? → block
    │ no
    ▼
[3] Blast radius ─ >5 namespaces affected this hour? → block
    │ under limit
    ▼
[4] CRD policy ── requireApproval=true? → block
    │ allowed
    ▼
[5] Circuit breaker ─ >5 actions on same workload in 1hr? → block + alert
    │ under threshold
    ▼
[6] Rate limiter ── >10 actions in 10 min? → block
    │ under limit
    ▼
[7] Dedup ────── same workload/reason within 5 min? → skip
    │ new event
    ▼
[8] HPA check ── deployment has HPA? → skip scaling
    │ no HPA
    ▼
  Execute action → FixTracker → verify recovery
```

## Layer Details

### 1. Mode Check
```yaml
AUTO_MODE: "observe"   # detect + alert only
AUTO_MODE: "suggest"   # detect + alert + recommend (no action)
AUTO_MODE: "fix"       # detect + alert + auto-remediate
AUTO_MODE: "dry-run"   # detect + simulate (log what WOULD happen)
```

### 2. Quiet Hours
```yaml
QUIET_HOURS: "02:00-06:00"          # single window (UTC)
QUIET_HOURS: "02:00-06:00,14:00-14:30"  # multiple windows
QUIET_HOURS: "22:00-06:00"          # overnight window
```
During quiet hours, all fix actions are blocked. Detection and alerting continue normally.

### 3. Blast Radius
Limits actions to **max 5 namespaces per hour**. If a bad ConfigMap update affects 10 namespaces simultaneously, the agent acts on the first 5 and blocks the rest — preventing cluster-wide disruption.

### 4. CRD Per-Policy Approval
```yaml
spec:
  safety:
    requireApproval: true    # blocks automated fixes for this workload
    maxActionsPerHour: 3     # per-workload rate limit
    cooldown: "10m"          # per-workload cooldown override
```

### 5. Circuit Breaker
If the agent takes **5+ actions on the same workload within 1 hour**, the circuit breaker trips:
- All further actions on that workload are blocked
- Fires a **critical Alertmanager alert**
- Stays tripped until the 1-hour window expires
- Prevents delete-recreate-delete loops

### 6. Global Rate Limiter
```yaml
MAX_ACTIONS_PER_10M: "10"    # max 10 actions per 10-minute sliding window
```
Applies across ALL workloads and namespaces. Once the limit is hit, all actions are blocked until the window slides.

### 7. Workload-Level Dedup
```yaml
DEDUP_TTL_SECONDS: "300"     # 5-minute dedup window
```
Same workload + same reason = deduplicated. Prevents hundreds of alerts from the same crashing deployment. Uses `ownerName` (Deployment level), not pod name — so controller-recreated pods are still deduped.

### 8. HPA Coexistence
```yaml
HPA_COEXISTENCE: "true"
```
If a Deployment has an HPA, the agent will NOT scale it — avoids fighting with the HPA controller.

## Dry-Run Mode

```yaml
AUTO_MODE: "dry-run"
```
The agent runs all detection and guardrail logic, but instead of executing actions, it logs what WOULD happen:
- "WOULD delete pod api-server-xxx"
- "WOULD be blocked by quiet hours"
- "WOULD be blocked by circuit breaker"

View simulations in the dashboard or via `GET /api/dry-run`.

## Fix Verification (FixTracker)

After every action, the FixTracker verifies recovery:
1. Action taken (e.g., pod deleted) → recorded as **Pending**
2. Every 30s, checks if the Deployment is healthy (all replicas Ready)
3. If healthy → marked as **Verified Fixed** (with recovery time)
4. If still broken after 15 minutes → marked as **Not Fixed**

View results in the **Actions** tab or `GET /api/fixes`.

## Escape Hatch

Exclude any workload from auto-agent:
```yaml
metadata:
  annotations:
    auto-agent.io/disable: "true"
```
The agent will completely ignore this pod/deployment — no detection, no alerts, no actions.

## Node Safety

- **PDB-aware eviction**: Uses the Eviction API which respects PodDisruptionBudgets
- **StatefulSet protection**: Skips StatefulSet pods during node drain (ordered lifecycle)
- **kube-system protection**: Never evicts kube-system pods
- **Priority-class protection**: Skips `system-*` and `*critical*` priority classes
- **Auto-uncordon**: Only uncordons nodes the agent itself cordoned (annotation marker)
