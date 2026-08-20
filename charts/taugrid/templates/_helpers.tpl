{{- define "taugrid.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: taugrid
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{/*
MultiKueue is a distribution-level Beta capability. Any value mentioning it,
including raw child-chart or custom queue values, must carry both independent
operator approvals. The reviewed activation path then explicitly turns on the
pinned Kueue fork and the Tau controller runtime capability.
*/}}
{{- define "taugrid.validateMultiKueueBetaGate" -}}
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
{{- $scanKueue := deepCopy (.Values.kueue | default dict) -}}
{{- $controllerManager := get $scanKueue "controllerManager" | default dict -}}
{{- $filteredFeatureGates := list -}}
{{- range (get $controllerManager "featureGates" | default list) -}}
  {{- if or (ne (lower (.name | default "")) "multikueue") (.enabled | default false) -}}
    {{- $filteredFeatureGates = append $filteredFeatureGates . -}}
  {{- end -}}
{{- end -}}
{{- $_ := set $controllerManager "featureGates" $filteredFeatureGates -}}
{{- $_ := set $scanKueue "controllerManager" $controllerManager -}}
{{- $scanAKSExtension := get $scanKueue "aksExtension" | default dict -}}
{{- if not (get $scanAKSExtension "enableMultiKueue" | default false) -}}
  {{- $_ := unset $scanAKSExtension "enableMultiKueue" -}}
{{- end -}}
{{- $_ := set $scanKueue "aksExtension" $scanAKSExtension -}}
{{- $_ := set $scan "kueue" $scanKueue -}}
{{- $scanTauController := deepCopy (get $scan "tau-core-controller" | default dict) -}}
{{- $scanTauCluster := deepCopy (get $scanTauController "tauCluster" | default dict) -}}
{{- $scanTauFeatures := deepCopy (get $scanTauCluster "features" | default dict) -}}
{{- if eq (lower (get $scanTauFeatures "multiKueue" | default "")) "disabled" -}}
{{- $_ := unset $scanTauFeatures "multiKueue" -}}
{{- end -}}
{{- $_ := set $scanTauCluster "features" $scanTauFeatures -}}
{{- $_ := set $scanTauController "tauCluster" $scanTauCluster -}}
{{- $_ := set $scan "tau-core-controller" $scanTauController -}}
{{- $mentionsMultiKueue := contains "multikueue" (lower (toJson $scan)) -}}
{{- if and $mentionsMultiKueue (not (and $featureEnabled $riskAcknowledged)) -}}
{{- fail "MultiKueue configuration was supplied without the required Beta gate; set both global.betaFeatures=[multikueue] and global.betaRiskAcknowledgements=[multikueue]" -}}
{{- end -}}

{{- if and $featureEnabled $riskAcknowledged -}}
  {{- if not .Values.components.kueue.enabled -}}
  {{- fail "MultiKueue Beta requires components.kueue.enabled=true" -}}
  {{- end -}}
  {{- if not .Values.components.tauCoreController.enabled -}}
  {{- fail "MultiKueue Beta requires components.tauCoreController.enabled=true so the runtime gate cannot be bypassed" -}}
  {{- end -}}
  {{- $kueueValues := .Values.kueue | default dict -}}
  {{- $aksExtension := get $kueueValues "aksExtension" | default dict -}}
  {{- if not (get $aksExtension "enableMultiKueue" | default false) -}}
  {{- fail "MultiKueue Beta approvals are set but the pinned Kueue controller is not enabled; use charts/taugrid/values-multikueue-beta.yaml rather than raw subchart values" -}}
  {{- end -}}
{{- end -}}
{{- end -}}
