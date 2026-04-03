# Contributing

## Adding a New Issue Detector

### 1. Create the handler function

```go
// internal/kube/my_handler.go

func CheckMyIssue(ctx context.Context, deps *Deps) {
    for ns := range deps.Policy.NamespaceAllow {
        // List resources
        // Check for the condition
        // Dedup check
        key := dedupKey(ns, name, "MyIssue")
        if !deps.Dedup.Check(key) { continue }

        // Build message
        msg := fmt.Sprintf("*MyIssue* on `%s/%s`\n", ns, name)

        // Post to Slack
        deps.Slack.Post(msg)

        // Fire Alertmanager
        fireAlert(ctx, deps, "MyIssue", ns, name, "", msg, "warning")

        // Record event for dashboard
        recordEvent(deps, eventsvc.Event{
            Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
            Namespace: ns, Workload: name, Reason: "MyIssue",
            Message: "description",
        })

        // Prometheus counter
        obs.IncidentsTotal.WithLabelValues("MyIssue", ns, name).Inc()
    }
}
```

### 2. Wire into the periodic loop

In `cmd/auto-agent/main.go`, add to the appropriate ticker:
```go
case <-jobTicker.C:
    // ... existing checks
    kube.CheckMyIssue(ctx, deps)
```

Or for pod-level detection, add to `watcher.go` in `handlePodUpdate()`.

### 3. Add to the detection table

Update `docs/wiki/04-detection.md` with the new detector.

### 4. Write a test

```go
// internal/kube/my_handler_test.go
func TestCheckMyIssue(t *testing.T) {
    deps, kc := newHandlerTestDeps(t)
    // Create resource that triggers the issue
    // Call CheckMyIssue(ctx, deps)
    // Verify Slack message / event recorded
}
```

### 5. Add chaos workload

Add a YAML resource to `deployment/test-apps/chaos-full.yaml` that triggers the issue, and a check in `chaos-test.sh`.

## Code Style

- Accept interfaces, return structs
- Wrap errors with context: `fmt.Errorf("handler: %w", err)`
- Use `klog.Infof` for normal operations, `klog.Warningf` for degraded
- All handlers go through `tryFixAction` for guardrail enforcement
- Use `recordEvent` for dashboard visibility
- Use `obs.IncidentsTotal` / `obs.ActionsTotal` for Prometheus

## PR Checklist

- [ ] New detector added to appropriate file
- [ ] Wired into periodic loop or watcher
- [ ] Dedup key is workload-level (not pod-level)
- [ ] Fires Alertmanager alert
- [ ] Records event for dashboard
- [ ] Increments Prometheus counter
- [ ] Unit test or integration test
- [ ] Chaos workload added
- [ ] Documentation updated
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
