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
Render the executable scripts and monitor configs stored in the shared ConfigMap.
The same rendered block is hashed for the ConfigMap name so its identity cannot
drift from its data.
*/}}
{{- define "gpu-monitoring.configMapData" -}}
{{- range $name := list "custom-plugin-monitor.json" "custom-plugin-monitor-h100.json" "custom-plugin-monitor-h100-nvl.json" "custom-plugin-monitor-gb200.json" "custom-plugin-monitor-spark.json" }}
{{ $name }}: |
{{ tpl ($.Files.Get (printf "configs/%s" $name)) $ | indent 2 }}
{{- end }}
system-log-monitor.json: |
{{ tpl (.Files.Get "configs/system-log-monitor.json") . | indent 2 }}
system-stats-monitor.json: |
{{ tpl (.Files.Get "configs/system-stats-monitor.json") . | indent 2 }}
kernel-monitor.json: |
{{ tpl (.Files.Get "configs/kernel-monitor.json") . | indent 2 }}
known-modules.json: |
{{ tpl (.Files.Get "configs/known-modules.json") . | indent 2 }}
check-nvidia-smi.sh: |
{{ tpl (.Files.Get "scripts/check-nvidia-smi.sh") . | indent 2 }}
check-nvidia-device-files.sh: |
{{ tpl (.Files.Get "scripts/check-nvidia-device-files.sh") . | indent 2 }}
check-dcgm-health.sh: |
{{ tpl (.Files.Get "scripts/check-dcgm-health.sh") . | indent 2 }}
check_gpu_count.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_count.sh") . | indent 2 }}
check_gpu_nvlink.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_nvlink.sh") . | indent 2 }}
check_gpu_xid.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_xid.sh") . | indent 2 }}
check_gpu_ecc.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_ecc.sh") . | indent 2 }}
check_gpu_ecc_from_sai.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_ecc_from_sai.sh") . | indent 2 }}
check_ib.sh: |
{{ tpl (.Files.Get "scripts/check_ib.sh") . | indent 2 }}
check_ib_pkeys.sh: |
{{ tpl (.Files.Get "scripts/check_ib_pkeys.sh") . | indent 2 }}
check_gpu_vbios.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_vbios.sh") . | indent 2 }}
check_gpu_throttle.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_throttle.sh") . | indent 2 }}
check_gpu_driver.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_driver.sh") . | indent 2 }}
check_gpu_ecc_remap_pending.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_ecc_remap_pending.sh") . | indent 2 }}
check_gpu_ecc_remap_failure.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_ecc_remap_failure.sh") . | indent 2 }}
check_gpu_nvlink_b200.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_nvlink_b200.sh") . | indent 2 }}
check_gpu_xid_always_fail.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_xid_always_fail.sh") . | indent 2 }}
check_ib_flaps.sh: |
{{ tpl (.Files.Get "scripts/check_ib_flaps.sh") . | indent 2 }}
check_nvme_mount.sh: |
{{ tpl (.Files.Get "scripts/check_nvme_mount.sh") . | indent 2 }}
check_temp_imex.sh: |
{{ tpl (.Files.Get "scripts/check_temp_imex.sh") . | indent 2 }}
nvidia-smi-wrapper.sh: |
{{ tpl (.Files.Get "scripts/nvidia-smi-wrapper.sh") . | indent 2 }}
check_roce.sh: |
{{ tpl (.Files.Get "scripts/check_roce.sh") . | indent 2 }}
{{- end }}

{{/*
Content-address the executable ConfigMap so updates create a new immutable object.
Truncating the configurable base keeps the complete DNS label within 63 bytes.
*/}}
{{- define "gpu-monitoring.configMapName" -}}
{{- $dataHash := include "gpu-monitoring.configMapData" . | sha256sum | trunc 10 -}}
{{- $baseName := .Values.configMap.name | default "gpu-monitoring-gpu" | trunc 52 | trimSuffix "-" -}}
{{- printf "%s-%s" $baseName $dataHash -}}
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
