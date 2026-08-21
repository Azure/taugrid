// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type FieldStatus string

const (
	statusSupported FieldStatus = "supported"
	statusReserved  FieldStatus = "reserved"
	// A config takes one of two paths: managed workflow when workflow.file is
	// set, direct Job/RayJob otherwise. These two mark fields the other path
	// rejects outright, so the status column answers "can my config use this?"
	// without the reader parsing the description.
	statusWorkflowOnly FieldStatus = "workflow-only"
	statusDirectOnly   FieldStatus = "direct-only"
)

type FieldInfo struct {
	Description      string
	Status           FieldStatus
	Values           []string
	DeprecatedValues []string
	Default          string
	Notes            string
}

var fieldCatalog = map[string]FieldInfo{
	"schema_version": {Status: statusReserved, Description: "Reserved for SDK-generated managed workflow manifests; not part of the direct tau run config contract."},
	"name":           {Status: statusSupported, Description: "Stable run or workload name. When omitted, a positional tau run NAME may provide it."},
	"engine":         {Status: statusSupported, Description: "Selects the workload kind when Tau cannot infer it.", Values: []string{"job", "rayjob"}, DeprecatedValues: []string{"ray"}},
	"entrypoint":     {Status: statusSupported, Description: "Script path for the workload, resolved relative to the config file."},
	"script":         {Status: statusSupported, Description: "Alias for entrypoint."},
	"image":          {Status: statusSupported, Description: "Container image override when runtime.image is not set."},

	"run":                            {Status: statusSupported, Description: "Nested aliases for run identity and entrypoint fields."},
	"run.name":                       {Status: statusSupported, Description: "Nested stable run or workload name."},
	"run.engine":                     {Status: statusSupported, Description: "Nested workload-kind selector.", Values: []string{"job", "rayjob"}, DeprecatedValues: []string{"ray"}},
	"run.entrypoint":                 {Status: statusSupported, Description: "Nested workload script path, resolved relative to the config file."},
	"run.script":                     {Status: statusSupported, Description: "Alias for run.entrypoint."},
	"run.source":                     {Status: statusDirectOnly, Description: "Immutable source tree staged from a digest-pinned OCI image by an init container. Direct Jobs only."},
	"run.source.image":               {Status: statusDirectOnly, Description: "Source OCI image pinned by an exact sha256 digest."},
	"run.source.path":                {Status: statusDirectOnly, Description: "Clean absolute source directory inside the pinned image. Tau copies it into /tau/source."},
	"run.ttl_seconds_after_finished": {Status: statusDirectOnly, Description: "Kubernetes Job retention in seconds after completion or failure (1-2147483647). Direct Jobs only.", Default: "28800"},
	"run.working_dir":                {Status: statusSupported, Description: "Project directory shipped with the run so sibling modules and local packages import on workers; resolved relative to the config file."},
	"run.working_dir_excludes":       {Status: statusSupported, Description: "Extra glob patterns excluded from the shipped project directory."},
	"run.image":                      {Status: statusSupported, Description: "Nested container image override when runtime.image is not set."},
	"run.main_script":                {Status: statusSupported, Description: "Main script passed to workflow rendering when workflow.file is used."},
	"run.workload_kind":              {Status: statusSupported, Description: "Workload kind selector for dispatch compatibility.", Values: []string{"job", "rayjob"}, DeprecatedValues: []string{"ray", "ray-train", "ray_train"}},
	"run.smoke_pairs":                {Status: statusSupported, Description: "Smoke-test pair count forwarded to workflow rendering when supported."},
	"workflow":                       {Status: statusSupported, Description: "Workflow rendering options for configs that delegate to a workflow manifest."},
	"workflow.file":                  {Status: statusSupported, Description: "Separate workflow manifest file to render."},
	"workflow.script":                {Status: statusSupported, Description: "Workflow script path alias."},
	"workflow.main_script":           {Status: statusSupported, Description: "Main script path for workflow rendering."},
	"workflow.workload_kind":         {Status: statusSupported, Description: "Workload kind for workflow rendering.", Values: []string{"job", "rayjob"}, DeprecatedValues: []string{"ray", "ray-train", "ray_train"}},
	"workflow.extra_scripts":         {Status: statusSupported, Description: "Additional local scripts staged with the workflow."},
	"workflow.upstream_checkpoint":   {Status: statusSupported, Description: "Upstream checkpoint name or path forwarded to workflow rendering."},
	"workflow.secret_payload":        {Status: statusSupported, Description: "Path to a generated secret payload consumed by workflow rendering."},
	"workflow.smoke_pairs":           {Status: statusSupported, Description: "Smoke-test pair count forwarded to workflow rendering when supported."},

	"runtime":               {Status: statusSupported, Description: "Container runtime settings for direct run configs."},
	"runtime.image":         {Status: statusSupported, Description: "Container image override."},
	"runtime.working_dir":   {Status: statusDirectOnly, Description: "Clean absolute initial working directory inside the main container. Direct Jobs only; maps to Kubernetes container.workingDir and does not ship local files.", Notes: "This is distinct from RayJob-only run.working_dir, which is a host-relative project directory packaged into Ray runtime_env."},
	"runtime.pip":           {Status: statusSupported, Description: "Python packages installed through Ray runtime_env for RayJob dispatch."},
	"runtime.env":           {Status: statusSupported, Description: "Literal non-secret environment variables."},
	"runtime.env_secret":    {Status: statusSupported, Description: "Secret-backed environment variables as a map from env var name to SECRET_NAME:KEY. Client dry-run redacts the referenced Secret name and key while showing that a secret dependency exists."},
	"runtime.env_kv":        {Status: statusWorkflowOnly, Description: "Key Vault-backed environment variables as a map from env var name to secret-name or vault/secret-name. Direct Job/RayJob configs reject this field; it needs the managed workflow path (workflow.file, or an SDK manifest with schema_version), plus --tenant-id, --workload-identity-client-id, and a pod ServiceAccount from --service-account or policy.workspace. Bare secret names also need --key-vault.", Notes: "All entries must resolve to the same vault: one SecretProviderClass is rendered per workload."},
	"runtime.security":      {Status: statusDirectOnly, Description: "Portable pod security settings for direct Job and RayJob configs."},
	"runtime.security.mode": {Status: statusDirectOnly, Description: "Apply Kubernetes Restricted Pod Security fields to every generated container and init container.", Values: []string{SecurityModeRestricted}},

	"compute":                       {Status: statusSupported, Description: "Workload sizing and dispatch hints."},
	"compute.workers":               {Status: statusSupported, Description: "Ray execution-worker count. Generated RayJobs add a separate control-only head on the system node pool.", Default: "1"},
	"compute.gpus":                  {Status: statusSupported, Description: "Pod-level GPU count for direct Job execution. Set 0 explicitly for a CPU Job."},
	"compute.gpus_per_worker":       {Status: statusSupported, Description: "GPU count per dedicated worker in a direct RayJob run; its head remains CPU-only. Not valid for direct Job execution.", Default: "1"},
	"compute.cpu_workers":           {Status: statusSupported, Description: "CPU eval worker count."},
	"compute.workload_kind":         {Status: statusSupported, Description: "Workload kind selector.", Values: []string{"job", "rayjob"}, DeprecatedValues: []string{"ray", "ray-train", "ray_train"}},
	"compute.gpu_resource_mode":     {Status: statusSupported, Description: "GPU resource mode forwarded to workflow rendering when supported.", Values: []string{"device-plugin", "nvidia", "dra", "mig"}},
	"compute.mig_profile":           {Status: statusSupported, Description: "MIG partition profile (e.g., 1g.18gb, 3g.71gb). Required when gpu_resource_mode=mig."},
	"compute.cpu_request":           {Status: statusSupported, Description: "CPU request for the Job container or per-pod default for direct RayJob configs."},
	"compute.memory_request":        {Status: statusSupported, Description: "Memory request for the Job container or per-pod default for direct RayJob configs."},
	"compute.cpu_limit":             {Status: statusSupported, Description: "CPU limit for the Job container or per-pod default for direct RayJob configs."},
	"compute.memory_limit":          {Status: statusSupported, Description: "Memory limit for the Job container or per-pod default for direct RayJob configs."},
	"compute.head_cpu_request":      {Status: statusSupported, Description: "CPU request for the Ray head pod in direct RayJob configs."},
	"compute.head_memory_request":   {Status: statusSupported, Description: "Memory request for the Ray head pod in direct RayJob configs."},
	"compute.head_cpu_limit":        {Status: statusSupported, Description: "CPU limit for the Ray head pod in direct RayJob configs."},
	"compute.head_memory_limit":     {Status: statusSupported, Description: "Memory limit for the Ray head pod in direct RayJob configs."},
	"compute.worker_cpu_request":    {Status: statusSupported, Description: "CPU request for Ray worker pods in direct RayJob configs."},
	"compute.worker_memory_request": {Status: statusSupported, Description: "Memory request for Ray worker pods in direct RayJob configs."},
	"compute.worker_cpu_limit":      {Status: statusSupported, Description: "CPU limit for Ray worker pods in direct RayJob configs."},
	"compute.worker_memory_limit":   {Status: statusSupported, Description: "Memory limit for Ray worker pods in direct RayJob configs."},

	"policy":                            {Status: statusSupported, Description: "Namespace, queue, topology, and priority placement settings."},
	"policy.workspace":                  {Status: statusSupported, Description: "TauWorkspace name used to load platform-owned namespace, queue, priority, output-root, and scratch defaults."},
	"policy.namespace":                  {Status: statusSupported, Description: "Target Kubernetes namespace."},
	"policy.preset":                     {Status: statusSupported, Description: "Azure managed compute preset (e.g. azure.research.training.{l,2x,4x,xl}); see `tau cluster validate topology`. When omitted, managed workflow configs infer a preset from policy.team, lane=training, and compute.gpus."},
	"policy.profile":                    {Status: statusSupported, Description: "Legacy scheduling label. Direct Job resources are declared under compute."},
	"policy.queue":                      {Status: statusSupported, Description: "Explicit Kueue LocalQueue name, or auto for live compatible-queue discovery. With presets, Tau infers policy.team from queue names like sample-training when policy.team is omitted."},
	"policy.team":                       {Status: statusSupported, Description: "Tau team that owns the quota slice (e.g. research, experimental). Used by managed workflow preset inference; defaults to the TAU_TEAM environment variable."},
	"policy.lane":                       {Status: statusSupported, Description: "Advanced/operator override: Tau lane.", Values: []string{"eval", "training", "large-memory", "elastic"}},
	"policy.gpu_class":                  {Status: statusSupported, Description: "Hardware-only GPU class matched exactly through the ResourceFlavor/node label " + workloadmeta.LabelGPUClass + ".", Values: topology.SupportedGPUClasses(), DeprecatedValues: []string{"a100-nvlink-80gb", "h100-standalone-95gb", "h200-nvlink-141gb"}, Notes: "any is unconstrained and renders no class selector. Legacy *-nvlink-* and *-standalone-* spellings are deprecated aliases; express placement/interconnect with policy.topology."},
	"policy.mode":                       {Status: statusSupported, Description: "Advanced/operator override: admission mode.", Values: []string{"fixed", "elastic"}},
	"policy.topology":                   {Status: statusSupported, Description: "Explicit placement semantics. Connected GPU preflight requires this when every compatible queue flavor uses TopologyAwareScheduling; offline validation cannot infer live flavor capabilities.", Values: []string{"independent", "single-node-nvlink", "multi-node-nccl", "elastic-workers"}},
	"policy.shape":                      {Status: statusSupported, Description: "Advanced/operator override: workload shape, e.g. 8xa100-80gb."},
	"policy.priority":                   {Status: statusSupported, Description: "Alias for policy.priority_tier."},
	"policy.priority_tier":              {Status: statusSupported, Description: "Advanced/operator override: Tau priority tier.", Values: []string{"default", "priority"}},
	"policy.topology_policy":            {Status: statusSupported, Description: "Advanced/platform override: topology policy file. Defaults to TAU_TOPOLOGY_POLICY, then an in-tree policy, then the embedded Azure policy."},
	"policy.workload_priority_class":    {Status: statusSupported, Description: "Advanced/operator override: Kueue WorkloadPriorityClass name for admission ordering."},
	"policy.pod_priority_class":         {Status: statusSupported, Description: "Advanced/operator override: Kubernetes PriorityClass name for pod scheduling and preemption."},
	"policy.node_selector":              {Status: statusSupported, Description: "Additional node selector labels."},
	"policy.clear_node_selector":        {Status: statusDirectOnly, Description: "Clear profile node selectors before applying the protected topology contract and policy.node_selector. Managed workflow configs reject this field.", Notes: "Within direct configs this also requires engine: job; RayJob dispatch cannot clear profile node selectors."},
	"policy.disable_default_priorities": {Status: statusSupported, Description: "Advanced/operator override: omit Tau default Kueue and Kubernetes priority classes for clusters that do not define taugrid-* priorities."},

	"storage":                          {Status: statusSupported, Description: "PVC and result-path settings."},
	"storage.data_pvc":                 {Status: statusSupported, Description: "Existing platform-managed PVC mounted at /data; Tau never creates the claim."},
	"storage.result_pvc":               {Status: statusSupported, Description: "Existing result PVC; direct job configs require it to match storage.data_pvc when both are set."},
	"storage.checkpoint":               {Status: statusSupported, Description: "File or directory, relative to the run checkpoint dir, that this run produces as its servable model (e.g. last.safetensors). Declaring it writes an artifact index after a successful run so tau serve deploy --from-finetune can resolve the model by run name."},
	"storage.output":                   {Status: statusSupported, Description: "Durable output path advertised to the workload."},
	"storage.publish":                  {Status: statusDirectOnly, Description: "Optional Tau-owned artifact publication mode. staged exposes TAU_OUTPUT_STAGING_DIR on pod-local /mnt, verifies closed regular files into storage.output, and writes a completion marker after successful execution. Managed workflow configs reject this field.", Values: []string{"staged"}},
	"storage.volumes":                  {Status: statusSupported, Description: "Additional run-time volume specs."},
	"storage.mounts":                   {Status: statusSupported, Description: "Additional run-time mount specs."},
	"storage.image_assets":             {Status: statusDirectOnly, Description: "Digest-pinned image directories copied by init containers into read-only main-container mounts. Direct Jobs only."},
	"storage.image_assets.name":        {Status: statusDirectOnly, Description: "Unique DNS label used to derive the init-container and volume names."},
	"storage.image_assets.image":       {Status: statusDirectOnly, Description: "Source OCI image pinned by an exact sha256 digest."},
	"storage.image_assets.source_path": {Status: statusDirectOnly, Description: "Clean absolute source directory inside the pinned image."},
	"storage.image_assets.mount_path":  {Status: statusDirectOnly, Description: "Clean absolute read-only mount path in the main container."},

	"profiler":          {Status: statusSupported, Description: "Optional Nsight profiler wrapper settings."},
	"profiler.mode":     {Status: statusSupported, Description: "Profiler mode.", Values: []string{"nsys", "ncu"}},
	"profiler.rank":     {Status: statusSupported, Description: "Rank selector such as 0, 0,8, or all.", Default: "0"},
	"profiler.warmup":   {Status: statusSupported, Description: "Warmup duration before profiling."},
	"profiler.duration": {Status: statusSupported, Description: "Profiler capture duration."},

	"metrics":                 {Status: statusSupported, Description: "Opt-in durable Stellar metrics for single-pod Jobs and RayJobs."},
	"metrics.history":         {Status: statusSupported, Description: "Published JSONL metric paths or globs. Every online row requires integer _step and finite positive Unix-seconds _timestamp fields. Cache-backed/object PVC producers must close and atomically rename unique immutable chunks before they match. Relative paths resolve beneath storage.output; absolute paths must be under /data."},
	"metrics.offload":         {Status: statusSupported, Description: "Job and RayJob metrics producer opt-in. Runtime image and endpoint policy remain platform-owned."},
	"metrics.offload.enabled": {Status: statusSupported, Description: "Render the checkpointed Tau metrics offloader sidecar for a single-pod Job or RayJob head pod. Multi-node Indexed Jobs are rejected.", Default: "false"},

	"experiment":         {Status: statusSupported, Description: "Stellar experiment grouping metadata."},
	"experiment.project": {Status: statusSupported, Description: "Project name. Metrics offload requires a lowercase identifier using alphanumerics with internal '-', '_', or '.'."},
	"experiment.name":    {Status: statusSupported, Description: "Stable experiment identifier: the set of runs being compared. Metrics offload requires it unless a compatibility fallback is present."},
	"experiment.group":   {Status: statusSupported, Description: "Arm within the experiment (baseline vs ablation). Metrics offload requires a lowercase identifier using alphanumerics with internal '-', '_', or '.'."},
	"experiment.title":   {Status: statusSupported, Description: "Deprecated pre-v0.1 alias used only to derive experiment.name when name is absent.", Notes: "New configs must use experiment.name. Tau does not emit title metadata on workloads."},

	"execution":                       {Status: statusSupported, Description: "Typed execution topology for the workload."},
	"execution.launcher":              {Status: statusSupported, Description: "Process launcher, scoped per workload kind. engine: job accepts python | torchrun (default: python). engine: rayjob accepts ray-train | ray-tune (default: ray-train).", Values: []string{"python", "torchrun", "ray-train", "ray-tune"}, Notes: "Cross-engine combinations are rejected: torchrun requires engine: job, ray-train/ray-tune require engine: rayjob."},
	"execution.processes_per_node":    {Status: statusSupported, Description: "Processes per node (torchrun --nproc_per_node). Requires launcher: torchrun. Validated against resolved GPU count."},
	"execution.nodes":                 {Status: statusSupported, Description: "Number of nodes for multi-node torchrun (engine: job only). Each node runs as one pod in a Kubernetes Indexed Job.", Default: "1"},
	"execution.metric":                {Status: statusSupported, Description: "Optimization metric name for Ray Tune. Requires launcher: ray-tune."},
	"execution.mode":                  {Status: statusSupported, Description: "Optimization direction for the metric.", Values: []string{"min", "max"}, Default: "min"},
	"execution.num_samples":           {Status: statusSupported, Description: "Number of sampled configurations to try. Each list value in configs generates one sample per grid point.", Default: "1"},
	"execution.max_concurrent_trials": {Status: statusSupported, Description: "Maximum number of Tune trials running at the same time. Limits GPU contention within the Ray cluster.", Default: "1"},
	"execution.configs":               {Status: statusSupported, Description: "Extra launcher configuration. job+python: script CLI flags. job+torchrun: torchrun flags. ray+ray-train: Ray Train config (TAU_RAY_TRAIN_CONFIG_JSON). ray+ray-tune: search space."},
	"execution.allow_nccl_override":   {Status: statusSupported, Description: "Opt-in bypass for NCCL_* reserved key validation. When true, NCCL_* keys in runtime.env are accepted. Other reserved keys remain blocked.", Default: "false"},

	"resilience":                 {Status: statusSupported, Description: "Automatic retry on transient job failure (preemption, eviction)."},
	"resilience.max_retries":     {Status: statusSupported, Description: "Maximum number of automatic retries on retryable failure. 0 disables retry.", Default: "0"},
	"resilience.retry_on":        {Status: statusSupported, Description: "Failure reasons that trigger retry.", Default: "Preempted, Evicted", Values: []string{"Preempted", "Evicted", "OOMKilled"}},
	"resilience.checkpoint_path": {Status: statusSupported, Description: "Checkpoint directory override injected as TAU_RESUME_FROM. Default: /data/checkpoints/finetunes/<name>."},
	"resilience.backoff_initial": {Status: statusSupported, Description: "Initial backoff duration between retries (Go duration string).", Default: "30s"},
	"resilience.backoff_max":     {Status: statusSupported, Description: "Maximum backoff duration between retries (Go duration string).", Default: "5m"},
}

