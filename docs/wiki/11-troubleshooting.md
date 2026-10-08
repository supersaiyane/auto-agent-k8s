# Troubleshooting

Symptoms, causes and fixes are in **[GUIDE section 9](../GUIDE.md#9-troubleshooting)**,
with the commands to run. Two first steps that answer most questions:

- `auto-agent check-config` in the pod: the settings the agent sees, and any
  key it does not read.
- The Audit tab: every decision of the mutation gate, including fixes only
  suggested because the namespace is outside the fix scope.
