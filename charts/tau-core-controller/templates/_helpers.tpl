{{- define "tau-core-controller.namespace" -}}
{{- .Release.Namespace -}}
{{- end -}}

{{- define "tau-core-controller.selectorLabels" -}}
app.kubernetes.io/name: tau-core-controller
{{- end -}}

{{- define "tau-core-controller.labels" -}}
{{ include "tau-core-controller.selectorLabels" . }}
app.kubernetes.io/part-of: {{ .Values.partOf }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "tau-core-controller.image" -}}
{{- $image := .Values.image -}}
{{- if $image.digest -}}
{{ $image.repository }}@{{ $image.digest }}
{{- else if $image.tag -}}
{{ $image.repository }}:{{ $image.tag }}
{{- else -}}
{{ fail "image.digest or image.tag must be set" }}
{{- end -}}
{{- end -}}
