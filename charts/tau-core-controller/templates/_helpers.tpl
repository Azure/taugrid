{{/*
System namespace that owns the controller, its RBAC, and every TauWorkspace.
The Helm release namespace is passed to the binary so every TauGrid system
component follows the chart installation namespace.
*/}}
{{- define "tau-core-controller.namespace" -}}
{{- .Release.Namespace -}}
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

{{- define "tau-core-controller.multiKueueBetaEnabled" -}}
{{- $global := .Values.global | default dict -}}
{{- $features := get $global "betaFeatures" | default list -}}
{{- $acknowledgements := get $global "betaRiskAcknowledgements" | default list -}}
{{- if and (has "multikueue" $features) (has "multikueue" $acknowledgements) -}}true{{- end -}}
{{- end -}}

{{- define "tau-core-controller.validateMultiKueueBetaGate" -}}
{{- $global := .Values.global | default dict -}}
{{- $features := get $global "betaFeatures" | default list -}}
{{- $acknowledgements := get $global "betaRiskAcknowledgements" | default list -}}
{{- $featureEnabled := has "multikueue" $features -}}
{{- $riskAcknowledged := has "multikueue" $acknowledgements -}}
{{- if ne $featureEnabled $riskAcknowledged -}}
{{- fail "MultiKueue Beta requires both global.betaFeatures=[multikueue] and global.betaRiskAcknowledgements=[multikueue]; enabling only one is not permitted" -}}
{{- end -}}
{{- $scan := deepCopy .Values -}}
{{- $scanGlobal := deepCopy $global -}}
{{- $_ := unset $scanGlobal "betaFeatures" -}}
{{- $_ := unset $scanGlobal "betaRiskAcknowledgements" -}}
{{- $_ := set $scan "global" $scanGlobal -}}
{{- $scanTauCluster := deepCopy (.Values.tauCluster | default dict) -}}
{{- $scanTauClusterFeatures := deepCopy (get $scanTauCluster "features" | default dict) -}}
{{- if eq (lower (get $scanTauClusterFeatures "multiKueue" | default "")) "disabled" -}}
{{- $_ := unset $scanTauClusterFeatures "multiKueue" -}}
{{- end -}}
{{- $_ := set $scanTauCluster "features" $scanTauClusterFeatures -}}
{{- $_ := set $scan "tauCluster" $scanTauCluster -}}
{{- if and (contains "multikueue" (lower (toJson $scan))) (not (and $featureEnabled $riskAcknowledged)) -}}
{{- fail "MultiKueue configuration was supplied without the required Beta gate; set both global.betaFeatures=[multikueue] and global.betaRiskAcknowledgements=[multikueue]" -}}
{{- end -}}
{{- end -}}
