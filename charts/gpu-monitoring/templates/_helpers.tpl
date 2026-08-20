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
{{- if trim (default "" .Values.namespace) -}}
{{- fail "gpu-monitoring.namespace is no longer supported; install the release with --namespace to move TauGrid system components together" -}}
{{- end -}}
{{- .Release.Namespace -}}
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

{{/* Custom plugin monitor configs bundled into the immutable executable Secret. */}}
{{- define "gpu-monitoring.bundledMonitorConfigNames" -}}
- custom-plugin-monitor.json
- custom-plugin-monitor-h100.json
- custom-plugin-monitor-h100-nvl.json
- custom-plugin-monitor-gb200.json
- custom-plugin-monitor-spark.json
{{- end }}

{{/* Validate profile fields consumed unconditionally by DaemonSet rendering. */}}
{{- define "gpu-monitoring.validateProfile" -}}
{{- $name := .name -}}
{{- $sku := .sku -}}
{{- if or (not (kindIs "map" $sku)) (eq (len $sku) 0) -}}
{{- fail (printf "enabledGpuSkus profile %q must resolve to a non-empty map" $name) -}}
{{- end -}}
{{- if not (hasKey $sku "instanceTypes") -}}
{{- fail (printf "gpuSkus.%s.instanceTypes must be a non-empty list of non-empty strings" $name) -}}
{{- end -}}
{{- $instanceTypes := index $sku "instanceTypes" -}}
{{- if or (not (kindIs "slice" $instanceTypes)) (eq (len $instanceTypes) 0) -}}
{{- fail (printf "gpuSkus.%s.instanceTypes must be a non-empty list of non-empty strings" $name) -}}
{{- end -}}
{{- range $instanceType := $instanceTypes -}}
{{- if or (not (kindIs "string" $instanceType)) (empty (trim $instanceType)) -}}
{{- fail (printf "gpuSkus.%s.instanceTypes must be a non-empty list of non-empty strings" $name) -}}
{{- end -}}
{{- end -}}
{{- $numGpus := index $sku "num_gpus" -}}
{{- $numericKinds := list "int" "int8" "int16" "int32" "int64" "uint" "uint8" "uint16" "uint32" "uint64" "float32" "float64" -}}
{{- if or (not (has (kindOf $numGpus) $numericKinds)) (not (regexMatch "^[0-9]+$" (toString $numGpus))) -}}
{{- fail (printf "gpuSkus.%s.num_gpus must be a nonnegative integer" $name) -}}
{{- end -}}
{{- $monitorConfig := index $sku "monitor_config" -}}
{{- $bundledMonitorConfigs := include "gpu-monitoring.bundledMonitorConfigNames" . | fromYamlArray -}}
{{- if or (not (kindIs "string" $monitorConfig)) (empty (trim $monitorConfig)) (not (has $monitorConfig $bundledMonitorConfigs)) -}}
{{- fail (printf "gpuSkus.%s.monitor_config must name a bundled custom-plugin monitor config" $name) -}}
{{- end -}}
{{- end }}

