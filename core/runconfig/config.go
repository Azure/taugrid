// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runconfig owns the direct `tau run --config` YAML contract.
package runconfig

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

// Config is the implementation-backed shape for direct `tau run --config`
// files. SDK-generated managed workflow manifests share some field names, but
// are intentionally documented and validated separately.
type Config struct {
	SchemaVersion any    `yaml:"schema_version"`
	Name          string `yaml:"name"`
	Engine        string `yaml:"engine"`
	Entrypoint    string `yaml:"entrypoint"`
	Script        string `yaml:"script"`
	Image         string `yaml:"image"`

	Run        Run        `yaml:"run"`
	Workflow   Workflow   `yaml:"workflow"`
	Runtime    Runtime    `yaml:"runtime"`
	Compute    Compute    `yaml:"compute"`
	Policy     Policy     `yaml:"policy"`
	Storage    Storage    `yaml:"storage"`
	Profiler   Profiler   `yaml:"profiler"`
	Metrics    Metrics    `yaml:"metrics"`
	Experiment Experiment `yaml:"experiment"`
	Execution  Execution  `yaml:"execution"`
	Resilience Resilience `yaml:"resilience"`
}

type managedConfigProjection struct {
	SchemaVersion any        `yaml:"schema_version"`
	Name          string     `yaml:"name"`
	Run           Run        `yaml:"run"`
	Workflow      Workflow   `yaml:"workflow"`
	Compute       Compute    `yaml:"compute"`
	Policy        Policy     `yaml:"policy"`
	Profiler      Profiler   `yaml:"profiler"`
	Metrics       Metrics    `yaml:"metrics"`
	Experiment    Experiment `yaml:"experiment"`
	Execution     Execution  `yaml:"execution"`
	Resilience    Resilience `yaml:"resilience"`
	Runtime       struct {
		Image string            `yaml:"image"`
		Pip   []string          `yaml:"pip"`
		EnvKV map[string]string `yaml:"env_kv"`
	} `yaml:"runtime"`
	Storage struct {
		DataPVC     string       `yaml:"data_pvc"`
		ResultPVC   string       `yaml:"result_pvc"`
		Output      string       `yaml:"output"`
		Publish     string       `yaml:"publish"`
		ImageAssets []ImageAsset `yaml:"image_assets"`
	} `yaml:"storage"`
}

type Run struct {
	Name                    string  `yaml:"name"`
	Engine                  string  `yaml:"engine"`
	Entrypoint              string  `yaml:"entrypoint"`
	Script                  string  `yaml:"script"`
	Image                   string  `yaml:"image"`
	MainScript              string  `yaml:"main_script"`
	WorkloadKind            string  `yaml:"workload_kind"`
	SmokePairs              *int    `yaml:"smoke_pairs"`
	Source                  *Source `yaml:"source"`
	TTLSecondsAfterFinished *int64  `yaml:"ttl_seconds_after_finished"`

	// WorkingDir ships a whole project directory with the run instead of the
	// entrypoint alone, so sibling modules and local packages import on
	// workers as well as the driver. Empty keeps single-file behaviour.
	WorkingDir string `yaml:"working_dir"`
	// WorkingDirExcludes are extra glob patterns to leave out of the shipped
	// project archive, on top of the built-in defaults.
	WorkingDirExcludes []string `yaml:"working_dir_excludes"`
}

const SourceMountPath = "/tau/source"

// Source stages an immutable source tree from a digest-pinned OCI image for a
// direct Job. Path is copied into SourceMountPath by an init container. The
// source image must provide /bin/sh, cp, and chmod.
type Source struct {
	Image string `yaml:"image"`
	Path  string `yaml:"path"`
}

type Workflow struct {
	File               string   `yaml:"file"`
	Script             string   `yaml:"script"`
	MainScript         string   `yaml:"main_script"`
	WorkloadKind       string   `yaml:"workload_kind"`
	ExtraScripts       []string `yaml:"extra_scripts"`
	UpstreamCheckpoint string   `yaml:"upstream_checkpoint"`
	SecretPayload      string   `yaml:"secret_payload"`
	SmokePairs         *int     `yaml:"smoke_pairs"`
}

type Runtime struct {
	Image     string            `yaml:"image"`
	Pip       []string          `yaml:"pip"`
	Env       map[string]string `yaml:"env"`
	EnvSecret map[string]string `yaml:"env_secret"`
	EnvKV     map[string]string `yaml:"env_kv"`
}

const (
	// MaxLiteralEnvValueBytes leaves ample room below Linux's per-string exec
	// limit and prevents source archives from becoming Kubernetes metadata.
	MaxLiteralEnvValueBytes = 64 * 1024
	// MaxLiteralEnvTotalBytes bounds the control-plane amplification when a Job
	// is copied into Kueue Workloads and Pods. Secret-backed values are excluded.
	MaxLiteralEnvTotalBytes = 128 * 1024
	// MaxTTLSecondsAfterFinished is Kubernetes' signed int32 ceiling for
	// batch/v1 Job spec.ttlSecondsAfterFinished.
	MaxTTLSecondsAfterFinished int64 = 1<<31 - 1
)

