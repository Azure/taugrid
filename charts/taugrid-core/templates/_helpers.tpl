{{/*
Common labels for all taugrid-core resources.
*/}}
{{- define "taugrid-core.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
tau.azure.com/component: taugrid-core
{{- end }}

{{/*
Whether `lookup` can see the cluster at all.

`lookup` returns empty both for an absent object and for every render that has
no API connection — `helm template`, `--dry-run=client`, ArgoCD manifest
generation. Probing an object that always exists separates the two, so a guard
can refuse a missing namespace at install time without failing every render.
*/}}
{{- define "taugrid-core.apiReachable" -}}
{{- if lookup "v1" "Namespace" "" "kube-system" -}}
reachable
{{- end -}}
{{- end }}

{{/*
Render a first-party image from exactly one release tag or immutable digest.
Call with: include "taugrid-core.image" (dict "component" "portal" "image" .Values.portal.image)
*/}}
{{- define "taugrid-core.image" -}}
{{- $component := .component -}}
{{- $image := .image -}}
{{- $repository := default "" $image.repository -}}
{{- $tag := default "" $image.tag -}}
{{- $digest := default "" $image.digest -}}
{{- if not $repository -}}
{{- fail (printf "%s.image.repository is required when %s.enabled=true" $component $component) -}}
{{- end -}}
{{- if eq (empty $tag) (empty $digest) -}}
{{- fail (printf "%s.image must set exactly one of tag or digest when %s.enabled=true" $component $component) -}}
{{- end -}}
{{- if and $digest (not (regexMatch "^sha256:[a-f0-9]{64}$" $digest)) -}}
{{- fail (printf "%s.image.digest must be sha256:<64 lowercase hex characters>" $component) -}}
{{- end -}}
{{- if $digest -}}
{{- printf "%s@%s" $repository $digest -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end }}

{{- define "taugrid-core.validateMultiKueueBetaGate" -}}
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
{{- if and (contains "multikueue" (lower (toJson $scan))) (not (and $featureEnabled $riskAcknowledged)) -}}
{{- fail "MultiKueue configuration was supplied without the required Beta gate; set both global.betaFeatures=[multikueue] and global.betaRiskAcknowledgements=[multikueue]" -}}
{{- end -}}
{{- end -}}