{{/*
The 0.1.6 chart publicly exposed a generic scrape-target availability/ownership
API (metricsCollector.scrapeTargets, metricsCollector.dcgmAvailability, and the
matching gpuSkus.<profile> keys, plus dcgm_health_required). Chart 0.1.7
replaced it with the constrained dcgmHealth.source / dcgmHealth.exporterUrl
contract. Fail loudly rather than silently ignoring the removed keys so a
values file written against 0.1.6 cannot deploy with an unintended default.
*/}}
{{- define "gpu-monitoring.validateNoLegacyDcgmKeys" -}}
{{- $migration := "migrate to dcgmHealth.source and dcgmHealth.exporterUrl (see the README \"DCGM health sources\" section)" -}}
{{- if hasKey .Values.metricsCollector "scrapeTargets" -}}
{{- fail (printf "metricsCollector.scrapeTargets was removed in gpu-monitoring 0.1.7; %s" $migration) -}}
{{- end -}}
{{- if hasKey .Values.metricsCollector "dcgmAvailability" -}}
{{- fail (printf "metricsCollector.dcgmAvailability was removed in gpu-monitoring 0.1.7; %s" $migration) -}}
{{- end -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if kindIs "map" $sku -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- fail (printf "gpuSkus.%s.scrapeTargets was removed in gpu-monitoring 0.1.7; %s" $skuName $migration) -}}
{{- end -}}
{{- if hasKey $sku "dcgmAvailability" -}}
{{- fail (printf "gpuSkus.%s.dcgmAvailability was removed in gpu-monitoring 0.1.7; %s" $skuName $migration) -}}
{{- end -}}
{{- if hasKey $sku "dcgm_health_required" -}}
{{- fail (printf "gpuSkus.%s.dcgm_health_required was removed in gpu-monitoring 0.1.7; %s" $skuName $migration) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Fail closed when a deployment selects an unknown or structurally unsafe profile.
An empty selection renders and therefore validates every configured profile.
*/}}
{{- define "gpu-monitoring.validateEnabledGpuSkus" -}}
{{- include "gpu-monitoring.validateNoLegacyDcgmKeys" . -}}
{{- if empty .Values.enabledGpuSkus -}}
{{- range $name, $sku := .Values.gpuSkus -}}
{{- include "gpu-monitoring.validateProfile" (dict "name" $name "sku" $sku) -}}
{{- end -}}
{{- else -}}
{{- range $name := .Values.enabledGpuSkus -}}
{{- if not (hasKey $.Values.gpuSkus $name) -}}
{{- fail (printf "enabledGpuSkus contains unknown profile %q" $name) -}}
{{- end -}}
{{- include "gpu-monitoring.validateProfile" (dict "name" $name "sku" (index $.Values.gpuSkus $name)) -}}
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

{{/* Environment names owned by the chart or its DCGM lifecycle wrappers. */}}
{{- define "gpu-monitoring.reservedEnvNames" -}}
- NODE_NAME
- NPD_GPU_REQUIRED
- NPD_DCGM_REQUIRED
- EXPECTED_NUM_GPU
- VBIOS_VERSIONS
- IB_DEVICES
- EXPECTED_IB_PKEY
- EXPECTED_IB_GBPS
- IB_FLAP_THRESHOLD_SHORT
- IB_FLAP_CHECK_WINDOW
- NVME_TOTAL
- NVME_SIZE_COUNT
- NVME_SIZE
- IMEX_PROCNAME
- CREATE_IMEX_CHANNEL_EXPECTED
- ROCE_DEVICES
- GPU_TYPE
- GPU_DRIVER_VERSIONS
- NPD_BINARY
- NPD_DCGM_WARMUP_SECONDS
- NPD_DCGM_SIMULATION
- NPD_GPU_SIMULATION
{{- end }}

{{/* User-provided entries must neither override chart state nor duplicate names. */}}
{{- define "gpu-monitoring.validateReservedEnv" -}}
{{- $reserved := include "gpu-monitoring.reservedEnvNames" . | fromYamlArray -}}
{{- $seen := dict -}}
{{- range .Values.env -}}
{{- $name := default "" .name -}}
{{- if has $name $reserved -}}
{{- fail (printf "env must not override reserved variable %s" $name) -}}
{{- end -}}
{{- if and $name (hasKey $seen $name) -}}
{{- fail (printf "env contains duplicate variable %s" $name) -}}
{{- end -}}
{{- $_ := set $seen $name true -}}
{{- end -}}
{{- end }}