func configFieldPaths() []string {
	var cfg Config
	paths := fieldPaths(reflect.TypeOf(cfg), "")
	sort.Strings(paths)
	return paths
}

// configStructPaths returns the subset of configFieldPaths whose values are
// nested Config structs, i.e. the paths whose child keys are schema rather than
// user data. Free-form maps and slices are deliberately excluded.
func configStructPaths() []string {
	var cfg Config
	paths := structFieldPaths(reflect.TypeOf(cfg), "")
	sort.Strings(paths)
	return paths
}

func structFieldPaths(t reflect.Type, prefix string) []string {
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	paths := []string{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name := yamlFieldName(field)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := deref(field.Type)
		if ft.Kind() == reflect.Struct && ft.PkgPath() == reflect.TypeOf(Config{}).PkgPath() && ft != reflect.TypeOf(Duration{}) {
			paths = append(paths, path)
			paths = append(paths, structFieldPaths(ft, path)...)
		}
	}
	return paths
}

func JSONSchema() ([]byte, error) {
	var cfg Config
	root := schemaForType(reflect.TypeOf(cfg), "")
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = "https://github.com/Azure/taugrid/cli/schemas/tau-run-config.schema.json"
	root["title"] = "Tau direct run config"
	root["description"] = "Implementation-backed schema for direct `tau run --config` files. SDK-generated managed manifests are outside this schema."
	return json.MarshalIndent(root, "", "  ")
}

