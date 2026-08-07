package runconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/envspec"
)

func TestParseRejectsUnknownDirectRunField(t *testing.T) {
	_, err := parse([]byte(`name: typo
engine: ray
entrypoint: train.py
runtime:
  pip:
    - torch==2.4.0
compute:
  gpus_per_pod: 1
`), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "field gpus_per_pod not found") {
		t.Fatalf("expected unknown field error for compute.gpus_per_pod, got %v", err)
	}
}

// TestRunConfigRejectsCheckpointEvery locks in that policy.checkpoint_every is
// not part of the direct run config contract. No renderer or runtime consumer
// parses a checkpoint cadence: the value's only downstream effect was
// contract.hasCheckpoint = true in core/topology/topology.go, which satisfies
// the "mode=elastic requires checkpoint/restart semantics" guard. Exposing it
// would let an elastic/preemptible job pass validation and still lose all
// progress on eviction. Re-add it only alongside a real checkpoint-and-resume
// contract and a preemption/resume regression test.
func TestRunConfigRejectsCheckpointEvery(t *testing.T) {
	_, err := parse([]byte(`name: elastic-run
engine: job
entrypoint: train.py
policy:
  mode: elastic
  checkpoint_every: 15m
`), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "field checkpoint_every not found") {
		t.Fatalf("expected policy.checkpoint_every to be rejected as an unknown field, got %v", err)
	}
}

func TestValidateDirectRejectsNegativeJobGPUCount(t *testing.T) {
	negative := -1
	cfg := Config{Compute: Compute{GPUs: &negative}}
	if err := cfg.ValidateDirect(); err == nil || !strings.Contains(err.Error(), "compute.gpus must be >= 0") {
		t.Fatalf("expected negative compute.gpus rejection, got %v", err)
	}
}

func TestParseAcceptsDigestPinnedImageAssetForDirectJob(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cfg, err := parse([]byte(`name: asset-staging
engine: job
entrypoint: generate.py
storage:
  image_assets:
    - name: pinned-reference-assets
      image: example.azurecr.io/reference-assets@sha256:`+digest+`
      source_path: /opt/source-assets
      mount_path: /opt/reference
`), "tau.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Storage.ImageAssets) != 1 || cfg.Storage.ImageAssets[0].Name != "pinned-reference-assets" {
		t.Fatalf("image assets = %+v", cfg.Storage.ImageAssets)
	}
	if err := cfg.ValidateExecution("job"); err != nil {
		t.Fatalf("validate job execution: %v", err)
	}
}

