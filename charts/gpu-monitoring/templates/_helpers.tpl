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
Fail closed when a deployment selects an unknown profile. A typo must not
silently render a cluster with no GPU health monitoring.
*/}}
{{- define "gpu-monitoring.validateEnabledGpuSkus" -}}
{{- range .Values.enabledGpuSkus -}}
{{- if not (hasKey $.Values.gpuSkus .) -}}
{{- fail (printf "enabledGpuSkus contains unknown profile %q" .) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Fail closed when two enabled profiles claim the same instance type. Profiles are
isolated by instance-type node affinity, so a node runs exactly one collector.
That isolation is what makes a Node condition single-owner: two collectors on one
node would race to publish the same availability condition from different
endpoints. Instance types are compared case-insensitively because profiles list
both the API and lowercase spellings of the same VM size.
*/}}
{{- define "gpu-monitoring.validateProfileInstanceTypes" -}}
{{- $owners := dict -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if and $sku (or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus)) -}}
{{- range $instanceType := (default (list) $sku.instanceTypes) -}}
{{- $key := lower (toString $instanceType) -}}
{{- if and (hasKey $owners $key) (ne (index $owners $key) $skuName) -}}
{{- fail (printf "instance type %q is claimed by profiles %q and %q; a node must run exactly one monitoring profile so its Node conditions have a single owner" $instanceType (index $owners $key) $skuName) -}}
{{- end -}}
{{- $_ := set $owners $key $skuName -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Render the executable scripts and monitor configs stored in the shared Secret.
The same rendered block is hashed for the Secret name so its identity cannot
drift from its data.
*/}}
{{- define "gpu-monitoring.executableBundleData" -}}
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
check_gpu_vbios_consistency.sh: |
{{ tpl (.Files.Get "scripts/check_gpu_vbios_consistency.sh") . | indent 2 }}
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
dcgmi-wrapper.sh: |
{{ tpl (.Files.Get "scripts/dcgmi-wrapper.sh") . | indent 2 }}
check_roce.sh: |
{{ tpl (.Files.Get "scripts/check_roce.sh") . | indent 2 }}
{{- end }}

{{/*
Require the host dcgmi check only when the selected profile scrapes a host-local
DCGM exporter. A profile can override the inference with dcgm_health_required.
*/}}
{{- define "gpu-monitoring.dcgmHealthRequired" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $scrapeTargets := $root.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- $required := false -}}
{{- range $scrapeTargets -}}
{{- if and (eq .name "dcgm-exporter") (regexMatch "^https?://(localhost|127\\.0\\.0\\.1)(:[0-9]+)?(/|$)" .url) -}}
{{- $required = true -}}
{{- end -}}
{{- end -}}
{{- if hasKey $sku "dcgm_health_required" -}}
{{- $required = $sku.dcgm_health_required -}}
{{- end -}}
{{- ternary "1" "0" $required -}}
{{- end }}

