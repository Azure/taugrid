// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/artifactindex"
	"github.com/Azure/taugrid/cli/internal/kvspec"
	"github.com/Azure/taugrid/cli/internal/payload"
	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/cli/internal/storageprobe"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

//go:embed assets/managed-workflow-job.yaml.tmpl assets/managed-workflow-rayjob.yaml.tmpl assets/managed-workflow-rayjob-eval.yaml.tmpl assets/managed-workflow-rayjob-cpu.yaml.tmpl
var assets embed.FS

// Asset returns the embedded file at "assets/<name>".
func Asset(name string) ([]byte, error) {
	return assets.ReadFile("assets/" + name)
}

// AssetNames returns the script files bundled into the script payload by
// default (currently empty — the trainer is always supplied via
// --main-script). Kept as a function for API stability and to give
// future generic helpers a hook to opt in.
func AssetNames() []string {
	return nil
}

// payloadFileNamePattern validates destination names for files embedded in a
// self-contained payload (script bundle or manifest copy) and for storage
// mount names. Kept permissive-but-safe: letters, digits, '.', '_', '-'.
var payloadFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var migProfileRE = regexp.MustCompile(`^\d+g\.\d+gb$`)

// Two independent self-contained payloads are embedded per workload (see
// Design A in issue #869): a script bundle (tau assets + researcher's
// train.py + any extra scripts) and a redacted copy of the manifest YAML.
// Each gets its own initContainer, volume, digest annotation, and target
// directory so the two never collide even though they share env var *names*
// (TAU_PAYLOAD_B64 etc. — safe because each initContainer has its own
// isolated env scope).
const (
	scriptPayloadInitContainerName   = "tau-script-payload"
	manifestPayloadInitContainerName = "tau-manifest-payload"

	scriptPayloadTargetDir   = "/script"
	manifestPayloadTargetDir = "/manifest"

	// maxRenderedWorkloadBytes bounds the final rendered workload object
	// (the Job/RayJob document alone, not the optional JobSecret or SPC
	// documents) as re-serialized to JSON — the shape `kubectl apply
	// --server-side` and MultiKueue's admission mirroring both see. This is
	// enforced in addition to, not instead of, payload.MaxDecodedBytes:
	// that cap bounds each individual payload's decoded size, this one
	// bounds the fully-rendered object carrying both payloads plus every
	// other field (env, labels, annotations, etc).
	maxRenderedWorkloadBytes = 200 * 1024
)

// ExtraScript is a researcher-supplied file mounted into /script alongside the
// embedded trainer sources.
type ExtraScript struct {
	Name string
	Data []byte
}

// WorkloadKind selects which CRD `Render` emits as the third document.
const (
	WorkloadKindJob        = "job"
	WorkloadKindRayJob     = "rayjob"
	WorkloadKindRayJobEval = "rayjob-eval"
	defaultWorkloadKind    = WorkloadKindJob
	rayOverwriteCmdKey     = "ray.io/overwrite-container-cmd"
	// defaultRayJobImage is the canonical first-party MCR GPU workload image
	// for Tau-managed GPU workloads. It is used by the single-pod batch Job,
	// GPU RayJobs, and the Job/RayJob payload init containers so shipped
	// workloads stay on the repo's first-party image contract by default.
	defaultRayJobImage = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"
)

const (
	GPUResourceModeDevicePlugin = "device-plugin"
	GPUResourceModeDRA          = "dra"
	GPUResourceModeMIG          = "mig"
)

// NormalizeGPUResourceMode converts user-facing aliases into the renderer's
// resource modes. Empty means the current default: standard NVIDIA device
// plugin resources.
func NormalizeGPUResourceMode(mode string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(mode))
	switch normalized {
	case "", GPUResourceModeDevicePlugin, "deviceplugin", "device", "nvidia", "nvidia.com/gpu", "gpu":
		return GPUResourceModeDevicePlugin, nil
	case GPUResourceModeDRA:
		return GPUResourceModeDRA, nil
	case GPUResourceModeMIG, "mig-slice":
		return GPUResourceModeMIG, nil
	default:
		return "", fmt.Errorf("want %s|nvidia|%s|%s, got %q", GPUResourceModeDevicePlugin, GPUResourceModeDRA, GPUResourceModeMIG, mode)
	}
}

// MIGResourceName returns the device-plugin extended resource name for a MIG
// profile (e.g., "1g.18gb" → "nvidia.com/mig-1g.18gb").
func MIGResourceName(profile string) string {
	return "nvidia.com/mig-" + strings.TrimSpace(profile)
}

// RenderOptions controls workload rendering.
type RenderOptions struct {
	// Manifest is the parsed + validated user manifest (drives name, gpus, etc.).
	Manifest *Manifest
	// ManifestRaw is the source manifest YAML the trainer reads in-pod after
	// renderer-owned sanitization, such as redacting secret refs from the
	// embedded manifest payload copy.
	ManifestRaw []byte
	// ManifestFilename is the basename to expose at /manifest/<filename>
	// inside the pod. The trainer's --manifest arg is set to this path.
	ManifestFilename string
	// Namespace defaults to the workload namespace if empty.
	Namespace string
	// SmokePairs caps (init,truth) pairs for fast iteration. 0 = full dataset.
	SmokePairs int
	// ExtraScripts are added to the script payload. Names must be valid
	// payload file names and must not collide with embedded trainer files.
	ExtraScripts []ExtraScript
	// TopologyProfile is the resolved profile backing a platform preset. The
	// finetune manifest still drives trainer semantics; this profile drives
	// scheduling metadata and run-profile explainability.
	TopologyProfile *profile.Profile
	// TopologyOptions carries preset/operator topology intent for Kueue.
	TopologyOptions topology.Options
	// ProfileName is stamped on the Job for run-profile output. Defaults to
	// "tau-finetune" for legacy manifest-only submits.
	ProfileName string
	// Labels and Annotations carry preset metadata resolved by the CLI.
	Labels      map[string]string
	Annotations map[string]string
	// WorkloadKind selects which CRD to emit as the third document. Empty
	// or "job" emits a batch/v1 Job (today's default; torchrun launcher).
	// "rayjob" emits a ray.io/v1 RayJob. CPU manifests (compute.gpus=0)
	// use CPU-only head + worker pod sets.
	WorkloadKind string
	// GPUResourceMode selects how GPU pods acquire GPUs:
	//   - device-plugin (default): request/limit nvidia.com/gpu
	//   - dra: use ResourceClaimTemplates (full-gpu / ds-*).
	//   - mig: request/limit nvidia.com/mig-{profile} (MIG partitioned GPUs).
	GPUResourceMode string
	// MIGProfile is the MIG slice profile name (e.g., "1g.18gb", "3g.71gb").
	// Required when GPUResourceMode is "mig".
	MIGProfile string
	// NodeSelector adds or overrides pod node selectors after topology-derived
	// selectors. Use this for standard clusters that label GPUs with keys like
	// gpu=a100 instead of tau.azure.com/gpu-class.
	NodeSelector map[string]string
	// MainScript supplies the researcher's trainer (or tau-py wrapper).
	// REQUIRED: tau does not ship a fallback trainer. The bash entrypoint
	// in every workload template invokes `python3 /script/train.py`, so
	// the bytes here become the training entrypoint embedded in the script
	// payload.
	MainScript []byte
	// UpstreamCheckpoint is the cluster-side path to an upstream training
	// run's checkpoint. Only meaningful for WorkloadKindRayJobEval — gets
	// rendered into the eval pod's TAU_UPSTREAM_CHECKPOINT env var, which
	// the cluster wrapper exposes as ctx.upstream_checkpoint to the user
	// fn. Empty (default) renders an empty env var, which the wrapper
	// turns into ctx.upstream_checkpoint = None.
	UpstreamCheckpoint string
	// JobSecret is an optional generated job-scoped Secret applied with the
	// workload. Client dry-runs redact values.
	JobSecret *JobSecret
	// RedactSecrets suppresses generated Secret values in rendered YAML.
	RedactSecrets bool
	// Profile configures opt-in Ray Train worker profiling for RayJob
	// workloads. Empty Mode preserves the default unprofiled behavior.
	Profile ProfileOptions
	// MetricsOffload configures an opt-in sidecar that tails
	// metrics-history.jsonl from the RayJob head pod and remote-writes the
	// imported rows for Stellar/Kusto. Empty Image disables the sidecar.
	MetricsOffload MetricsOffloadOptions
	// KVSpec holds the parsed Key Vault spec when runtime.env_kv is set.
	// When non-nil, Render emits a SecretProviderClass document and injects
	// CSI volume/mount + workload-identity pod label into the workload.
	KVSpec *kvspec.Spec
	// ServiceAccountName sets the pod spec's serviceAccountName field.
	// Typically sourced from the resolved workload identity.
	ServiceAccountName string
}

// ProfileOptions configures opt-in Nsight Systems profiling for Ray Train
// worker actors. Rank may be a non-negative rank, a comma-separated rank set,
// or "all". Duration must be positive because RayJob profiling wraps Ray Train
// worker process launch with nsys for a bounded collection window.
type ProfileOptions struct {
	Mode     string
	Rank     string
	Warmup   time.Duration
	Duration time.Duration
}

type JobSecret struct {
	Name       string            `json:"name" yaml:"name"`
	StringData map[string]string `json:"stringData" yaml:"stringData"`
	// OwnerName is the Job/RayJob name that owns this Secret. Used for
	// labelling; the actual ownerReference (which needs the UID) is patched
	// after apply.
	OwnerName string `json:"-" yaml:"-"`
	// OwnerKind is "Job" or "RayJob".
	OwnerKind string `json:"-" yaml:"-"`
}

