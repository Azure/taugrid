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
Fail closed when a deployment selects an unknown or structurally unsafe profile.
An empty selection renders and therefore validates every configured profile.
*/}}
{{- define "gpu-monitoring.validateEnabledGpuSkus" -}}
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
Classify a dcgm-exporter URL using only the endpoint forms supported by this
chart: localhost/127.0.0.1 or an absolute remote HTTP(S) service URL.
*/}}
{{- define "gpu-monitoring.dcgmTargetKind" -}}
{{- $url := .url -}}
{{- if not (kindIs "string" $url) -}}
invalid
{{- else -}}
{{- $lowerURL := lower $url -}}
{{- $withoutEscapes := regexReplaceAll "%[0-9A-Fa-f]{2}" $url "" -}}
{{- $percentSafe := not (contains "%" $withoutEscapes) -}}
{{- $lowercaseScheme := regexMatch "^https?://" $url -}}
{{- $backslashSafe := not (contains "\\" $url) -}}
{{- $ipv6Host := "(([0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}|([0-9A-Fa-f]{1,4}:){1,7}:|([0-9A-Fa-f]{1,4}:){1,6}:[0-9A-Fa-f]{1,4}|([0-9A-Fa-f]{1,4}:){1,5}(:[0-9A-Fa-f]{1,4}){1,2}|([0-9A-Fa-f]{1,4}:){1,4}(:[0-9A-Fa-f]{1,4}){1,3}|([0-9A-Fa-f]{1,4}:){1,3}(:[0-9A-Fa-f]{1,4}){1,4}|([0-9A-Fa-f]{1,4}:){1,2}(:[0-9A-Fa-f]{1,4}){1,5}|[0-9A-Fa-f]{1,4}:((:[0-9A-Fa-f]{1,4}){1,6})|:((:[0-9A-Fa-f]{1,4}){1,7}|:))" -}}
{{- $urlSuffix := "([/?#][^\\\\[:space:]]*)?" -}}
{{- $hostLocalURLPattern := printf "^https?://(localhost|127\\.0\\.0\\.1)(:[0-9]+)?%s$" $urlSuffix -}}
{{- $remoteURLPattern := printf "^https?://(\\[%s\\]|[^/@\\\\:?#%%\\[\\][:space:]]+)(:[0-9]+)?%s$" $ipv6Host $urlSuffix -}}
{{- if and $lowercaseScheme $percentSafe $backslashSafe (regexMatch $hostLocalURLPattern $lowerURL) -}}
host-local
{{- else if and $lowercaseScheme $percentSafe $backslashSafe (regexMatch $remoteURLPattern $url) -}}
remote
{{- else -}}
invalid
{{- end -}}
{{- end -}}
{{- end }}

{{/* Classify one profile by its effective dcgm-exporter target locality. */}}
{{- define "gpu-monitoring.dcgmProfileKind" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $scrapeTargets := $root.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- $hostLocal := false -}}
{{- $remote := false -}}
{{- range $target := $scrapeTargets -}}
{{- if and (kindIs "map" $target) (kindIs "string" $target.name) (eq $target.name "dcgm-exporter") -}}
{{- $kind := include "gpu-monitoring.dcgmTargetKind" $target -}}
{{- if eq $kind "host-local" -}}
{{- $hostLocal = true -}}
{{- else if eq $kind "remote" -}}
{{- $remote = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $hostLocal -}}
host-local
{{- else if $remote -}}
remote
{{- else -}}
none
{{- end -}}
{{- end }}

{{/* Kubernetes-owned Node conditions must never be written by the collector. */}}
{{- define "gpu-monitoring.kubernetesCoreConditionTypes" -}}
- Ready
- MemoryPressure
- DiskPressure
- PIDPressure
- NetworkUnavailable
{{- end }}

{{/* Convert scalar identifiers to the exact string emitted by quote/toYaml. */}}
{{- define "gpu-monitoring.scalarIdentifier" -}}
{{- $value := .value -}}
{{- if or (kindIs "string" $value) (kindIs "bool" $value) (kindIs "float64" $value) (kindIs "int" $value) (kindIs "int64" $value) -}}
{{- toString $value -}}
{{- end -}}
{{- end }}