func TestImageAssetsFailClosed(t *testing.T) {
	digest := strings.Repeat("b", 64)
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "mutable image",
			cfg: Config{Storage: Storage{ImageAssets: []ImageAsset{{
				Name: "reference", Image: "example.azurecr.io/reference-assets:latest",
				SourcePath: "/opt/reference", MountPath: "/opt/reference",
			}}}},
			want: "pinned",
		},
		{
			name: "missing image name",
			cfg: Config{Storage: Storage{ImageAssets: []ImageAsset{{
				Name: "reference", Image: "@sha256:" + digest,
				SourcePath: "/opt/reference", MountPath: "/opt/reference",
			}}}},
			want: "complete lowercase OCI image reference",
		},
		{
			name: "invalid image name",
			cfg: Config{Storage: Storage{ImageAssets: []ImageAsset{{
				Name: "reference", Image: "https://example.azurecr.io/reference-assets@sha256:" + digest,
				SourcePath: "/opt/reference", MountPath: "/opt/reference",
			}}}},
			want: "complete lowercase OCI image reference",
		},
		{
			name: "reserved mount",
			cfg: Config{Storage: Storage{ImageAssets: []ImageAsset{{
				Name: "reference", Image: "example.azurecr.io/reference-assets@sha256:" + digest,
				SourcePath: "/opt/reference", MountPath: "/data/reference",
			}}}},
			want: "Tau-reserved",
		},
		{
			name: "parent of reserved mount",
			cfg: Config{Storage: Storage{ImageAssets: []ImageAsset{{
				Name: "reference", Image: "example.azurecr.io/reference-assets@sha256:" + digest,
				SourcePath: "/opt/reference", MountPath: "/var",
			}}}},
			want: "Tau-reserved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.ValidateDirect(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	cfg := Config{Storage: Storage{ImageAssets: []ImageAsset{{
		Name: "reference", Image: "example.azurecr.io/reference-assets@sha256:" + digest,
		SourcePath: "/opt/reference", MountPath: "/opt/reference",
	}}}}
	if err := cfg.ValidateExecution("ray"); err == nil || !strings.Contains(err.Error(), "engine: job") {
		t.Fatalf("ray error = %v", err)
	}
	cfg.Engine = "job"
	cfg.Workflow.File = "workflow.yaml"
	if err := cfg.ValidateExecution("job"); err == nil || !strings.Contains(err.Error(), "workflow.file") {
		t.Fatalf("workflow error = %v", err)
	}

	managed, err := parse([]byte(`schema_version: 1
name: managed
run:
  workload_kind: job
storage:
  image_assets:
    - name: reference
      image: example.azurecr.io/reference-assets@sha256:`+digest+`
      source_path: /opt/reference
      mount_path: /opt/reference
`), "managed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := managed.ValidateExecution("job"); err == nil || !strings.Contains(err.Error(), "direct Job dispatch") {
		t.Fatalf("managed workflow error = %v", err)
	}

	hidden := Config{Storage: Storage{ImageAssets: []ImageAsset{{
		Name: "reference", Image: "example.azurecr.io/reference-assets@sha256:" + digest,
		SourcePath: "/tau-asset/reference", MountPath: "/opt/reference",
	}}}}
	if err := hidden.ValidateDirect(); err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("hidden source error = %v", err)
	}
}

func TestParseKeepsManagedManifestPassThrough(t *testing.T) {
	cfg, err := parse([]byte(`schema_version: 1
name: managed
run:
  entrypoint: train.py
  workload_kind: rayjob
compute:
  gpus: 1
  workers: 2
runtime:
  image: example.com/tau:exported
  env:
    - name: MODE
      value: train
  pip:
    - torch==2.4.0
storage:
  data_pvc: durable-data
  mounts:
    - name: cache
      pvc: model-cache
      mountPath: /models
policy:
  namespace: research
workflow:
  upstream_checkpoint: /data/checkpoint
research:
  experiment: demo
`), "manifest.yaml")
	if err != nil {
		t.Fatalf("managed manifest should keep permissive parse: %v", err)
	}
	if !cfg.LooksLikeManagedWorkflow() {
		t.Fatal("expected managed workflow detection")
	}
	if cfg.Run.WorkloadKind != "rayjob" || cfg.Runtime.Image != "example.com/tau:exported" {
		t.Fatalf("managed projection lost execution fields: %+v", cfg)
	}
	if cfg.Compute.Workers == nil || *cfg.Compute.Workers != 2 {
		t.Fatalf("managed projection workers = %v, want 2", cfg.Compute.Workers)
	}
	if cfg.Policy.Namespace != "research" || cfg.Storage.DataPVC != "durable-data" {
		t.Fatalf("managed projection lost policy/storage fields: %+v", cfg)
	}
	if cfg.Workflow.UpstreamCheckpoint != "/data/checkpoint" {
		t.Fatalf("managed projection upstream = %q", cfg.Workflow.UpstreamCheckpoint)
	}
}

func TestParseAcceptsEnvSecretRefs(t *testing.T) {
	cfg, err := parse([]byte(`name: secret-config
engine: ray
entrypoint: train.py
runtime:
  env_secret:
    HF_TOKEN: hf-token:token
`), "tau.yaml")
	if err != nil {
		t.Fatalf("parse should accept runtime.env_secret: %v", err)
	}
	vars, err := cfg.Runtime.EnvSecretVars()
	if err != nil {
		t.Fatalf("EnvSecretVars: %v", err)
	}
	if len(vars) != 1 {
		t.Fatalf("env secret vars = %+v", vars)
	}
	ref := vars[0].ValueFrom.SecretKeyRef
	if vars[0].Name != "HF_TOKEN" || ref.Name != "hf-token" || ref.Key != "token" {
		t.Fatalf("unexpected env secret var: %+v", vars[0])
	}
}