// Render returns the rendered workload document(s) — a self-contained
// workload ([Job or RayJob, selected by RenderOptions.WorkloadKind], plus an
// optional leading Secret document and an optional trailing
// SecretProviderClass document) — concatenated with `---` separators,
// exactly what `kubectl apply -f -` expects. Researchers never see this;
// tau submits it for them.
//
// Design A (issue #869 PR2): the researcher's script bundle and the
// (redacted) manifest copy are embedded directly in the workload's head/
// single pod as two independent self-contained payloads (internal/payload),
// each decoded by its own initContainer into its own emptyDir volume, rather
// than mirrored via generated ConfigMaps. This makes the workload
// self-contained: MultiKueue mirrors the workload object to a worker
// cluster but not auxiliary objects like a ConfigMap, so a ConfigMap-backed
// script/manifest would silently vanish on the worker.
//
// Worker payload delivery is NOT uniform across the four templates, but it
// is uniform *within* every RayJob template: every RayJob worker pod set
// (managed-workflow-rayjob, managed-workflow-rayjob-cpu, and managed-workflow-rayjob-eval alike)
// gets the script-only payload; the manifest payload is always head-only.
//   - managed-workflow-job.yaml.tmpl is single-pod: head and "worker" are the same
//     pod, so it always gets both payloads.
//   - managed-workflow-rayjob.yaml.tmpl and managed-workflow-rayjob-cpu.yaml.tmpl, when
//     rendering multi-node (Workers>1), dispatch to the SDK wrapper's
//     ray.train.torch.TorchTrainer path. That path's worker actor closure
//     re-reads /script/<trainer-filename> from each worker pod's own local
//     disk (verified against tau-py/_cluster.py) — Ray's runtime_env
//     working_dir mechanism does not reliably reach TorchTrainer's actor
//     pool on a pre-existing cluster, and RunConfig(worker_runtime_env=...)
//     is unsupported on Ray 2.39 (the H200/RDMA image), so there is no
//     Ray-native substitute.
//   - managed-workflow-rayjob-eval.yaml.tmpl's CPU workers run plain ray.remote
//     score tasks against the head's actor. The head DOES ship the
//     researcher's user module to them via Ray's own runtime_env
//     working_dir mechanism (tau-py's _prepare_eval_worker_working_dir) —
//     but that mechanism only stages the single user-module file (plus a
//     small importable tau shim), never any --extra-script files the
//     researcher's module may import from disk at runtime. The pre-PR2
//     ConfigMap era mounted the *entire* script bundle (train.py + every
//     extra script) onto every CPU worker pod's local disk, so a helper
//     module imported from /script "just worked" there; removing that
//     without a substitute silently breaks any eval workload using
//     --extra-script once work actually reaches a CPU worker. There is no
//     Ray-native mechanism that ships extra on-disk files to a pre-existing
//     cluster's static worker pool for plain ray.remote scheduling either,
//     so CPU eval workers get the same script-only payload (tau-
//     script-payload initContainer + "script" emptyDir) as the two Ray
//     Train templates' workers.
//
// No RayJob worker pod (train or eval) ever gets the manifest payload:
// /manifest is parsed exactly once, on the head, before any worker actor is
// spawned or any task is dispatched.
func Render(opts RenderOptions) ([]byte, error) {
	if opts.Manifest == nil {
		return nil, fmt.Errorf("Render: Manifest required")
	}
	if len(opts.MainScript) == 0 {
		return nil, fmt.Errorf("Render: MainScript required (tau ships no fallback trainer; supply --main-script pointing at your trainer .py, or use the tau-py SDK which generates a wrapper for you)")
	}
	ns := opts.Namespace
	if ns == "" {
		ns = "tau"
	}
	name := opts.Manifest.Name
	resourceName := opts.Manifest.ResourceName()
	mfName := opts.ManifestFilename
	if mfName == "" {
		mfName = name + ".yaml"
	}

	scripts := map[string][]byte{}
	for _, fn := range AssetNames() {
		b, err := Asset(fn)
		if err != nil {
			return nil, fmt.Errorf("asset %s: %w", fn, err)
		}
		scripts[fn] = b
	}
	// MainScript becomes the trainer entrypoint embedded at /script/train.py.
	// The bash launcher in every workload template invokes
	// `python3 /script/train.py`.
	scripts["train.py"] = opts.MainScript
	for _, extra := range opts.ExtraScripts {
		if err := validateExtraScript(extra); err != nil {
			return nil, err
		}
		if _, exists := scripts[extra.Name]; exists {
			return nil, fmt.Errorf("extra script %q collides with an existing script entry (use a different destination name)", extra.Name)
		}
		scripts[extra.Name] = extra.Data
	}

	kind := opts.WorkloadKind
	if kind == "" {
		kind = defaultWorkloadKind
	}
	gpuResourceMode, err := NormalizeGPUResourceMode(opts.GPUResourceMode)
	if err != nil {
		return nil, err
	}
	migProfile := strings.TrimSpace(opts.MIGProfile)
	if gpuResourceMode == GPUResourceModeMIG && migProfile == "" {
		return nil, fmt.Errorf("gpu_resource_mode=mig requires mig_profile to be set (e.g., 1g.18gb, 3g.71gb)")
	}
	if migProfile != "" && gpuResourceMode != GPUResourceModeMIG {
		return nil, fmt.Errorf("mig_profile=%q is set but gpu_resource_mode=%q (must be mig)", migProfile, gpuResourceMode)
	}
	if migProfile != "" && !migProfileRE.MatchString(migProfile) {
		return nil, fmt.Errorf("mig_profile %q must match <N>g.<M>gb (e.g. 1g.18gb, 3g.71gb)", migProfile)
	}

	manifestRaw, err := redactManifestSecretRefs(opts.ManifestRaw)
	if err != nil {
		return nil, err
	}

	// Encode the two self-contained payloads. Each is independently subject
	// to payload.MaxEnvEntryBytes, which bounds the rendered
	// TAU_PAYLOAD_B64=<encoded> environment entry — the quantity the kernel
	// actually limits (MAX_ARG_STRLEN) when kubelet execve(2)s the
	// initContainer. That is what catches payloads packing many small files
	// under long names, which stay trivially under the decoded-content
	// ceiling but blow past the env entry limit once JSON framing, per-file
	// name overhead, and base64 expansion are applied.
	scriptEncoded, scriptDigest, err := payload.Encode(payload.New(scripts))
	if err != nil {
		return nil, fmt.Errorf("script payload: %w", err)
	}
	manifestEncoded, manifestDigest, err := payload.Encode(payload.New(map[string][]byte{mfName: manifestRaw}))
	if err != nil {
		return nil, fmt.Errorf("manifest payload: %w", err)
	}
	embeds := payloadEmbeds{
		ScriptEncoded:   scriptEncoded,
		ScriptDigest:    scriptDigest,
		ManifestEncoded: manifestEncoded,
		ManifestDigest:  manifestDigest,
		// artifacts.checkpoint drives the post-training artifact index step.
		// Empty means the manifest declared no checkpoint, and the entrypoint
		// renders exactly as it did before this field existed.
		CheckpointArtifact: opts.Manifest.Artifacts.Checkpoint,
	}
	// Stamp both digests on the workload's own annotations *before*
	// buildSchedulingMetadata runs, so they land in metadata.annotations —
	// `kubectl describe`/`get -o yaml` then shows which payload bytes a
	// running workload was rendered with.
	opts.Annotations = mergeStringMaps(opts.Annotations, map[string]string{
		workloadmeta.AnnotationScriptPayloadDigest:   scriptDigest,
		workloadmeta.AnnotationManifestPayloadDigest: manifestDigest,
		// Records the declaration so `tau run get` can tell a run that
		// produced nothing from one whose promised artifact is missing.
		// mergeStringMaps drops empty values, so a manifest with no
		// artifacts.checkpoint renders no annotation.
		workloadmeta.AnnotationCheckpointArtifact: strings.TrimSpace(opts.Manifest.Artifacts.Checkpoint),
	})

	var buf bytes.Buffer
	if opts.JobSecret != nil {
		if err := writeSecret(&buf, ns, *opts.JobSecret, opts.RedactSecrets); err != nil {
			return nil, err
		}
		buf.WriteString("---\n")
	}
	if opts.Manifest.IsCPUOnly() {
		opts.TopologyOptions.DisableKueueTopologyAnnotations = true
	}
	experiment := opts.Manifest.ResearchExperiment()
	if experiment != "" {
		opts.Annotations = mergeStringMaps(opts.Annotations, map[string]string{
			workloadmeta.LabelExperiment: experiment,
		})
		if validKubernetesLabelValue(experiment) {
			opts.Labels = mergeStringMaps(opts.Labels, map[string]string{
				workloadmeta.LabelExperiment: experiment,
			})
		}
	}
	scheduling, err := buildSchedulingMetadata(opts)
	if err != nil {
		return nil, err
	}
	pip := opts.Manifest.RuntimePip()
	runtimeEnv := opts.Manifest.RuntimeEnv()
	if experiment != "" {
		runtimeEnv, err = envspec.Merge(runtimeEnv, envspec.FromMap(map[string]string{
			"TAU_EXPERIMENT": experiment,
			"TAU_GROUP":      experiment,
		}))
		if err != nil {
			return nil, err
		}
	}
	if opts.RedactSecrets {
		runtimeEnv = envspec.RedactSecretRefs(runtimeEnv)
	}
	rdma := opts.Manifest.RuntimeRDMA()
	dataPVC := opts.Manifest.DataPVC()
	storageMounts := opts.Manifest.StorageMounts()
	resources := resourceSizingFor(opts.Manifest, kind)

	// Key Vault integration: merge KV-synced env vars and stamp the
	// workload-identity pod label so the mutating webhook injects the
	// projected service account token volume.
	spcName := ""
	if opts.KVSpec != nil && len(opts.KVSpec.Entries) > 0 {
		spcName = kvspec.SPCName(resourceName)
		syncedSecret := kvspec.SyncedSecretName(resourceName)
		kvEnv := opts.KVSpec.EnvVars(syncedSecret)
		runtimeEnv, err = envspec.Merge(runtimeEnv, kvEnv)
		if err != nil {
			return nil, fmt.Errorf("merge kv env: %w", err)
		}
		scheduling.PodLabels[workloadmeta.LabelAzureWorkloadIdentityUse] = "true"
		scheduling.PodMetadataBlock = renderPodMetadata(scheduling.PodLabels, scheduling.PodAnnotations, 4)
	}

	if opts.ServiceAccountName != "" {
		scheduling.ServiceAccountName = opts.ServiceAccountName
	}

	prof := opts.profileOptions()
	if prof.Mode != "" {
		if err := validateProfileOptions(prof); err != nil {
			return nil, err
		}
		if kind != WorkloadKindRayJob {
			return nil, fmt.Errorf("--profiler currently supports workload kind %q only (got %q)", WorkloadKindRayJob, kind)
		}
		if opts.Manifest.IsCPUOnly() {
			return nil, fmt.Errorf("--profiler nsys requires compute.gpus > 0")
		}
		if dataPVC == "" {
			return nil, fmt.Errorf("--profiler nsys requires storage.data_pvc or --data-pvc so profile artifacts survive Ray pod cleanup")
		}
		profileEnv, profileAnnotations := buildRayJobProfileContract(opts.Manifest, ns, dataPVC, prof)
		runtimeEnv, err = envspec.Merge(runtimeEnv, profileEnv)
		if err != nil {
			return nil, err
		}
		scheduling = withProfileAnnotations(scheduling, profileAnnotations)
	}
	var workloadYAML []byte
	metricsOffload, err := opts.metricsOffloadRuntime(kind)
	if err != nil {
		return nil, err
	}

	// Inject execution contract env vars for RayJob workloads (backfill #1025
	// gap: the manifest renderer was missing these authoritative vars).
	if kind == WorkloadKindRayJob || kind == WorkloadKindRayJobEval {
		gpus := opts.Manifest.Compute.GPUs
		workers := opts.Manifest.Compute.Workers
		backend := "gloo"
		if gpus > 0 {
			backend = "nccl"
		}
		numWorkers := workers
		if gpus > 0 {
			numWorkers = workers * gpus
		}
		execEnv := envspec.FromMap(map[string]string{
			"TAU_DIST_BACKEND": backend,
			"TAU_NUM_WORKERS":  strconv.Itoa(numWorkers),
			"TAU_WORLD_SIZE":   strconv.Itoa(numWorkers),
		})
		runtimeEnv, err = envspec.Merge(runtimeEnv, execEnv)
		if err != nil {
			return nil, err
		}
	}
	if err := runconfig.ValidateLiteralEnvPayloads(envspec.DirectMap(runtimeEnv)); err != nil {
		return nil, err
	}

	switch kind {
	case WorkloadKindJob:
		if opts.Manifest.IsMultiNode() {
			return nil, fmt.Errorf("workload kind %q cannot be multi-node (compute.workers=%d); use --workload-kind=rayjob or omit it", kind, opts.Manifest.Compute.Workers)
		}
		if opts.Manifest.IsEval() {
			return nil, fmt.Errorf("workload kind %q cannot serve eval manifests (eval.cpu_workers=%d, eval.upstream=%q); use --workload-kind=rayjob-eval", kind, opts.Manifest.Eval.CPUWorkers, opts.Manifest.Eval.Upstream)
		}
		workloadYAML, err = buildJob(name, resourceName, ns, mfName, opts.Manifest.Compute.GPUs, opts.SmokePairs, pip, runtimeEnv, dataPVC, storageMounts, scheduling, gpuResourceMode, migProfile, resources, spcName, embeds)
	case WorkloadKindRayJob:
		if err := opts.Manifest.ValidateRayJobResourceName(); err != nil {
			return nil, err
		}
		if opts.Manifest.IsEval() {
			return nil, fmt.Errorf("workload kind %q cannot serve eval manifests (eval.cpu_workers=%d, eval.upstream=%q); use --workload-kind=rayjob-eval", kind, opts.Manifest.Eval.CPUWorkers, opts.Manifest.Eval.Upstream)
		}
		if opts.Manifest.IsCPUOnly() {
			image := opts.Manifest.RuntimeImage()
			if image == "" {
				return nil, fmt.Errorf("workload kind %q with compute.gpus=0 requires runtime.image; CPU RayJobs do not have a stable default image", kind)
			}
			if err := validateRayLogOffloadStorageMounts(kind, true, storageMounts); err != nil {
				return nil, err
			}
			scheduling = withRayOverwriteContainerCommand(scheduling)
			workloadYAML, err = buildRayJobCPU(
				name,
				resourceName,
				ns,
				mfName,
				opts.Manifest.Compute.Workers,
				image,
				opts.SmokePairs,
				pip,
				runtimeEnv,
				strings.TrimSpace(opts.Manifest.Storage.DataPVC),
				storageMounts,
				scheduling,
				resources,
				rdma,
				spcName,
				embeds,
			)
		} else {
			image := opts.Manifest.RuntimeImage()
			if image == "" {
				image = defaultRayJobImage
			}
			if err := validateRayLogOffloadStorageMounts(kind, false, storageMounts); err != nil {
				return nil, err
			}
			workloadYAML, err = buildRayJob(name, resourceName, ns, mfName, opts.Manifest.Compute.GPUs, opts.Manifest.Compute.Workers, image, opts.SmokePairs, pip, runtimeEnv, dataPVC, storageMounts, scheduling, gpuResourceMode, migProfile, resources, rdma, metricsOffload, spcName, embeds)
		}
	case WorkloadKindRayJobEval:
		if err := opts.Manifest.ValidateRayJobResourceName(); err != nil {
			return nil, err
		}
		// Eval-specific guards: cpu_workers must be set (the whole point of
		// rayjob-eval is the CPU fanout — without it, just use rayjob);
		// multi-node training shape (compute.workers > 1) is incoherent for
		// an eval (eval is always 1 GPU worker + N CPU workers, not N GPU workers).
		if opts.Manifest.Compute.GPUs <= 0 {
			return nil, fmt.Errorf("workload kind %q requires compute.gpus > 0 for the eval GPU worker; use @tau.train/--workload-kind=rayjob for CPU-only work", kind)
		}
		if opts.Manifest.Eval.CPUWorkers <= 0 {
			return nil, fmt.Errorf("workload kind %q requires eval.cpu_workers > 0 in the manifest (or --cpu-workers on the CLI); the eval RayJob shape is a system head, 1 GPU actor worker, and N CPU worker pods", kind)
		}
		if opts.Manifest.IsMultiNode() {
			return nil, fmt.Errorf("workload kind %q cannot be multi-node (compute.workers=%d); eval has one GPU actor worker plus CPU-worker fanout", kind, opts.Manifest.Compute.Workers)
		}
		image := opts.Manifest.RuntimeImage()
		if image == "" {
			image = defaultRayJobImage
		}
		if err := validateRayLogOffloadStorageMounts(kind, false, storageMounts); err != nil {
			return nil, err
		}
		workloadYAML, err = buildRayJobEval(name, resourceName, ns, mfName, opts.Manifest.Compute.GPUs, opts.Manifest.Eval.CPUWorkers, image, opts.SmokePairs, opts.UpstreamCheckpoint, pip, runtimeEnv, dataPVC, storageMounts, scheduling, gpuResourceMode, migProfile, resources, rdma, metricsOffload, spcName, embeds)
	default:
		return nil, fmt.Errorf("workload kind %q is not supported (use %q, %q, or %q)", kind, WorkloadKindJob, WorkloadKindRayJob, WorkloadKindRayJobEval)
	}
	if err != nil {
		return nil, err
	}
	if err := enforceRenderedSizeLimit(workloadYAML); err != nil {
		return nil, err
	}
	buf.Write(workloadYAML)
	if !bytes.HasSuffix(workloadYAML, []byte("\n")) {
		buf.WriteByte('\n')
	}

	if spcName != "" {
		spcYAML, err := kvspec.RenderSPC(spcName, ns, opts.KVSpec)
		if err != nil {
			return nil, fmt.Errorf("render SecretProviderClass: %w", err)
		}
		if spcYAML != nil {
			buf.WriteString("---\n")
			buf.Write(spcYAML)
			if !bytes.HasSuffix(spcYAML, []byte("\n")) {
				buf.WriteByte('\n')
			}
		}
	}

	return buf.Bytes(), nil
}

