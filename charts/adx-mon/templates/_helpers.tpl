{{- define "adx-mon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "adx-mon.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "adx-mon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "adx-mon.namespace" -}}
{{- default .Release.Namespace .Values.global.namespace -}}
{{- end }}

{{- define "adx-mon.clusterName" -}}
{{- $name := .Values.global.clusterName | default "" -}}
{{- if and (not $name) .Values.adx.configMapRef -}}
{{- $cm := lookup "v1" "ConfigMap" (include "adx-mon.namespace" .) .Values.adx.configMapRef -}}
{{- if and $cm $cm.data -}}
{{- $name = (get $cm.data "ADX_CLUSTER_NAME" | default "") -}}
{{- end -}}
{{- end -}}
{{- $name -}}
{{- end }}

{{- define "adx-mon.labels" -}}
helm.sh/chart: {{ include "adx-mon.chart" . }}
{{ include "adx-mon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "adx-mon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "adx-mon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "adx-mon.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "adx-mon.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "adx-mon.componentLabels" -}}
{{ include "adx-mon.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "adx-mon.serviceAccountName" -}}
{{- printf "%s-%s" (include "adx-mon.fullname" .root) .component | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "adx-mon.workloadIdentityClientId" -}}
{{- .Values.adx.clientId | trim | required "adx.clientId must be set when adx.workloadIdentity.enabled is true" -}}
{{- end }}

{{- define "adx-mon.imageTag" -}}
{{- .image.tag | default .chart.AppVersion }}
{{- end }}

{{- define "adx-mon.image" -}}
{{- printf "%s:%s" .image.repository (include "adx-mon.imageTag" (dict "image" .image "chart" .chart)) }}
{{- end }}

{{- define "adx-mon.collectorScrapeDropMetrics" -}}
{{- $ps := .Values.collector.prometheusScrape -}}
{{- $exclude := $ps.scrapeDropMetricsExclude | default (list) -}}
{{- $result := list -}}
{{- range ($ps.dropMetrics | default (list)) -}}
{{- if not (has . $exclude) -}}
{{- $result = append $result . -}}
{{- end -}}
{{- end -}}
{{- $result | toJson -}}
{{- end -}}