{{/*
Resolve one target's effective availability contract. Global DCGM availability
is injected only into the unique dcgm-exporter when it has no explicit
condition. Callers must validate the returned fields before rendering them.
*/}}
{{- define "gpu-monitoring.effectiveScrapeTargetAvailability" -}}
{{- $target := .target -}}
{{- $availability := .availability -}}
{{- $name := "" -}}
{{- if and (kindIs "map" $target) (hasKey $target "name") -}}
{{- $name = index $target "name" -}}
{{- end -}}
{{- $nameKey := include "gpu-monitoring.scalarIdentifier" (dict "value" $name) -}}
{{- $condition := "" -}}
{{- if and (kindIs "map" $target) (hasKey $target "availabilityCondition") -}}
{{- $condition = index $target "availabilityCondition" -}}
{{- end -}}
{{- $required := false -}}
{{- if and (kindIs "map" $target) (hasKey $target "required") -}}
{{- $required = index $target "required" -}}
{{- end -}}
{{- $unavailableFor := "" -}}
{{- if and (kindIs "map" $target) (hasKey $target "unavailableFor") -}}
{{- $unavailableFor = index $target "unavailableFor" -}}
{{- end -}}
{{- $availableFor := "" -}}
{{- if and (kindIs "map" $target) (hasKey $target "availableFor") -}}
{{- $availableFor = index $target "availableFor" -}}
{{- end -}}
{{- $globalEnabled := and (kindIs "bool" $availability.enabled) $availability.enabled -}}
{{- $injectGlobal := and (eq $nameKey "dcgm-exporter") $globalEnabled (empty $condition) -}}
{{- if $injectGlobal -}}
{{- $required = true -}}
{{- $condition = default "" $availability.condition -}}
{{- $unavailableFor = default "" $availability.unavailableFor -}}
{{- $availableFor = default "" $availability.availableFor -}}
{{- end -}}
{{- $declares := or $injectGlobal (and (kindIs "bool" $required) $required) (not (empty $condition)) -}}
{{- dict "declares" $declares "required" $required "condition" $condition "unavailableFor" $unavailableFor "availableFor" $availableFor | toYaml -}}
{{- end }}

