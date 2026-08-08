// Package manifest implements the schema_version: 1 managed workflow
// manifest: the schema used to parse, validate, and render researcher-
// authored (and Tau Python SDK-generated) configs for `tau run --config`.
//
// `tau run --config tau.yaml` renders a single self-contained workload
// object from a researcher-authored manifest:
//
//  1. Workload <prefix>-<name> — Job or RayJob (single-pod or multi-node,
//     selected by --workload-kind). The trainer script and the (redacted)
//     manifest are embedded directly in the workload's pod spec via
//     init containers (see internal/payload) that unpack them to /script
//     and /manifest at pod startup — no separate ConfigMaps are created.
//     The manifest payload is always head-only (/manifest is never needed
//     by workers). The script payload is head-only for a single-pod Job,
//     but is embedded on BOTH the head and every worker pod template for
//     all three RayJob templates: tau-py's Ray Train TorchTrainer path
//     re-imports the user's training module from each GPU/CPU worker's
//     own local /script disk, and rayjob-eval's CPU workers need the same
//     local /script disk because Ray's runtime_env working_dir mechanism
//     only ships the single user-module file to them — never any
//     --extra-script file — so a worker-side script payload is required
//     for parity with any extra helper module a researcher's eval task
//     may import (see internal/manifest/assets/managed-workflow-rayjob*.yaml.tmpl).
//
// Researchers never see the payload plumbing. The public path is the
// config-first `tau run --config` surface; this package remains the internal
// renderer for schema_version: 1 managed workflow configs.
//
// The generated manifest schema is documented by the public run-config guide.
// Validator here enforces the contract that scheduling relies on:
//
//   - schema_version == 1
//   - GPU mode: compute.gpus ∈ {1, 2, 3, 4, 5, 6, 7, 8}
//     (DRA claim templates; per-worker)
//   - CPU mode: compute.gpus == 0 with optional CPU pod sizing fields.
//   - compute.workers ≥ 1 (default 1; >1 requires workload_kind=rayjob).
//     Multi-node dispatch is enforced at the CLI/Render layer, not here.
//   - eval.cpu_workers ≥ 0 (default 0; >0 only meaningful for
//     workload_kind=rayjob-eval).
//   - eval.upstream is the optional name of an upstream finetune job whose
//     checkpoint the eval reads via the TAU_UPSTREAM_CHECKPOINT env var.
//   - runtime.pip is REQUIRED — declares the per-pod Python environment
//     shipped to head + workers via Ray runtime_env.
//   - runtime.rdma is optional and opt-in for RayJob pods that need RDMA
//     devices and NCCL verbs memlock privileges.
//   - artifacts.checkpoint, when set, is a relative pod path for the
//     completed-train checkpoint artifact.
//   - model.* fields are optional registry metadata used for durable model
//     discovery; checkpoint ownership still lives in artifacts.*.
package manifest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/envspec"
	"gopkg.in/yaml.v3"
)

const (
	defaultResourcePrefix    = "tau"
	maxResourceNameLen       = 63
	maxRayJobResourceNameLen = 47
	defaultRDMAResourceName  = "rdma/rdma_shared_device_a"
	defaultRDMAResourceCount = 1
	maxRDMAResourcePrefixLen = 253
	maxRDMAResourceNameLen   = 63
	maxRDMAQualifiedNameLen  = maxRDMAResourcePrefixLen + 1 + maxRDMAResourceNameLen
)