func ReferenceMarkdown() string {
	paths := configFieldPaths()
	var b strings.Builder
	b.WriteString("# Tau run config reference\n\n")
	b.WriteString("Generated from the Go implementation-backed `core/runconfig` contract.\n\n")
	b.WriteString("This reference covers direct `tau run --config` files. SDK-generated managed manifests are a separate path and are not covered by this schema/reference.\n\n")
	b.WriteString("| Field | Status | Description | Values / defaults |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, path := range paths {
		info := fieldCatalog[path]
		extra := referenceExtra(info)
		b.WriteString("| `")
		b.WriteString(path)
		b.WriteString("` | ")
		b.WriteString(string(info.Status))
		b.WriteString(" | ")
		b.WriteString(markdownCell(info.Description))
		b.WriteString(" | ")
		b.WriteString(markdownCell(extra))
		b.WriteString(" |\n")
	}
	return b.String()
}

func referenceExtra(info FieldInfo) string {
	parts := []string{}
	if len(info.Values) > 0 {
		parts = append(parts, "values: "+strings.Join(info.Values, ", "))
	}
	if info.Default != "" {
		parts = append(parts, "default: "+info.Default)
	}
	if info.Notes != "" {
		parts = append(parts, info.Notes)
	}
	return strings.Join(parts, "; ")
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	return value
}