{{/*
Classify a dcgm-exporter URL using only the endpoint forms this chart accepts:
an absolute, entirely lowercase HTTP(S) URL with no whitespace, backslashes,
userinfo credentials, query string, or fragment, and either a loopback host
(localhost/127.0.0.1) or a remote host/service. Callers must reject "invalid"
before rendering the URL.
*/}}
{{- define "gpu-monitoring.dcgmExporterUrlKind" -}}
{{- $url := .url -}}
{{- if not (kindIs "string" $url) -}}
invalid
{{- else if empty $url -}}
invalid
{{- else if ne $url (lower $url) -}}
invalid
{{- else if regexMatch "[[:space:]]" $url -}}
invalid
{{- else if contains "\\" $url -}}
invalid
{{- else if contains "@" $url -}}
invalid
{{- else -}}
{{- $withoutEscapes := regexReplaceAll "%[0-9a-f]{2}" $url "" -}}
{{- $percentSafe := not (contains "%" $withoutEscapes) -}}
{{- $ipv6Host := "(([0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|([0-9a-f]{1,4}:){1,7}:|([0-9a-f]{1,4}:){1,6}:[0-9a-f]{1,4}|([0-9a-f]{1,4}:){1,5}(:[0-9a-f]{1,4}){1,2}|([0-9a-f]{1,4}:){1,4}(:[0-9a-f]{1,4}){1,3}|([0-9a-f]{1,4}:){1,3}(:[0-9a-f]{1,4}){1,4}|([0-9a-f]{1,4}:){1,2}(:[0-9a-f]{1,4}){1,5}|[0-9a-f]{1,4}:((:[0-9a-f]{1,4}){1,6})|:((:[0-9a-f]{1,4}){1,7}|:))" -}}
{{- $urlSuffix := "(/[^\\\\?#[:space:]]*)?" -}}
{{- $loopbackPattern := printf "^https?://(localhost|127(\\.[0-9]{1,3}){3}|\\[::1\\]|\\[0:0:0:0:0:0:0:1\\])(:[0-9]+)?%s$" $urlSuffix -}}
{{- $remotePattern := printf "^https?://(\\[%s\\]|[^/@\\\\:?#%%\\[\\][:space:]]+)(:[0-9]+)?%s$" $ipv6Host $urlSuffix -}}
{{- if not $percentSafe -}}
invalid
{{- else if regexMatch $loopbackPattern $url -}}
loopback
{{- else if regexMatch $remotePattern $url -}}
remote
{{- else -}}
invalid
{{- end -}}
{{- end -}}
{{- end }}

{{/* Convert scalar identifiers to the exact string emitted by quote/toYaml. */}}
{{- define "gpu-monitoring.scalarIdentifier" -}}
{{- $value := .value -}}
{{- if or (kindIs "string" $value) (kindIs "bool" $value) (kindIs "float64" $value) (kindIs "int" $value) (kindIs "int64" $value) -}}
{{- toString $value -}}
{{- end -}}
{{- end }}

{{/* A human-readable type label for a validation error message; nil reports as "null" rather than Go's "invalid" reflect.Kind name. */}}
{{- define "gpu-monitoring.dcgmHealthKindLabel" -}}
{{- if kindIs "invalid" .value -}}
null
{{- else -}}
{{- kindOf .value -}}
{{- end -}}
{{- end }}

{{/*
Resolve one profile's effective DCGM health source contract. Global dcgmHealth
values are the default; gpuSkus.<profile>.dcgmHealth overrides only the keys
it sets, so a profile can override just `source`, just `exporterUrl`, or both
while a mixed cluster keeps the rest of its configuration untouched.
Returns the settings as YAML; callers decode it with fromYaml.

Only a key that is completely absent from gpuSkus.<profile> counts as "no
per-profile override". A key present with a null or any other non-map value
(a string, bool, number, or list) fails loudly here, with the profile name in
the message, before it ever reaches deepCopy/mergeOverwrite — those Sprig
functions panic with an unreadable reflect error on a non-map input, and a
silently-ignored null would hide a typo behind the global default instead of
surfacing it.
*/}}
{{- define "gpu-monitoring.effectiveDcgmHealth" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $skuName := default "profile" .skuName -}}
{{- $global := $root.Values.dcgmHealth -}}
{{- if not (kindIs "map" $global) -}}
{{- fail (printf "gpuSkus.%s: global dcgmHealth must be a map with source and/or exporterUrl keys; got %s" $skuName (include "gpu-monitoring.dcgmHealthKindLabel" (dict "value" $global))) -}}
{{- end -}}
{{- $effective := deepCopy $global -}}
{{- if hasKey $sku "dcgmHealth" -}}
{{- $override := index $sku "dcgmHealth" -}}
{{- if not (kindIs "map" $override) -}}
{{- fail (printf "gpuSkus.%s.dcgmHealth must be a map with source and/or exporterUrl keys; got %s" $skuName (include "gpu-monitoring.dcgmHealthKindLabel" (dict "value" $override))) -}}
{{- end -}}
{{- $effective = mergeOverwrite $effective (deepCopy $override) -}}
{{- end -}}
{{- $effective | toYaml -}}
{{- end }}