type Compute struct {
	Workers         *int   `yaml:"workers"`
	GPUs            *int   `yaml:"gpus"`
	GPUsPerWorker   *int   `yaml:"gpus_per_worker"`
	CPUWorkers      *int   `yaml:"cpu_workers"`
	WorkloadKind    string `yaml:"workload_kind"`
	GPUResourceMode string `yaml:"gpu_resource_mode"`
	MIGProfile      string `yaml:"mig_profile"`
	CPURequest      string `yaml:"cpu_request"`
	MemoryRequest   string `yaml:"memory_request"`
	CPULimit        string `yaml:"cpu_limit"`
	MemoryLimit     string `yaml:"memory_limit"`
	HeadCPURequest  string `yaml:"head_cpu_request"`
	HeadMemRequest  string `yaml:"head_memory_request"`
	HeadCPULimit    string `yaml:"head_cpu_limit"`
	HeadMemLimit    string `yaml:"head_memory_limit"`
	WorkerCPUReq    string `yaml:"worker_cpu_request"`
	WorkerMemReq    string `yaml:"worker_memory_request"`
	WorkerCPULimit  string `yaml:"worker_cpu_limit"`
	WorkerMemLimit  string `yaml:"worker_memory_limit"`
}

type Policy struct {
	Workspace                string            `yaml:"workspace"`
	Namespace                string            `yaml:"namespace"`
	Preset                   string            `yaml:"preset"`
	Profile                  string            `yaml:"profile"`
	Queue                    string            `yaml:"queue"`
	Team                     string            `yaml:"team"`
	Lane                     string            `yaml:"lane"`
	GPUClass                 string            `yaml:"gpu_class"`
	Mode                     string            `yaml:"mode"`
	Topology                 string            `yaml:"topology"`
	Shape                    string            `yaml:"shape"`
	Priority                 string            `yaml:"priority"`
	PriorityTier             string            `yaml:"priority_tier"`
	TopologyPolicy           string            `yaml:"topology_policy"`
	WorkloadPriorityClass    string            `yaml:"workload_priority_class"`
	PodPriorityClass         string            `yaml:"pod_priority_class"`
	NodeSelector             map[string]string `yaml:"node_selector"`
	ClearNodeSelector        bool              `yaml:"clear_node_selector"`
	DisableDefaultPriorities bool              `yaml:"disable_default_priorities"`
}

type Storage struct {
	DataPVC     string       `yaml:"data_pvc"`
	ResultPVC   string       `yaml:"result_pvc"`
	Output      string       `yaml:"output"`
	Publish     string       `yaml:"publish"`
	Volumes     []string     `yaml:"volumes"`
	Mounts      []string     `yaml:"mounts"`
	ImageAssets []ImageAsset `yaml:"image_assets"`

	// Checkpoint names the file or directory, relative to the run's
	// checkpoint directory, that this run produces as its servable model
	// (e.g. "last.safetensors"). Declaring it lets tau index the artifact
	// after a successful run so `tau serve deploy --from-finetune` and
	// `tau data model` can resolve it by run name instead of requiring the
	// researcher to pass an absolute --checkpoint path. Optional; when
	// empty, no artifact index is written and nothing else changes.
	Checkpoint string `yaml:"checkpoint"`
}

type ImageAsset struct {
	Name       string `yaml:"name"`
	Image      string `yaml:"image"`
	SourcePath string `yaml:"source_path"`
	MountPath  string `yaml:"mount_path"`
}

type Profiler struct {
	Mode     string `yaml:"mode"`
	Rank     string `yaml:"rank"`
	Warmup   string `yaml:"warmup"`
	Duration string `yaml:"duration"`
}

type Metrics struct {
	History []string       `yaml:"history"`
	Offload MetricsOffload `yaml:"offload"`
}

type MetricsOffload struct {
	Enabled bool `yaml:"enabled"`
}

// Experiment names where a run belongs in the identity hierarchy:
//
//	workspace -> project -> experiment -> run
//
// Name is the experiment: the set of runs being compared. Group is an arm
// inside that set (baseline vs ablation), not a level of its own, so two
// groups under one Name are directly comparable.
//
// Name is optional for input compatibility. A pre-v0.1 Title is normalized
// first, then Group remains the final fallback for configs whose experiment
// axis used the group. New configs set Name explicitly.
type Experiment struct {
	Project string `yaml:"project"`
	Name    string `yaml:"name"`
	Group   string `yaml:"group"`
	// Title is the pre-v0.1 experiment identity spelling. Keep accepting it so
	// existing configs migrate safely, but new configs must use Name.
	Title string `yaml:"title"`
}

func (e Experiment) resolvedID() string {
	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}
	if title := strings.TrimSpace(e.Title); title != "" {
		return experiment.IDFromTitle(title)
	}
	return strings.TrimSpace(e.Group)
}

