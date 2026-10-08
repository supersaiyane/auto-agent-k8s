# Configuration

Every setting, with its Helm value, allowed values, default, effect and the
file that reads it, is in **[docs/CONFIGURATION.md](../CONFIGURATION.md)**. A
test fails when that page and the code disagree, so it is the only list.

Quick pointers:

- Where the agent reads and where it acts: `agent.watchNamespaces`,
  `agent.fixNamespaces`, `agent.fixCeiling`, `rbac.fixAnywhere` (ADR-002), and
  the dashboard's Settings tab ([GUIDE section 6.4](../GUIDE.md#64-the-settings-tab-where-the-agent-may-fix)).
- Which keys take effect without a restart: the "Live reload" paragraph of
  CONFIGURATION.md.
- Checking a running pod: `auto-agent check-config` prints every setting
  (secrets redacted) and names keys the agent does not read.