{{/*
Validate one profile's effective DCGM health source contract. `source` is a
closed two-value enum and the chart itself owns the dcgm-exporter scrape
target and its fixed DcgmExporterUnavailable condition and debounce, so no
other availability field is accepted here. The error text never echoes the
URL value so a credentialed or token-bearing endpoint cannot leak into it.
*/}}
{{- define "gpu-monitoring.validateDcgmHealth" -}}
{{- $skuName := .skuName -}}
{{- $effective := .effective -}}
{{- if not (kindIs "map" $effective) -}}
{{- fail (printf "gpuSkus.%s effective dcgmHealth must be a map" $skuName) -}}
{{- end -}}
{{- $source := index $effective "source" -}}
{{- $validSources := list "host-dcgmi" "exporter" -}}
{{- if not (and (kindIs "string" $source) (has $source $validSources)) -}}
{{- fail (printf "gpuSkus.%s effective dcgmHealth.source must be exactly one of \"host-dcgmi\" or \"exporter\"" $skuName) -}}
{{- end -}}
{{- $urlKind := include "gpu-monitoring.dcgmExporterUrlKind" (dict "url" (index $effective "exporterUrl")) -}}
{{- if eq $urlKind "invalid" -}}
{{- fail (printf "gpuSkus.%s effective dcgmHealth.exporterUrl must be a nonempty absolute lowercase http(s) URL without whitespace, backslashes, credentials, or URL suffix parameters" $skuName) -}}
{{- end -}}
{{- if and (eq $source "host-dcgmi") (ne $urlKind "loopback") -}}
{{- fail (printf "gpuSkus.%s dcgmHealth.source=host-dcgmi requires a loopback dcgmHealth.exporterUrl" $skuName) -}}
{{- end -}}
{{- if and (eq $source "exporter") (ne $urlKind "remote") -}}
{{- fail (printf "gpuSkus.%s dcgmHealth.source=exporter requires an explicit non-loopback dcgmHealth.exporterUrl; it must not inherit the host-dcgmi loopback default" $skuName) -}}
{{- end -}}
{{- end }}

