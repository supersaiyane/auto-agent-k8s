{{- define "auto-agent.labels" -}}
app.kubernetes.io/name: auto-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
{{- end -}}

{{/*
The fix ceiling (ADR-002): agent.fixCeiling, or agent.fixNamespaces when the
ceiling is empty. Fails on the removed agent.namespaceAllowlist and on a fix
namespace outside the ceiling, so a typo never installs silently.
*/}}
{{- define "auto-agent.fixCeiling" -}}
{{- if .Values.agent.namespaceAllowlist -}}
{{- fail "agent.namespaceAllowlist was replaced by agent.watchNamespaces, agent.fixNamespaces and agent.fixCeiling (ADR-002, docs/CONFIGURATION.md)" -}}
{{- end -}}
{{- $ceiling := .Values.agent.fixCeiling | default .Values.agent.fixNamespaces -}}
{{- if not .Values.rbac.fixAnywhere -}}
{{- range .Values.agent.fixNamespaces -}}
{{- if not (has . $ceiling) -}}
{{- fail (printf "agent.fixNamespaces names %q, which is not in agent.fixCeiling" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- join "," $ceiling -}}
{{- end -}}

{{/* The agent's namespace: .Values.namespace, else the release namespace (helm -n). */}}
{{- define "auto-agent.namespace" -}}
{{- .Values.namespace | default .Release.Namespace -}}
{{- end -}}

{{/* Where the leader Lease lives: leaderElection.namespace, else the agent's namespace. */}}
{{- define "auto-agent.leaseNamespace" -}}
{{- .Values.leaderElection.namespace | default (include "auto-agent.namespace" .) -}}
{{- end -}}

{{/* The agent image, pinned by digest when image.digest is set (constraint 11). */}}
{{- define "auto-agent.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end -}}