type Execution struct {
	Launcher            *string        `yaml:"launcher"`
	ProcessesPerNode    *int           `yaml:"processes_per_node"`
	Nodes               *int           `yaml:"nodes"`
	Metric              string         `yaml:"metric"`
	Mode                string         `yaml:"mode"`
	NumSamples          *int           `yaml:"num_samples"`
	MaxConcurrentTrials *int           `yaml:"max_concurrent_trials"`
	Configs             map[string]any `yaml:"configs"`
	AllowNCCLOverride   bool           `yaml:"allow_nccl_override"`
}

type Resilience struct {
	MaxRetries     int      `yaml:"max_retries"`
	RetryOn        []string `yaml:"retry_on"`
	CheckpointPath string   `yaml:"checkpoint_path"`
	BackoffInitial Duration `yaml:"backoff_initial"`
	BackoffMax     Duration `yaml:"backoff_max"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Load reads and validates a direct run config. SDK-generated managed workflow
// manifests keep their legacy permissive parse so `tau run` does not start
// rejecting pass-through manifest data while that contract is clarified.
//
// Callers that can surface diagnostics should prefer LoadWithDiagnostics: this
// wrapper drops the unknown-key warnings on the floor.
func Load(path string) (Config, error) {
	cfg, _, err := LoadWithDiagnostics(path)
	return cfg, err
}

// LoadWithDiagnostics is Load plus non-fatal warnings, currently unknown keys in
// managed workflow manifests.
func LoadWithDiagnostics(path string) (Config, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, nil, err
	}
	return parseWithDiagnostics(raw, path)
}

// parse keeps the two-value shape for callers that only care about the config.
func parse(raw []byte, source string) (Config, error) {
	cfg, _, err := parseWithDiagnostics(raw, source)
	return cfg, err
}

func parseWithDiagnostics(raw []byte, source string) (Config, []string, error) {
	if err := rejectRemovedEvalFields(raw, source); err != nil {
		return Config{}, nil, err
	}
	managed, err := rawLooksLikeManagedWorkflow(raw)
	if err != nil {
		return Config{}, nil, fmt.Errorf("parse %s: %w", source, err)
	}
	var cfg Config
	if managed {
		var projection managedConfigProjection
		if err := yaml.Unmarshal(raw, &projection); err != nil {
			return Config{}, nil, fmt.Errorf("parse %s: %w", source, err)
		}
		// The projection is partial by design, so unknown keys are reported
		// rather than rejected. See unknown.go.
		unknown, uerr := UnknownKeys(raw)
		if uerr != nil {
			return Config{}, nil, fmt.Errorf("parse %s: %w", source, uerr)
		}
		warnings := make([]string, 0, len(unknown))
		for _, key := range unknown {
			warnings = append(warnings, describeUnknownKey(key, source))
		}
		return projection.config(), warnings, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if err == io.EOF {
			return cfg, nil, nil
		}
		return Config{}, nil, fmt.Errorf("parse %s: %w", source, err)
	}
	if err := cfg.ValidateDirect(); err != nil {
		return Config{}, nil, fmt.Errorf("validate %s: %w", source, err)
	}
	return cfg, nil, nil
}

func (p managedConfigProjection) config() Config {
	return Config{
		SchemaVersion: p.SchemaVersion,
		Name:          p.Name,
		Run:           p.Run,
		Workflow:      p.Workflow,
		Runtime: Runtime{
			Image: p.Runtime.Image,
			Pip:   p.Runtime.Pip,
			EnvKV: p.Runtime.EnvKV,
		},
		Compute: p.Compute,
		Policy:  p.Policy,
		Storage: Storage{
			DataPVC:     p.Storage.DataPVC,
			ResultPVC:   p.Storage.ResultPVC,
			Output:      p.Storage.Output,
			Publish:     p.Storage.Publish,
			ImageAssets: append([]ImageAsset{}, p.Storage.ImageAssets...),
		},
		Profiler:   p.Profiler,
		Metrics:    p.Metrics,
		Experiment: p.Experiment,
		Execution:  p.Execution,
		Resilience: p.Resilience,
	}
}

func (c Config) ValidateDirect() error {
	if c.Compute.GPUs != nil && *c.Compute.GPUs < 0 {
		return fmt.Errorf("compute.gpus must be >= 0")
	}
	if err := c.Run.Source.Validate(); err != nil {
		return err
	}
	if err := c.Runtime.ValidateEnvSecrets(); err != nil {
		return err
	}
	if err := c.Runtime.ValidateEnvKV(); err != nil {
		return err
	}
	if err := c.Runtime.ValidateReservedEnvKeys(c.Execution.AllowNCCLOverride); err != nil {
		return err
	}
	if err := ValidateLiteralEnvPayloads(c.Runtime.Env); err != nil {
		return err
	}
	if err := c.Metrics.Validate(c.Experiment); err != nil {
		return err
	}
	return c.Storage.Validate()
}

func (s Storage) Validate() error {
	if err := s.ValidateCheckpoint(); err != nil {
		return err
	}
	if err := s.ValidateImageAssets(); err != nil {
		return err
	}
	switch s.Publish {
	case "", "staged":
		return nil
	default:
		return fmt.Errorf("storage.publish must be one of: staged")
	}
}

var (
	imageAssetNameRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	imageAssetDigestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func (s *Source) Validate() error {
	if s == nil {
		return nil
	}
	if err := validatePinnedImage("run.source.image", s.Image); err != nil {
		return err
	}
	if err := validateImageAssetPath("run.source.path", s.Path); err != nil {
		return err
	}
	if s.Path == "/tau-source" || strings.HasPrefix(s.Path, "/tau-source/") {
		return fmt.Errorf("run.source.path %q is hidden by Tau's staging volume", s.Path)
	}
	return nil
}

func (s *Source) ValidateEntrypoint(entrypoint string) error {
	if s == nil {
		return nil
	}
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		return fmt.Errorf("run.source requires entrypoint")
	}
	if path.IsAbs(entrypoint) || entrypoint == "." || entrypoint == ".." || path.Clean(entrypoint) != entrypoint || strings.HasPrefix(entrypoint, "../") || strings.Contains(entrypoint, `\`) {
		return fmt.Errorf("entrypoint %q must be a clean relative path inside run.source", entrypoint)
	}
	return nil
}

func (s Storage) ValidateImageAssets() error {
	if len(s.ImageAssets) > 8 {
		return fmt.Errorf("storage.image_assets supports at most 8 entries")
	}
	names := map[string]struct{}{}
	mounts := map[string]struct{}{}
	for i, asset := range s.ImageAssets {
		field := fmt.Sprintf("storage.image_assets[%d]", i)
		if asset.Name == "" || len(asset.Name) > 50 || !imageAssetNameRE.MatchString(asset.Name) {
			return fmt.Errorf("%s.name must be a lowercase DNS label no longer than 50 characters", field)
		}
		if _, ok := names[asset.Name]; ok {
			return fmt.Errorf("storage.image_assets name %q is declared more than once", asset.Name)
		}
		names[asset.Name] = struct{}{}
		if err := validatePinnedImage(field+".image", asset.Image); err != nil {
			return err
		}
		if err := validateImageAssetPath(field+".source_path", asset.SourcePath); err != nil {
			return err
		}
		if asset.SourcePath == "/tau-asset" || strings.HasPrefix(asset.SourcePath, "/tau-asset/") {
			return fmt.Errorf("%s.source_path %q is hidden by Tau's staging volume", field, asset.SourcePath)
		}
		if err := validateImageAssetPath(field+".mount_path", asset.MountPath); err != nil {
			return err
		}
		for _, reserved := range []string{"/data", "/mnt", "/script", "/manifest", "/tmp", "/dev/shm", "/var/run/tau", SourceMountPath} {
			if imageAssetPathsOverlap(asset.MountPath, reserved) {
				return fmt.Errorf("%s.mount_path %q overlaps Tau-reserved path %s", field, asset.MountPath, reserved)
			}
		}
		if _, ok := mounts[asset.MountPath]; ok {
			return fmt.Errorf("storage.image_assets mount_path %q is declared more than once", asset.MountPath)
		}
		for mount := range mounts {
			if imageAssetPathsOverlap(asset.MountPath, mount) {
				return fmt.Errorf("storage.image_assets mount paths %q and %q overlap", mount, asset.MountPath)
			}
		}
		mounts[asset.MountPath] = struct{}{}
	}
	return nil
}

func validatePinnedImage(field, image string) error {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("%s must be a complete lowercase OCI image reference pinned by an @sha256:<64 lowercase hex> digest", field)
	}
	digested, pinned := named.(reference.Digested)
	if !pinned || named.String() != image || image != strings.ToLower(image) || !imageAssetDigestRE.MatchString(digested.Digest().String()) {
		return fmt.Errorf("%s must be a complete lowercase OCI image reference pinned by an @sha256:<64 lowercase hex> digest", field)
	}
	return nil
}

func imageAssetPathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validateImageAssetPath(field, value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || value == "/" || path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path below /", field)
	}
	return nil
}

// ValidateCheckpoint enforces that storage.checkpoint stays inside the run's
// own artifact directory. The finalizer joins this value onto
// <durable>/finetunes/<run>/artifacts/, and that PVC is shared by every run in
// the namespace, so an unchecked value is a write primitive into someone
// else's run: "../../victim/model.bin" resolves to
// <durable>/finetunes/victim/model.bin. An absolute value is worse than it
// looks, because pathlib discards every component before it — joining
// "/etc/passwd" yields "/etc/passwd", not a path under the run directory.
//
// These are the same rules the manifest path already enforces in
// validateCheckpointArtifact; this field reached the same finalizer without
// them.
func (s Storage) ValidateCheckpoint() error {
	checkpoint := strings.TrimSpace(s.Checkpoint)
	if checkpoint == "" {
		return nil
	}
	if checkpoint != s.Checkpoint {
		return fmt.Errorf("storage.checkpoint: must not have surrounding whitespace (got %q)", s.Checkpoint)
	}
	if strings.HasPrefix(checkpoint, "/") || strings.Contains(checkpoint, `\`) {
		return fmt.Errorf("storage.checkpoint: must be a relative path inside the run's checkpoint dir, without '.' or '..' segments (got %q)", s.Checkpoint)
	}
	for _, part := range strings.Split(checkpoint, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("storage.checkpoint: must be a relative path inside the run's checkpoint dir, without '.' or '..' segments (got %q)", s.Checkpoint)
		}
	}
	return nil
}

func (m Metrics) Validate(experimentConfig Experiment) error {
	for i, raw := range m.History {
		value := strings.TrimSpace(raw)
		if value == "" {
			return fmt.Errorf("metrics.history[%d] must not be empty", i)
		}
		clean := path.Clean(value)
		if path.IsAbs(clean) {
			if clean != "/data" && !strings.HasPrefix(clean, "/data/") {
				return fmt.Errorf("metrics.history[%d] %q: absolute paths must be under /data", i, raw)
			}
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("metrics.history[%d] %q: relative paths must not escape storage.output", i, raw)
		}
	}
	if !m.Offload.Enabled {
		return nil
	}
	if len(m.History) == 0 {
		return fmt.Errorf("metrics.history requires at least one JSONL path when metrics.offload.enabled is true")
	}
	projectID := strings.TrimSpace(experimentConfig.Project)
	if projectID == "" {
		return fmt.Errorf("experiment.project is required when metrics.offload.enabled is true")
	}
	experimentID := experimentConfig.resolvedID()
	if experimentID == "" {
		return fmt.Errorf("experiment.name is required when metrics.offload.enabled is true")
	}
	if err := exptelemetry.ValidateID("project", projectID); err != nil {
		return fmt.Errorf("experiment.project: %w", err)
	}
	if err := exptelemetry.ValidateID("experiment", experimentID); err != nil {
		return fmt.Errorf("experiment.name: %w", err)
	}
	if groupID := strings.TrimSpace(experimentConfig.Group); groupID != "" {
		if err := exptelemetry.ValidateID("group", groupID); err != nil {
			return fmt.Errorf("experiment.group: %w", err)
		}
	}
	return nil
}

// TauEnvAllowed is every TAU_-prefixed env key a user may set. The retry loop
// injects these four and researcher code reads them back, so they have to
// survive a round trip through runtime.env; the rest of the namespace is
// Tau's. This is the whole rule — see ReservedTauEnvKey.
var TauEnvAllowed = []string{
	"TAU_RESUME_FROM",
	"TAU_RETRY_ATTEMPT",
	"TAU_RETRY_MAX",
	"TAU_RETRY_REASON",
}

var reservedEnvKeys = map[string]bool{
	"MASTER_ADDR": true,
	"MASTER_PORT": true,
}

// ReservedTauEnvKey reports whether name is inside the Tau-owned TAU_
// namespace without being one of the permitted keys. It is the single gate for
// both `tau run --config` validation and the Job and RayJob renderers, so a
// key rejected at render is rejected at load with the same message.
//
// The namespace test is case-insensitive but the escape hatch is exact-case,
// and the asymmetry is the point. Environment variables are case-sensitive, so
// `tau_resume_from` is not the key the retry loop injects: code reading
// TAU_RESUME_FROM would find nothing, silently, on the retry path. Permitting
// it as a distinct variable hands the researcher that failure; rejecting it
// shows them the misspelling, because the error lists the keys in the case
// that works.
//
// The name is matched as given. A legal env name has no surrounding space, so
// trimming one off could only let a malformed name read as a permitted key.
// Leading space is not a bypass either: " TAU_RESUME_FROM" is not a name this
// predicate claims, and every path that reaches a workload validates names
// against envspec's C_IDENTIFIER rule, which rejects any surrounding space.
// Reporting such a name as a TAU_ namespace violation would misdescribe an
// ordinary typo -- " PATH" is not a Tau key.
func ReservedTauEnvKey(name string) bool {
	if !strings.HasPrefix(strings.ToUpper(name), "TAU_") {
		return false
	}
	return !slices.Contains(TauEnvAllowed, name)
}

// ReservedTauEnvKeyError is the rejection message for a reserved TAU_ key. It
// names the permitted keys rather than only asserting the namespace is owned,
// so the reader's next action is defined.
func ReservedTauEnvKeyError(name string) error {
	return fmt.Errorf("env %q is reserved: Tau owns the TAU_ namespace; the only TAU_ keys you can set are %s", name, strings.Join(TauEnvAllowed, ", "))
}

func (r Runtime) ValidateReservedEnvKeys(allowNCCLOverride bool) error {
	if len(r.Env) == 0 && len(r.EnvSecret) == 0 {
		return nil
	}
	var conflicts []string
	collect := func(name string) {
		upper := strings.ToUpper(name)
		switch {
		case reservedEnvKeys[upper], ReservedTauEnvKey(name):
			conflicts = append(conflicts, name)
		case !allowNCCLOverride && strings.HasPrefix(upper, "NCCL_"):
			conflicts = append(conflicts, name)
		}
	}
	for name := range r.Env {
		collect(name)
	}
	for name := range r.EnvSecret {
		collect(name)
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return fmt.Errorf("runtime.env contains Tau-managed keys that cannot be overridden: %s; remove them from runtime.env (settable TAU_ keys: %s)", strings.Join(conflicts, ", "), strings.Join(TauEnvAllowed, ", "))
}

// ValidateLiteralEnvPayloads rejects content-sized literal environment values.
// Kubernetes repeats literal values across Job, Workload, and Pod objects, and
// Linux includes them in the argv+environment budget when starting a process.
// Secret-backed references are intentionally not passed to this function.
func ValidateLiteralEnvPayloads(env map[string]string) error {
	total := 0
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := env[name]
		valueBytes := len(value)
		if valueBytes > MaxLiteralEnvValueBytes {
			return fmt.Errorf(
				"literal environment variable %q is %d bytes; maximum is %d bytes: publish source or artifacts once and reference a digest-pinned OCI image with run.source instead of embedding content in environment variables",
				name,
				valueBytes,
				MaxLiteralEnvValueBytes,
			)
		}
		total += len(name) + 1 + valueBytes + 1
	}
	if total > MaxLiteralEnvTotalBytes {
		return fmt.Errorf(
			"literal environment payload is %d bytes; maximum is %d bytes: publish source or artifacts once and reference a digest-pinned OCI image with run.source instead of embedding content in environment variables",
			total,
			MaxLiteralEnvTotalBytes,
		)
	}
	return nil
}

// ValidateExecution checks that execution.launcher is valid for the given
// engine and that engine-specific constraints (nodes, processes_per_node,
// clear_node_selector) are met.
func (c Config) ValidateExecution(engine string) error {
	engine = strings.ToLower(strings.TrimSpace(engine))
	if c.Run.TTLSecondsAfterFinished != nil {
		if *c.Run.TTLSecondsAfterFinished <= 0 {
			return fmt.Errorf("run.ttl_seconds_after_finished must be > 0")
		}
		if *c.Run.TTLSecondsAfterFinished > MaxTTLSecondsAfterFinished {
			return fmt.Errorf(
				"run.ttl_seconds_after_finished must be <= %d (Kubernetes int32 maximum)",
				MaxTTLSecondsAfterFinished,
			)
		}
		if c.Workflow.File != "" || c.LooksLikeManagedWorkflow() {
			return fmt.Errorf("run.ttl_seconds_after_finished requires direct Job dispatch and cannot be used with workflow.file")
		}
		if engine != "job" {
			return fmt.Errorf("run.ttl_seconds_after_finished requires engine: job")
		}
	}
	if c.Run.Source != nil {
		if c.Workflow.File != "" || c.LooksLikeManagedWorkflow() {
			return fmt.Errorf("run.source requires direct Job dispatch and cannot be used with workflow.file")
		}
		if engine != "job" {
			return fmt.Errorf("run.source requires engine: job")
		}
		if strings.TrimSpace(c.Run.WorkingDir) != "" {
			return fmt.Errorf("run.source and run.working_dir cannot be used together")
		}
		entrypoint := firstNonEmpty(c.Run.Entrypoint, c.Run.Script, c.Entrypoint, c.Script)
		if err := c.Run.Source.ValidateEntrypoint(entrypoint); err != nil {
			return err
		}
	}
	if len(c.Storage.ImageAssets) > 0 {
		if c.Workflow.File != "" || c.LooksLikeManagedWorkflow() {
			return fmt.Errorf("storage.image_assets requires direct Job dispatch and cannot be used with workflow.file")
		}
		if engine != "job" {
			return fmt.Errorf("storage.image_assets requires engine: job")
		}
	}

	var launcher string
	if c.Execution.Launcher != nil {
		launcher = strings.ToLower(strings.TrimSpace(*c.Execution.Launcher))
	}

	if c.Execution.Nodes != nil && *c.Execution.Nodes < 1 {
		return fmt.Errorf("execution.nodes must be >= 1 (got %d)", *c.Execution.Nodes)
	}

	switch engine {
	case "job":
		switch launcher {
		case "", "python", "torchrun":
		case "ray-train", "ray-tune":
			return fmt.Errorf("execution.launcher %q requires engine: ray (needs a Ray cluster); got engine: job", launcher)
		default:
			return fmt.Errorf("execution.launcher %q is not valid for engine: job; use python or torchrun", launcher)
		}
	case "ray":
		switch launcher {
		case "", "ray-train", "ray-tune":
		case "torchrun":
			return fmt.Errorf("execution.launcher torchrun is for engine: job; Ray Train manages distributed init via TorchConfig")
		case "python":
			return fmt.Errorf("execution.launcher python is for engine: job; use ray-train for engine: ray")
		default:
			return fmt.Errorf("execution.launcher %q is not valid for engine: ray; use ray-train or ray-tune", launcher)
		}
		if c.Execution.Nodes != nil && *c.Execution.Nodes > 1 {
			return fmt.Errorf("execution.nodes is for engine: job; use compute.workers for Ray pod count")
		}
	case "":
		switch launcher {
		case "", "python", "torchrun":
		case "ray-train", "ray-tune":
			return fmt.Errorf("execution.launcher %q requires engine: ray; set engine: ray explicitly", launcher)
		default:
			return fmt.Errorf("execution.launcher %q is not valid; use python, torchrun (engine: job) or ray-train, ray-tune (engine: ray)", launcher)
		}
	default:
		return fmt.Errorf("engine %q is not valid; use job or ray", engine)
	}

	if c.Policy.ClearNodeSelector {
		switch {
		case c.Workflow.File != "" || c.LooksLikeManagedWorkflow():
			return fmt.Errorf("policy.clear_node_selector requires native job dispatch; managed workflow dispatch cannot clear profile/topology node selectors")
		case engine == "ray":
			return fmt.Errorf("policy.clear_node_selector requires engine: job; the ray engine cannot clear profile/topology node selectors")
		}
	}

	if c.Execution.ProcessesPerNode != nil && *c.Execution.ProcessesPerNode > 1 && launcher != "torchrun" {
		return fmt.Errorf("execution.processes_per_node > 1 requires execution.launcher: torchrun")
	}

	hasTuneFields := c.Execution.Metric != "" || c.Execution.Mode != "" ||
		c.Execution.NumSamples != nil || c.Execution.MaxConcurrentTrials != nil
	if hasTuneFields && launcher != "ray-tune" {
		return fmt.Errorf("execution.metric/mode/num_samples/max_concurrent_trials require execution.launcher: ray-tune")
	}
	if launcher == "ray-tune" {
		if c.Execution.Metric == "" {
			return fmt.Errorf("execution.metric is required for launcher: ray-tune")
		}
		if len(c.Execution.Configs) == 0 {
			return fmt.Errorf("execution.configs is required for launcher: ray-tune (defines the search space)")
		}
		switch strings.ToLower(c.Execution.Mode) {
		case "", "min", "max":
		default:
			return fmt.Errorf("execution.mode must be min or max, got %q", c.Execution.Mode)
		}
	}

	return c.validateConfigs(engine, launcher)
}

var torchrunDenylist = map[string]bool{
	"standalone":     true,
	"nnodes":         true,
	"nproc-per-node": true,
	"node-rank":      true,
	"rdzv-backend":   true,
	"rdzv-endpoint":  true,
	"rdzv-id":        true,
	"master-addr":    true,
	"master-port":    true,
	"module":         true,
	"no-python":      true,
	"run-path":       true,
}

var rayTrainAllowedSections = map[string]bool{
	"torch_config":   true,
	"scaling_config": true,
	"failure_config": true,
}

var rayTrainScalingDenylist = map[string]bool{
	"num_workers":          true,
	"resources_per_worker": true,
}

func (c Config) validateConfigs(engine, launcher string) error {
	if len(c.Execution.Configs) == 0 {
		return nil
	}

	// Resolve effective launcher for configs dispatch.
	effectiveLauncher := launcher
	if effectiveLauncher == "" {
		switch engine {
		case "ray":
			effectiveLauncher = "ray-train"
		default:
			effectiveLauncher = "python"
		}
	}

	switch effectiveLauncher {
	case "torchrun":
		return validateTorchrunConfigs(c.Execution.Configs)
	case "python":
		return validateJobConfigs(c.Execution.Configs)
	case "ray-train":
		return validateRayTrainConfigs(c.Execution.Configs)
	case "ray-tune":
		// No denylist for tune — all keys are search dimensions.
		return nil
	}
	return nil
}

func validateTorchrunConfigs(configs map[string]any) error {
	seen := make(map[string]string) // canonical → original
	for key, val := range configs {
		if err := validateConfigKey(key); err != nil {
			return fmt.Errorf("execution.configs: %w", err)
		}
		if err := validateConfigScalarValue(key, val); err != nil {
			return fmt.Errorf("execution.configs: %w", err)
		}
		canonical := strings.ReplaceAll(strings.ToLower(key), "_", "-")
		if torchrunDenylist[canonical] {
			return fmt.Errorf("execution.configs: %q is a Tau-managed torchrun flag and cannot be overridden", key)
		}
		if prev, ok := seen[canonical]; ok {
			return fmt.Errorf("execution.configs: %q and %q are the same flag (canonical: --%s)", prev, key, canonical)
		}
		seen[canonical] = key
	}
	return nil
}

func validateJobConfigs(configs map[string]any) error {
	for key, val := range configs {
		if err := validateConfigKey(key); err != nil {
			return fmt.Errorf("execution.configs: %w", err)
		}
		if err := validateConfigScalarValue(key, val); err != nil {
			return fmt.Errorf("execution.configs: %w", err)
		}
	}
	return nil
}

func validateRayTrainConfigs(configs map[string]any) error {
	for section := range configs {
		if !rayTrainAllowedSections[section] {
			return fmt.Errorf("execution.configs: %q is not a valid Ray Train config section; use torch_config, scaling_config, or failure_config", section)
		}
		sectionMap, ok := configs[section].(map[string]any)
		if !ok {
			return fmt.Errorf("execution.configs.%s must be a map, not a scalar or list", section)
		}
		if section == "scaling_config" {
			for field := range sectionMap {
				if rayTrainScalingDenylist[field] {
					return fmt.Errorf("execution.configs.scaling_config.%s is Tau-managed (derived from compute.workers × gpus_per_worker); remove it", field)
				}
			}
		}
	}
	return nil
}

var configKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func validateConfigKey(key string) error {
	if !configKeyRE.MatchString(key) {
		return fmt.Errorf("key %q is invalid; must match [A-Za-z0-9][A-Za-z0-9_.-]*", key)
	}
	return nil
}

func validateConfigScalarValue(key string, val any) error {
	if val == nil {
		return fmt.Errorf("key %q has null value; use empty string for a bare flag", key)
	}
	switch val.(type) {
	case string, int, int64, float64, bool:
		return nil
	}
	return fmt.Errorf("key %q has non-scalar value (type %T); only string, int, float, and bool are allowed", key, val)
}

func (r Runtime) ValidateEnvSecrets() error {
	if len(r.EnvSecret) == 0 {
		return nil
	}
	for name := range r.EnvSecret {
		if _, ok := r.Env[name]; ok {
			return fmt.Errorf("runtime.env_secret.%s conflicts with runtime.env.%s; set each env var in only one place", name, name)
		}
	}
	_, err := r.EnvSecretVars()
	return err
}

func (r Runtime) EnvSecretVars() ([]envspec.Var, error) {
	if len(r.EnvSecret) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(r.EnvSecret))
	for name := range r.EnvSecret {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	out := make([]envspec.Var, 0, len(keys))
	for _, name := range keys {
		ref, err := envspec.ParseSecretKeyRefSpec(r.EnvSecret[name])
		if err != nil {
			return nil, fmt.Errorf("runtime.env_secret.%s: %w", name, err)
		}
		out = append(out, envspec.Secret(name, ref.Name, ref.Key))
	}
	if err := envspec.Validate(out); err != nil {
		return nil, fmt.Errorf("runtime.env_secret: %w", err)
	}
	return out, nil
}

func (r Runtime) ValidateEnvKV() error {
	if len(r.EnvKV) == 0 {
		return nil
	}
	for name := range r.EnvKV {
		if _, ok := r.Env[name]; ok {
			return fmt.Errorf("runtime.env_kv.%s conflicts with runtime.env.%s; set each env var in only one place", name, name)
		}
		if _, ok := r.EnvSecret[name]; ok {
			return fmt.Errorf("runtime.env_kv.%s conflicts with runtime.env_secret.%s; set each env var in only one place", name, name)
		}
	}
	return nil
}

func (c Config) LooksLikeManagedWorkflow() bool {
	return c.SchemaVersion != nil && firstNonEmpty(c.Run.Engine, c.Engine) == ""
}

func rawLooksLikeManagedWorkflow(raw []byte) (bool, error) {
	var probe struct {
		SchemaVersion any    `yaml:"schema_version"`
		Engine        string `yaml:"engine"`
		Run           struct {
			Engine string `yaml:"engine"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return false, err
	}
	return probe.SchemaVersion != nil && firstNonEmpty(probe.Run.Engine, probe.Engine) == "", nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// rejectRemovedEvalFields turns `task: eval` and `eval.*` into migration