func fieldPaths(t reflect.Type, prefix string) []string {
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	paths := []string{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name := yamlFieldName(field)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		paths = append(paths, path)
		ft := deref(field.Type)
		if ft.Kind() == reflect.Slice {
			ft = deref(ft.Elem())
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() == reflect.TypeOf(Config{}).PkgPath() && ft != reflect.TypeOf(Duration{}) {
			paths = append(paths, fieldPaths(ft, path)...)
		}
	}
	return paths
}

func schemaForType(t reflect.Type, path string) map[string]any {
	t = deref(t)
	schema := map[string]any{}
	if info, ok := fieldCatalog[path]; ok {
		schema["description"] = info.Description
		schema["x-tau-status"] = string(info.Status)
		if len(info.Values) > 0 {
			if len(info.DeprecatedValues) == 0 {
				schema["enum"] = info.Values
			} else {
				schema["oneOf"] = []any{
					map[string]any{"enum": info.Values},
					map[string]any{
						"enum":        info.DeprecatedValues,
						"deprecated":  true,
						"description": "Deprecated compatibility aliases; Tau normalizes these to canonical values.",
					},
				}
			}
		}
		if info.Default != "" {
			schema["default"] = info.Default
		}
		if info.Notes != "" {
			schema["x-tau-notes"] = info.Notes
		}
	}
	switch {
	case t == reflect.TypeOf(Duration{}):
		schema["type"] = "string"
	case t.Kind() == reflect.Struct:
		schema["type"] = "object"
		schema["additionalProperties"] = false
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name := yamlFieldName(field)
			if name == "" {
				continue
			}
			childPath := name
			if path != "" {
				childPath = path + "." + name
			}
			props[name] = schemaForType(field.Type, childPath)
		}
		schema["properties"] = props
	case t.Kind() == reflect.String:
		schema["type"] = "string"
	case t.Kind() == reflect.Int:
		schema["type"] = "integer"
	case t.Kind() == reflect.Bool:
		schema["type"] = "boolean"
	case t.Kind() == reflect.Slice:
		schema["type"] = "array"
		schema["items"] = schemaForType(t.Elem(), path)
	case t.Kind() == reflect.Map:
		schema["type"] = "object"
		if t.Elem().Kind() == reflect.Interface {
			schema["additionalProperties"] = true
		} else {
			schema["additionalProperties"] = schemaForType(t.Elem(), path)
		}
	case t.Kind() == reflect.Interface:
		schema["type"] = []string{"string", "number", "integer", "boolean", "object", "array", "null"}
	default:
		schema["description"] = fmt.Sprintf("unsupported Go schema type %s", t.Kind())
	}
	return schema
}

func yamlFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("yaml")
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
