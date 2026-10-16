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

func Asset(name string) ([]byte, error) {
	return assets.ReadFile("assets/" + name)
}

func AssetNames() []string {
	return nil
}

var payloadFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var migProfileRE = regexp.MustCompile(`^\d+g\.\d+gb$`)

const (
	scriptPayloadInitContainerName   = "tau-script-payload"
	manifestPayloadInitContainerName = "tau-manifest-payload"

	scriptPayloadTargetDir   = "/script"
	manifestPayloadTargetDir = "/manifest"

	maxRenderedWorkloadBytes = 200 * 1024
)

type ExtraScript struct {
	Name string
	Data []byte
}

const (
	WorkloadKindJob        = "job"
	WorkloadKindRayJob     = "rayjob"
	WorkloadKindRayJobEval = "rayjob-eval"
	defaultWorkloadKind    = WorkloadKindJob
	rayOverwriteCmdKey     = "ray.io/overwrite-container-cmd"
	defaultRayJobImage     = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"
)

const (
	GPUResourceModeDevicePlugin = "device-plugin"
	GPUResourceModeDRA          = "dra"
	GPUResourceModeMIG          = "mig"
)

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

func MIGResourceName(profile string) string {
	return "nvidia.com/mig-" + strings.TrimSpace(profile)
}

type RenderOptions struct {
	Manifest                *Manifest
	ManifestRaw             []byte
	ManifestFilename        string
	Namespace               string
	SmokePairs              int
	ExtraScripts            []ExtraScript
	TopologyProfile         *profile.Profile
	TopologyOptions         topology.Options
	ProfileName             string
	Labels                  map[string]string
	Annotations             map[string]string
	WorkloadKind            string
	GPUResourceMode         string
	MIGProfile              string
	NodeSelector            map[string]string
	MainScript              []byte
	UpstreamCheckpoint      string
	JobSecret               *JobSecret
	RedactSecrets           bool
	Profile                 ProfileOptions
	MetricsOffload          MetricsOffloadOptions
	KVSpec                  *kvspec.Spec
	ServiceAccountName      string
	TTLSecondsAfterFinished int64
}

type ProfileOptions struct {
	Mode     string
	Rank     string
	Warmup   time.Duration
	Duration time.Duration
}

