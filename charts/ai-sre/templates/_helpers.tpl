{{- define "aisre.name" -}}{{ .Release.Name }}{{- end -}}

{{- define "aisre.labels" -}}
app.kubernetes.io/part-of: ai-sre
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "aisre.selector" -}}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/name: {{ .name }}
{{- end -}}

{{- define "aisre.image" -}}
{{- $v := .root.Values -}}
{{- if not (has $v.edition (list "community" "enterprise")) }}{{ fail "edition must be community or enterprise" }}{{ end -}}
{{- $registry := $v.image.registry -}}
{{- if eq $v.edition "enterprise" -}}
{{- $registry = required "image.enterpriseRegistry is required for edition: enterprise" $v.image.enterpriseRegistry -}}
{{- end -}}
{{ $registry }}/{{ .name }}:{{ $v.image.tag | default .root.Chart.AppVersion }}
{{- end -}}

{{/* License token for the enterprise edition, as an env entry (nothing without a secret). */}}
{{- define "aisre.licenseEnv" -}}
{{- with .Values.license.existingSecret }}
- name: AISRE_LICENSE
  valueFrom:
    secretKeyRef: {name: {{ . }}, key: license}
{{- end }}
- {name: AISRE_EDITION, value: {{ .Values.edition | quote }}}
{{- end -}}

{{- define "aisre.natsUrl" -}}nats://{{ .Release.Name }}-nats:4222{{- end -}}
{{- define "aisre.ingestUrl" -}}http://{{ .Release.Name }}-ingest:8080{{- end -}}
{{- define "aisre.guardUrl" -}}http://{{ .Release.Name }}-action-guard:8080{{- end -}}

{{/* Reuses a generated secret value across upgrades. */}}
{{- define "aisre.secretValue" -}}
{{- $existing := lookup "v1" "Secret" .root.Release.Namespace .secret -}}
{{- if and $existing (index $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum 40 -}}
{{- end -}}
{{- end -}}

{{- define "aisre.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: [ALL]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/* Pod placement and image pull secrets for every workload of the chart (subcharts take
their own values). */}}
{{- define "aisre.scheduling" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.scheduling.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.scheduling.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.scheduling.affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
