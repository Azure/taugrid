// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/runconfig"
	"gopkg.in/yaml.v3"
)

const (
	defaultResourcePrefix    = "tau"
	maxResourceNameLen       = 63
	maxRayJobResourceNameLen = 47
)

var resourceNamePartRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type Manifest struct {
	SchemaVersion int    `yaml:"schema_version"`
	Name          string `yaml:"name"`
	Eval          struct {
		CPUWorkers int    `yaml:"cpu_workers,omitempty"`
		Upstream   string `yaml:"upstream,omitempty"`
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
	Runtime struct {
		Pip   []string          `yaml:"pip,omitempty"`
		Image string            `yaml:"image,omitempty"`
		Env   []envspec.Var     `yaml:"env,omitempty"`
		EnvKV map[string]string `yaml:"env_kv,omitempty"`
		RDMA  runconfig.RDMA    `yaml:"rdma,omitempty"`
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

type RuntimeRDMA = runconfig.RDMA

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
	} else if g < 1 || g > 8 {
		return fmt.Errorf("compute.gpus: want 1..8 (per-worker), got %d", g)
	}
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
	if err := runconfig.ValidateLiteralEnvPayloads(envspec.DirectMap(m.Runtime.Env)); err != nil {
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
	return validateModelMetadata(m.Model)
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

func (m *Manifest) ResourcePrefix() string {
	prefix := strings.TrimSpace(m.ResourceNaming.Prefix)
	if prefix == "" {
		return defaultResourcePrefix
	}
	return prefix
}

func (m *Manifest) ResourceName() string {
	return m.ResourcePrefix() + "-" + m.Name
}

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

func (m *Manifest) RuntimeImage() string {
	return strings.TrimSpace(m.Runtime.Image)
}

func (m *Manifest) RuntimeRDMA() runconfig.NormalizedRDMA {
	return runconfig.NormalizeRDMA(m.Runtime.RDMA)
}

func (m *Manifest) DataPVC() string {
	return strings.TrimSpace(m.Storage.DataPVC)
}

func (m *Manifest) StorageMounts() []StorageMount {
	out := make([]StorageMount, len(m.Storage.Mounts))
	copy(out, m.Storage.Mounts)
	return out
}

func (m *Manifest) IsCPUOnly() bool {
	return m.Compute.GPUs == 0
}

func (m *Manifest) IsEval() bool {
	return m.Eval.CPUWorkers > 0 || m.Eval.Upstream != ""
}

func (m *Manifest) IsMultiNode() bool {
	return m.Compute.Workers > 1
}

func Claim(gpus int) string {
	if gpus <= 0 {
		return ""
	}
	if gpus == 1 {
		return "full-gpu"
	}
	return fmt.Sprintf("ds-%dgpus", gpus)
}