type JobSecret struct {
	Name       string            `json:"name" yaml:"name"`
	StringData map[string]string `json:"stringData" yaml:"stringData"`
	OwnerName  string            `json:"-" yaml:"-"`
	OwnerKind  string            `json:"-" yaml:"-"`
}

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

	scriptEncoded, scriptDigest, err := payload.Encode(payload.New(scripts))
	if err != nil {
		return nil, fmt.Errorf("script payload: %w", err)
	}
	manifestEncoded, manifestDigest, err := payload.Encode(payload.New(map[string][]byte{mfName: manifestRaw}))
	if err != nil {
		return nil, fmt.Errorf("manifest payload: %w", err)
	}
	embeds := payloadEmbeds{
		ScriptEncoded:      scriptEncoded,
		ScriptDigest:       scriptDigest,
		ManifestEncoded:    manifestEncoded,
		ManifestDigest:     manifestDigest,
		CheckpointArtifact: opts.Manifest.Artifacts.Checkpoint,
	}
	opts.Annotations = mergeStringMaps(opts.Annotations, map[string]string{
		workloadmeta.AnnotationScriptPayloadDigest:   scriptDigest,
		workloadmeta.AnnotationManifestPayloadDigest: manifestDigest,
		workloadmeta.AnnotationCheckpointArtifact:    strings.TrimSpace(opts.Manifest.Artifacts.Checkpoint),
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
	if metricsOffload.Enabled {
		scheduling.PodLabels[workloadmeta.LabelAzureWorkloadIdentityUse] = "true"
		scheduling.PodMetadataBlock = renderPodMetadata(scheduling.PodLabels, scheduling.PodAnnotations, 4)
	}

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
		workloadYAML, err = buildJob(name, resourceName, ns, mfName, opts.Manifest.Compute.GPUs, opts.SmokePairs, pip, runtimeEnv, dataPVC, storageMounts, scheduling, gpuResourceMode, migProfile, resources, spcName, embeds, opts.TTLSecondsAfterFinished)
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

	topologyOptions := opts.TopologyOptions
	if opts.Manifest != nil && opts.Manifest.RuntimeRDMA().Enabled {
		topologyOptions = topology.WithNetworkDomainRequirement(p, topologyOptions)
	}
	plan, err := topology.Build(p, topologyOptions)
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
	return len(v) <= maxResourceNameLen && runconfig.QualifiedNameSegmentRE().MatchString(v)
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

type payloadEmbeds struct {
	ScriptEncoded      string
	ScriptDigest       string
	ManifestEncoded    string
	ManifestDigest     string
	CheckpointArtifact string
}

func payloadInitContainersYAML(image string, embeds payloadEmbeds, indent int) string {
	return initContainersBlock(indent, payloadInitContainerItemsYAML(image, embeds, indent, true))
}

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

func payloadInitContainerYAML(image, name, encoded, digest, targetDir, volumeName string, indent int) string {
	item := strings.Repeat(" ", indent+2)
	field := strings.Repeat(" ", indent+4)
	listItem := strings.Repeat(" ", indent+6)
	cont := strings.Repeat(" ", indent+8)

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

func yamlLiteralBlock(text string, indent int) string {
	prefix := strings.Repeat(" ", indent)
	lines := strings.Split(text, "\n")
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

func buildJob(name, resourceName, namespace, manifestName string, gpus, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, spcName string, embeds payloadEmbeds, ttlSecondsAfterFinished int64) ([]byte, error) {
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

		TTLSecondsAfterFinished: ttlSecondsAfterFinished,
	})
}

func buildRayJob(name, resourceName, namespace, manifestName string, gpus, workers int, image string, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, rdma runconfig.NormalizedRDMA, metricsOffload metricsOffloadRuntime, spcName string, embeds payloadEmbeds) ([]byte, error) {
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

func buildRayJobCPU(name, resourceName, namespace, manifestName string, workers int, image string, smokePairs int, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, resources workloadResourceSizing, rdma runconfig.NormalizedRDMA, spcName string, embeds payloadEmbeds) ([]byte, error) {
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

func buildRayJobEval(name, resourceName, namespace, manifestName string, gpus, cpuWorkers int, image string, smokePairs int, upstreamCheckpoint string, pip []string, runtimeEnv []envspec.Var, dataPVC string, storageMounts []StorageMount, scheduling schedulingMetadata, gpuResourceMode, migProfile string, resources workloadResourceSizing, rdma runconfig.NormalizedRDMA, metricsOffload metricsOffloadRuntime, spcName string, embeds payloadEmbeds) ([]byte, error) {
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
		Workers:            1,
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
	PayloadInitContainers          string
	WorkerPayloadInitContainers    string
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
		PayloadInitContainers: payloadInitContainersYAML(image, embeds, 6),
	}
	if spcName != "" {
		b.ExtraVolumeMountsJob = appendYAMLBlock(b.ExtraVolumeMountsJob, kvspec.VolumeMountYAML(12))
		b.ExtraVolumesJob = appendYAMLBlock(b.ExtraVolumesJob, kvspec.VolumeYAML(spcName, 8))
	}
	return b
}

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
		PayloadInitContainers:          initContainersBlock(10, prepareRayTmpBlock+payloadInitContainerItemsYAML(image, embeds, 10, true)+metricsOffloadInitBlock),
		WorkerPayloadInitContainers:    workerScriptPayloadInitContainersYAML(image, embeds, 12),
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

type workloadTemplateInput struct {
	Asset                   string
	Name                    string
	ResourceName            string
	Namespace               string
	ManifestName            string
	GPUs                    int
	GPUResourceMode         string
	MIGProfile              string
	Resources               workloadResourceSizing
	Image                   string
	Workers                 int
	CPUWorkers              int
	SmokePairs              int
	UpstreamCheckpoint      string
	RuntimePip              []string
	RuntimeEnv              []envspec.Var
	DataPVC                 string
	StorageMounts           []StorageMount
	Scheduling              schedulingMetadata
	RDMA                    runconfig.NormalizedRDMA
	Blocks                  workloadBlocks
	MetricsOffload          metricsOffloadRuntime
	CheckpointArtifact      string
	TTLSecondsAfterFinished int64
}

const defaultManagedJobTTLSecondsAfterFinished int64 = 86400

func jobTTLSecondsAfterFinished(override int64) int64 {
	if override > 0 {
		return override
	}
	return defaultManagedJobTTLSecondsAfterFinished
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
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_JOB}", "{{.PayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_HEAD}", "{{.PayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${PAYLOAD_INIT_CONTAINERS_WORKER}", "{{.WorkerPayloadInitContainers}}")
	src = strings.ReplaceAll(src, "${STORAGE_PREFLIGHT}", "{{.StoragePreflight}}")
	src = strings.ReplaceAll(src, "${ARTIFACT_FINALIZE}", "{{.ArtifactFinalize}}")
	src = strings.ReplaceAll(src, "${PIP_PACKAGES}", "{{.PipPackages}}")
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
		"JobTTLSecondsAfterFinished":     jobTTLSecondsAfterFinished(in.TTLSecondsAfterFinished),
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
