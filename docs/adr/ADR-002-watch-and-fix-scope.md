# ADR-002: Separate watch scope and fix scope, editable from a Settings tab

Status: accepted, 2026-10-08 (owner decision, option 3)
Plan: PLAN-002 11.8

## Context

One list, `NAMESPACE_ALLOWLIST`, decides both what the agent reads and where
it acts. That keeps writes safe but hides most of the cluster: the dashboard,
the detectors and the kubectl panel see only a few namespaces, although the
agent already holds cluster-wide read permissions. The owner wants to see
every namespace, to choose any or all of them in the UI, and to change where
the agent fixes from the UI, starting from values set in Helm.

Writes are the risk. The agent deletes and evicts pods, patches Deployments
and deletes Jobs. Kubernetes lets it do that only where a write Role or
ClusterRole exists, so "where may the UI enable fixing" is the same question
as "where does the agent hold write permissions".

## Decision

Three controls, each with one owner:

| Control | Set in | Default | Meaning |
| --- | --- | --- | --- |
| Watch scope | Helm `agent.watchNamespaces` (`WATCH_NAMESPACES`) | all namespaces except `kube-system`, `kube-public`, `kube-node-lease` and the agent's own | Where the agent reads, detects and reports, and what the dashboard can show |
| Fix ceiling | Helm `agent.fixCeiling`, or `rbac.fixAnywhere: true` | the initial fix list | Where write permissions exist; the most the UI can ever enable |
| Fix scope | Helm `agent.fixNamespaces` initially, then the Settings tab | `[]` plus anything set in Helm | Where the agent may act today, always inside the ceiling |

**Two install modes, chosen per cluster.**

- **Ceiling mode (default).** The chart renders write Roles only in the
  `fixCeiling` namespaces. The Settings tab can tick or untick any of them;
  namespaces outside the ceiling are shown greyed out with "change Helm".
  A leaked dashboard or agent token can do no harm outside the ceiling.
- **Fix anywhere (`rbac.fixAnywhere: true`).** The chart renders one write
  ClusterRole, and the ceiling becomes every namespace except the system ones.
  The Settings tab can tick any namespace. The agent logs a warning at every
  start, `/api/status` reports the mode, and the Settings tab and the docs
  carry a warning that a leaked token can then disrupt any namespace.

**Helm sets the start, the UI wins after that.** The UI writes its choice to
its own ConfigMap, `auto-agent-scope`, which Helm does not manage, so a Helm
upgrade never undoes it. The effective fix scope is the UI choice when one
exists, else the Helm `fixNamespaces`, always intersected with the ceiling.
Clearing the UI choice from the Settings tab returns to the Helm values.

**Every change is explicit and recorded.** The Settings tab states, for each
control, what it does, how it works and why it exists. A change shows the
before and after and asks for confirmation; enabling fixing in a namespace
asks for the namespace name to be typed. Each change is an audit event
(before, after, when, from where) shown in the Audit tab. Until the
approvals work (PLAN-002 phase 15) adds named approvers, anyone with the
dashboard token can change the fix scope; that limit is stated on the tab.

**The view dropdown is separate.** A namespace selector in the top bar
("All namespaces" or one namespace) filters every tab. It only changes what
the page shows, never what the agent does.

## Rules that change

CLAUDE.md constraint 4 becomes: reads follow the watch scope; writes follow
the fix scope, which is always inside the fix ceiling; the dashboard can
narrow or widen the fix scope only inside that ceiling. The mutation gate
refuses an action outside the fix scope even where RBAC would allow it.

## Consequences

- The whole cluster becomes visible by default, which also sends more logs
  and events to outbound integrations; redaction still applies to all of it.
- The controller gains `patch` (pinned to `auto-agent-scope`) and `create`
  (namespaced) on ConfigMaps in its own namespace. Both roles already list
  and watch ConfigMaps there for the policy reload, so both apply the same
  fix scope. Until that ConfigMap has been read, the fix scope is empty.
  (Built 2026-10-08; `patch` instead of the `update` first written here, per
  constraint 6.)
- `fixAnywhere` trades least privilege for flexibility; the default does not.
- `NAMESPACE_ALLOWLIST` stays readable for one release as an alias for both
  the watch scope and the initial fix scope, with a deprecation warning.

## Rejected

- **One list for everything (today).** Hides most of the cluster.
- **UI can enable any namespace by having the agent create its own Roles.**
  The agent would grant itself permissions, so the dashboard token would be
  equivalent to cluster admin.