func validateRayLogOffloadStorageMounts(kind string, cpuOnly bool, mounts []StorageMount) error {
	label := rayLogOffloadWorkloadLabel(kind, cpuOnly)
	for i, mount := range mounts {
		if mount.Name == raylogoffload.VolumeName {
			return fmt.Errorf("storage.mounts[%d].name: %q is reserved by Tau for Ray driver log offload on %s workloads", i, mount.Name, label)
		}
		if rayLogOffloadPathConflict(mount.MountPath) {
			return fmt.Errorf("storage.mounts[%d].mountPath: %q is reserved by Tau for Ray driver log offload on %s workloads", i, mount.MountPath, label)
		}
	}
	return nil
}

func rayLogOffloadPathConflict(mountPath string) bool {
	cleanMount := filepath.Clean(mountPath)
	cleanReserved := filepath.Clean(raylogoffload.VolumeMountPath)
	return cleanMount == cleanReserved || strings.HasPrefix(cleanMount, cleanReserved+string(filepath.Separator))
}

func rayLogOffloadWorkloadLabel(kind string, cpuOnly bool) string {
	switch kind {
	case WorkloadKindRayJob:
		if cpuOnly {
			return "CPU RayJob"
		}
		return "RayJob"
	case WorkloadKindRayJobEval:
		return "RayJobEval"
	default:
		return kind
	}
}

func validateExtraScript(extra ExtraScript) error {
	if extra.Name == "" {
		return fmt.Errorf("extra script: destination name is required")
	}
	if !payloadFileNamePattern.MatchString(extra.Name) {
		return fmt.Errorf("extra script %q: destination must be a payload file name using only letters, digits, '.', '_' or '-'", extra.Name)
	}
	if extra.Name == "." || extra.Name == ".." {
		return fmt.Errorf("extra script %q: invalid destination", extra.Name)
	}
	return nil
}