// guidance rather than the bare "field not found" that strict decoding would
// otherwise produce. Eval was removed because it rendered the same Job as any
// other engine: job run; the only thing it added was two labels and two
// environment variables the config can set directly.
func rejectRemovedEvalFields(raw []byte, source string) error {
	var probe struct {
		Task string         `yaml:"task"`
		Eval map[string]any `yaml:"eval"`
		Run  struct {
			Task string `yaml:"task"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		// Let the real decoder report malformed YAML.
		return nil
	}
	task := strings.ToLower(strings.TrimSpace(firstNonEmpty(probe.Run.Task, probe.Task)))
	_, hasHarness := probe.Eval["harness"]
	_, hasModel := probe.Eval["model"]
	if task != "eval" && !hasHarness && !hasModel {
		return nil
	}
	return fmt.Errorf(`validate %s: task: eval, eval.harness, and eval.model were removed; evaluation is an ordinary run.
Drop task/eval.harness/eval.model from the config, keep engine: job, and pass the checkpoint and
results paths to your image through runtime.env, for example:

  runtime:
    env:
      MODEL_PATH: /data/checkpoints/my-7b
      RESULTS_PATH: /data/evals/mmlu.json

Keep storage.output pointing at the same results path so tau run get can
retrieve it`, source)
}
