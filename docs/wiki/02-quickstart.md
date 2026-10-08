# Quick Start

This page only points to the owner of the topic, so the steps cannot drift.

- **Try it on a laptop** (kind, about ten minutes): [GUIDE section 3](../GUIDE.md#3-try-it-locally-in-10-minutes).
- **Deploy to a real cluster**: [GUIDE section 4](../GUIDE.md#4-deploy-to-a-real-cluster).
- **The raw manifests and demo apps** (`deployment/deploy.sh`, chaos tests): [Testing](08-testing.md).
- **Every setting**: [CONFIGURATION.md](../CONFIGURATION.md).

The agent starts in `dry-run`, watches every namespace except the system ones,
and acts nowhere until you set `agent.fixNamespaces` or enable a namespace in
the dashboard's Settings tab.