{{/*
Every rendered profile enters host mount namespaces through wrapper scripts.
Only dcgmHealth.source=host-dcgmi additionally requires host networking, since
it is the only source that reaches the host-local exporter over loopback.
*/}}
{{- define "gpu-monitoring.validateHostNamespaces" -}}
{{- $profileCount := 0 -}}
{{- $hostDcgmi := false -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus) -}}
{{- $profileCount = add1 $profileCount -}}
{{- $effective := fromYaml (include "gpu-monitoring.effectiveDcgmHealth" (dict "root" $ "sku" $sku "skuName" $skuName)) -}}
{{- include "gpu-monitoring.validateDcgmHealth" (dict "skuName" $skuName "effective" $effective) -}}
{{- if eq $effective.source "host-dcgmi" -}}
{{- $hostDcgmi = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if gt $profileCount 0 -}}
{{- $hostPID := index .Values.daemonset "hostPID" -}}
{{- if or (not (kindIs "bool" $hostPID)) (not $hostPID) -}}
{{- fail "daemonset.hostPID must be boolean true when any GPU profile is enabled" -}}
{{- end -}}
{{- end -}}
{{- if $hostDcgmi -}}
{{- $hostNetwork := index .Values.daemonset "hostNetwork" -}}
{{- if or (not (kindIs "bool" $hostNetwork)) (not $hostNetwork) -}}
{{- fail "daemonset.hostNetwork must be boolean true when an enabled profile uses dcgmHealth.source=host-dcgmi" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Both remaining dcgmHealth sources include the fixed DcgmExporterUnavailable
scrape target and its collector rules: host-dcgmi still scrapes the host-local
dcgm-exporter over loopback, and exporter scrapes a remote/Service exporter.
The metrics collector is therefore mandatory for every enabled profile — it is
the only component that renders that scrape target, its availability
condition, and every DCGM_* rule. Deployment-level chart/component disable
(not disabling the collector alone) is the only opt-out.
*/}}
{{- define "gpu-monitoring.validateDcgmHealthContracts" -}}
{{- $customPluginMonitor := .Values.monitors.customPluginMonitor -}}
{{- if not (kindIs "bool" $customPluginMonitor) -}}
{{- fail "monitors.customPluginMonitor must be a boolean" -}}
{{- end -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus) -}}
{{- $effective := fromYaml (include "gpu-monitoring.effectiveDcgmHealth" (dict "root" $ "sku" $sku "skuName" $skuName)) -}}
{{- include "gpu-monitoring.validateDcgmHealth" (dict "skuName" $skuName "effective" $effective) -}}
{{- $collectorEnabled := $.Values.metricsCollector.enabled -}}
{{- if not (and (kindIs "bool" $collectorEnabled) $collectorEnabled) -}}
{{- fail (printf "gpuSkus.%s uses dcgmHealth.source=%s and requires metricsCollector.enabled=true" $skuName $effective.source) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The chart owns the fixed DcgmExporterUnavailable condition for any profile
whose effective dcgmHealth.source scrapes an exporter (host-dcgmi or
exporter). This is a minimal collision guard, not a generic condition
language: it only checks that no collector rule or effective NPD monitor
condition claims that reserved name.
*/}}
{{- define "gpu-monitoring.validateDcgmExporterUnavailableGuard" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $skuName := .skuName -}}
{{- $fixedCondition := "DcgmExporterUnavailable" -}}
{{- range $rule := (default (list) $root.Values.metricsCollector.rules) -}}
{{- if and (kindIs "map" $rule) (hasKey $rule "conditionType") (eq (include "gpu-monitoring.scalarIdentifier" (dict "value" (index $rule "conditionType"))) $fixedCondition) -}}
{{- fail (printf "gpuSkus.%s cannot use conditionType %q; it is reserved for the chart-owned dcgm-exporter availability condition" $skuName $fixedCondition) -}}
{{- end -}}
{{- end -}}
{{- $npdConditions := dict -}}
{{- if $root.Values.monitors.customPluginMonitor -}}
{{- $monitorConfig := include "gpu-monitoring.renderMonitorConfig" (dict "root" $root "sku" $sku "skuName" $skuName) | fromJson -}}
{{- range $condition := (default (list) $monitorConfig.conditions) -}}
{{- if and (kindIs "map" $condition) (kindIs "string" $condition.type) (not (empty $condition.type)) -}}
{{- $_ := set $npdConditions $condition.type true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $monitor := list (dict "enabled" $root.Values.monitors.systemLogMonitor "file" "system-log-monitor.json") (dict "enabled" $root.Values.monitors.systemStatsMonitor "file" "system-stats-monitor.json") (dict "enabled" $root.Values.monitors.kernelMonitor "file" "kernel-monitor.json") -}}
{{- if $monitor.enabled -}}
{{- $monitorConfig := $root.Files.Get (printf "configs/%s" $monitor.file) | fromJson -}}
{{- range $condition := (default (list) $monitorConfig.conditions) -}}
{{- if and (kindIs "map" $condition) (kindIs "string" $condition.type) (not (empty $condition.type)) -}}
{{- $_ := set $npdConditions $condition.type true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if hasKey $npdConditions $fixedCondition -}}
{{- fail (printf "gpuSkus.%s cannot use condition %q; it is reserved for the chart-owned dcgm-exporter availability condition" $skuName $fixedCondition) -}}
{{- end -}}
{{- end }}

{{/*
Render a selected custom plugin monitor config. Profiles without host dcgmi keep
the permanent condition rule so NPD transitions initial or stale False to
Unknown, but omit the temporary rule that would emit recurring warning events.
*/}}
{{- define "gpu-monitoring.renderMonitorConfig" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $skuName := .skuName -}}
{{- $config := mustFromJson ($root.Files.Get (printf "configs/%s" $sku.monitor_config)) -}}
{{- if eq (include "gpu-monitoring.dcgmHealthRequired" (dict "root" $root "sku" $sku "skuName" $skuName)) "0" -}}
{{- $rules := list -}}
{{- range $rule := index $config "rules" -}}
{{- if or (ne (default "" $rule.path) "/custom-config/check-dcgm-health.sh") (eq (default "" $rule.type) "permanent") -}}
{{- $rules = append $rules $rule -}}
{{- end -}}
{{- end -}}
{{- $_ := set $config "rules" $rules -}}
{{- end -}}
{{- $config | toPrettyJson -}}
{{- end }}

{{/* Return the Secret data key mounted as the profile's custom monitor config. */}}
{{- define "gpu-monitoring.monitorConfigKey" -}}
{{- if eq (include "gpu-monitoring.dcgmHealthRequired" .) "0" -}}
{{- $config := include "gpu-monitoring.renderMonitorConfig" . -}}
{{- printf "no-host-dcgm-monitor-%s.json" ($config | sha256sum | trunc 10) -}}
{{- else -}}
{{- .sku.monitor_config -}}
{{- end -}}
{{- end }}