func TestParseRejectsInvalidEnvSecretRefs(t *testing.T) {
	_, err := parse([]byte(`name: secret-config
engine: ray
entrypoint: train.py
runtime:
  env:
    HF_TOKEN: literal
  env_secret:
    HF_TOKEN: hf-token
`), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "runtime.env_secret.HF_TOKEN conflicts with runtime.env.HF_TOKEN") {
		t.Fatalf("expected env_secret conflict error, got %v", err)
	}
}

func TestParseAcceptsDirectJobMetricsOffload(t *testing.T) {
	cfg, err := parse([]byte(`name: tracked-job
engine: job
entrypoint: train.sh
metrics:
  history:
    - metrics-history-attempt-*/*.jsonl
    - /data/shared/eval-*.jsonl
  offload:
    enabled: true
experiment:
  project: pretraining.v1
  name: modernbert-fineweb
  group: modernbert_fine-web
`), "tau.yaml")
	if err != nil {
		t.Fatalf("parse should accept direct Job metrics offload: %v", err)
	}

	if !cfg.Metrics.Offload.Enabled || len(cfg.Metrics.History) != 2 {
		t.Fatalf("unexpected metrics config: %+v", cfg.Metrics)
	}
}

func TestParseAcceptsRayMetricsAndStagedOutput(t *testing.T) {
	cfg, err := parse([]byte(`name: tracked-ray
engine: ray
entrypoint: train.py
storage:
  data_pvc: research-workspace
  output: /data/research-workspace/runs/tracked-ray
  publish: staged
metrics:
  history: [metrics-history-attempt-*/*.jsonl]
  offload:
    enabled: true
experiment:
  project: pretraining
  name: modernbert-ray
`), "tau.yaml")
	if err != nil {
		t.Fatalf("parse should accept Ray metrics and staged output: %v", err)
	}
	if cfg.Storage.Publish != "staged" || !cfg.Metrics.Offload.Enabled {
		t.Fatalf("unexpected Ray config: storage=%+v metrics=%+v", cfg.Storage, cfg.Metrics)
	}
}

func TestParseRejectsNonCanonicalPublicationMode(t *testing.T) {
	_, err := parse([]byte(`name: tracked-ray
engine: ray
entrypoint: train.py
storage:
  data_pvc: research-workspace
  output: /data/research-workspace/runs/tracked-ray
  publish: STAGED
`), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "storage.publish") {
		t.Fatalf("non-canonical publication mode error = %v", err)
	}
}

func TestParseAcceptsLegacyHumanReadableExperimentTitle(t *testing.T) {
	cfg, err := parse([]byte(`name: tracked-job
engine: job
entrypoint: train.sh
metrics:
  history: [metrics-history-attempt-*/*.jsonl]
  offload:
    enabled: true
experiment:
  project: modernbert
  title: "ModernBERT FineWeb: Round 1"
`), "tau.yaml")
	if err != nil {
		t.Fatalf("Parse should retain the pre-v0.1 title compatibility alias: %v", err)
	}
	if got := ExperimentRunMetadata(cfg.Experiment).ExperimentID; got != "modernbert-fineweb-round-1" {
		t.Fatalf("legacy title experiment ID = %q", got)
	}
}

func TestParsePreservesNonOffloadExperimentMetadata(t *testing.T) {
	_, err := parse([]byte(`name: untracked-job
engine: job
entrypoint: train.sh
experiment:
  project: ModernBERT
  title: FineWeb
  group: "Bounded Runs"
`), "tau.yaml")
	if err != nil {
		t.Fatalf("parse should not tighten experiment metadata without metrics offload: %v", err)
	}
}