{{/*
Validate only targets that declare availability, plus ownership collisions they
create. Optional legacy targets and duplicate rule-to-rule conditions remain
accepted.
*/}}
{{- define "gpu-monitoring.validateProfileAvailability" -}}
{{- $root := .root -}}
{{- $skuName := .skuName -}}
{{- $sku := .sku -}}
{{- $scrapeTargets := $root.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- $availability := fromYaml (include "gpu-monitoring.dcgmAvailability" (dict "root" $root "sku" $sku)) -}}
{{- $nodeExporterEnabled := $root.Values.nodeExporter.enabled -}}
{{- if hasKey $sku "nodeExporter" -}}
{{- $nodeExporterEnabled = $sku.nodeExporter -}}
{{- end -}}
{{- $nodeExporterScrapeEnabled := $nodeExporterEnabled -}}
{{- if hasKey $root.Values.metricsCollector "nodeExporterScrape" -}}
{{- $nodeExporterScrapeEnabled = $root.Values.metricsCollector.nodeExporterScrape -}}
{{- end -}}
{{- if hasKey $sku "nodeExporterScrape" -}}
{{- $nodeExporterScrapeEnabled = $sku.nodeExporterScrape -}}
{{- end -}}
{{- $ruleConditions := dict -}}
{{- range $rule := (default (list) $root.Values.metricsCollector.rules) -}}
{{- if and (kindIs "map" $rule) (hasKey $rule "conditionType") -}}
{{- $conditionKey := include "gpu-monitoring.scalarIdentifier" (dict "value" (index $rule "conditionType")) -}}
{{- if $conditionKey -}}
{{- $_ := set $ruleConditions $conditionKey true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $npdConditions := dict -}}
{{- if $root.Values.monitors.customPluginMonitor -}}
{{- $monitorConfig := include "gpu-monitoring.renderMonitorConfig" (dict "root" $root "sku" $sku) | fromJson -}}
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
{{- $coreConditions := include "gpu-monitoring.kubernetesCoreConditionTypes" $root | fromYamlArray -}}
{{- $seenNames := dict -}}
{{- if $root.Values.metricsCollector.npdScrape -}}
{{- $_ := set $seenNames "node-problem-detector" false -}}
{{- end -}}
{{- $conditionOwners := dict -}}
{{- range $index, $target := $scrapeTargets -}}
{{- if kindIs "map" $target -}}
{{- $name := "" -}}
{{- if hasKey $target "name" -}}
{{- $name = index $target "name" -}}
{{- end -}}
{{- $nameKey := include "gpu-monitoring.scalarIdentifier" (dict "value" $name) -}}
{{- $rendered := not (and (eq $nameKey "node-exporter") (not $nodeExporterScrapeEnabled)) -}}
{{- if $rendered -}}
{{- $effective := include "gpu-monitoring.effectiveScrapeTargetAvailability" (dict "target" $target "availability" $availability) | fromYaml -}}
{{- $declares := eq (default false $effective.declares) true -}}
{{- if $nameKey -}}
{{- if hasKey $seenNames $nameKey -}}
{{- $priorDeclares := index $seenNames $nameKey -}}
{{- if or $declares $priorDeclares -}}
{{- fail (printf "gpuSkus.%s has duplicate scrapeTarget name %q where at least one target declares availability" $skuName $nameKey) -}}
{{- end -}}
{{- end -}}
{{- $_ := set $seenNames $nameKey (or $declares (default false (index $seenNames $nameKey))) -}}
{{- end -}}
{{- if $declares -}}
{{- if not (and (kindIs "string" $name) (not (empty $name))) -}}
{{- fail (printf "gpuSkus.%s scrapeTarget at index %d declares availability but must set a nonempty string name" $skuName $index) -}}
{{- end -}}
{{- $url := "" -}}
{{- if hasKey $target "url" -}}
{{- $url = index $target "url" -}}
{{- end -}}
{{- if not (and (kindIs "string" $url) (not (empty $url)) (ne (include "gpu-monitoring.dcgmTargetKind" (dict "url" $url)) "invalid")) -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q declares availability but must set an absolute lowercase HTTP(S) url with a host" $skuName $name) -}}
{{- end -}}
{{- $condition := $effective.condition -}}
{{- $conditionKey := include "gpu-monitoring.scalarIdentifier" (dict "value" $condition) -}}
{{- if not (and (kindIs "bool" $effective.required) $effective.required) -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q sets availabilityCondition without required: true" $skuName $name) -}}
{{- end -}}
{{- range $field := list "unavailableFor" "availableFor" -}}
{{- $value := index $effective $field -}}
{{- if and (not (empty $value)) (not (and (kindIs "string" $value) (regexMatch "^(0|[0-9]+(ms|s|m|h))$" $value))) -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q %s must be a quoted nonnegative duration such as \"2m\"" $skuName $name $field) -}}
{{- end -}}
{{- end -}}
{{- if has $conditionKey $coreConditions -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q availabilityCondition %q is owned by Kubernetes" $skuName $name $condition) -}}
{{- end -}}
{{- if hasKey $ruleConditions $conditionKey -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q availabilityCondition %q collides with a collector rule" $skuName $name $condition) -}}
{{- end -}}
{{- if hasKey $npdConditions $conditionKey -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q availabilityCondition %q collides with its effective NPD monitor" $skuName $name $condition) -}}
{{- end -}}
{{- if hasKey $conditionOwners $conditionKey -}}
{{- fail (printf "gpuSkus.%s availabilityCondition %q is claimed by both scrapeTargets %q and %q" $skuName $conditionKey (index $conditionOwners $conditionKey) $name) -}}
{{- end -}}
{{- if not (and (kindIs "string" $condition) (regexMatch "^[A-Za-z][A-Za-z0-9]*$" $condition)) -}}
{{- fail (printf "gpuSkus.%s scrapeTarget %q must set a nonempty alphanumeric availabilityCondition" $skuName $name) -}}
{{- end -}}
{{- $_ := set $conditionOwners $conditionKey $nameKey -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Every rendered profile enters host mount namespaces through wrapper scripts.
Host-local DCGM endpoints additionally require host networking.
*/}}
{{- define "gpu-monitoring.validateHostNamespaces" -}}
{{- $profileCount := 0 -}}
{{- $hostLocalDcgm := false -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus) -}}
{{- $profileCount = add1 $profileCount -}}
{{- $scrapeTargets := $.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- range $target := $scrapeTargets -}}
{{- if and (kindIs "map" $target) (kindIs "string" $target.name) (eq $target.name "dcgm-exporter") -}}
{{- if eq (include "gpu-monitoring.dcgmTargetKind" $target) "host-local" -}}
{{- $hostLocalDcgm = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if gt $profileCount 0 -}}
{{- $hostPID := index .Values.daemonset "hostPID" -}}
{{- if or (not (kindIs "bool" $hostPID)) (not $hostPID) -}}
{{- fail "daemonset.hostPID must be boolean true when any GPU profile is enabled" -}}
{{- end -}}
{{- end -}}
{{- if $hostLocalDcgm -}}
{{- $hostNetwork := index .Values.daemonset "hostNetwork" -}}
{{- if or (not (kindIs "bool" $hostNetwork)) (not $hostNetwork) -}}
{{- fail "daemonset.hostNetwork must be boolean true when an enabled profile uses host-local DCGM" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
A remote exporter has no host dcgmi observer, so the collector and one effective
availability owner are mandatory for every enabled remote target.
*/}}
{{- define "gpu-monitoring.validateRemoteDcgmObserver" -}}
{{- range $skuName, $sku := .Values.gpuSkus -}}
{{- if or (empty $.Values.enabledGpuSkus) (has $skuName $.Values.enabledGpuSkus) -}}
{{- $scrapeTargets := $.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- $availability := deepCopy (default (dict) $.Values.metricsCollector.dcgmAvailability) -}}
{{- if hasKey $sku "dcgmAvailability" -}}
{{- $availability = mergeOverwrite $availability (deepCopy $sku.dcgmAvailability) -}}
{{- end -}}
{{- $dcgmTargetCount := 0 -}}
{{- range $target := $scrapeTargets -}}
{{- if and (kindIs "map" $target) (kindIs "string" $target.name) (eq $target.name "dcgm-exporter") -}}
{{- $dcgmTargetCount = add1 $dcgmTargetCount -}}
{{- end -}}
{{- end -}}
{{- if ne $dcgmTargetCount 1 -}}
{{- fail (printf "gpuSkus.%s must define exactly one effective dcgm-exporter scrapeTarget; found %d" $skuName $dcgmTargetCount) -}}
{{- end -}}
{{- if and (kindIs "bool" $.Values.metricsCollector.enabled) $.Values.metricsCollector.enabled -}}
{{- include "gpu-monitoring.validateProfileAvailability" (dict "root" $ "skuName" $skuName "sku" $sku) -}}
{{- end -}}
{{- range $target := $scrapeTargets -}}
{{- if and (kindIs "map" $target) (kindIs "string" $target.name) (eq $target.name "dcgm-exporter") -}}
{{- $targetKind := include "gpu-monitoring.dcgmTargetKind" $target -}}
{{- if eq $targetKind "invalid" -}}
{{- fail "effective dcgm-exporter scrapeTargets.url must use localhost, 127.0.0.1, or an absolute remote HTTP(S) service URL" -}}
{{- end -}}
{{- if eq $targetKind "remote" -}}
{{- $collectorEnabled := $.Values.metricsCollector.enabled -}}
{{- if not (and (kindIs "bool" $collectorEnabled) $collectorEnabled) -}}
{{- fail (printf "gpuSkus.%s uses a remote dcgm-exporter target and requires metricsCollector.enabled=true" $skuName) -}}
{{- end -}}
{{- $effective := include "gpu-monitoring.effectiveScrapeTargetAvailability" (dict "target" $target "availability" $availability) | fromYaml -}}
{{- $effectiveCondition := $effective.condition -}}
{{- $effectiveRequired := and (kindIs "bool" $effective.required) $effective.required -}}
{{- if not (and $effectiveRequired (kindIs "string" $effectiveCondition) (regexMatch "^[A-Za-z][A-Za-z0-9]*$" $effectiveCondition)) -}}
{{- fail (printf "gpuSkus.%s uses a remote dcgm-exporter target and requires effective exporter availability ownership" $skuName) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
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
{{- $config := mustFromJson ($root.Files.Get (printf "configs/%s" $sku.monitor_config)) -}}
{{- if eq (include "gpu-monitoring.dcgmHealthRequired" (dict "root" $root "sku" $sku)) "0" -}}
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
{{- if eq (include "gpu-monitoring.dcgmHealthRequired" (dict "root" $ "sku" $sku)) "0" -}}
{{- $config := include "gpu-monitoring.renderMonitorConfig" (dict "root" $ "sku" $sku) -}}
{{- $key := include "gpu-monitoring.monitorConfigKey" (dict "root" $ "sku" $sku) -}}
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
Require the host dcgmi check only when the selected profile scrapes a host-local
DCGM exporter. A profile can disable that inference with dcgm_health_required,
but cannot enable host operations for a remote Service target.
*/}}
{{- define "gpu-monitoring.dcgmHealthRequired" -}}
{{- $root := .root -}}
{{- $sku := .sku -}}
{{- $scrapeTargets := $root.Values.metricsCollector.scrapeTargets -}}
{{- if hasKey $sku "scrapeTargets" -}}
{{- $scrapeTargets = $sku.scrapeTargets -}}
{{- end -}}
{{- $hostLocal := eq (include "gpu-monitoring.dcgmProfileKind" (dict "root" $root "sku" $sku)) "host-local" -}}
{{- $required := $hostLocal -}}
{{- if hasKey $sku "dcgm_health_required" -}}
{{- if and $sku.dcgm_health_required (not $hostLocal) -}}
{{- fail "dcgm_health_required=true requires a host-local dcgm-exporter scrape target; remote Service profiles must rely on exporter availability" -}}
{{- end -}}
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
{{- $effective := include "gpu-monitoring.effectiveScrapeTargetAvailability" (dict "target" $target "availability" $availability) | fromYaml -}}
{{- if $effective.declares -}}
required: true
availabilityCondition: {{ $effective.condition | quote }}
{{- if $effective.unavailableFor }}
unavailableFor: {{ $effective.unavailableFor | quote }}
{{- end }}
{{- if $effective.availableFor }}
availableFor: {{ $effective.availableFor | quote }}
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
{{- $dcgmTargetCount := 0 -}}
{{- range $target := $scrapeTargets -}}
{{- if and (kindIs "map" $target) (kindIs "string" $target.name) (eq $target.name "dcgm-exporter") -}}
{{- $dcgmTargetCount = add1 $dcgmTargetCount -}}
{{- end -}}
{{- end -}}
{{- if ne $dcgmTargetCount 1 -}}
{{- fail (printf "profile must define exactly one effective dcgm-exporter scrapeTarget; found %d" $dcgmTargetCount) -}}
{{- end -}}
{{- include "gpu-monitoring.validateProfileAvailability" (dict "root" $root "skuName" (default "profile" .skuName) "sku" $sku) -}}
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
  {{- range $target := $scrapeTargets }}
  {{- if and (kindIs "string" $target.name) (eq $target.name "node-exporter") }}
  {{- if $nodeExporterScrapeEnabled }}
  - name: {{ $target.name | quote }}
    url: {{ $target.url | quote }}
    {{- with (include "gpu-monitoring.scrapeTargetAvailability" (dict "target" $target "availability" $availability)) }}{{ . | nindent 4 }}{{- end }}
  {{- end }}
  {{- else }}
  - name: {{ $target.name | quote }}
    url: {{ $target.url | quote }}
    {{- with (include "gpu-monitoring.scrapeTargetAvailability" (dict "target" $target "availability" $availability)) }}{{ . | nindent 4 }}{{- end }}
  {{- end }}
  {{- end }}
  {{- if $root.Values.metricsCollector.npdScrape }}
  - name: "node-problem-detector"
    url: {{ printf "http://localhost:%d/metrics" (add $root.Values.npdPort 1) | quote }}
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
