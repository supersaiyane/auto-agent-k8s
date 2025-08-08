{{- define "auto-agent.labels" -}}
app.kubernetes.io/name: auto-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
{{- end -}}
