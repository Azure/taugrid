{{/*
Platform namespace that owns the controller, its RBAC, and every TauWorkspace.
It is also passed to the binary as --platform-namespace, so the two can never
disagree.
*/}}
{{- define "tau-core-controller.namespace" -}}
{{- required "platformNamespace must not be empty" .Values.platformNamespace -}}
{{- end -}}

{{/*
Selector labels. These land in Deployment.spec.selector, which is immutable
after creation, so they must never gain release-scoped values.
*/}}
{{- define "tau-core-controller.selectorLabels" -}}
app.kubernetes.io/name: tau-core-controller
{{- end -}}

{{/*
Common labels. Matches the convention used by the other TauGrid charts so a
release owns and can prune its objects.
*/}}
{{- define "tau-core-controller.labels" -}}
{{ include "tau-core-controller.selectorLabels" . }}
app.kubernetes.io/part-of: {{ .Values.partOf }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Fully qualified controller image. A digest is preferred; a tag is accepted so a
platform can run a pre-release build without editing templates.
*/}}
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
