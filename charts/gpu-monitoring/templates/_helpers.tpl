{{/*
Expand the name of the chart.
*/}}
{{- define "gpu-monitoring.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "gpu-monitoring.fullname" -}}
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
Create a default namespace.
*/}}
{{- define "gpu-monitoring.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "gpu-monitoring.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gpu-monitoring.labels" -}}
helm.sh/chart: {{ include "gpu-monitoring.chart" . }}
{{ include "gpu-monitoring.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.labels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gpu-monitoring.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gpu-monitoring.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Render a first-party image from exactly one immutable digest or legacy tag.
Call with: include "gpu-monitoring.image" (dict "component" "metricsCollector" "image" .Values.metricsCollector.image)
*/}}
{{- define "gpu-monitoring.image" -}}
{{- $component := .component -}}
{{- $image := .image -}}
{{- $repository := default "" $image.repository -}}
{{- $tag := default "" $image.tag -}}
{{- $digest := default "" $image.digest -}}
{{- if not $repository -}}
{{- fail (printf "%s.image.repository is required" $component) -}}
{{- end -}}
{{- if eq (empty $tag) (empty $digest) -}}
{{- fail (printf "%s.image must set exactly one of tag or digest" $component) -}}
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