{{/*
Render executable scripts and monitor configs stored in the shared Secret. The
same block is hashed for the Secret name so its identity cannot drift from data.
*/}}
{{- define "gpu-monitoring.executableBundleData" -}}
{{- range $name := (include "gpu-monitoring.bundledMonitorConfigNames" . | fromYamlArray) }}
{{ $name }}: |
{{ tpl ($.Files.Get (printf "configs/%s" $name)) $ | indent 2 }}
{{- end }}
{{- $renderedRemoteConfigs := dict -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if and $sku (or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus)) -}}
{{- if eq (include "gpu-monitoring.dcgmHealthRequired" (dict "root" $ "sku" $sku "skuName" $skuName)) "0" -}}
{{- $config := include "gpu-monitoring.renderMonitorConfig" (dict "root" $ "sku" $sku "skuName" $skuName) -}}
{{- $key := include "gpu-monitoring.monitorConfigKey" (dict "root" $ "sku" $sku "skuName" $skuName) -}}
{{- $_ := set $renderedRemoteConfigs $key $config -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $key, $config := $renderedRemoteConfigs }}
{{ $key }}: |
{{ $config | indent 2 }}
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
check-dcgm-watches.sh: |
{{ tpl (.Files.Get "scripts/check-dcgm-watches.sh") . | indent 2 }}
init-dcgm-health.sh: |
{{ tpl (.Files.Get "scripts/init-dcgm-health.sh") . | indent 2 }}
start-node-problem-detector.sh: |
{{ tpl (.Files.Get "scripts/start-node-problem-detector.sh") . | indent 2 }}
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
Require the host dcgmi lifecycle only when the profile's effective
dcgmHealth.source is host-dcgmi. exporter profiles never run it.
*/}}
{{- define "gpu-monitoring.dcgmHealthRequired" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $skuName := .skuName -}}
{{- $effective := fromYaml (include "gpu-monitoring.effectiveDcgmHealth" (dict "root" $root "sku" $sku "skuName" $skuName)) -}}
{{- ternary "1" "0" (and $root.Values.monitors.customPluginMonitor (eq $effective.source "host-dcgmi")) -}}
{{- end }}

{{/*
Render one SKU's metrics collector rules. The DaemonSet hashes this exact payload
so ConfigMap changes roll only the affected profile.

The chart constructs and owns the dcgm-exporter scrape target and its fixed
DcgmExporterUnavailable contract; there is no configurable required,
availabilityCondition, condition name, or debounce. Both remaining sources
(host-dcgmi and exporter) scrape the profile's effective
dcgmHealth.exporterUrl, so every accepted config renders the dcgm-exporter
target and keeps every DCGM_* collector rule.
*/}}
{{- define "gpu-monitoring.metricsCollectorConfig" -}}
{{- $root := .root }}
{{- $sku := .sku }}
{{- $skuName := default "profile" .skuName }}
{{- $effective := fromYaml (include "gpu-monitoring.effectiveDcgmHealth" (dict "root" $root "sku" $sku "skuName" $skuName)) }}
{{- include "gpu-monitoring.validateDcgmHealth" (dict "skuName" $skuName "effective" $effective) }}
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
{{- $rules := $root.Values.metricsCollector.rules }}
{{- include "gpu-monitoring.validateDcgmExporterUnavailableGuard" (dict "root" $root "sku" $sku "skuName" $skuName) }}
scrapeTargets:
  - name: "dcgm-exporter"
    url: {{ $effective.exporterUrl | quote }}
    required: true
    availabilityCondition: "DcgmExporterUnavailable"
    unavailableFor: "2m"
    availableFor: "1m"
  {{- if $nodeExporterScrapeEnabled }}
  - name: "node-exporter"
    url: "http://localhost:9100/metrics"
  {{- end }}
  {{- if $root.Values.metricsCollector.npdScrape }}
  - name: "node-problem-detector"
    url: {{ printf "http://localhost:%d/metrics" (add $root.Values.npdPort 1) | quote }}
  {{- end }}
rules:
  {{- toYaml $rules | nindent 2 }}
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