func TestParseRejectsUnsafeDirectJobMetricsOffload(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing history",
			yaml: `metrics:
  offload:
    enabled: true
experiment:
  project: pretraining
  title: tracked
`,
			want: "metrics.history requires at least one",
		},
		{
			name: "missing project",
			yaml: `metrics:
  history: [metrics.jsonl]
  offload:
    enabled: true
experiment:
  title: tracked
`,
			want: "experiment.project is required",
		},
		{
			name: "missing experiment identity",
			yaml: `metrics:
  history: [metrics.jsonl]
  offload:
    enabled: true
experiment:
  project: pretraining
`,
			want: "experiment.name is required",
		},
		{
			name: "invalid project ID",
			yaml: `metrics:
  history: [metrics.jsonl]
  offload:
    enabled: true
experiment:
  project: ModernBERT
  name: tracked
`,
			want: `experiment.project: project "ModernBERT" is invalid`,
		},
		{
			name: "invalid group ID",
			yaml: `metrics:
  history: [metrics.jsonl]
  offload:
    enabled: true
experiment:
  project: modernbert
  name: tracked
  group: bounded/run
`,
			want: `experiment.group: group "bounded/run" is invalid`,
		},
		{
			name: "absolute path outside data",
			yaml: `metrics:
  history: [/tmp/metrics.jsonl]
`,
			want: "absolute paths must be under /data",
		},
		{
			name: "relative path escapes output",
			yaml: `metrics:
  history: [../metrics.jsonl]
`,
			want: "must not escape storage.output",
		},
		{
			name: "unsupported policy",
			yaml: `metrics:
  history: [metrics.jsonl]
  offload:
    enabled: true
    endpoint: https://example.invalid
`,
			want: "field endpoint not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml), "tau.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
}

func TestFieldCatalogCoversConfigFields(t *testing.T) {
	catalog := fieldCatalogSnapshot()
	for _, path := range configFieldPaths() {
		info, ok := catalog[path]
		if !ok {
			t.Fatalf("missing field metadata for %s", path)
		}
		if strings.TrimSpace(info.Description) == "" {
			t.Fatalf("missing field description for %s", path)
		}
	}
}

func TestConfigFieldPathsIncludeStructSliceChildren(t *testing.T) {
	paths := map[string]bool{}
	for _, path := range configFieldPaths() {
		paths[path] = true
	}
	for _, want := range []string{
		"storage.image_assets.name",
		"storage.image_assets.image",
		"storage.image_assets.source_path",
		"storage.image_assets.mount_path",
	} {
		if !paths[want] {
			t.Fatalf("config field paths missing %s", want)
		}
	}
}

func TestJSONSchemaCoversCoreFields(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("root schema should reject unknown fields: %v", schema["additionalProperties"])
	}
	props := schema["properties"].(map[string]any)
	for _, section := range []string{"runtime", "compute", "policy", "storage", "profiler", "experiment"} {
		if _, ok := props[section]; !ok {
			t.Fatalf("schema missing core section %s", section)
		}
	}
	runtime := props["runtime"].(map[string]any)
	runtimeProps := runtime["properties"].(map[string]any)
	envSecret := runtimeProps["env_secret"].(map[string]any)
	if envSecret["x-tau-status"] != string(statusSupported) {
		t.Fatalf("runtime.env_secret status = %v, want supported", envSecret["x-tau-status"])
	}
}

func TestJSONSchemaDurationRendersAsString(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	resilience := props["resilience"].(map[string]any)
	rProps := resilience["properties"].(map[string]any)
	bi := rProps["backoff_initial"].(map[string]any)
	if bi["type"] != "string" {
		t.Fatalf("backoff_initial schema type = %v, want 'string'", bi["type"])
	}
	bm := rProps["backoff_max"].(map[string]any)
	if bm["type"] != "string" {
		t.Fatalf("backoff_max schema type = %v, want 'string'", bm["type"])
	}
}

func TestJSONSchemaAcceptsDeprecatedGPUClassAliases(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	policy := props["policy"].(map[string]any)["properties"].(map[string]any)
	gpuClass := policy["gpu_class"].(map[string]any)
	choices, ok := gpuClass["oneOf"].([]any)
	if !ok || len(choices) != 2 {
		t.Fatalf("gpu_class oneOf = %#v, want canonical and deprecated choices", gpuClass["oneOf"])
	}
	deprecated := choices[1].(map[string]any)
	if deprecated["deprecated"] != true {
		t.Fatalf("deprecated gpu_class aliases are not marked deprecated: %#v", deprecated)
	}
	got := deprecated["enum"].([]any)
	want := []string{"a100-nvlink-80gb", "h100-standalone-95gb", "h200-nvlink-141gb"}
	if len(got) != len(want) {
		t.Fatalf("deprecated aliases = %#v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deprecated alias[%d] = %v, want %q", i, got[i], want[i])
		}
	}
}