var (
	resourceNamePartRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	qualifiedNameSegmentRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// Manifest is the subset of the v1 managed workflow manifest we need to make
// scheduling decisions. The user's trainer reads the full manifest itself
// (embedded in the workload's manifest payload); we only validate the
// fields whose mistakes would cause silent or expensive failures (wrong GPU
// claim, etc.).
type Manifest struct {
	SchemaVersion int    `yaml:"schema_version"`
	Name          string `yaml:"name"`
	// ResourceNameOverride is assigned by Tau at submission time. It is not
	// part of the user-authored manifest schema.
	ResourceNameOverride string `yaml:"-" json:"-"`
	Eval                 struct {
		// CPUWorkers is the number of CPU-only Ray worker pods to provision
		// when this manifest is rendered as a rayjob-eval (Ray actor on a
		// dedicated GPU worker + ray.remote tasks fanned out across CPU pods). Optional;
		// 0 (default) means "no separate CPU workers" — only meaningful when
		// the workload kind is rayjob-eval.
		CPUWorkers int `yaml:"cpu_workers,omitempty"`
		// Upstream is the optional name of an upstream managed workflow job
		// whose checkpoint this eval reads. Used by the tau-py orchestrator
		// to wire TAU_UPSTREAM_CHECKPOINT into the eval pod's env. Purely
		// descriptive — no admission ordering is enforced from here.
		Upstream string `yaml:"upstream,omitempty"`
	} `yaml:"eval"`
	Compute struct {
		GPUs              int    `yaml:"gpus"`
		CPUs              int    `yaml:"cpus,omitempty"`
		CPULimit          int    `yaml:"cpu_limit,omitempty"`
		WorkerCPUs        int    `yaml:"worker_cpus,omitempty"`
		WorkerCPULimit    int    `yaml:"worker_cpu_limit,omitempty"`
		Memory            string `yaml:"memory,omitempty"`
		MemoryLimit       string `yaml:"memory_limit,omitempty"`
		WorkerMemory      string `yaml:"worker_memory,omitempty"`
		WorkerMemoryLimit string `yaml:"worker_memory_limit,omitempty"`
		Workers           int    `yaml:"workers"`
	} `yaml:"compute"`
	// Runtime declares the per-pod environment shipped to head + workers.
	// Required: tau does not ship a fallback pip list. The user's manifest
	// must declare the exact deps the trainer needs.
	//
	// Image overrides the Ray base image for RayJob workloads.
	Runtime struct {
		Pip   []string          `yaml:"pip,omitempty"`
		Image string            `yaml:"image,omitempty"`
		Env   []envspec.Var     `yaml:"env,omitempty"`
		EnvKV map[string]string `yaml:"env_kv,omitempty"`
		RDMA  RuntimeRDMA       `yaml:"rdma,omitempty"`
	} `yaml:"runtime,omitempty"`
	Storage struct {
		DataPVC string         `yaml:"data_pvc,omitempty"`
		Mounts  []StorageMount `yaml:"mounts,omitempty"`
	} `yaml:"storage,omitempty"`
	Artifacts struct {
		Checkpoint string `yaml:"checkpoint,omitempty"`
	} `yaml:"artifacts,omitempty"`
	Research struct {
		Experiment string `yaml:"experiment,omitempty"`
	} `yaml:"research,omitempty"`
	Model          ModelMetadata `yaml:"model,omitempty"`
	ResourceNaming struct {
		Prefix string `yaml:"prefix,omitempty"`
	} `yaml:"resource_naming,omitempty"`
}

// RuntimeRDMA opts RayJob containers into RDMA device resources and the memlock
// capabilities NCCL NET/IB needs for verbs memory registration. Disabled by
// default so existing CPU/GPU jobs keep their current pod security posture.
type RuntimeRDMA struct {
	Enabled      bool   `yaml:"enabled,omitempty"`
	ResourceName string `yaml:"resource_name,omitempty"`
	Count        *int   `yaml:"count,omitempty"`
}

type runtimeRDMAConfig struct {
	Enabled      bool
	ResourceName string
	Count        int
}

type ModelMetadata struct {
	Name            string            `yaml:"name,omitempty"`
	Base            string            `yaml:"base,omitempty"`
	Task            string            `yaml:"task,omitempty"`
	Tags            map[string]string `yaml:"tags,omitempty"`
	PrimaryMetric   string            `yaml:"primary_metric,omitempty"`
	MetricDirection string            `yaml:"metric_direction,omitempty"`
}

type StorageMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	PVC       string `yaml:"pvc"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

// Parse loads + validates a manifest YAML from raw bytes. Returns the parsed
// Manifest on success, or an error pointing at the first violation.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest yaml: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the subset of fields tau cares about for scheduling.
func (m *Manifest) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("schema_version: want 1, got %d", m.SchemaVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("name: required (kebab-case)")
	}
	if !resourceNamePartRE.MatchString(m.Name) {
		return fmt.Errorf("name: %q is invalid (use lowercase alphanumerics with internal hyphens)", m.Name)
	}
	prefix := m.ResourcePrefix()
	if !resourceNamePartRE.MatchString(prefix) {
		return fmt.Errorf("resource_naming.prefix: %q is invalid (use lowercase alphanumerics with internal hyphens)", prefix)
	}
	resourceName := m.ResourceName()
	if len(resourceName) > maxResourceNameLen {
		return fmt.Errorf("resource name %q is too long (%d chars; Kubernetes name limit is %d)", resourceName, len(resourceName), maxResourceNameLen)
	}
	g := m.Compute.GPUs
	if g < 0 || g > 8 {
		return fmt.Errorf("compute.gpus: want 0..8 (per-worker; 0 = CPU-only), got %d", g)
	}
	// compute.workers is additive optional; absent (yaml zero) → 1. Coerce
	// here so downstream callers (render, preset matching, cluster ctx) see
	// a normalized value.
	if m.Compute.Workers == 0 {
		m.Compute.Workers = 1
	}
	if m.Compute.Workers < 1 {
		return fmt.Errorf("compute.workers: want ≥ 1, got %d", m.Compute.Workers)
	}
	if err := validateComputeResourceFields(m); err != nil {
		return err
	}
	if g == 0 {
		memoryProvided := strings.TrimSpace(m.Compute.Memory) != ""
		if m.Compute.CPUs == 0 {
			m.Compute.CPUs = 1
		}
		if m.Compute.CPULimit == 0 {
			m.Compute.CPULimit = m.Compute.CPUs
		}
		if m.Compute.WorkerCPUs == 0 {
			m.Compute.WorkerCPUs = m.Compute.CPUs
		}
		if m.Compute.WorkerCPULimit == 0 {
			m.Compute.WorkerCPULimit = m.Compute.WorkerCPUs
		}
		if strings.TrimSpace(m.Compute.Memory) == "" {
			m.Compute.Memory = "2Gi"
		}
		if strings.TrimSpace(m.Compute.MemoryLimit) == "" {
			m.Compute.MemoryLimit = m.Compute.Memory
		}
		if strings.TrimSpace(m.Compute.WorkerMemory) == "" {
			if memoryProvided {
				m.Compute.WorkerMemory = m.Compute.Memory
			} else {
				m.Compute.WorkerMemory = "4Gi"
			}
		}
		if strings.TrimSpace(m.Compute.WorkerMemoryLimit) == "" {
			m.Compute.WorkerMemoryLimit = m.Compute.WorkerMemory
		}
		if err := validateEffectiveResourcePair("compute", m.Compute.CPUs, m.Compute.CPULimit, m.Compute.Memory, m.Compute.MemoryLimit); err != nil {
			return err
		}
		if err := validateEffectiveResourcePair("compute.worker", m.Compute.WorkerCPUs, m.Compute.WorkerCPULimit, m.Compute.WorkerMemory, m.Compute.WorkerMemoryLimit); err != nil {
			return err
		}
	} else {
		if g < 1 || g > 8 {
			return fmt.Errorf("compute.gpus: want 1..8 (per-worker), got %d", g)
		}
	}
	// Eval-only optional fields. cpu_workers must be non-negative when set;
	// the dispatch layer (render.go) is responsible for cross-checking that
	// it's only meaningful for the rayjob-eval workload kind.
	if m.Eval.CPUWorkers < 0 {
		return fmt.Errorf("eval.cpu_workers: want ≥ 0, got %d", m.Eval.CPUWorkers)
	}
	if m.IsCPUOnly() && m.IsEval() {
		return fmt.Errorf("compute.gpus=0 is CPU-only training and cannot be combined with eval.cpu_workers or eval.upstream; use compute.gpus > 0 for rayjob-eval")
	}
	if len(m.Runtime.Pip) == 0 {
		return fmt.Errorf("runtime.pip: required (declare the per-pod Python environment your trainer needs; tau ships no default). Example: runtime:\n  pip:\n    - torch==2.4.0\n    - transformers")
	}
	for i, p := range m.Runtime.Pip {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("runtime.pip[%d]: blank entry", i)
		}
	}
	if err := envspec.Validate(m.Runtime.Env); err != nil {
		return err
	}
	if err := validateRuntimeRDMA(m.Runtime.RDMA); err != nil {
		return err
	}
	if err := validateStorage(m.Storage.DataPVC, m.Storage.Mounts); err != nil {
		return err
	}
	if m.Artifacts.Checkpoint != "" {
		if err := validateCheckpointArtifact(m.Artifacts.Checkpoint); err != nil {
			return err
		}
	}
	if err := validateModelMetadata(m.Model); err != nil {
		return err
	}
	return nil
}

func validateRuntimeRDMA(r RuntimeRDMA) error {
	if !r.Enabled {
		if strings.TrimSpace(r.ResourceName) != r.ResourceName {
			return fmt.Errorf("runtime.rdma.resource_name: must not have surrounding whitespace")
		}
		if r.Count != nil && *r.Count < 0 {
			return fmt.Errorf("runtime.rdma.count: want ≥ 1 when set, got %d", *r.Count)
		}
		return nil
	}
	if strings.TrimSpace(r.ResourceName) != r.ResourceName {
		return fmt.Errorf("runtime.rdma.resource_name: must not have surrounding whitespace")
	}
	resourceName := r.ResourceName
	if resourceName == "" {
		resourceName = defaultRDMAResourceName
	}
	if err := validateRDMAResourceName(resourceName); err != nil {
		return fmt.Errorf("runtime.rdma.resource_name: %w", err)
	}
	if r.Count != nil && *r.Count < 1 {
		return fmt.Errorf("runtime.rdma.count: want ≥ 1 when set, got %d", *r.Count)
	}
	return nil
}

func validateRDMAResourceName(resourceName string) error {
	if len(resourceName) > maxRDMAQualifiedNameLen {
		return fmt.Errorf("%q is too long (%d chars; max %d)", resourceName, len(resourceName), maxRDMAQualifiedNameLen)
	}
	parts := strings.Split(resourceName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("%q is invalid (want an extended resource name like %q)", resourceName, defaultRDMAResourceName)
	}
	prefix, name := parts[0], parts[1]
	if isReservedResourcePrefix(prefix) {
		return fmt.Errorf("%q uses reserved Kubernetes resource prefix %q", resourceName, prefix)
	}
	if len(prefix) > maxRDMAResourcePrefixLen {
		return fmt.Errorf("%q has a prefix longer than %d chars", resourceName, maxRDMAResourcePrefixLen)
	}
	for _, label := range strings.Split(prefix, ".") {
		if len(label) == 0 || len(label) > maxResourceNameLen || !resourceNamePartRE.MatchString(label) {
			return fmt.Errorf("%q has invalid DNS-1123 prefix %q", resourceName, prefix)
		}
	}
	if len(name) > maxRDMAResourceNameLen || !qualifiedNameSegmentRE.MatchString(name) {
		return fmt.Errorf("%q has invalid resource name segment %q", resourceName, name)
	}
	return nil
}

func isReservedResourcePrefix(prefix string) bool {
	return prefix == "kubernetes.io" ||
		strings.HasSuffix(prefix, ".kubernetes.io") ||
		prefix == "k8s.io" ||
		strings.HasSuffix(prefix, ".k8s.io")
}

func validateComputeResourceFields(m *Manifest) error {
	if m.Compute.CPUs < 0 {
		return fmt.Errorf("compute.cpus: want ≥ 1 when set, got %d", m.Compute.CPUs)
	}
	if m.Compute.CPULimit < 0 {
		return fmt.Errorf("compute.cpu_limit: want ≥ 1 when set, got %d", m.Compute.CPULimit)
	}
	if m.Compute.WorkerCPUs < 0 {
		return fmt.Errorf("compute.worker_cpus: want ≥ 1 when set, got %d", m.Compute.WorkerCPUs)
	}
	if m.Compute.WorkerCPULimit < 0 {
		return fmt.Errorf("compute.worker_cpu_limit: want ≥ 1 when set, got %d", m.Compute.WorkerCPULimit)
	}
	if m.Compute.CPUs < 0 || m.Compute.CPULimit < 0 || m.Compute.WorkerCPUs < 0 || m.Compute.WorkerCPULimit < 0 {
		return fmt.Errorf("compute cpu fields must be positive when set")
	}
	for field, value := range map[string]string{
		"compute.memory":              m.Compute.Memory,
		"compute.memory_limit":        m.Compute.MemoryLimit,
		"compute.worker_memory":       m.Compute.WorkerMemory,
		"compute.worker_memory_limit": m.Compute.WorkerMemoryLimit,
	} {
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("%s: must not have surrounding whitespace", field)
		}
		if value != "" {
			if _, err := parseMemoryQuantity(value); err != nil {
				return fmt.Errorf("%s: %w", field, err)
			}
		}
	}
	if m.Compute.CPUs > 0 && m.Compute.CPULimit > 0 && m.Compute.CPULimit < m.Compute.CPUs {
		return fmt.Errorf("compute.cpu_limit=%d must be >= compute.cpus=%d", m.Compute.CPULimit, m.Compute.CPUs)
	}
	if m.Compute.WorkerCPUs > 0 && m.Compute.WorkerCPULimit > 0 && m.Compute.WorkerCPULimit < m.Compute.WorkerCPUs {
		return fmt.Errorf("compute.worker_cpu_limit=%d must be >= compute.worker_cpus=%d", m.Compute.WorkerCPULimit, m.Compute.WorkerCPUs)
	}
	if m.Compute.Memory != "" && m.Compute.MemoryLimit != "" {
		if err := validateMemoryLimitAtLeastRequest("compute.memory", m.Compute.Memory, "compute.memory_limit", m.Compute.MemoryLimit); err != nil {
			return err
		}
	}
	if m.Compute.WorkerMemory != "" && m.Compute.WorkerMemoryLimit != "" {
		if err := validateMemoryLimitAtLeastRequest("compute.worker_memory", m.Compute.WorkerMemory, "compute.worker_memory_limit", m.Compute.WorkerMemoryLimit); err != nil {
			return err
		}
	}
	return nil
}

func validateEffectiveResourcePair(prefix string, cpuRequest, cpuLimit int, memoryRequest, memoryLimit string) error {
	if cpuRequest < 1 {
		return fmt.Errorf("%s.cpus: want ≥ 1, got %d", prefix, cpuRequest)
	}
	if cpuLimit < cpuRequest {
		return fmt.Errorf("%s.cpu_limit=%d must be >= %s.cpus=%d", prefix, cpuLimit, prefix, cpuRequest)
	}
	return validateMemoryLimitAtLeastRequest(prefix+".memory", memoryRequest, prefix+".memory_limit", memoryLimit)
}

func validateMemoryLimitAtLeastRequest(requestField, requestValue, limitField, limitValue string) error {
	request, err := parseMemoryQuantity(requestValue)
	if err != nil {
		return fmt.Errorf("%s: %w", requestField, err)
	}
	limit, err := parseMemoryQuantity(limitValue)
	if err != nil {
		return fmt.Errorf("%s: %w", limitField, err)
	}
	if limit < request {
		return fmt.Errorf("%s=%s must be >= %s=%s", limitField, limitValue, requestField, requestValue)
	}
	return nil
}

func parseMemoryQuantity(value string) (int64, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, fmt.Errorf("must not be empty")
	}
	matches := regexp.MustCompile(`^([0-9]+)([EPTGMK]i?|[eptgmk]i?)?$`).FindStringSubmatch(raw)
	if matches == nil {
		return 0, fmt.Errorf("must be a Kubernetes memory quantity like 512Mi or 32Gi")
	}
	base, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity: %w", err)
	}
	if base <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	unit := strings.ToLower(matches[2])
	multiplier := int64(1)
	switch unit {
	case "":
		multiplier = 1
	case "k":
		multiplier = 1000
	case "m":
		multiplier = 1000 * 1000
	case "g":
		multiplier = 1000 * 1000 * 1000
	case "t":
		multiplier = 1000 * 1000 * 1000 * 1000
	case "p":
		multiplier = 1000 * 1000 * 1000 * 1000 * 1000
	case "e":
		multiplier = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	case "ki":
		multiplier = 1024
	case "mi":
		multiplier = 1024 * 1024
	case "gi":
		multiplier = 1024 * 1024 * 1024
	case "ti":
		multiplier = 1024 * 1024 * 1024 * 1024
	case "pi":
		multiplier = 1024 * 1024 * 1024 * 1024 * 1024
	case "ei":
		multiplier = 1024 * 1024 * 1024 * 1024 * 1024 * 1024
	}
	return base * multiplier, nil
}

func validateStorage(dataPVC string, mounts []StorageMount) error {
	if strings.TrimSpace(dataPVC) != dataPVC {
		return fmt.Errorf("storage.data_pvc: must not have surrounding whitespace")
	}
	reservedNames := map[string]bool{
		"script": true, "manifest": true, "data": true, "tau-hot": true, "dshm": true,
	}
	reservedPaths := map[string]bool{
		"/script": true, "/manifest": true, "/data": true, "/mnt": true, "/dev/shm": true,
	}
	seenNames := map[string]bool{}
	seenPaths := map[string]bool{}
	for i, mount := range mounts {
		if mount.Name == "" || !payloadFileNamePattern.MatchString(mount.Name) || mount.Name == "." || mount.Name == ".." {
			return fmt.Errorf("storage.mounts[%d].name: invalid volume name %q", i, mount.Name)
		}
		if reservedNames[mount.Name] {
			return fmt.Errorf("storage.mounts[%d].name: %q is reserved by Tau", i, mount.Name)
		}
		if seenNames[mount.Name] {
			return fmt.Errorf("storage.mounts[%d].name: duplicate volume name %q", i, mount.Name)
		}
		seenNames[mount.Name] = true
		if !strings.HasPrefix(mount.MountPath, "/") {
			return fmt.Errorf("storage.mounts[%d].mountPath: must be an absolute pod path", i)
		}
		if reservedPaths[mount.MountPath] {
			return fmt.Errorf("storage.mounts[%d].mountPath: %q is reserved by Tau", i, mount.MountPath)
		}
		if seenPaths[mount.MountPath] {
			return fmt.Errorf("storage.mounts[%d].mountPath: duplicate mount path %q", i, mount.MountPath)
		}
		seenPaths[mount.MountPath] = true
		if strings.TrimSpace(mount.PVC) == "" {
			return fmt.Errorf("storage.mounts[%d].pvc: required", i)
		}
	}
	return nil
}

func validateCheckpointArtifact(value string) error {
	checkpoint := strings.TrimSpace(value)
	if checkpoint == "" {
		return fmt.Errorf("artifacts.checkpoint: must not be empty")
	}
	if strings.HasPrefix(checkpoint, "/") {
		return fmt.Errorf("artifacts.checkpoint: must be a relative pod path without '.' or '..' segments (got %q)", value)
	}
	for _, part := range strings.Split(checkpoint, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("artifacts.checkpoint: must be a relative pod path without '.' or '..' segments (got %q)", value)
		}
	}
	return nil
}

func validateModelMetadata(model ModelMetadata) error {
	if strings.TrimSpace(model.Name) != model.Name {
		return fmt.Errorf("model.name: must not have surrounding whitespace")
	}
	if model.Name != "" && !resourceNamePartRE.MatchString(model.Name) {
		return fmt.Errorf("model.name: %q is invalid (use lowercase alphanumerics with internal hyphens)", model.Name)
	}
	for key, value := range model.Tags {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("model.tags: tag key must not be empty")
		}
		if strings.TrimSpace(key) != key {
			return fmt.Errorf("model.tags[%q]: key must not have surrounding whitespace", key)
		}
		if strings.Contains(key, "/") || strings.Contains(key, "..") {
			return fmt.Errorf("model.tags[%q]: key must not contain '/' or '..'", key)
		}
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("model.tags[%q]: value must not have surrounding whitespace", key)
		}
	}
	if model.PrimaryMetric != "" {
		if strings.TrimSpace(model.PrimaryMetric) != model.PrimaryMetric {
			return fmt.Errorf("model.primary_metric: must not have surrounding whitespace")
		}
		if strings.Contains(model.PrimaryMetric, "..") {
			return fmt.Errorf("model.primary_metric: must not contain '..'")
		}
	}
	if model.MetricDirection != "" && model.MetricDirection != "lower" && model.MetricDirection != "higher" {
		return fmt.Errorf("model.metric_direction: want lower|higher, got %q", model.MetricDirection)
	}
	return nil
}

// ValidateRayJobResourceName checks KubeRay's stricter RayJob metadata.name
// limit. Kubernetes allows 63 chars, but KubeRay rejects RayJob names above 47.
func (m *Manifest) ValidateRayJobResourceName() error {
	resourceName := m.ResourceName()
	if len(resourceName) <= maxRayJobResourceNameLen {
		return nil
	}
	prefix := m.ResourcePrefix()
	budget := maxRayJobResourceNameLen - len(prefix) - 1
	if budget < 1 {
		return fmt.Errorf("RayJob name %q is too long (%d chars; KubeRay limit is %d). resource_naming.prefix=%q leaves no room for manifest.name; shorten the prefix", resourceName, len(resourceName), maxRayJobResourceNameLen, prefix)
	}
	return fmt.Errorf("RayJob name %q is too long (%d chars; KubeRay limit is %d). With resource_naming.prefix=%q, manifest name must be at most %d chars; shorten name or prefix", resourceName, len(resourceName), maxRayJobResourceNameLen, prefix, budget)
}

// ResourcePrefix returns the resource-name prefix for new Kubernetes objects.
// The breaking v1 contract is "tau-<name>" by default; downstream projects can
// set resource_naming.prefix to render e.g. "diffusion-<name>".
func (m *Manifest) ResourcePrefix() string {
	prefix := strings.TrimSpace(m.ResourceNaming.Prefix)
	if prefix == "" {
		return defaultResourcePrefix
	}
	return prefix
}

// ResourceName is the Kubernetes object name used for the workload and its
// optional JobSecret.
func (m *Manifest) ResourceName() string {
	if name := strings.TrimSpace(m.ResourceNameOverride); name != "" {
		return name
	}
	return m.ResourcePrefix() + "-" + m.Name
}

// RuntimePip returns a defensive copy of the runtime pip list. The list is
// validated as non-empty by Validate(); callers can rely on len(pip) > 0.
func (m *Manifest) RuntimePip() []string {
	out := make([]string, len(m.Runtime.Pip))
	copy(out, m.Runtime.Pip)
	return out
}

func (m *Manifest) RuntimeEnv() []envspec.Var {
	out := make([]envspec.Var, len(m.Runtime.Env))
	copy(out, m.Runtime.Env)
	return out
}

func (m *Manifest) ResearchExperiment() string {
	return strings.TrimSpace(m.Research.Experiment)
}

// RuntimeImage returns the Ray image requested by the manifest.
func (m *Manifest) RuntimeImage() string {
	return strings.TrimSpace(m.Runtime.Image)
}

// RuntimeRDMA returns the normalized opt-in RDMA pod config.
func (m *Manifest) RuntimeRDMA() runtimeRDMAConfig {
	if !m.Runtime.RDMA.Enabled {
		return runtimeRDMAConfig{}
	}
	resourceName := strings.TrimSpace(m.Runtime.RDMA.ResourceName)
	if resourceName == "" {
		resourceName = defaultRDMAResourceName
	}
	count := defaultRDMAResourceCount
	if m.Runtime.RDMA.Count != nil {
		count = *m.Runtime.RDMA.Count
	}
	return runtimeRDMAConfig{
		Enabled:      true,
		ResourceName: resourceName,
		Count:        count,
	}
}

func (m *Manifest) DataPVC() string {
	return strings.TrimSpace(m.Storage.DataPVC)
}

func (m *Manifest) StorageMounts() []StorageMount {
	out := make([]StorageMount, len(m.Storage.Mounts))
	copy(out, m.Storage.Mounts)
	return out
}

// IsCPUOnly reports whether the manifest opts out of GPU DRA and should render
// as a CPU-only RayJob.
func (m *Manifest) IsCPUOnly() bool {
	return m.Compute.GPUs == 0
}

// IsEval reports whether this manifest describes an eval workload (i.e.
// it sets eval.cpu_workers > 0 OR eval.upstream is set). Used by the
// render dispatcher to default the workload kind to rayjob-eval when the
// CLI didn't pass --workload-kind explicitly.
func (m *Manifest) IsEval() bool {
	return m.Eval.CPUWorkers > 0 || m.Eval.Upstream != ""
}

// IsMultiNode reports whether the manifest requests multi-node training
// (i.e. compute.workers > 1). Callers use this to switch the workload kind
// (multi-node requires rayjob) and to decide whether the tau-py SDK
// wrapper is required (embedded trainer is single-node only).
func (m *Manifest) IsMultiNode() bool {
	return m.Compute.Workers > 1
}

// Claim returns the DRA ResourceClaimTemplate name for the given GPU count.
// 0 GPUs → no claim; 1 GPU → "full-gpu" (the existing claim for single-GPU
// jobs); ≥2 GPUs → "ds-Ngpus" (intra-node multi-GPU claims pre-created in the
// ray namespace).
func Claim(gpus int) string {
	if gpus <= 0 {
		return ""
	}
	if gpus == 1 {
		return "full-gpu"
	}
	return fmt.Sprintf("ds-%dgpus", gpus)
}
