# Dashboard

The dashboard is described in one place: [docs/GUIDE.md, section 6](../GUIDE.md#6-the-dashboard)
(opening it, every tab, filters, refresh, deep links, the Terminal tab) and
section 7 for the HTTP API behind it.

Short version:

```bash
kubectl -n <agent namespace> port-forward svc/auto-agent 8080:8080
```

Open `http://localhost:8080` and sign in with `DASHBOARD_TOKEN` from the
agent's Secret. The Service reaches the controllers, so every refresh shows
the same cluster-wide view (ADR-001).