func TestParseResilienceConfig(t *testing.T) {
	cfg, err := parse([]byte(`name: retry-test
engine: job
entrypoint: train.sh
resilience:
  max_retries: 3
  retry_on: [Preempted, Evicted]
  checkpoint_path: /data/ckpt
  backoff_initial: 30s
  backoff_max: 5m
`), "tau.yaml")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.Resilience.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", cfg.Resilience.MaxRetries)
	}
	if len(cfg.Resilience.RetryOn) != 2 || cfg.Resilience.RetryOn[0] != "Preempted" {
		t.Errorf("RetryOn = %v, want [Preempted Evicted]", cfg.Resilience.RetryOn)
	}
	if cfg.Resilience.CheckpointPath != "/data/ckpt" {
		t.Errorf("CheckpointPath = %q, want /data/ckpt", cfg.Resilience.CheckpointPath)
	}
	if cfg.Resilience.BackoffInitial.Duration != 30*time.Second {
		t.Errorf("BackoffInitial = %v, want 30s", cfg.Resilience.BackoffInitial.Duration)
	}
	if cfg.Resilience.BackoffMax.Duration != 5*time.Minute {
		t.Errorf("BackoffMax = %v, want 5m", cfg.Resilience.BackoffMax.Duration)
	}
}

func TestReferenceMarkdownCallsOutScopeAndUnsupportedFields(t *testing.T) {
	got := ReferenceMarkdown()
	for _, want := range []string{
		"direct `tau run --config` files",
		"`run.image` | supported",
		"`runtime.env_secret` | supported",
		"`metrics.offload` | supported",
		"`metrics.offload.enabled` | supported",
		"`experiment.project` | supported",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reference missing %q:\n%s", want, got)
		}
	}
}

// Fields a config path rejects outright, mapped to the path that accepts them.
//
// This list is maintained by hand. A fail-closed guard would have to walk the
// rejection sites, and those straddle two modules -- core/runconfig/config.go
// and cli/internal/cli/run.go -- so a walk rooted here would see only half of
// them and read as complete. Adding a path-scoped rejection means adding the
// field here too.
var pathScopedStatuses = map[string]FieldStatus{
	"runtime.env_kv":             statusWorkflowOnly,
	"storage.publish":            statusDirectOnly,
	"policy.clear_node_selector": statusDirectOnly,
}

// Fields that a config path rejects outright must not read as `supported`.
// The status column exists so a reader can tell at a glance whether their
// config can use a field; burying "workflow.file only" mid-description puts a
// path restriction next to ordinary prerequisites like --key-vault.
func TestPathScopedFieldsDoNotReadAsSupported(t *testing.T) {
	catalog := fieldCatalogSnapshot()
	for path, want := range pathScopedStatuses {
		if got := catalog[path].Status; got != want {
			t.Errorf("%s status = %q, want %q: the other config path rejects this field outright", path, got, want)
		}
	}
}

// The rendered table is what researchers actually read, so assert the status
// reaches it rather than trusting the catalog alone.
func TestReferenceMarkdownShowsPathScopedStatus(t *testing.T) {
	got := ReferenceMarkdown()
	for path, want := range pathScopedStatuses {
		if row := fmt.Sprintf("`%s` | %s", path, want); !strings.Contains(got, row) {
			t.Errorf("reference missing %q", row)
		}
		if stale := fmt.Sprintf("`%s` | %s", path, statusSupported); strings.Contains(got, stale) {
			t.Errorf("reference still renders %q", stale)
		}
	}
}