func redactManifestSecretRefs(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("redact manifest secret refs: %w", err)
	}
	runtimeBlock, ok := doc["runtime"].(map[string]any)
	if !ok {
		return raw, nil
	}
	envList, ok := runtimeBlock["env"].([]any)
	if !ok {
		return raw, nil
	}
	changed := false
	for _, item := range envList {
		envMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		valueFrom, ok := envMap["valueFrom"].(map[string]any)
		if !ok {
			continue
		}
		secretKeyRef, ok := valueFrom["secretKeyRef"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := secretKeyRef["name"]; ok {
			secretKeyRef["name"] = "<redacted>"
			changed = true
		}
		if _, ok := secretKeyRef["key"]; ok {
			secretKeyRef["key"] = "<redacted>"
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("redact manifest secret refs: %w", err)
	}
	return out, nil
}

// schedulingMetadata carries the topology/Kueue decoration the workload
// templates need. Two parallel views are exposed so the Job and RayJob
// builders can share the resolution logic but render at different YAML
// indents:
//
//   - Pre-rendered string blocks (JobLabels, JobAnnotationsBlock,
//     PodMetadataBlock, NodeSelectorBlock, PodPriorityClassBlock) are
//     baked at the indents the existing Job template expects (4/2/4/6/6).
//     The Job builder substitutes them verbatim.
//
//   - Raw maps + scalars (Labels, Annotations, PodLabels, PodAnnotations,
//     NodeSelector, PodPriorityClassName) carry the same data
//     un-indented so the RayJob builder can render them at its own deeper
//     indents (rayClusterSpec.headGroupSpec.template.spec is two levels
//     below the Job's template.spec).
type schedulingMetadata struct {
	ProfileName           string
	LaneLabel             string
	QueueName             string
	JobLabels             string
	JobAnnotationsBlock   string
	PodMetadataBlock      string
	NodeSelectorBlock     string
	PodPriorityClassBlock string
	ServiceAccountName    string

	// Raw views — used by buildRayJob to re-render at deeper indents.
	Labels               map[string]string
	Annotations          map[string]string
	PodLabels            map[string]string
	PodAnnotations       map[string]string
	NodeSelector         map[string]string
	PodPriorityClassName string
}

func buildSchedulingMetadata(opts RenderOptions) (schedulingMetadata, error) {
	profileName := opts.ProfileName
	var p profile.Profile
	if opts.TopologyProfile != nil {
		p = *opts.TopologyProfile
		if profileName == "" {
			profileName = p.Name
		}
	} else {
		p = profile.Profile{Name: profileName}
	}
	if profileName == "" {
		profileName = "tau-finetune"
	}
	if p.Name == "" {
		p.Name = profileName
	}

	plan, err := topology.Build(p, opts.TopologyOptions)
	if err != nil {
		return schedulingMetadata{}, err
	}
	if err := topology.ValidateGPUClassNodeSelector(plan.Labels[workloadmeta.LabelGPUClass], opts.NodeSelector); err != nil {
		return schedulingMetadata{}, err
	}
	nodeSelector := mergeStringMaps(plan.NodeSelector, opts.NodeSelector)

	lane := opts.TopologyOptions.Lane
	if lane == "" {
		lane = "train"
	}

	labels := mergeStringMaps(opts.Labels, withoutGeneratedTauMetadata(plan.Labels))
	labels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue
	annotations := mergeStringMaps(opts.Annotations, withoutGeneratedTauMetadata(plan.Annotations))

	queue := plan.QueueName
	if queue == "" {
		queue = topology.SharedGPUQueueName
	}
	delete(labels, topology.QueueLabel)

	delete(labels, workloadmeta.LabelProfile)

	podLabels := map[string]string{}
	for k, v := range labels {
		if k != "" && v != "" {
			podLabels[k] = v
		}
	}
	podAnnotations := mergeStringMaps(
		workloadmeta.PodCorrelationAnnotations(opts.Annotations),
		withoutGeneratedTauMetadata(plan.Annotations),
	)

	return schedulingMetadata{
		ProfileName:           profileName,
		LaneLabel:             lane,
		QueueName:             queue,
		JobLabels:             renderMapEntries(labels, 4),
		JobAnnotationsBlock:   renderMapSection("annotations", annotations, 2),
		PodMetadataBlock:      renderPodMetadata(podLabels, podAnnotations, 4),
		NodeSelectorBlock:     renderMapSection("nodeSelector", nodeSelector, 6),
		PodPriorityClassBlock: renderPodPriorityClass(plan.PodPriorityClassName),

		Labels:               labels,
		Annotations:          annotations,
		PodLabels:            podLabels,
		PodAnnotations:       podAnnotations,
		NodeSelector:         nodeSelector,
		PodPriorityClassName: plan.PodPriorityClassName,
	}, nil
}

func mergeStringMaps(first, second map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range first {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	for k, v := range second {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func validKubernetesLabelValue(v string) bool {
	return len(v) <= maxResourceNameLen && qualifiedNameSegmentRE.MatchString(v)
}

func (opts RenderOptions) profileOptions() ProfileOptions {
	prof := opts.Profile
	prof.Mode = strings.TrimSpace(strings.ToLower(prof.Mode))
	prof.Rank = strings.TrimSpace(strings.ToLower(prof.Rank))
	if prof.Rank == "" {
		prof.Rank = "0"
	}
	return prof
}

func validateProfileOptions(prof ProfileOptions) error {
	if prof.Mode != "nsys" {
		return fmt.Errorf("--profiler %q: finetune RayJob profiling supports nsys only", prof.Mode)
	}
	if err := validateProfileRankSelector(prof.Rank); err != nil {
		return err
	}
	if prof.Warmup < 0 {
		return fmt.Errorf("--profile-warmup must be >= 0")
	}
	if prof.Duration <= 0 {
		return fmt.Errorf("--profile-duration must be > 0 for RayJob actor profiling")
	}
	return nil
}

func validateProfileRankSelector(selector string) error {
	if selector == "all" {
		return nil
	}
	parts := strings.Split(selector, ",")
	for _, part := range parts {
		rankText := strings.TrimSpace(part)
		if rankText == "" {
			return fmt.Errorf("--profile-rank %q: expected a non-negative integer, comma-separated ranks, or all", selector)
		}
		rank, err := strconv.Atoi(rankText)
		if err != nil || rank < 0 {
			return fmt.Errorf("--profile-rank %q: expected a non-negative integer, comma-separated ranks, or all", selector)
		}
	}
	return nil
}

func buildRayJobProfileContract(m *Manifest, namespace, dataPVC string, prof ProfileOptions) ([]envspec.Var, map[string]string) {
	outDir := storage.DurableFinetuneDir(m.Name) + "/profile"
	worldSize := strconv.Itoa(m.Compute.Workers * m.Compute.GPUs)
	env := map[string]string{
		"TAU_PROFILE_MODE":       prof.Mode,
		"TAU_PROFILE_TOOL":       "nsys",
		"TAU_PROFILE_RANK":       prof.Rank,
		"TAU_PROFILE_OUT_DIR":    outDir,
		"TAU_PROFILE_RUN_ID":     m.Name,
		"TAU_PROFILE_NAMESPACE":  namespace,
		"TAU_PROFILE_EXT":        "nsys-rep",
		"TAU_PROFILE_WORLD_SIZE": worldSize,
		"TAU_PROFILE_WARMUP_SEC": profileDurationSeconds(prof.Warmup),
		"TAU_PROFILE_ACTIVE_SEC": profileDurationSeconds(prof.Duration),
	}
	annotations := map[string]string{
		workloadmeta.AnnotationProfilerMode:      prof.Mode,
		workloadmeta.AnnotationProfilerPath:      outDir,
		workloadmeta.AnnotationProfilerPVC:       dataPVC,
		workloadmeta.AnnotationProfilerRank:      prof.Rank,
		workloadmeta.AnnotationProfilerWorldSize: worldSize,
		workloadmeta.AnnotationProfilerDuration:  prof.Duration.String(),
	}
	if prof.Rank == "all" || strings.Contains(prof.Rank, ",") {
		env["TAU_PROFILE_OUT_PATTERN"] = outDir + "/rank-<rank>.nsys-rep"
	} else {
		env["TAU_PROFILE_OUT"] = outDir + "/rank-" + prof.Rank + ".nsys-rep"
	}
	if prof.Warmup > 0 {
		annotations[workloadmeta.AnnotationProfilerWarmup] = prof.Warmup.String()
	}
	return envspec.FromMap(env), annotations
}

func profileDurationSeconds(d time.Duration) string {
	sec := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(sec, 10)
}

func withRayOverwriteContainerCommand(s schedulingMetadata) schedulingMetadata {
	annotations := mergeStringMaps(s.Annotations, map[string]string{rayOverwriteCmdKey: "true"})
	s.Annotations = annotations
	s.JobAnnotationsBlock = renderMapSection("annotations", annotations, 2)
	return s
}

func withProfileAnnotations(s schedulingMetadata, values map[string]string) schedulingMetadata {
	annotations := mergeStringMaps(s.Annotations, values)
	s.Annotations = annotations
	s.JobAnnotationsBlock = renderMapSection("annotations", annotations, 2)
	return s
}

func renderPodMetadata(labels, annotations map[string]string, indent int) string {
	if len(labels) == 0 && len(annotations) == 0 {
		return ""
	}
	var b strings.Builder
	spaces := strings.Repeat(" ", indent)
	fmt.Fprintf(&b, "%smetadata:\n", spaces)
	if len(labels) > 0 {
		fmt.Fprintf(&b, "%s  labels:\n", spaces)
		b.WriteString(renderMapEntries(labels, indent+4))
	}
	if block := renderMapSection("annotations", annotations, indent+2); block != "" {
		b.WriteString(block)
	}
	return b.String()
}

func renderPodPriorityClass(name string) string {
	return renderPodPriorityClassAt(name, 6)
}

func renderPodPriorityClassAt(name string, indent int) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("%spriorityClassName: %s\n", strings.Repeat(" ", indent), quoteYAMLString(name))
}

func renderServiceAccountName(name string, indent int) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("%sserviceAccountName: %s\n", strings.Repeat(" ", indent), name)
}

func renderMapSection(name string, values map[string]string, indent int) string {
	if len(values) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s:\n", strings.Repeat(" ", indent), name)
	b.WriteString(renderMapEntries(values, indent+2))
	return b.String()
}

func renderMapEntries(values map[string]string, indent int) string {
	keys := sortedKeys(values)
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	spaces := strings.Repeat(" ", indent)
	for _, k := range keys {
		v := values[k]
		if k == "" || v == "" {
			continue
		}
		fmt.Fprintf(&b, "%s%s: %s\n", spaces, k, quoteYAMLString(v))
	}
	return b.String()
}

func withoutGeneratedTauMetadata(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range values {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isGeneratedTauMetadataKey(key string) bool {
	return strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelGPUClass
}

func renderStorageMounts(mounts []StorageMount, indent int) string {
	if len(mounts) == 0 {
		return ""
	}
	var b strings.Builder
	spaces := strings.Repeat(" ", indent)
	child := strings.Repeat(" ", indent+2)
	for _, mount := range mounts {
		fmt.Fprintf(&b, "%s- name: %s\n", spaces, quoteYAMLString(mount.Name))
		fmt.Fprintf(&b, "%smountPath: %s\n", child, quoteYAMLString(mount.MountPath))
		if mount.ReadOnly {
			fmt.Fprintf(&b, "%sreadOnly: true\n", child)
		}
	}
	return b.String()
}

func renderStorageVolumes(mounts []StorageMount, indent int) string {
	if len(mounts) == 0 {
		return ""
	}
	var b strings.Builder
	spaces := strings.Repeat(" ", indent)
	child := strings.Repeat(" ", indent+2)
	grandchild := strings.Repeat(" ", indent+4)
	for _, mount := range mounts {
		fmt.Fprintf(&b, "%s- name: %s\n", spaces, quoteYAMLString(mount.Name))
		fmt.Fprintf(&b, "%spersistentVolumeClaim:\n", child)
		fmt.Fprintf(&b, "%sclaimName: %s\n", grandchild, quoteYAMLString(mount.PVC))
	}
	return b.String()
}

func quoteYAMLString(v string) string {
	return strconv.Quote(v)
}

// payloadEmbeds carries the two encoded self-contained payloads (see Design
// A in issue #869 PR2) that get embedded in a workload's head/single pod:
// the script bundle and the redacted manifest copy. Each field pair
// (Encoded, Digest) is the return value of a payload.Encode call.
type payloadEmbeds struct {
	ScriptEncoded   string
	ScriptDigest    string
	ManifestEncoded string
	ManifestDigest  string
	// CheckpointArtifact is the manifest's artifacts.checkpoint value. It
	// travels with the payload embeds because, like them, it is baked into
	// the rendered entrypoint rather than into the pod spec.
	CheckpointArtifact string
}

// payloadInitContainersYAML renders both initContainers (script payload,
// manifest payload) that decode/verify/write the two embedded payloads into
// their own emptyDir volumes ("script", "manifest") before the main
// container starts. image is the workload's own image — no additional image
// is pulled since every Tau-managed image ships Python 3, which is all
// payload.InitContainerScript needs. indent is the indentation, in spaces,
// of the surrounding pod spec's `containers:`/`volumes:` keys; the new
// `initContainers:` key (and everything under it) is rendered at that same
// indent so it drops in immediately above `containers:` unchanged.
//
// Used for every head/single pod (managed-workflow-job.yaml.tmpl, and the head pod
// of the three RayJob templates). Every RayJob worker pod (Ray Train GPU/
// CPU workers, and eval CPU workers alike) instead uses
// workerScriptPayloadInitContainersYAML below — a script-only variant (see
// the package doc on Render's Design A comment for why no RayJob worker
// pod ever needs the manifest payload).
func payloadInitContainersYAML(image string, embeds payloadEmbeds, indent int) string {
	return initContainersBlock(indent, payloadInitContainerItemsYAML(image, embeds, indent, true))
}

// workerScriptPayloadInitContainersYAML renders only the script payload's
// initContainer (never the manifest payload's) at the given indent, for use
// on any RayJob worker pod template — Ray Train GPU/CPU workers and eval
// CPU workers alike. See the package doc on Render's Design A comment: Ray
// Train worker actors re-read /script/<trainer-filename> from local disk
// directly (tau-py's TorchTrainer path), while eval CPU workers need the
// full script bundle on local disk because Ray's runtime_env working_dir
// mechanism only ships the single user-module file to them, never any
// --extra-script files. Neither worker kind ever reads /manifest, which is
// loaded exactly once on the head before any worker is spawned.
func workerScriptPayloadInitContainersYAML(image string, embeds payloadEmbeds, indent int) string {
	var b strings.Builder
	b.WriteString(payloadInitContainerItemsYAML(image, embeds, indent, false))
	return initContainersBlock(indent, b.String())
}

func initContainersBlock(indent int, items string) string {
	spaces := strings.Repeat(" ", indent)
	var b strings.Builder
	fmt.Fprintf(&b, "%sinitContainers:\n", spaces)
	b.WriteString(items)
	return b.String()
}

func payloadInitContainerItemsYAML(image string, embeds payloadEmbeds, indent int, includeManifest bool) string {
	var b strings.Builder
	b.WriteString(payloadInitContainerYAML(image, scriptPayloadInitContainerName, embeds.ScriptEncoded, embeds.ScriptDigest, scriptPayloadTargetDir, "script", indent))
	if includeManifest {
		b.WriteString(payloadInitContainerYAML(image, manifestPayloadInitContainerName, embeds.ManifestEncoded, embeds.ManifestDigest, manifestPayloadTargetDir, "manifest", indent))
	}
	return b.String()
}

// payloadInitContainerYAML renders a single initContainer entry that decodes
// one payload.Encode envelope into volumeName's emptyDir at targetDir. indent
// is the indentation of the parent `initContainers:` key (matching
// payloadInitContainersYAML's contract).
func payloadInitContainerYAML(image, name, encoded, digest, targetDir, volumeName string, indent int) string {
	item := strings.Repeat(" ", indent+2)     // "- name: ..." line
	field := strings.Repeat(" ", indent+4)    // sibling keys under the list item
	listItem := strings.Repeat(" ", indent+6) // entries within command:/env:/volumeMounts:/resources sub-lists
	cont := strings.Repeat(" ", indent+8)     // continuation lines (env value:, literal block content)

	var b strings.Builder
	fmt.Fprintf(&b, "%s- name: %s\n", item, name)
	fmt.Fprintf(&b, "%simage: %s\n", field, quoteYAMLString(image))
	fmt.Fprintf(&b, "%scommand:\n", field)
	fmt.Fprintf(&b, "%s- python3\n", listItem)
	fmt.Fprintf(&b, "%s- -c\n", listItem)
	fmt.Fprintf(&b, "%s- |\n", listItem)
	b.WriteString(yamlLiteralBlock(payload.InitContainerScript, indent+8))
	fmt.Fprintf(&b, "%senv:\n", field)
	fmt.Fprintf(&b, "%s- name: %s\n", listItem, payload.EnvB64)
	fmt.Fprintf(&b, "%svalue: %s\n", cont, quoteYAMLString(encoded))
	fmt.Fprintf(&b, "%s- name: %s\n", listItem, payload.EnvDigest)
	fmt.Fprintf(&b, "%svalue: %s\n", cont, quoteYAMLString(digest))
	fmt.Fprintf(&b, "%s- name: %s\n", listItem, payload.EnvTargetDir)
	fmt.Fprintf(&b, "%svalue: %s\n", cont, quoteYAMLString(targetDir))
	fmt.Fprintf(&b, "%svolumeMounts:\n", field)
	fmt.Fprintf(&b, "%s- name: %s\n", listItem, volumeName)
	fmt.Fprintf(&b, "%smountPath: %s\n", cont, quoteYAMLString(targetDir))
	fmt.Fprintf(&b, "%sresources:\n", field)
	fmt.Fprintf(&b, "%srequests:\n", listItem)
	fmt.Fprintf(&b, "%scpu: \"10m\"\n", cont)
	fmt.Fprintf(&b, "%smemory: \"32Mi\"\n", cont)
	fmt.Fprintf(&b, "%slimits:\n", listItem)
	fmt.Fprintf(&b, "%scpu: \"250m\"\n", cont)
	fmt.Fprintf(&b, "%smemory: \"128Mi\"\n", cont)
	return b.String()
}

// yamlLiteralBlock re-indents text for embedding as a YAML literal block
// scalar's content (following a "- |" indicator whose own line is indented
// by indent-2). Every non-blank line must be indented by at least the
// indicator's content indent for the block scalar to parse correctly; blank
// lines are emitted perfectly empty (no trailing whitespace), since YAML
// treats them as an empty content line regardless of indentation and
// trailing whitespace would fail lint/formatting checks on the rendered
// output.
func yamlLiteralBlock(text string, indent int) string {
	prefix := strings.Repeat(" ", indent)
	lines := strings.Split(text, "\n")
	// A trailing "\n" in text (InitContainerScript ends with one) produces
	// a spurious empty trailing element from strings.Split; drop it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// enforceRenderedSizeLimit re-parses the rendered workload YAML and
// re-serializes it as JSON to measure the actual size the API server (and
// MultiKueue's admission mirroring) will see for this object — not an
// estimate derived from the payloads' decoded byte counts, which would miss
// YAML/JSON framing overhead, labels, annotations, and every other field the
// workload carries. This is enforced in addition to, not instead of,
// payload.MaxDecodedBytes (which independently bounds each payload's
// decoded size before this check ever runs). It only ever sees the workload
// document itself — an optional JobSecret or SecretProviderClass document is
// rendered and measured separately, so neither can hide an oversized
// workload from this check, nor can this check flag an unrelated document.
func enforceRenderedSizeLimit(workloadYAML []byte) error {
	var doc map[string]any
	if err := yaml.Unmarshal(workloadYAML, &doc); err != nil {
		return fmt.Errorf("render: rendered workload is not valid YAML: %w", err)
	}
	jsonBytes, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("render: measuring rendered workload size: %w", err)
	}
	if len(jsonBytes) > maxRenderedWorkloadBytes {
		return fmt.Errorf("render: rendered workload is %d bytes as JSON, exceeds the %d byte limit (reduce script/manifest/extra-script size or split the workload)", len(jsonBytes), maxRenderedWorkloadBytes)
	}
	return nil
}

func writeSecret(w *bytes.Buffer, namespace string, secret JobSecret, redact bool) error {
	if secret.Name == "" {
		return fmt.Errorf("job secret: name is required")
	}
	if len(secret.StringData) == 0 {
		return fmt.Errorf("job secret %q: stringData is required", secret.Name)
	}
	fmt.Fprintln(w, "apiVersion: v1")
	fmt.Fprintln(w, "kind: Secret")
	fmt.Fprintln(w, "metadata:")
	fmt.Fprintf(w, "  name: %s\n", secret.Name)
	fmt.Fprintf(w, "  namespace: %s\n", namespace)
	fmt.Fprintln(w, "  labels:")
	fmt.Fprintf(w, "    %s: %s\n", workloadmeta.LabelManagedBy, workloadmeta.ManagedByValue)
	if secret.OwnerName != "" {
		fmt.Fprintf(w, "  annotations:\n")
		fmt.Fprintf(w, "    %s: %s\n", workloadmeta.AnnotationOwnerName, secret.OwnerName)
		fmt.Fprintf(w, "    %s: %s\n", workloadmeta.AnnotationOwnerKind, secret.OwnerKind)
	}
	fmt.Fprintln(w, "type: Opaque")
	fmt.Fprintln(w, "stringData:")
	for _, k := range sortedKeys(secret.StringData) {
		value := secret.StringData[k]
		if redact {
			value = "<redacted>"
		}
		fmt.Fprintf(w, "  %s: %s\n", k, quoteYAMLString(value))
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// stable order so test golden files don't churn.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

type containerResourceSizing struct {
	CPUs        int
	CPULimit    int
	Memory      string
	MemoryLimit string
}

type workloadResourceSizing struct {
	Control containerResourceSizing
	Head    containerResourceSizing
	Worker  containerResourceSizing
}

func resourceSizingFor(m *Manifest, kind string) workloadResourceSizing {
	control := resourcesWithDefaults(0, 0, "", "", 2, 4, "8Gi", "16Gi")
	switch {
	case m.IsCPUOnly():
		head := resourcesWithDefaults(
			m.Compute.CPUs, m.Compute.CPULimit,
			m.Compute.Memory, m.Compute.MemoryLimit,
			1, 1, "2Gi", "2Gi",
		)
		worker := resourcesWithDefaults(
			m.Compute.WorkerCPUs, m.Compute.WorkerCPULimit,
			m.Compute.WorkerMemory, m.Compute.WorkerMemoryLimit,
			head.CPUs, head.CPULimit, head.Memory, head.MemoryLimit,
		)
		return workloadResourceSizing{Control: control, Head: head, Worker: worker}
	case kind == WorkloadKindRayJobEval:
		head := resourcesWithDefaults(
			m.Compute.CPUs, m.Compute.CPULimit,
			m.Compute.Memory, m.Compute.MemoryLimit,
			4, 8, "32Gi", "64Gi",
		)
		worker := resourcesWithDefaults(
			m.Compute.WorkerCPUs, m.Compute.WorkerCPULimit,
			m.Compute.WorkerMemory, m.Compute.WorkerMemoryLimit,
			1, 2, "4Gi", "8Gi",
		)
		return workloadResourceSizing{Control: control, Head: head, Worker: worker}
	default:
		head := resourcesWithDefaults(
			m.Compute.CPUs, m.Compute.CPULimit,
			m.Compute.Memory, m.Compute.MemoryLimit,
			8, 16, "64Gi", "128Gi",
		)
		workerDefault := containerResourceSizing{CPUs: 8, CPULimit: 16, Memory: "64Gi", MemoryLimit: "128Gi"}
		if hasHeadResourceOverride(m) {
			workerDefault = head
		}
		worker := resourcesWithDefaults(
			m.Compute.WorkerCPUs, m.Compute.WorkerCPULimit,
			m.Compute.WorkerMemory, m.Compute.WorkerMemoryLimit,
			workerDefault.CPUs, workerDefault.CPULimit, workerDefault.Memory, workerDefault.MemoryLimit,
		)
		return workloadResourceSizing{Control: control, Head: head, Worker: worker}
	}
}

func resourcesWithDefaults(cpuRequest, cpuLimit int, memoryRequest, memoryLimit string, defaultCPURequest, defaultCPULimit int, defaultMemoryRequest, defaultMemoryLimit string) containerResourceSizing {
	out := containerResourceSizing{
		CPUs:        defaultCPURequest,
		CPULimit:    defaultCPULimit,
		Memory:      defaultMemoryRequest,
		MemoryLimit: defaultMemoryLimit,
	}
	if cpuRequest > 0 {
		out.CPUs = cpuRequest
	}
	if cpuLimit > 0 {
		out.CPULimit = cpuLimit
	} else if cpuRequest > 0 {
		out.CPULimit = out.CPUs
	}
	if strings.TrimSpace(memoryRequest) != "" {
		out.Memory = strings.TrimSpace(memoryRequest)
	}
	if strings.TrimSpace(memoryLimit) != "" {
		out.MemoryLimit = strings.TrimSpace(memoryLimit)
	} else if strings.TrimSpace(memoryRequest) != "" {
		out.MemoryLimit = out.Memory
	}
	return out
}

func hasHeadResourceOverride(m *Manifest) bool {
	return m.Compute.CPUs > 0 ||
		m.Compute.CPULimit > 0 ||
		strings.TrimSpace(m.Compute.Memory) != "" ||
		strings.TrimSpace(m.Compute.MemoryLimit) != ""
}

// buildJob renders the embedded Job template via text/template. The template
// uses shell-style ${VAR} markers (so the same file works with envsubst from
// the bash path); we translate to {{.Var}} before parsing.
func buildJob(name, resourceName, namespace, manifestName string, gpus, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, spcName string, embeds payloadEmbeds) ([]byte, error) {
	return renderWorkloadTemplate(workloadTemplateInput{
		Asset:              "managed-workflow-job.yaml.tmpl",
		CheckpointArtifact: embeds.CheckpointArtifact,
		Name:               name,
		ResourceName:       resourceName,
		Namespace:          namespace,
		ManifestName:       manifestName,
		GPUs:               gpus,
		GPUResourceMode:    gpuResourceMode,
		MIGProfile:         migProfile,
		Resources:          resources,
		Image:              defaultRayJobImage,
		Workers:            1,
		SmokePairs:         smokePairs,
		RuntimePip:         pip,
		RuntimeEnv:         runtimeEnv,
		DataPVC:            dataPVC,
		StorageMounts:      storageMounts,
		Scheduling:         scheduling,
		Blocks:             jobBlocks(scheduling, runtimeEnv, storageMounts, spcName, embeds, defaultRayJobImage),
	})
}

// buildRayJob renders the embedded RayJob template. Same ${VAR} translation
// as the Job path; the only differences are (1) the template asset and (2)
// the indent depth at which the per-pod blocks (PodMetadata, NodeSelector,
// PodPriorityClass) land — the RayJob nests pod template under
// rayClusterSpec.headGroupSpec.template, two levels deeper than Job's
// spec.template. We re-render those blocks at the deeper indents instead of
// reusing the Job-indented strings.
//
// `workers` is the execution-worker count. A separate control-only head is
// always rendered on the system node pool.
func buildRayJob(name, resourceName, namespace, manifestName string, gpus, workers int, image string, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, rdma runtimeRDMAConfig, metricsOffload metricsOffloadRuntime, spcName string, embeds payloadEmbeds) ([]byte, error) {
	blocks, err := rayJobBlocks(scheduling, runtimeEnv, storageMounts, spcName, embeds, image, metricsOffload)
	if err != nil {
		return nil, err
	}
	return renderWorkloadTemplate(workloadTemplateInput{
		Asset:              "managed-workflow-rayjob.yaml.tmpl",
		CheckpointArtifact: embeds.CheckpointArtifact,
		Name:               name,
		ResourceName:       resourceName,
		Namespace:          namespace,
		ManifestName:       manifestName,
		GPUs:               gpus,
		GPUResourceMode:    gpuResourceMode,
		MIGProfile:         migProfile,
		Resources:          resources,
		Image:              image,
		Workers:            workers,
		SmokePairs:         smokePairs,
		RuntimePip:         pip,
		RuntimeEnv:         runtimeEnv,
		DataPVC:            dataPVC,
		StorageMounts:      storageMounts,
		Scheduling:         scheduling,
		RDMA:               rdma,
		Blocks:             blocks,
		MetricsOffload:     metricsOffload,
	})
}

// buildRayJobCPU renders a CPU-only RayJob with a system head and `workers`
// dedicated CPU execution workers.
func buildRayJobCPU(name, resourceName, namespace, manifestName string, workers int, image string, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, resources workloadResourceSizing, rdma runtimeRDMAConfig, spcName string, embeds payloadEmbeds) ([]byte, error) {
	blocks, err := rayJobBlocks(scheduling, runtimeEnv, storageMounts, spcName, embeds, image, metricsOffloadRuntime{})
	if err != nil {
		return nil, err
	}
	return renderWorkloadTemplate(workloadTemplateInput{
		Asset:              "managed-workflow-rayjob-cpu.yaml.tmpl",
		CheckpointArtifact: embeds.CheckpointArtifact,
		Name:               name,
		ResourceName:       resourceName,
		Namespace:          namespace,
		ManifestName:       manifestName,
		Resources:          resources,
		Image:              image,
		Workers:            workers,
		SmokePairs:         smokePairs,
		RuntimePip:         pip,
		RuntimeEnv:         runtimeEnv,
		DataPVC:            dataPVC,
		StorageMounts:      storageMounts,
		Scheduling:         scheduling,
		RDMA:               rdma,
		Blocks:             blocks,
	})
}

// buildRayJobEval renders the eval RayJob template (system control head,
// 1 GPU actor worker, and N CPU fanout workers). Used
// by the tau-py @tau.eval decorator to spin up Ray actor + ray.remote
// fanout patterns.
//
// `cpuWorkers` is the CPU worker pod count (must be ≥ 1; the dispatcher
// in Render() rejects 0 with a clear error). `upstreamCheckpoint` is the
// cluster-side path to an upstream training run's checkpoint; rendered
// into the eval pod's TAU_UPSTREAM_CHECKPOINT env var (empty string if
// the eval doesn't depend on a train job, in which case the cluster
// wrapper exposes ctx.upstream_checkpoint as None).
func buildRayJobEval(name, resourceName, namespace, manifestName string, gpus, cpuWorkers int, image string, smokePairs int, upstreamCheckpoint string, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, rdma runtimeRDMAConfig, metricsOffload metricsOffloadRuntime, spcName string, embeds payloadEmbeds) ([]byte, error) {
	blocks, err := rayJobBlocks(scheduling, runtimeEnv, storageMounts, spcName, embeds, image, metricsOffload)
	if err != nil {
		return nil, err
	}
	return renderWorkloadTemplate(workloadTemplateInput{
		Asset:              "managed-workflow-rayjob-eval.yaml.tmpl",
		Name:               name,
		ResourceName:       resourceName,
		Namespace:          namespace,
		ManifestName:       manifestName,
		GPUs:               gpus,
		GPUResourceMode:    gpuResourceMode,
		MIGProfile:         migProfile,
		Resources:          resources,
		Image:              image,
		Workers:            1, // one dedicated GPU worker plus CPU eval workers
		CPUWorkers:         cpuWorkers,
		SmokePairs:         smokePairs,
		UpstreamCheckpoint: upstreamCheckpoint,
		RuntimePip:         pip,
		RuntimeEnv:         runtimeEnv,
		DataPVC:            dataPVC,
		StorageMounts:      storageMounts,
		Scheduling:         scheduling,
		RDMA:               rdma,
		Blocks:             blocks,
		MetricsOffload:     metricsOffload,
	})
}

// workloadBlocks carries the indent-rendered per-template substitution
// values that differ between Job and RayJob (everything else is shared).
//
// Worker* fields are RayJob-only and re-render the head's per-pod blocks
// at the deeper indent the workerGroupSpec needs (template lives one
// level deeper under the worker group than under headGroupSpec).
type workloadBlocks struct {
	JobLabels                      string
	JobAnnotationsBlock            string
	PodMetadataBlock               string
	NodeSelectorBlock              string
	PodPriorityClassBlock          string
	ServiceAccountBlock            string
	WorkerPodMetadataBlock         string
	WorkerNodeSelectorBlock        string
	WorkerPodPriorityClassBlock    string
	WorkerServiceAccountBlock      string
	CPUWorkerPodMetadataBlock      string
	CPUWorkerNodeSelectorBlock     string
	CPUWorkerPodPriorityClassBlock string
	CPUWorkerServiceAccountBlock   string
	RuntimeEnvJob                  string
	RuntimeEnvHead                 string
	RuntimeEnvWorker               string
	ExtraVolumeMountsJob           string
	ExtraVolumeMountsHead          string
	ExtraVolumeMountsWorker        string
	ExtraVolumesJob                string
	ExtraVolumesHead               string
	ExtraVolumesWorker             string
	DriverLogOffloadSidecar        string
	DriverLogCompletionSetup       string
	// PayloadInitContainers is the rendered `initContainers:` block (script
	// + manifest payload decode/verify) for the head/single pod only.
	// WorkerPayloadInitContainers is the script-only variant (see
	// workerScriptPayloadInitContainersYAML), always populated for RayJob
	// callers — every RayJob worker pod template (Ray Train GPU/CPU
	// workers and eval GPU/CPU workers alike) substitutes it in.
	PayloadInitContainers       string
	WorkerPayloadInitContainers string
}

func appendYAMLBlock(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "\n" + extra
}

func renderYAMLBlock(value any, indent int) (string, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return "", err
	}
	return yamlLiteralBlock(string(data), indent), nil
}

func jobBlocks(s schedulingMetadata, runtimeEnv []envspec.Var, mounts []StorageMount, spcName string, embeds payloadEmbeds, image string) workloadBlocks {
	b := workloadBlocks{
		JobLabels:             s.JobLabels,
		JobAnnotationsBlock:   s.JobAnnotationsBlock,
		PodMetadataBlock:      s.PodMetadataBlock,
		NodeSelectorBlock:     s.NodeSelectorBlock,
		PodPriorityClassBlock: s.PodPriorityClassBlock,
		ServiceAccountBlock:   renderServiceAccountName(s.ServiceAccountName, 6),
		RuntimeEnvJob:         envspec.RenderYAML(runtimeEnv, 12),
		ExtraVolumeMountsJob:  renderStorageMounts(mounts, 12),
		ExtraVolumesJob:       renderStorageVolumes(mounts, 8),
		// indent 6 matches managed-workflow-job.yaml.tmpl's `containers:`/`volumes:`
		// key indent under spec.template.spec — the Job is single-pod, so
		// this is the only PayloadInitContainers value the Job template
		// needs (no separate worker block exists).
		PayloadInitContainers: payloadInitContainersYAML(image, embeds, 6),
	}
	if spcName != "" {
		b.ExtraVolumeMountsJob = appendYAMLBlock(b.ExtraVolumeMountsJob, kvspec.VolumeMountYAML(12))
		b.ExtraVolumesJob = appendYAMLBlock(b.ExtraVolumesJob, kvspec.VolumeYAML(spcName, 8))
	}
	return b
}

// rayJobBlocks re-renders the per-pod blocks at the RayJob's deeper
// indents:
//   - RayJob metadata.labels live at indent 4 (same as Job) — reuse.
//   - RayJob metadata.annotations live at indent 2 (same as Job) — reuse.
//   - headGroupSpec.template.metadata starts at indent 8 (Job: 4).
//   - headGroupSpec.template.spec.nodeSelector starts at indent 10 (Job: 6).
//   - headGroupSpec.template.spec.priorityClassName indents 10 (Job: 6).
//   - workerGroupSpecs[].template.metadata starts at indent 10 (head: 8).
//   - workerGroupSpecs[].template.spec.nodeSelector starts at indent 12.
//   - workerGroupSpecs[].template.spec.priorityClassName indents 12.
//
// WorkerPayloadInitContainers is always populated (script-only, never
// manifest) — every RayJob worker pod template (the two Ray Train
// templates' GPU/CPU workers, and rayjob-eval's CPU workers alike) needs
// the full script bundle on local disk; see the package doc on Render's
// Design A comment for why this now applies uniformly to all three RayJob
// templates rather than varying per template.
func rayJobBlocks(s schedulingMetadata, runtimeEnv []envspec.Var, mounts []StorageMount, spcName string, embeds payloadEmbeds, image string, metricsOffload metricsOffloadRuntime) (workloadBlocks, error) {
	prepareRayTmpBlock, err := renderYAMLBlock([]any{raylogoffload.PrepareInitContainer(image)}, 12)
	if err != nil {
		return workloadBlocks{}, fmt.Errorf("render ray tmp init container: %w", err)
	}
	headMountBlock, err := renderYAMLBlock([]any{raylogoffload.VolumeMount(false)}, 16)
	if err != nil {
		return workloadBlocks{}, fmt.Errorf("render ray head log volume mount: %w", err)
	}
	headVolumeBlock, err := renderYAMLBlock([]any{raylogoffload.Volume()}, 12)
	if err != nil {
		return workloadBlocks{}, fmt.Errorf("render ray head log volume: %w", err)
	}
	driverSidecarBlock, err := renderYAMLBlock([]any{raylogoffload.SidecarContainer(image)}, 12)
	if err != nil {
		return workloadBlocks{}, fmt.Errorf("render ray log offload sidecar: %w", err)
	}
	metricsOffloadInitBlock := ""
	if metricsOffload.Enabled {
		metricsOffloadInitBlock, err = renderYAMLBlock([]any{metricsOffloadSentinelInitContainer(image, metricsOffload)}, 12)
		if err != nil {
			return workloadBlocks{}, fmt.Errorf("render metrics offload sentinel init container: %w", err)
		}
	}
	headAnnotations := topology.WithoutKueueTopologyAnnotations(s.PodAnnotations)
	systemAffinityBlock, err := renderYAMLBlock(map[string]any{"affinity": topology.SystemNodeAffinity()}, 10)
	if err != nil {
		return workloadBlocks{}, fmt.Errorf("render system node affinity: %w", err)
	}
	b := workloadBlocks{
		JobLabels:                      s.JobLabels,
		JobAnnotationsBlock:            s.JobAnnotationsBlock,
		PodMetadataBlock:               renderPodMetadata(s.PodLabels, raylogoffload.HeadPodAnnotations(headAnnotations), 8),
		NodeSelectorBlock:              systemAffinityBlock,
		PodPriorityClassBlock:          renderPodPriorityClassAt(s.PodPriorityClassName, 10),
		ServiceAccountBlock:            renderServiceAccountName(s.ServiceAccountName, 10),
		WorkerPodMetadataBlock:         renderPodMetadata(s.PodLabels, s.PodAnnotations, 10),
		WorkerNodeSelectorBlock:        renderMapSection("nodeSelector", s.NodeSelector, 12),
		WorkerPodPriorityClassBlock:    renderPodPriorityClassAt(s.PodPriorityClassName, 12),
		WorkerServiceAccountBlock:      renderServiceAccountName(s.ServiceAccountName, 12),
		CPUWorkerPodMetadataBlock:      renderPodMetadata(s.PodLabels, headAnnotations, 10),
		CPUWorkerNodeSelectorBlock:     "",
		CPUWorkerPodPriorityClassBlock: renderPodPriorityClassAt(s.PodPriorityClassName, 12),
		CPUWorkerServiceAccountBlock:   renderServiceAccountName(s.ServiceAccountName, 12),
		RuntimeEnvHead:                 envspec.RenderYAML(runtimeEnv, 16),
		RuntimeEnvWorker:               envspec.RenderYAML(runtimeEnv, 18),
		ExtraVolumeMountsHead:          appendYAMLBlock(renderStorageMounts(mounts, 16), headMountBlock),
		ExtraVolumeMountsWorker:        renderStorageMounts(mounts, 18),
		ExtraVolumesHead:               appendYAMLBlock(renderStorageVolumes(mounts, 12), headVolumeBlock),
		ExtraVolumesWorker:             renderStorageVolumes(mounts, 14),
		DriverLogOffloadSidecar:        driverSidecarBlock,
		DriverLogCompletionSetup:       yamlLiteralBlock(raylogoffload.CompletionSetupScript, 4),
		// indent 10 matches all three RayJob templates'
		// headGroupSpec.template.spec.containers/volumes key indent.
		PayloadInitContainers: initContainersBlock(10, prepareRayTmpBlock+payloadInitContainerItemsYAML(image, embeds, 10, true)+metricsOffloadInitBlock),
		// indent 12 matches workerGroupSpecs[].template.spec's
		// containers/volumes key indent — confirmed identical across all
		// three RayJob templates (managed-workflow-rayjob.yaml.tmpl,
		// managed-workflow-rayjob-cpu.yaml.tmpl, managed-workflow-rayjob-eval.yaml.tmpl).
		WorkerPayloadInitContainers: workerScriptPayloadInitContainersYAML(image, embeds, 12),
	}
	if spcName != "" {
		b.ExtraVolumeMountsHead = appendYAMLBlock(b.ExtraVolumeMountsHead, kvspec.VolumeMountYAML(16))
		b.ExtraVolumeMountsWorker = appendYAMLBlock(b.ExtraVolumeMountsWorker, kvspec.VolumeMountYAML(18))
		b.ExtraVolumesHead = appendYAMLBlock(b.ExtraVolumesHead, kvspec.VolumeYAML(spcName, 12))
		b.ExtraVolumesWorker = appendYAMLBlock(b.ExtraVolumesWorker, kvspec.VolumeYAML(spcName, 14))
	}
	return b, nil
}

func metricsOffloadSentinelInitContainer(image string, runtime metricsOffloadRuntime) map[string]any {
	command := fmt.Sprintf("rm -f %s %s", shellQuote(runtime.CompletionFile), shellQuote(runtime.DoneFile))
	return map[string]any{
		"name":    "prepare-metrics-offload",
		"image":   image,
		"command": []any{"/bin/sh", "-c", command},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"cpu": "50m", "memory": "64Mi"},
		},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
			"readOnlyRootFilesystem":   true,
			"runAsNonRoot":             true,
			"runAsUser":                int64(65532),
			"runAsGroup":               int64(65532),
			"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
		},
		"volumeMounts": []any{
			map[string]any{"name": "data", "mountPath": storage.DurableRoot},
		},
	}
}

// workloadTemplateInput aggregates the args renderWorkloadTemplate needs.
// Using a struct avoids the 9-positional-args footgun the previous
// signature had become — easy to mis-order workers/cpuWorkers/smokePairs.
type workloadTemplateInput struct {
	Asset              string
	Name               string
	ResourceName       string
	Namespace          string
	ManifestName       string
	GPUs               int
	GPUResourceMode    string
	MIGProfile         string
	Resources          workloadResourceSizing
	Image              string
	Workers            int
	CPUWorkers         int
	SmokePairs         int
	UpstreamCheckpoint string
	RuntimePip         []string
	RuntimeEnv         []envspec.Var
	DataPVC            string
	StorageMounts      []StorageMount
	Scheduling         schedulingMetadata
	RDMA               runtimeRDMAConfig
	Blocks             workloadBlocks
	MetricsOffload     metricsOffloadRuntime
	CheckpointArtifact string
}

func renderWorkloadTemplate(in workloadTemplateInput) ([]byte, error) {
	raw, err := Asset(in.Asset)
	if err != nil {
		return nil, err
	}
	src := string(raw)
	for k, v := range map[string]string{
		"${NAME}":                 "{{.Name}}",
		"${RESOURCE_NAME}":        "{{.ResourceName}}",
		"${MANIFEST_NAME}":        "{{.ManifestName}}",
		"${SMOKE_PAIRS}":          "{{.SmokePairs}}",
		"${GPUS}":                 "{{.GPUs}}",
		"${CONTROL_CPUS}":         "{{.ControlCPUs}}",
		"${CONTROL_CPU_LIMIT}":    "{{.ControlCPULimit}}",
		"${CONTROL_MEMORY}":       "{{.ControlMemory}}",
		"${CONTROL_MEMORY_LIMIT}": "{{.ControlMemoryLimit}}",
		"${CPUS}":                 "{{.CPUs}}",
		"${CPU_LIMIT}":            "{{.CPULimit}}",
		"${WORKER_CPUS}":          "{{.WorkerCPUs}}",
		"${WORKER_CPU_LIMIT}":     "{{.WorkerCPULimit}}",
		"${MEMORY}":               "{{.Memory}}",
		"${MEMORY_LIMIT}":         "{{.MemoryLimit}}",
		"${WORKER_MEMORY}":        "{{.WorkerMemory}}",
		"${WORKER_MEMORY_LIMIT}":  "{{.WorkerMemoryLimit}}",
		"${IMAGE}":                "{{.Image}}",
		"${WORKERS}":              "{{.Workers}}",
		"${CPU_WORKERS}":          "{{.CPUWorkers}}",
		"${CLAIM}":                "{{.Claim}}",
		"${PROFILE_NAME}":         "{{.ProfileName}}",
		"${LANE_LABEL}":           "{{.LaneLabel}}",
		"${QUEUE_NAME}":           "{{.QueueName}}",
		"${UPSTREAM_CHECKPOINT}":  "{{.UpstreamCheckpoint}}",
		"${DATA_PVC}":             "{{.DataPVC}}",
	} {
		src = strings.ReplaceAll(src, k, v)
	}
	src = strings.ReplaceAll(src, "${JOB_LABELS}", "{{.JobLabels}}")
	src = strings.ReplaceAll(src, "${JOB_ANNOTATIONS_BLOCK}", "{{.JobAnnotationsBlock}}")
	src = strings.ReplaceAll(src, "${POD_METADATA_BLOCK}", "{{.PodMetadataBlock}}")
	src = strings.ReplaceAll(src, "${NODE_SELECTOR_BLOCK}", "{{.NodeSelectorBlock}}")
	src = strings.ReplaceAll(src, "${POD_PRIORITY_CLASS_BLOCK}", "{{.PodPriorityClassBlock}}")
	src = strings.ReplaceAll(src, "${SERVICE_ACCOUNT_BLOCK}", "{{.ServiceAccountBlock}}")
	src = strings.ReplaceAll(src, "${WORKER_POD_METADATA_BLOCK}", "{{.WorkerPodMetadataBlock}}")
	src = strings.ReplaceAll(src, "${WORKER_NODE_SELECTOR_BLOCK}", "{{.WorkerNodeSelectorBlock}}")
	src = strings.ReplaceAll(src, "${WORKER_POD_PRIORITY_CLASS_BLOCK}", "{{.WorkerPodPriorityClassBlock}}")
	src = strings.ReplaceAll(src, "${WORKER_SERVICE_ACCOUNT_BLOCK}", "{{.WorkerServiceAccountBlock}}")
	src = strings.ReplaceAll(src, "${CPU_WORKER_POD_METADATA_BLOCK}", "{{.CPUWorkerPodMetadataBlock}}")
	src = strings.ReplaceAll(src, "${CPU_WORKER_NODE_SELECTOR_BLOCK}", "{{.CPUWorkerNodeSelectorBlock}}")
	src = strings.ReplaceAll(src, "${CPU_WORKER_POD_PRIORITY_CLASS_BLOCK}", "{{.CPUWorkerPodPriorityClassBlock}}")
	src = strings.ReplaceAll(src, "${CPU_WORKER_SERVICE_ACCOUNT_BLOCK}", "{{.CPUWorkerServiceAccountBlock}}")
	src = strings.ReplaceAll(src, "${RUNTIME_ENV_JOB}", "{{.RuntimeEnvJob}}")
	src = strings.ReplaceAll(src, "${RUNTIME_ENV_HEAD}", "{{.RuntimeEnvHead}}")
	src = strings.ReplaceAll(src, "${RUNTIME_ENV_WORKER}", "{{.RuntimeEnvWorker}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUME_MOUNTS_JOB}", "{{.ExtraVolumeMountsJob}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUME_MOUNTS_HEAD}", "{{.ExtraVolumeMountsHead}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUME_MOUNTS_WORKER}", "{{.ExtraVolumeMountsWorker}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUMES_JOB}", "{{.ExtraVolumesJob}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUMES_HEAD}", "{{.ExtraVolumesHead}}")
	src = strings.ReplaceAll(src, "${EXTRA_VOLUMES_WORKER}", "{{.ExtraVolumesWorker}}")
	src = strings.ReplaceAll(src, "${DRIVER_LOG_OFFLOAD_SIDECAR}", "{{.DriverLogOffloadSidecar}}")
	src = strings.ReplaceAll(src, "${DRIVER_LOG_COMPLETION_SETUP}", "{{.DriverLogCompletionSetup}}")
	// PAYLOAD_INIT_CONTAINERS_JOB (managed-workflow-job.yaml.tmpl's single pod) and
	// PAYLOAD_INIT_CONTAINERS_HEAD (the three RayJob templates' head pod)
	// both resolve to the same Blocks.PayloadInitContainers value — each
	// template asset only ever contains one of the two markers, so there is
	// no ambiguity. PAYLOAD_INIT_CONTAINERS_WORKER resolves to the
	// script-only WorkerPayloadInitContainers value; only
	// managed-workflow-rayjob.yaml.tmpl and managed-workflow-rayjob-cpu.yaml.tmpl contain
	// this marker (in their worker template, gated behind
	// {{if gt .Workers 1}}) — managed-workflow-rayjob-eval.yaml.tmpl's CPU worker
	// template has no such marker at all, and rayJobBlocks leaves the field
	// empty for that caller regardless.
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_JOB}", "{{.PayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_HEAD}", "{{.PayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_WORKER}", "{{.WorkerPayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${STORAGE_PREFLIGHT}", "{{.StoragePreflight}}")
	src = strings.ReplaceAll(src, "${ARTIFACT_FINALIZE}", "{{.ArtifactFinalize}}")
	// PIP_PACKAGES is the shell-quoted runtime pip list shipped to the head
	// pod's entrypoint. The worker runtimeEnvYAML uses {{range .RuntimePip}}
	// directly inside the template (multi-line YAML list).
	src = strings.ReplaceAll(src, "${PIP_PACKAGES}", "{{.PipPackages}}")
	// The templates carry ${NAMESPACE} rather than a literal so no namespace is
	// ever baked into a rendered workload. Substituting here (vs a YAML
	// round-trip) keeps the rendered shape byte-identical except for the
	// namespace.
	src = strings.ReplaceAll(src, "${NAMESPACE}", "{{.Namespace}}")
	t, err := template.New("workload").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	workers := in.Workers
	if workers < 1 {
		workers = 1
	}
	gpuResourceMode, err := NormalizeGPUResourceMode(in.GPUResourceMode)
	if err != nil {
		return nil, err
	}
	if gpuResourceMode == GPUResourceModeMIG && strings.TrimSpace(in.MIGProfile) == "" {
		return nil, fmt.Errorf("gpu_resource_mode=mig requires mig_profile to be set")
	}
	useDRA := in.GPUs > 0 && gpuResourceMode == GPUResourceModeDRA
	useDevicePlugin := in.GPUs > 0 && (gpuResourceMode == GPUResourceModeDevicePlugin || gpuResourceMode == GPUResourceModeMIG)

	gpuResourceName := "nvidia.com/gpu"
	if gpuResourceMode == GPUResourceModeMIG {
		gpuResourceName = MIGResourceName(in.MIGProfile)
	}
	pip := in.RuntimePip
	if len(pip) == 0 {
		return nil, fmt.Errorf("renderWorkloadTemplate: empty RuntimePip (manifest must declare runtime.pip)")
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]any{
		"Name":                           in.Name,
		"Namespace":                      in.Namespace,
		"ResourceName":                   in.ResourceName,
		"ManifestName":                   in.ManifestName,
		"SmokePairs":                     in.SmokePairs,
		"GPUs":                           in.GPUs,
		"HasGPU":                         in.GPUs > 0,
		"GPUResourceMode":                gpuResourceMode,
		"GPUResourceName":                gpuResourceName,
		"UseDRA":                         useDRA,
		"UseDevicePlugin":                useDevicePlugin,
		"ControlCPUs":                    in.Resources.Control.CPUs,
		"ControlCPULimit":                in.Resources.Control.CPULimit,
		"ControlMemory":                  in.Resources.Control.Memory,
		"ControlMemoryLimit":             in.Resources.Control.MemoryLimit,
		"CPUs":                           in.Resources.Head.CPUs,
		"CPULimit":                       in.Resources.Head.CPULimit,
		"Memory":                         in.Resources.Head.Memory,
		"MemoryLimit":                    in.Resources.Head.MemoryLimit,
		"WorkerCPUs":                     in.Resources.Worker.CPUs,
		"WorkerCPULimit":                 in.Resources.Worker.CPULimit,
		"WorkerMemory":                   in.Resources.Worker.Memory,
		"WorkerMemoryLimit":              in.Resources.Worker.MemoryLimit,
		"Image":                          in.Image,
		"Workers":                        workers,
		"WorkerReplicas":                 workers,
		"CPUWorkers":                     in.CPUWorkers,
		"Claim":                          Claim(in.GPUs),
		"ProfileName":                    in.Scheduling.ProfileName,
		"LaneLabel":                      in.Scheduling.LaneLabel,
		"QueueName":                      in.Scheduling.QueueName,
		"UpstreamCheckpoint":             in.UpstreamCheckpoint,
		"DataPVC":                        in.DataPVC,
		"RuntimePip":                     pip,
		"RDMAEnabled":                    in.RDMA.Enabled,
		"RDMAResourceName":               in.RDMA.ResourceName,
		"RDMACount":                      in.RDMA.Count,
		"PipPackages":                    shellQuotePipPackages(pip),
		"JobLabels":                      in.Blocks.JobLabels,
		"JobAnnotationsBlock":            in.Blocks.JobAnnotationsBlock,
		"PodMetadataBlock":               in.Blocks.PodMetadataBlock,
		"NodeSelectorBlock":              in.Blocks.NodeSelectorBlock,
		"PodPriorityClassBlock":          in.Blocks.PodPriorityClassBlock,
		"ServiceAccountBlock":            in.Blocks.ServiceAccountBlock,
		"WorkerPodMetadataBlock":         in.Blocks.WorkerPodMetadataBlock,
		"WorkerNodeSelectorBlock":        in.Blocks.WorkerNodeSelectorBlock,
		"WorkerPodPriorityClassBlock":    in.Blocks.WorkerPodPriorityClassBlock,
		"WorkerServiceAccountBlock":      in.Blocks.WorkerServiceAccountBlock,
		"CPUWorkerPodMetadataBlock":      in.Blocks.CPUWorkerPodMetadataBlock,
		"CPUWorkerNodeSelectorBlock":     in.Blocks.CPUWorkerNodeSelectorBlock,
		"CPUWorkerPodPriorityClassBlock": in.Blocks.CPUWorkerPodPriorityClassBlock,
		"CPUWorkerServiceAccountBlock":   in.Blocks.CPUWorkerServiceAccountBlock,
		"RuntimeEnvJob":                  in.Blocks.RuntimeEnvJob,
		"RuntimeEnvHead":                 in.Blocks.RuntimeEnvHead,
		"RuntimeEnvWorker":               in.Blocks.RuntimeEnvWorker,
		"ExtraVolumeMountsJob":           in.Blocks.ExtraVolumeMountsJob,
		"ExtraVolumeMountsHead":          in.Blocks.ExtraVolumeMountsHead,
		"ExtraVolumeMountsWorker":        in.Blocks.ExtraVolumeMountsWorker,
		"ExtraVolumesJob":                in.Blocks.ExtraVolumesJob,
		"ExtraVolumesHead":               in.Blocks.ExtraVolumesHead,
		"ExtraVolumesWorker":             in.Blocks.ExtraVolumesWorker,
		"DriverLogOffloadSidecar":        in.Blocks.DriverLogOffloadSidecar,
		"DriverLogCompletionSetup":       in.Blocks.DriverLogCompletionSetup,
		"PayloadInitContainers":          in.Blocks.PayloadInitContainers,
		"WorkerPayloadInitContainers":    in.Blocks.WorkerPayloadInitContainers,
		"MetricsOffload":                 in.MetricsOffload.templateData(),
		"StoragePreflight":               storageprobe.IndentedScript(storagePreflightIndent(in.Asset)),
		"ArtifactFinalize": artifactindex.IndentedScript(artifactindex.Config{
			Artifact:     in.CheckpointArtifact,
			Run:          in.Name,
			ResourceName: in.ResourceName,
			Namespace:    in.Namespace,
		}, storagePreflightIndent(in.Asset)),
	}); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), nil
}

func storagePreflightIndent(asset string) int {
	if asset == "managed-workflow-job.yaml.tmpl" {
		return 14
	}
	return 4
}

func shellQuotePipPackages(packages []string) string {
	out := make([]string, 0, len(packages))
	for _, pkg := range packages {
		out = append(out, shellQuote(pkg))
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
