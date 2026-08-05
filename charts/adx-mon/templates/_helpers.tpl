{{/*
Expand the name of the chart.
*/}}
{{- define "adx-mon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "adx-mon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default namespace.
Usage: {{ include "adx-mon.namespace" . }}
*/}}
{{- define "adx-mon.namespace" -}}
{{- default .Release.Namespace .Values.global.namespace -}}
{{- end }}

{{/*
Resolve cluster name for telemetry labels. Preference order:
1. explicit global.clusterName
2. ADX_CLUSTER_NAME from the referenced ConfigMap via Helm lookup (Helm install/upgrade path)
3. empty string
*/}}
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

{{/*
Common labels.
*/}}
{{- define "adx-mon.labels" -}}
helm.sh/chart: {{ include "adx-mon.chart" . }}
{{ include "adx-mon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "adx-mon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "adx-mon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component-specific selector labels.
Usage: {{ include "adx-mon.componentSelectorLabels" (dict "root" . "component" "operator") }}
*/}}
{{- define "adx-mon.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "adx-mon.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Component labels (common + component).
*/}}
{{- define "adx-mon.componentLabels" -}}
{{ include "adx-mon.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Create the name of the service account to use for a component.
Usage: {{ include "adx-mon.serviceAccountName" (dict "root" . "component" "operator") }}
*/}}
{{- define "adx-mon.serviceAccountName" -}}
{{- printf "%s-%s" (include "adx-mon.fullname" .root) .component | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Image tag helper — falls back to .Chart.AppVersion if tag is empty.
Usage: {{ include "adx-mon.imageTag" (dict "image" .Values.operator.image "chart" .Chart) }}
*/}}
{{- define "adx-mon.imageTag" -}}
{{- .image.tag | default .chart.AppVersion }}
{{- end }}

{{/*
Full image reference for a component.
Usage: {{ include "adx-mon.image" (dict "image" .Values.operator.image "chart" .Chart) }}
*/}}
{{- define "adx-mon.image" -}}
{{- printf "%s:%s" .image.repository (include "adx-mon.imageTag" (dict "image" .image "chart" .chart)) }}
{{- end }}

{{/*
Effective DaemonSet collector drop-metrics: dropMetrics minus scrapeDropMetricsExclude
(set difference). Singleton + remote-write keep the full dropMetrics. See PR #975 / #953.
Returns a JSON array string. Call with root context.
*/}}
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
