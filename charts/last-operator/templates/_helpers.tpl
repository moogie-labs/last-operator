{{- define "last.image" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}
{{ $root.Values.image.registry }}/last-{{ $name }}:{{ $root.Values.image.tag | default $root.Chart.AppVersion }}
{{- end }}

{{/* Reuse a Secret value across upgrades; generate one on first install. */}}
{{- define "last.persistedSecretValue" -}}
{{- $ns := index . 0 -}}{{- $name := index . 1 -}}{{- $key := index . 2 -}}
{{- $existing := lookup "v1" "Secret" $ns $name -}}
{{- if and $existing $existing.data (hasKey $existing.data $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- randAlphaNum 40 -}}
{{- end -}}
{{- end }}

{{- define "last.auditEndpoint" -}}
{{- if .Values.audit.bundled.enabled -}}
http://last-rustfs.{{ .Values.namespaces.audit }}.svc:9000
{{- else -}}
{{ required "audit.external.endpoint is required when audit.bundled.enabled=false" .Values.audit.external.endpoint }}
{{- end -}}
{{- end }}

{{- define "last.writerSecret" -}}
{{- if .Values.audit.bundled.enabled -}}
last-audit-writer
{{- else -}}
{{ required "audit.external.existingSecret is required when audit.bundled.enabled=false" .Values.audit.external.existingSecret }}
{{- end -}}
{{- end }}

{{- define "last.labels" -}}
app.kubernetes.io/part-of: last-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