func TestValidateReservedEnvKeys(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"exact match MASTER_ADDR", map[string]string{"MASTER_ADDR": "x"}, "MASTER_ADDR"},
		{"exact match MASTER_PORT", map[string]string{"MASTER_PORT": "6379"}, "MASTER_PORT"},
		{"TAU_DIST_BACKEND", map[string]string{"TAU_DIST_BACKEND": "nccl"}, "TAU_DIST_BACKEND"},
		{"TAU_WORLD_SIZE", map[string]string{"TAU_WORLD_SIZE": "8"}, "TAU_WORLD_SIZE"},
		{"TAU_NUM_WORKERS", map[string]string{"TAU_NUM_WORKERS": "4"}, "TAU_NUM_WORKERS"},
		// The namespace rule is what makes config validation agree with the
		// renderers. Under the old exact-key denylist these three passed here
		// and were rejected later, at render.
		{"unlisted TAU_ key", map[string]string{"TAU_EXPERIMENT": "x"}, "TAU_EXPERIMENT"},
		{"TAU_ key added after this build", map[string]string{"TAU_SOME_FUTURE_KEY": "x"}, "TAU_SOME_FUTURE_KEY"},
		{"lowercase TAU_ key", map[string]string{"tau_experiment": "x"}, "tau_experiment"},
		{"NCCL prefix", map[string]string{"NCCL_DEBUG": "INFO"}, "NCCL_DEBUG"},
		{"NCCL prefix other", map[string]string{"NCCL_SOCKET_IFNAME": "ib0"}, "NCCL_SOCKET_IFNAME"},
		{"case-insensitive", map[string]string{"master_addr": "x"}, "master_addr"},
		{"allowed key passes", map[string]string{"MY_VAR": "ok"}, ""},
		// Injected by the retry loop and read back by researcher code, so they
		// have to survive a round trip through runtime.env.
		{"retry key passes", map[string]string{"TAU_RESUME_FROM": "/data/ckpt"}, ""},
		{"multiple conflicts first reported", map[string]string{"MASTER_ADDR": "x", "NCCL_DEBUG": "INFO"}, "MASTER_ADDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Runtime{Env: tt.env}
			err := r.ValidateReservedEnvKeys(false)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestReservedTauEnvKeyIsANamespaceRule pins the shape of the reservation, not
// just its current membership. The bug this replaces was two gates built as
// opposites -- a denylist that defaults to permit in front of an allowlist that
// defaults to deny -- so the case that matters most is the unlisted key.
func TestReservedTauEnvKeyIsANamespaceRule(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"TAU_RESUME_FROM", false},
		{"TAU_RETRY_ATTEMPT", false},
		{"TAU_RETRY_MAX", false},
		{"TAU_RETRY_REASON", false},
		{"TAU_EXPERIMENT", true},
		{"TAU_DIST_BACKEND", true},
		{"TAU_", true},
		// The namespace test is case-insensitive but the escape hatch is not.
		// `tau_resume_from` is a different env var from the one the retry loop
		// injects, so permitting it would hand the researcher a silent miss on
		// the retry path; rejecting it surfaces the misspelling.
		{"tau_resume_from", true},
		{"Tau_Resume_From", true},
		{"tau_experiment", true},
		// Surrounding space is not trimmed away: a legal env name has none, so
		// trimming could only let a malformed name read as a permitted key. A
		// leading space is a different case -- the name no longer starts with
		// TAU_, so it is outside this namespace entirely. envspec.Validate
		// rejects both as malformed.
		{"TAU_RESUME_FROM ", true},
		{" TAU_RESUME_FROM", false},
		// The prefix has to be anchored: these are ordinary user keys.
		{"MY_TAU_VAR", false},
		{"TAUX_THING", false},
		{"MY_VAR", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReservedTauEnvKey(tt.name); got != tt.want {
				t.Fatalf("ReservedTauEnvKey(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The rejection message has to name the way forward. Its NCCL sibling names an
// exact config key; this one previously named nothing, so the reader's next
// action was undefined.
func TestReservedTauEnvKeyErrorNamesThePermittedKeys(t *testing.T) {
	msg := ReservedTauEnvKeyError("TAU_EXPERIMENT").Error()
	if !strings.Contains(msg, "TAU_EXPERIMENT") {
		t.Fatalf("message does not name the rejected key: %s", msg)
	}
	for _, allowed := range TauEnvAllowed {
		if !strings.Contains(msg, allowed) {
			t.Fatalf("message does not name permitted key %q: %s", allowed, msg)
		}
	}
}

// A leading space was raised as a possible bypass of the TAU_ prefix check.
// It is not one: no path renders such a name. envspec's C_IDENTIFIER rule
// rejects any surrounding space, on the Job map path via envspec.Merge and
// everywhere else via envspec.Validate. This pins that so the reasoning is
// enforced rather than only argued in a comment.
func TestWhitespaceEnvNamesNeverReachAWorkload(t *testing.T) {
	for _, name := range []string{" TAU_RESUME_FROM", "TAU_RESUME_FROM ", " PATH", "MY_VAR "} {
		if err := envspec.Validate([]envspec.Var{{Name: name, Value: "v"}}); err == nil {
			t.Errorf("envspec.Validate(%q) = nil; a name with surrounding space must be rejected", name)
		}
		if _, err := envspec.Merge(envspec.FromMap(map[string]string{name: "v"})); err == nil {
			t.Errorf("envspec.Merge(%q) = nil; the Job map path must reject it too", name)
		}
	}
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestValidateExecution(t *testing.T) {
	tests := []struct {
		name      string
		engine    string
		execution Execution
		wantErr   string
	}{
		// --- engine: ray valid combinations ---
		{name: "ray/ray-train", engine: "ray", execution: Execution{Launcher: strPtr("ray-train")}, wantErr: ""},
		{name: "ray/ray-tune", engine: "ray", execution: Execution{Launcher: strPtr("ray-tune"), Metric: "loss", Configs: map[string]any{"lr": []any{0.01}}}, wantErr: ""},
		{name: "ray/nil-launcher", engine: "ray", execution: Execution{}, wantErr: ""},

		// --- engine: ray cross-engine rejections ---
		{name: "ray/torchrun", engine: "ray", execution: Execution{Launcher: strPtr("torchrun")}, wantErr: "torchrun is for engine: job"},
		{name: "ray/python", engine: "ray", execution: Execution{Launcher: strPtr("python")}, wantErr: "python is for engine: job"},
		{name: "ray/unknown", engine: "ray", execution: Execution{Launcher: strPtr("vllm")}, wantErr: "not valid for engine: ray"},
		{name: "ray/nodes>1", engine: "ray", execution: Execution{Nodes: intPtr(2)}, wantErr: "execution.nodes is for engine: job"},

		// --- engine: job valid combinations ---
		{name: "job/python", engine: "job", execution: Execution{Launcher: strPtr("python")}, wantErr: ""},
		{name: "job/torchrun", engine: "job", execution: Execution{Launcher: strPtr("torchrun")}, wantErr: ""},
		{name: "job/nil-launcher", engine: "job", execution: Execution{}, wantErr: ""},

		// --- engine: job cross-engine rejections ---
		{name: "job/ray-train", engine: "job", execution: Execution{Launcher: strPtr("ray-train")}, wantErr: "requires engine: ray"},
		{name: "job/ray-tune", engine: "job", execution: Execution{Launcher: strPtr("ray-tune")}, wantErr: "requires engine: ray"},
		{name: "job/unknown", engine: "job", execution: Execution{Launcher: strPtr("deepspeed")}, wantErr: "not valid for engine: job"},

		// --- inferred engine (empty string) ---
		{name: "inferred/torchrun", engine: "", execution: Execution{Launcher: strPtr("torchrun")}, wantErr: ""},
		{name: "inferred/python", engine: "", execution: Execution{Launcher: strPtr("python")}, wantErr: ""},
		{name: "inferred/nil-launcher", engine: "", execution: Execution{}, wantErr: ""},
		{name: "inferred/unknown", engine: "", execution: Execution{Launcher: strPtr("bogus")}, wantErr: "not valid"},
		{name: "inferred/ray-train-requires-engine", engine: "", execution: Execution{Launcher: strPtr("ray-train")}, wantErr: "requires engine: ray"},
		{name: "inferred/ray-tune-requires-engine", engine: "", execution: Execution{Launcher: strPtr("ray-tune")}, wantErr: "requires engine: ray"},

		// --- processes_per_node constraints ---
		{name: "ppn/torchrun-ok", engine: "job", execution: Execution{Launcher: strPtr("torchrun"), ProcessesPerNode: intPtr(4)}, wantErr: ""},
		{name: "ppn/python-ppn1-ok", engine: "job", execution: Execution{Launcher: strPtr("python"), ProcessesPerNode: intPtr(1)}, wantErr: ""},
		{name: "ppn/python-ppn2-rejected", engine: "job", execution: Execution{Launcher: strPtr("python"), ProcessesPerNode: intPtr(2)}, wantErr: "requires execution.launcher: torchrun"},
		{name: "ppn/ray-train-rejected", engine: "ray", execution: Execution{Launcher: strPtr("ray-train"), ProcessesPerNode: intPtr(4)}, wantErr: "requires execution.launcher: torchrun"},
		{name: "ppn/nil-launcher-rejected", engine: "job", execution: Execution{ProcessesPerNode: intPtr(2)}, wantErr: "requires execution.launcher: torchrun"},

		// --- nodes constraints ---
		{name: "nodes<1", engine: "job", execution: Execution{Nodes: intPtr(0)}, wantErr: "execution.nodes must be >= 1"},
		{name: "nodes=1/ray-ok", engine: "ray", execution: Execution{Nodes: intPtr(1)}, wantErr: ""},
		{name: "nodes=2/job-ok", engine: "job", execution: Execution{Nodes: intPtr(2)}, wantErr: ""},

		// --- case insensitivity ---
		{name: "case/Ray-Train", engine: "ray", execution: Execution{Launcher: strPtr("Ray-Train")}, wantErr: ""},
		{name: "case/TORCHRUN", engine: "job", execution: Execution{Launcher: strPtr("TORCHRUN")}, wantErr: ""},

		// --- ray-tune field validation ---
		{name: "tune/valid", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-tune"), Metric: "val_loss", Mode: "min",
			NumSamples: intPtr(5), MaxConcurrentTrials: intPtr(2),
			Configs: map[string]any{"lr": []any{0.001, 0.01}},
		}, wantErr: ""},
		{name: "tune/missing-metric", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-tune"), Configs: map[string]any{"lr": []any{0.001}},
		}, wantErr: "execution.metric is required"},
		{name: "tune/missing-param-space", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-tune"), Metric: "val_loss",
		}, wantErr: "execution.configs is required"},
		{name: "tune/bad-mode", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-tune"), Metric: "val_loss", Mode: "fast",
			Configs: map[string]any{"lr": []any{0.001}},
		}, wantErr: "execution.mode must be min or max"},
		{name: "tune/fields-without-tune-launcher", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-train"), Metric: "val_loss",
		}, wantErr: "require execution.launcher: ray-tune"},
		{name: "tune/mode-default-empty-ok", engine: "ray", execution: Execution{
			Launcher: strPtr("ray-tune"), Metric: "val_loss",
			Configs: map[string]any{"lr": []any{0.001}},
		}, wantErr: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Execution: tt.execution}
			err := cfg.ValidateExecution(tt.engine)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseRejectsRemovedEvalFields(t *testing.T) {
	cases := map[string]string{
		"task":     "name: e\ntask: eval\nengine: job\nscript: run.sh\n",
		"run.task": "name: e\nrun:\n  task: eval\n  engine: job\n  script: run.sh\n",
		"harness":  "name: e\nengine: job\nscript: run.sh\neval:\n  harness: custom\n",
		"model":    "name: e\nengine: job\nscript: run.sh\neval:\n  model: /data/ckpt\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parse([]byte(raw), "tau.yaml")
			if err == nil {
				t.Fatalf("expected removal error, got nil")
			}
			for _, want := range []string{"were removed", "runtime.env", "storage.output"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("migration error missing %q: %v", want, err)
				}
			}
		})
	}
}

func TestParseAllowsManagedWorkflowEvalBlock(t *testing.T) {
	raw := "schema_version: 1\nname: e\nworkflow:\n  file: manifest.yaml\n  main_script: train.py\neval:\n  cpu_workers: 2\n  upstream: train-model\n"
	if _, err := parse([]byte(raw), "tau.yaml"); err != nil {
		t.Fatalf("managed workflow eval block rejected: %v", err)
	}
}