{{/*
Resolve the effective DCGM scrape-target availability contract for one profile.
Per-profile `dcgmAvailability` merges over the chart-level defaults, so a profile
that only overrides the DCGM endpoint keeps the availability guarantee.
Returns the settings as YAML; callers decode it with fromYaml.
*/}}
{{- define "gpu-monitoring.dcgmAvailability" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $availability := deepCopy (default (dict) $root.Values.metricsCollector.dcgmAvailability) -}}
{{- if hasKey $sku "dcgmAvailability" -}}
{{- $availability = mergeOverwrite $availability (deepCopy $sku.dcgmAvailability) -}}
{{- end -}}
{{- if $availability.enabled -}}
{{- $condition := default "" $availability.condition -}}
{{- if not (kindIs "string" $condition) -}}
{{- fail "metricsCollector.dcgmAvailability.condition must be a quoted string" -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z][A-Za-z0-9]*$" $condition) -}}
{{- fail (printf "metricsCollector.dcgmAvailability.condition %q must be an alphanumeric Node condition type" $condition) -}}
{{- end -}}
{{- range $field := list "unavailableFor" "availableFor" -}}
{{- $value := default "" (index $availability $field) -}}
{{- if not (and (kindIs "string" $value) (regexMatch "^[0-9]+(ms|s|m|h)$" $value)) -}}
{{- fail (printf "metricsCollector.dcgmAvailability.%s must be a quoted duration such as \"2m\" (got %v)" $field $value) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $availability | toYaml -}}
{{- end }}

{{/*
Render one scrape target's availability contract, if it declares one. A target
that publishes an availability condition is always required: the collector
rejects a condition without `required: true`, and a required target without a
condition would be monitored only by logs.
Call with: dict "target" $target "availability" $availability
*/}}
{{- define "gpu-monitoring.scrapeTargetAvailability" -}}
{{- $target := .target -}}
{{- $availability := .availability -}}
{{- $condition := default "" $target.availabilityCondition -}}
{{- $unavailableFor := default "" $target.unavailableFor -}}
{{- $availableFor := default "" $target.availableFor -}}
{{- if and (eq $target.name "dcgm-exporter") (default false $availability.enabled) (not $condition) -}}
{{- $condition = $availability.condition -}}
{{- $unavailableFor = default "" $availability.unavailableFor -}}
{{- $availableFor = default "" $availability.availableFor -}}
{{- else if and (default false $target.required) (not $condition) -}}
{{- fail (printf "scrapeTarget %q is required but sets no availabilityCondition" $target.name) -}}
{{- else if and $condition (not (default false $target.required)) -}}
{{- fail (printf "scrapeTarget %q sets availabilityCondition without required: true" $target.name) -}}
{{- end -}}
{{- if $condition -}}
{{- range $field := list "unavailableFor" "availableFor" -}}
{{- $value := index (dict "unavailableFor" $unavailableFor "availableFor" $availableFor) $field -}}
{{- if and $value (not (and (kindIs "string" $value) (regexMatch "^[0-9]+(ms|s|m|h)$" $value))) -}}
{{- fail (printf "scrapeTarget %q %s must be a quoted duration such as \"2m\" (got %v)" $target.name $field $value) -}}
{{- end -}}
{{- end -}}
required: true
availabilityCondition: {{ $condition }}
{{- if $unavailableFor }}
unavailableFor: {{ $unavailableFor }}
{{- end }}
{{- if $availableFor }}
availableFor: {{ $availableFor }}
{{- end }}
{{- end -}}
{{- end }}

{{/*
Render one SKU's metrics collector rules. The DaemonSet hashes this exact payload
so ConfigMap changes roll only the affected profile.
*/}}
{{- define "gpu-monitoring.metricsCollectorConfig" -}}
{{- $root := .root }}
{{- $sku := .sku }}
{{- $scrapeTargets := $root.Values.metricsCollector.scrapeTargets }}
{{- if hasKey $sku "scrapeTargets" }}
{{- $scrapeTargets = $sku.scrapeTargets }}
{{- end }}
{{- $availability := fromYaml (include "gpu-monitoring.dcgmAvailability" (dict "root" $root "sku" $sku)) }}
{{- $nodeExporterEnabled := $root.Values.nodeExporter.enabled }}
{{- if hasKey $sku "nodeExporter" }}
{{- $nodeExporterEnabled = $sku.nodeExporter }}
{{- end }}
{{- $nodeExporterScrapeEnabled := $nodeExporterEnabled }}
{{- if hasKey $root.Values.metricsCollector "nodeExporterScrape" }}
{{- $nodeExporterScrapeEnabled = $root.Values.metricsCollector.nodeExporterScrape }}
{{- end }}
{{- if hasKey $sku "nodeExporterScrape" }}
{{- $nodeExporterScrapeEnabled = $sku.nodeExporterScrape }}
{{- end }}
scrapeTargets:
  {{- $dcgmTargetFound := false }}
  {{- range $target := $scrapeTargets }}
  {{- if eq $target.name "node-exporter" }}
  {{- if $nodeExporterScrapeEnabled }}
  - name: {{ $target.name }}
    url: {{ $target.url }}
    {{- with (include "gpu-monitoring.scrapeTargetAvailability" (dict "target" $target "availability" $availability)) }}{{ . | nindent 4 }}{{- end }}
  {{- end }}
  {{- else }}
  {{- if eq $target.name "dcgm-exporter" }}
  {{- $dcgmTargetFound = true }}
  {{- end }}
  - name: {{ $target.name }}
    url: {{ $target.url }}
    {{- with (include "gpu-monitoring.scrapeTargetAvailability" (dict "target" $target "availability" $availability)) }}{{ . | nindent 4 }}{{- end }}
  {{- end }}
  {{- end }}
  {{- if and (default false $availability.enabled) (not $dcgmTargetFound) }}
  {{- fail "metricsCollector.dcgmAvailability is enabled but no scrapeTarget is named \"dcgm-exporter\"; add the target or set metricsCollector.dcgmAvailability.enabled=false" }}
  {{- end }}
  {{- if $root.Values.metricsCollector.npdScrape }}
  - name: node-problem-detector
    url: http://localhost:{{ add $root.Values.npdPort 1 }}/metrics
  {{- end }}
rules:
  {{- toYaml $root.Values.metricsCollector.rules | nindent 2 }}
{{- end }}

{{/*
Content-address the executable bundle so updates create a new immutable object.
Truncating the configurable base keeps the complete DNS label within 63 bytes.
*/}}
{{- define "gpu-monitoring.executableBundleName" -}}
{{- $dataHash := include "gpu-monitoring.executableBundleData" . | sha256sum | trunc 10 -}}
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
