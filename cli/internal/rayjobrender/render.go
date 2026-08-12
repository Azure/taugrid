// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package rayjobrender renders researcher-authored Ray Train scripts as Kueue
// admitted RayJob workloads. Rendered RayJobs are self-contained: the driver
// script is embedded directly in the RayJob's pod templates (see the
// internal/payload package) instead of depending on a per-run ConfigMap, so
// the workload can be dispatched to any MultiKueue worker without a separate
// object to mirror or pre-provision. Plain Ray drivers stage source only on the
// head; Ray Tune also stages it on workers because TorchTrainer reloads the
// researcher function there.
package rayjobrender

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/artifactindex"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/jsonutil"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/payload"
	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	RayVersion      = "2.56.0"
	DefaultGPUImage = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"
	DefaultCPUImage = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0"

	metricsPort              = 8080
	dashboardPort            = 8265
	agentPort                = 52365
	gcsPort                  = 6379
	clientPort               = 10001
	maxRayJobResourceNameLen = 47

	// rayJobTTLSecondsAfterFinished bounds how long a completed RayJob keeps its
	// RayCluster — and the node capacity its head/worker pods occupy — alive.
	//
	// It is deliberately short rather than zero. The raylogoffload sidecar drains
	// for five seconds after the entrypoint completion marker, and KubeRay reconciles
	// on a roughly three-second cadence. Fifteen seconds leaves margin for both while
	// bounding the interval in which Kueue has released quota but the RayCluster still
	// holds physical devices.
	//
	// It is not longer because nothing else survives the window: the head pod goes
	// NotReady before GC, so the portal already greys out the Ray dashboard link
	// (see portal jobdetail.DetailLinks.RayDashboardReachable). Time beyond this is
	// pure capacity hoarding.
	rayJobTTLSecondsAfterFinished = 15
)

var (
	resourceNamePartRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	rayImageVersionRE  = regexp.MustCompile(`-ray((\d+)\.(\d+)\.\d+)`)
	migProfileRE       = regexp.MustCompile(`^\d+g\.\d+gb$`)
	pythonModuleNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// payloadTargetDir is where the tau-payload initContainer materialises the
// embedded payload inside each pod.
const payloadTargetDir = "/script"

// projectArchiveFilename is the payload entry holding the project zip that Ray
// consumes as working_dir.
const projectArchiveFilename = "_tau_project.zip"

// MaxProjectArchiveBytes bounds the packaged working_dir archive.
//
// Ray resolves a file:// working_dir independently on every node, so the
// archive has to be embedded in the head template *and* every worker
// template. That doubling, not MAX_ARG_STRLEN, is the binding constraint
// here: the rendered object is stored verbatim in kubectl's
// last-applied-configuration annotation, which Kubernetes caps at ~256 KiB.
// Two copies of the encoded payload plus the rest of the spec has to fit in
// that with margin, which puts the ceiling near 45 KiB of archive.
//
// This is far less restrictive than it sounds: the archive is deflated, and
// Python source compresses roughly 3.5x, so 64 KiB of archive is on the order
// of 200+ KiB of real source spread across as many files as the project likes.
const MaxProjectArchiveBytes = 45 << 10

type Options struct {
	Name               string
	Namespace          string
	ServiceAccountName string
	ScriptName         string
	Script             []byte
	// ProjectArchive is a deterministic zip of the project directory. When
	// set, it is shipped to every node and handed to Ray as a runtime_env
	// working_dir, which puts the unpacked tree on PYTHONPATH so sibling
	// modules and local packages import correctly in workers as well as the
	// driver. When empty, the run keeps the single-file behaviour of
	// embedding only ScriptName.
	ProjectArchive []byte
	Image          string
	// Workers is the execution-worker count. RayJobs add a separate control-only
	// head on the system pool.
	Workers         int
	GPUsPerWorker   int
	Launcher        string
	GPUResourceMode string
	MIGProfile      string
	RuntimePip      []string
	Env             map[string]string
	EnvSecrets      []envspec.Var
	RedactSecrets   bool
	DataPVC         string
	Profile         profile.Profile
	TopologyOptions topology.Options
	NodeSelector    map[string]string
	Labels          map[string]string
	Annotations     map[string]string
	Resources       Resources
	OutputDir       string
	ArtifactPublish artifactpublish.Runtime

	// CheckpointArtifact is storage.checkpoint: the file or directory,
	// relative to the run checkpoint dir, that this run produces as its
	// servable model. When set, the entrypoint writes an artifact index
	// after the training script exits successfully.
	CheckpointArtifact string
	MetricsOffload     metricsoffload.Runtime

	TuneMetric              string
	TuneMode                string
	TuneNumSamples          int
	TuneMaxConcurrentTrials int
	TuneParamSpace          string // JSON-encoded param space

	AzureWorkloadIdentity bool

	RayTrainConfig    map[string]any
	AllowNCCLOverride bool
}

type Resources struct {
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
	Head          ResourceOverrides
	Worker        ResourceOverrides
}

type ResourceOverrides struct {
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

func Render(o Options) ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	files := map[string][]byte{o.ScriptName: o.Script}
	if isTuneLauncher(o.Launcher) {
		files[tuneDriverFilename] = []byte(tuneDriverScript)
	}
	if len(o.ProjectArchive) > 0 {
		// The archive already contains the entrypoint, so shipping the loose
		// copy as well would double the payload for no benefit.
		files = map[string][]byte{projectArchiveFilename: o.ProjectArchive}
		if isTuneLauncher(o.Launcher) {
			files[tuneDriverFilename] = []byte(tuneDriverScript)
		}
	}
	encodedPayload, payloadDigest, err := payload.Encode(payload.New(files))
	if err != nil {
		return nil, fmt.Errorf("script payload: %w", err)
	}
	plan, err := topology.Build(o.Profile, o.TopologyOptions)
	if err != nil {
		return nil, err
	}
	if plan.QueueName == "" {
		if q, ok := o.Profile.Spec["queue"].(map[string]any); ok {
			if local, ok := q["localQueue"].(string); ok && local != "" {
				plan.QueueName = local
			}
		}
	}
	if plan.QueueName == "" {
		plan.QueueName = topology.SharedGPUQueueName
	}

	rayJob, err := buildRayJob(o, plan, encodedPayload, payloadDigest)
	if err != nil {
		return nil, err
	}
	objects := []any{
		rayJob,
	}
	var out strings.Builder
	for i, obj := range objects {
		if i > 0 {
			out.WriteString("---\n")
		}
		data, err := marshal(obj)
		if err != nil {
			return nil, err
		}
		out.Write(data)
	}
	return []byte(out.String()), nil
}

func (o Options) validate() error {
	if o.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !resourceNamePartRE.MatchString(o.Name) {
		return fmt.Errorf("name %q is invalid (use lowercase alphanumerics with internal hyphens)", o.Name)
	}
	if len(o.Name) > maxRayJobResourceNameLen {
		return fmt.Errorf("RayJob name %q is too long (%d chars; KubeRay limit is %d)", o.Name, len(o.Name), maxRayJobResourceNameLen)
	}
	if o.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(o.ScriptName) == "" {
		return fmt.Errorf("script name is required")
	}
	if len(o.ProjectArchive) > 0 {
		// The entrypoint is a module path relative to the project root, so
		// nested paths are legitimate here; entrypointModule rejects escapes.
		if _, err := entrypointModule(o.ScriptName); err != nil {
			return err
		}
	} else {
		base := filepath.Base(o.ScriptName)
		if base == "." || base == ".." || base != o.ScriptName {
			return fmt.Errorf("script name %q must be a single file name", o.ScriptName)
		}
	}
	if len(o.Script) == 0 {
		return fmt.Errorf("script %q is empty", o.ScriptName)
	}
	if o.Workers < 1 {
		return fmt.Errorf("workers must be >= 1")
	}
	if o.GPUsPerWorker < 0 || o.GPUsPerWorker > 8 {
		return fmt.Errorf("gpus per worker must be 0..8, got %d", o.GPUsPerWorker)
	}
	for _, pkg := range o.RuntimePip {
		if strings.TrimSpace(pkg) == "" || pkg != strings.TrimSpace(pkg) {
			return fmt.Errorf("runtime pip packages must not be empty or have surrounding whitespace")
		}
	}
	for k := range o.Env {
		upper := strings.ToUpper(k)
		if runconfig.ReservedTauEnvKey(k) {
			return runconfig.ReservedTauEnvKeyError(k)
		}
		if !o.AllowNCCLOverride && strings.HasPrefix(upper, "NCCL_") {
			return fmt.Errorf("env %q is reserved (NCCL_* keys are managed by Tau; set execution.allow_nccl_override: true to bypass)", k)
		}
		if upper == "MASTER_ADDR" || upper == "MASTER_PORT" {
			return fmt.Errorf("env %q is reserved (set by Ray runtime)", k)
		}
	}
	if err := envspec.Validate(envspec.FromMap(o.Env)); err != nil {
		return err
	}
	if err := envspec.Validate(o.EnvSecrets); err != nil {
		return err
	}
	for _, v := range o.EnvSecrets {
		upper := strings.ToUpper(v.Name)
		if runconfig.ReservedTauEnvKey(v.Name) {
			return runconfig.ReservedTauEnvKeyError(v.Name)
		}
		if !o.AllowNCCLOverride && strings.HasPrefix(upper, "NCCL_") {
			return fmt.Errorf("env %q is reserved (NCCL_* keys are managed by Tau; set execution.allow_nccl_override: true to bypass)", v.Name)
		}
		if upper == "MASTER_ADDR" || upper == "MASTER_PORT" {
			return fmt.Errorf("env %q is reserved (set by Ray runtime)", v.Name)
		}
	}
	if o.GPUResourceMode != "" && o.GPUResourceMode != "device-plugin" && o.GPUResourceMode != "mig" {
		return fmt.Errorf("gpu-resource-mode %q is not supported by the Ray engine (supported: device-plugin, mig)", o.GPUResourceMode)
	}
	if o.GPUResourceMode == "mig" && o.MIGProfile == "" {
		return fmt.Errorf("gpu-resource-mode=mig requires --mig-profile to be set")
	}
	if o.MIGProfile != "" && !migProfileRE.MatchString(o.MIGProfile) {
		return fmt.Errorf("mig-profile %q must match <N>g.<M>gb (e.g. 1g.18gb, 3g.71gb)", o.MIGProfile)
	}
	if err := validateRayTrainConfigOptions(o.RayTrainConfig); err != nil {
		return err
	}
	if o.ArtifactPublish.Enabled() {
		if o.DataPVC == "" {
			return fmt.Errorf("staged artifact publication requires a durable data PVC")
		}
		if err := o.ArtifactPublish.Validate(); err != nil {
			return err
		}
	}
	if o.MetricsOffload.Enabled() {
		if o.DataPVC == "" {
			return fmt.Errorf("metrics offload requires a durable data PVC")
		}
		if err := o.MetricsOffload.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateRayTrainConfigOptions(configs map[string]any) error {
	if len(configs) == 0 {
		return nil
	}
	allowed := map[string]bool{"torch_config": true, "scaling_config": true, "failure_config": true}
	for section := range configs {
		if !allowed[section] {
			return fmt.Errorf("ray_train_config: %q is not a valid section; use torch_config, scaling_config, or failure_config", section)
		}
		sectionMap, ok := configs[section].(map[string]any)
		if !ok {
			return fmt.Errorf("ray_train_config.%s must be a map", section)
		}
		if section == "scaling_config" {
			blocked := map[string]bool{"num_workers": true, "resources_per_worker": true}
			for field := range sectionMap {
				if blocked[field] {
					return fmt.Errorf("ray_train_config.scaling_config.%s is Tau-managed; remove it", field)
				}
			}
		}
	}
	return nil
}

func buildRayJob(o Options, plan topology.Plan, encodedPayload, payloadDigest string) (map[string]any, error) {
	if err := topology.ValidateGPUClassNodeSelector(plan.Labels[workloadmeta.LabelGPUClass], o.NodeSelector); err != nil {
		return nil, err
	}
	image := o.Image
	if image == "" {
		if o.GPUsPerWorker > 0 {
			image = DefaultGPUImage
		} else {
			image = DefaultCPUImage
		}
	}
	labels := map[string]any{
		topology.QueueLabel: plan.QueueName,
	}
	for k, v := range o.Labels {
		if k != "" && v != "" {
			labels[k] = v
		}
	}
	for k, v := range plan.Labels {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			labels[k] = v
		}
	}
	labels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue

	annotations := map[string]any{
		payload.AnnotationDigest: payloadDigest,
	}
	// Record the declaration on the workload so `tau run get` can tell a run
	// that produced nothing from one whose promised artifact is missing.
	if checkpoint := strings.TrimSpace(o.CheckpointArtifact); checkpoint != "" {
		annotations[workloadmeta.AnnotationCheckpointArtifact] = checkpoint
	}
	for k, v := range o.Annotations {
		if k != "" && v != "" {
			annotations[k] = v
		}
	}
	for k, v := range plan.Annotations {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			annotations[k] = v
		}
	}
	if o.Launcher != "" {
		annotations[workloadmeta.AnnotationSpecExecution] = fmt.Sprintf(`{"launcher":%q}`, strings.ToLower(o.Launcher))
	}

	podLabels := map[string]any{}
	for k, v := range labels {
		if k != "" && v != "" {
			podLabels[k] = v
		}
	}
	if o.AzureWorkloadIdentity && o.ServiceAccountName != "" {
		podLabels[workloadmeta.LabelAzureWorkloadIdentityUse] = "true"
	}
	podAnnotations := map[string]any{
		"adx-mon/scrape": "true",
		"adx-mon/port":   fmt.Sprintf("%d", metricsPort),
	}
	for k, v := range workloadmeta.PodCorrelationAnnotations(o.Annotations) {
		podAnnotations[k] = v
	}
	for k, v := range plan.Annotations {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			podAnnotations[k] = v
		}
	}
	podMetadata := map[string]any{
		"labels":      podLabels,
		"annotations": podAnnotations,
	}
	headPodAnnotations := topology.WithoutKueueTopologyAnnotations(stringMapFromAnyMap(podAnnotations))
	headPodMetadata := map[string]any{
		"labels":      podLabels,
		"annotations": raylogoffload.HeadPodAnnotations(headPodAnnotations),
	}

	workerNodeSelector := mergeSelectors(plan.NodeSelector, o.NodeSelector)
	headPod, err := buildPodSpec(o, image, "ray-head", nil, plan.PodPriorityClassName, encodedPayload, payloadDigest, true)
	if err != nil {
		return nil, err
	}
	headPod["affinity"] = topology.SystemNodeAffinity()
	workerPod, err := buildPodSpec(o, image, "ray-worker", workerNodeSelector, plan.PodPriorityClassName, encodedPayload, payloadDigest, false)
	if err != nil {
		return nil, err
	}

	headStartParams := map[string]any{
		"dashboard-host": "0.0.0.0",
		"num-cpus":       "0",
		"num-gpus":       "0",
	}
	head := map[string]any{
		"rayStartParams": headStartParams,
		"template": map[string]any{
			"metadata": headPodMetadata,
			"spec":     headPod,
		},
	}
	workers := []any{}
	if o.Workers > 0 {
		workers = append(workers, map[string]any{
			"replicas":    o.Workers,
			"minReplicas": o.Workers,
			"maxReplicas": o.Workers,
			"groupName":   o.Name + "-w",
			"rayStartParams": map[string]any{
				"num-gpus": fmt.Sprintf("%d", o.GPUsPerWorker),
			},
			"template": map[string]any{
				"metadata": podMetadata,
				"spec":     workerPod,
			},
		})
	}

	jobEntrypoint, err := entrypoint(o)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{
		"suspend":                  true,
		"shutdownAfterJobFinishes": true,
		// shutdownAfterJobFinishes and this TTL compose: the TTL is the delay before
		// cleanup, not an independent knob (KubeRay's own default is 0). Until it
		// elapses a finished RayJob keeps its head/worker pods Running and holding
		// node CPU/GPU, so a job can report SUCCEEDED while still consuming the
		// cluster. Keep a short grace window so `tau run logs` can still read from
		// the Ray dashboard just after completion, then release the capacity.
		// Matches the eval/CPU templates, which delete immediately for the same reason.
		"ttlSecondsAfterFinished": int64(rayJobTTLSecondsAfterFinished),
		"submissionMode":          "HTTPMode",
		"entrypoint":              jobEntrypoint,
		"rayClusterSpec": map[string]any{
			"rayVersion":       rayVersionString(image),
			"headGroupSpec":    head,
			"workerGroupSpecs": workers,
		},
	}
	if runtimeEnv := runtimeEnvYAML(o.RuntimePip, o.ProjectArchive); runtimeEnv != "" {
		spec["runtimeEnvYAML"] = runtimeEnv
	}

	metadata := map[string]any{
		"name":        o.Name,
		"namespace":   o.Namespace,
		"labels":      labels,
		"annotations": annotations,
	}
	return map[string]any{
		"apiVersion": "ray.io/v1",
		"kind":       "RayJob",
		"metadata":   metadata,
		"spec":       spec,
	}, nil
}

// buildPodSpec renders a pod template for either the Ray head or a worker
// group. Plain Ray drivers carry the embedded payload only on the head because
// the Ray Job Submission API runs their entrypoint there. Ray Tune is
// different: TorchTrainer deserializes the generated train loop on execution
// workers, where it reloads the researcher function from local staged source.
// Project archives likewise have to exist on every node for Ray's file://
// working_dir resolution.
func buildPodSpec(o Options, image, containerName string, nodeSelector map[string]string, priorityClass string, encodedPayload, payloadDigest string, isHead bool) (map[string]any, error) {
	env, err := envList(o, isHead)
	if err != nil {
		return nil, err
	}

	container := map[string]any{
		"name":         containerName,
		"image":        image,
		"ports":        containerPorts(containerName),
		"env":          env,
		"resources":    resources(o, containerName),
		"volumeMounts": volumeMounts(o, isHead),
	}
	// Only set explicit probes on workers. Head probes are omitted so KubeRay
	// injects its version-aware defaults (raylet + GCS health).
	if containerName != "ray-head" {
		container["startupProbe"] = workerStartupProbe(parseRayVersion(image))
		container["readinessProbe"] = tcpHealthProbe(60, 10, 3, 10)
		container["livenessProbe"] = tcpHealthProbe(60, 30, 6, 10)
	}

	containers := []any{container}
	if isHead {
		containers = append(containers, raylogoffload.SidecarContainer(image))
		if o.MetricsOffload.Enabled() {
			containers = append(containers, metricsoffload.BuildContainer(o.MetricsOffload, []metricsoffload.Mount{
				{Name: "data", Path: storage.DurableRoot},
			}))
		}
	}

	pod := map[string]any{
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": int64(600),
		"containers":                    containers,
		"volumes":                       volumes(o, isHead),
	}
	if o.ServiceAccountName != "" {
		pod["serviceAccountName"] = o.ServiceAccountName
	}
	if isHead {
		pod["initContainers"] = []any{
			raylogoffload.PrepareInitContainer(image),
			payloadInitContainer(image, encodedPayload, payloadDigest),
		}
	} else if workerNeedsSourcePayload(o) {
		// Project archives must exist on every node for file:// working_dir
		// resolution. Tune's single-file contract likewise needs the staged
		// researcher module on every TorchTrainer worker. Neither case needs
		// the head-only log/metrics plumbing.
		pod["initContainers"] = []any{
			payloadInitContainer(image, encodedPayload, payloadDigest),
		}
	}
	if len(nodeSelector) > 0 {
		pod["nodeSelector"] = nodeSelector
	}
	if priorityClass != "" {
		pod["priorityClassName"] = priorityClass
	}
	if isHead {
		pod["tolerations"] = []any{
			map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists", "effect": "NoSchedule"},
		}
	} else if o.GPUsPerWorker > 0 {
		pod["tolerations"] = []any{
			map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
		}
	}
	return pod, nil
}

// payloadInitContainer decodes, verifies, and writes the embedded payload
// into the shared "script" emptyDir before the main Ray container starts. It
// reuses the workload's own image (rather than pulling an additional one)
// because every Tau-managed Ray image already ships Python 3, which is all
// InitContainerScript needs.
func payloadInitContainer(image, encodedPayload, payloadDigest string) map[string]any {
	return map[string]any{
		"name":    payload.InitContainerName,
		"image":   image,
		"command": []any{"python3", "-c", payload.InitContainerScript},
		"env": []any{
			map[string]any{"name": payload.EnvB64, "value": encodedPayload},
			map[string]any{"name": payload.EnvDigest, "value": payloadDigest},
			map[string]any{"name": payload.EnvTargetDir, "value": payloadTargetDir},
		},
		"volumeMounts": []any{
			map[string]any{"name": "script", "mountPath": payloadTargetDir},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"cpu": "250m", "memory": "128Mi"},
		},
	}
}

func containerPorts(containerName string) []any {
	ports := []any{
		containerPort("metrics", metricsPort),
	}
	if containerName == "ray-head" {
		ports = append(ports,
			containerPort("gcs", gcsPort),
			containerPort("dashboard", dashboardPort),
			containerPort("client", clientPort),
		)
	}
	return ports
}

func containerPort(name string, port int) map[string]any {
	return map[string]any{"name": name, "containerPort": int64(port), "protocol": "TCP"}
}

// parseRayVersion extracts the Ray major.minor from an image tag (e.g.
// "ray:py3.10-ray2.39.0-cuda13.0" → [2,39]). Falls back to the compiled
// RayVersion constant if the image tag doesn't match.
func parseRayVersion(image string) [2]int {
	if m := rayImageVersionRE.FindStringSubmatch(image); m != nil {
		major, _ := strconv.Atoi(m[2])
		minor, _ := strconv.Atoi(m[3])
		return [2]int{major, minor}
	}
	parts := strings.SplitN(RayVersion, ".", 3)
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return [2]int{major, minor}
}

// rayVersionString extracts the full Ray version string from an image tag
// (e.g. "ray:py3.10-ray2.39.0-..." → "2.39.0"). Falls back to RayVersion.
func rayVersionString(image string) string {
	if m := rayImageVersionRE.FindStringSubmatch(image); m != nil {
		return m[1]
	}
	return RayVersion
}

// workerStartupProbe returns an HTTP startup probe for workers. The unified
// /api/healthz endpoint was introduced in Ray 2.53; older versions only serve
// /api/local_raylet_healthz on the agent port.
func workerStartupProbe(ver [2]int) map[string]any {
	path := "/api/healthz"
	if ver[0] < 2 || (ver[0] == 2 && ver[1] < 53) {
		path = "/api/local_raylet_healthz"
	}
	return map[string]any{
		"httpGet": map[string]any{
			"path": path,
			"port": int64(agentPort),
		},
		"periodSeconds":    int64(5),
		"timeoutSeconds":   int64(2),
		"failureThreshold": int64(60),
	}
}

// tcpHealthProbe returns a TCP socket probe on the agent port. Under heavy GPU
// training load the dashboard agent's HTTP handler can stall, causing
// false-positive probe failures. TCP avoids this while detecting a dead process.
func tcpHealthProbe(initialDelay, period, failureThreshold, timeout int) map[string]any {
	return map[string]any{
		"tcpSocket": map[string]any{
			"port": int64(agentPort),
		},
		"initialDelaySeconds": int64(initialDelay),
		"periodSeconds":       int64(period),
		"timeoutSeconds":      int64(timeout),
		"failureThreshold":    int64(failureThreshold),
	}
}

func entrypoint(o Options) (string, error) {
	var b strings.Builder
	b.WriteString("set -eu\n")
	// A plain install fails with EACCES on the canonical Ray CUDA image
	// (DefaultGPUImage): pip cannot write to the root-owned
	// /usr/lib/python3.12/site-packages/nvidia. Observed on
	// ray:py3.12-ray2.56.0-cuda13.0. It installs cleanly on DefaultCPUImage,
	// where this fallback simply never fires. Retry into the user site rather
	// than making every researcher set PIP_USER=1 by hand.
	b.WriteString("if [ -s /script/requirements.txt ]; then python3 -m pip install --quiet --no-cache-dir -r /script/requirements.txt || python3 -m pip install --quiet --no-cache-dir --user -r /script/requirements.txt; fi\n")
	// --user drops console scripts in the user base bin dir, which is not on
	// PATH here; without this `torchrun` and friends are installed but unusable.
	b.WriteString("PATH=\"$(python3 -m site --user-base)/bin:$PATH\"; export PATH\n")
	if len(o.RuntimePip) > 0 {
		var pkgs strings.Builder
		for _, pkg := range o.RuntimePip {
			pkgs.WriteByte(' ')
			pkgs.WriteString(shellQuote(pkg))
		}
		b.WriteString("python3 -m pip install --quiet --no-cache-dir" + pkgs.String())
		b.WriteString(" || python3 -m pip install --quiet --no-cache-dir --user" + pkgs.String())
		b.WriteByte('\n')
	}
	b.WriteString("cd /data\n")
	switch {
	case isTuneLauncher(o.Launcher):
		b.WriteString("python3 " + payloadTargetDir + "/")
		b.WriteString(tuneDriverFilename)
	case len(o.ProjectArchive) > 0:
		// In working_dir mode the entrypoint lives inside the archive that
		// Ray unpacks per node, not at a path we control, so it is run as a
		// module. Ray prepends the unpacked directory to PYTHONPATH, which
		// makes -m resolve the entrypoint and its siblings from the same
		// tree regardless of the working directory.
		module, err := entrypointModule(o.ScriptName)
		if err != nil {
			return "", err
		}
		b.WriteString("python3 -m ")
		b.WriteString(shellQuote(module))
	default:
		b.WriteString("python3 " + payloadTargetDir + "/")
		b.WriteString(shellQuote(o.ScriptName))
	}
	b.WriteByte('\n')
	script := b.String()
	var err error
	// Index the declared checkpoint after a successful run so
	// `tau serve deploy --from-finetune` can resolve this run by name.
	if strings.TrimSpace(o.CheckpointArtifact) != "" {
		// Without a PVC the "data" volume is an emptyDir (see volumes), so the
		// checkpoint and artifacts.json would be deleted along with the pod and
		// `tau serve deploy --from-finetune` would resolve a missing model.
		if strings.TrimSpace(o.DataPVC) == "" {
			return "", fmt.Errorf("storage.checkpoint requires writable durable PVC storage to write the artifact index")
		}
		script = script + "\n" + artifactindex.Script(artifactindex.Config{
			Artifact:     o.CheckpointArtifact,
			Run:          o.Name,
			ResourceName: o.Name,
			Namespace:    o.Namespace,
		}) + "\n"
	}
	if o.ArtifactPublish.Enabled() {
		script, err = artifactpublish.WrapShellScript(script, o.ArtifactPublish)
		if err != nil {
			return "", err
		}
	}
	if o.MetricsOffload.Enabled() {
		script, err = metricsoffload.WrapShellScript(script, o.MetricsOffload)
		if err != nil {
			return "", err
		}
	}
	return raylogoffload.WrapShellScript(script), nil
}

// entrypointModule converts an archive-relative script path into the dotted
// module name `python3 -m` expects.
func entrypointModule(scriptName string) (string, error) {
	clean := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(scriptName)), "./")
	if !strings.HasSuffix(clean, ".py") {
		return "", fmt.Errorf("run.entrypoint %q must be a .py file to run inside a working_dir project", scriptName)
	}
	clean = strings.TrimSuffix(clean, ".py")
	if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
		return "", fmt.Errorf("run.entrypoint %q must be a path inside the project directory", scriptName)
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("run.entrypoint %q must be a path inside the project directory", scriptName)
		}
	}
	return strings.Join(parts, "."), nil
}

func isTuneLauncher(launcher string) bool {
	return strings.EqualFold(launcher, "ray-tune")
}

func tuneModuleName(o Options) (string, error) {
	if len(o.ProjectArchive) > 0 {
		return entrypointModule(o.ScriptName)
	}
	name := strings.TrimSuffix(o.ScriptName, ".py")
	if pythonModuleNameRE.MatchString(name) {
		return name, nil
	}
	return "_tau_user_train", nil
}

// runtimeEnvYAML renders the RayJob runtime environment.
//
// working_dir is emitted as a file:// URI rather than a bare path because Ray
// resolves working_dir through parse_uri, which rejects any value without a
// recognised scheme. file:// is resolved independently on each node against
// its own filesystem, which is exactly what the per-pod tau-payload
// initContainer provides, so no object store, credential, or shared volume is
// involved and the workload stays self-contained.
func runtimeEnvYAML(pkgs []string, projectArchive []byte) string {
	if len(pkgs) == 0 && len(projectArchive) == 0 {
		return ""
	}
	var b strings.Builder
	if len(projectArchive) > 0 {
		b.WriteString("working_dir: \"file://")
		b.WriteString(payloadTargetDir + "/" + projectArchiveFilename)
		b.WriteString("\"\n")
	}
	if len(pkgs) > 0 {
		b.WriteString("pip:\n")
		for _, pkg := range pkgs {
			b.WriteString("  - ")
			b.WriteString(pkg)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func isGeneratedTauMetadataKey(key string) bool {
	return strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelGPUClass
}

func resources(o Options, containerName string) map[string]any {
	requests := map[string]any{
		"cpu":    "2",
		"memory": "8Gi",
	}
	limits := map[string]any{
		"cpu":    "4",
		"memory": "16Gi",
	}
	if o.GPUsPerWorker > 0 && containerName == "ray-worker" {
		requests["cpu"] = "8"
		requests["memory"] = "64Gi"
		limits["cpu"] = "16"
		limits["memory"] = "128Gi"
		gpuResource := "nvidia.com/gpu"
		if o.GPUResourceMode == "mig" && o.MIGProfile != "" {
			gpuResource = "nvidia.com/mig-" + o.MIGProfile
		}
		requests[gpuResource] = o.GPUsPerWorker
		limits[gpuResource] = o.GPUsPerWorker
	}
	applyResourceOverrides(requests, limits, ResourceOverrides{
		CPURequest:    o.Resources.CPURequest,
		MemoryRequest: o.Resources.MemoryRequest,
		CPULimit:      o.Resources.CPULimit,
		MemoryLimit:   o.Resources.MemoryLimit,
	})
	switch containerName {
	case "ray-head":
		applyResourceOverrides(requests, limits, o.Resources.Head)
	case "ray-worker":
		applyResourceOverrides(requests, limits, o.Resources.Worker)
	}
	return map[string]any{
		"requests": requests,
		"limits":   limits,
	}
}

func applyResourceOverrides(requests, limits map[string]any, overrides ResourceOverrides) {
	if overrides.CPURequest != "" {
		requests["cpu"] = overrides.CPURequest
		if overrides.CPULimit == "" {
			limits["cpu"] = overrides.CPURequest
		}
	}
	if overrides.MemoryRequest != "" {
		requests["memory"] = overrides.MemoryRequest
		if overrides.MemoryLimit == "" {
			limits["memory"] = overrides.MemoryRequest
		}
	}
	if overrides.CPULimit != "" {
		limits["cpu"] = overrides.CPULimit
	}
	if overrides.MemoryLimit != "" {
		limits["memory"] = overrides.MemoryLimit
	}
}

func envList(o Options, isHead bool) ([]any, error) {
	env := map[string]string{
		"TAU_DATA_DIR":       storage.DurableRoot,
		"TAU_HOT_DIR":        storage.HotRoot,
		"HF_HOME":            "/tmp/hf",
		"NCCL_DEBUG":         "WARN",
		"NCCL_SOCKET_IFNAME": "eth0",
	}

	for k, v := range o.Env {
		env[k] = v
	}

	// Execution contract vars are authoritative — set AFTER user merge so
	// they cannot be overridden by user env.
	backend := "gloo"
	if o.GPUsPerWorker > 0 {
		backend = "nccl"
	}
	numWorkers := o.Workers
	if o.GPUsPerWorker > 0 {
		numWorkers = o.Workers * o.GPUsPerWorker
	}
	env["TAU_DIST_BACKEND"] = backend
	env["TAU_NUM_WORKERS"] = strconv.Itoa(numWorkers)
	env["TAU_WORLD_SIZE"] = strconv.Itoa(numWorkers)

	if len(o.RayTrainConfig) > 0 && !isTuneLauncher(o.Launcher) {
		b, err := jsonutil.SortedMarshal(o.RayTrainConfig)
		if err != nil {
			return nil, fmt.Errorf("execution.configs: %w", err)
		}
		env["TAU_RAY_TRAIN_CONFIG_JSON"] = string(b)
	}

	if isTuneLauncher(o.Launcher) {
		moduleName, moduleErr := tuneModuleName(o)
		if moduleErr != nil {
			return nil, moduleErr
		}
		if len(o.ProjectArchive) == 0 {
			env["TAU_TUNE_TRAIN_PATH"] = filepath.ToSlash(filepath.Join(payloadTargetDir, o.ScriptName))
		}
		env["TAU_TUNE_TRAIN_MODULE"] = moduleName
		env["TAU_TUNE_METRIC"] = o.TuneMetric
		if o.TuneMode != "" {
			env["TAU_TUNE_MODE"] = o.TuneMode
		}
		if o.TuneNumSamples > 0 {
			env["TAU_TUNE_NUM_SAMPLES"] = strconv.Itoa(o.TuneNumSamples)
		}
		if o.TuneMaxConcurrentTrials > 0 {
			env["TAU_TUNE_MAX_CONCURRENT_TRIALS"] = strconv.Itoa(o.TuneMaxConcurrentTrials)
		}
		if o.TuneParamSpace != "" {
			env["TAU_TUNE_PARAM_SPACE"] = o.TuneParamSpace
		}
	}
	if isHead {
		for key, value := range o.ArtifactPublish.Env() {
			env[key] = value
		}
	}
	if o.OutputDir != "" {
		env["TAU_OUTPUT_DIR"] = o.OutputDir
	}
	if err := runconfig.ValidateLiteralEnvPayloads(env); err != nil {
		return nil, err
	}
	if o.RedactSecrets {
		o.EnvSecrets = envspec.RedactSecretRefs(o.EnvSecrets)
	}
	vars, err := envspec.Merge(envspec.FromMap(env), o.EnvSecrets)
	if err != nil {
		return nil, err
	}
	return envspec.K8sList(vars), nil
}

// volumes returns the pod-level volumes for a Ray head or worker template.
func volumes(o Options, isHead bool) []any {
	data := map[string]any{"name": "data"}
	if o.DataPVC != "" {
		data["persistentVolumeClaim"] = map[string]any{"claimName": o.DataPVC}
	} else {
		data["emptyDir"] = map[string]any{}
	}
	vols := []any{}
	if !isHead && workerNeedsSourcePayload(o) {
		vols = append(vols, map[string]any{
			"name":     "script",
			"emptyDir": map[string]any{},
		})
	}
	if isHead {
		// "script" is populated at runtime by the tau-payload initContainer
		// from the payload embedded directly in this pod spec, so the RayJob
		// no longer depends on a per-run ConfigMap being present on whichever
		// cluster the workload is dispatched to.
		vols = append(vols, map[string]any{
			"name":     "script",
			"emptyDir": map[string]any{},
		}, raylogoffload.Volume())
		if o.MetricsOffload.Enabled() {
			vols = append(vols, metricsoffload.RuntimeVolume())
		}
	}
	return append(vols,
		data,
		map[string]any{"name": "tau-hot", "emptyDir": map[string]any{}},
		map[string]any{"name": "dshm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "16Gi"}},
	)
}

// volumeMounts returns the main container's volume mounts. The script mount is
// head-only for plain Ray drivers. Project archives and Tune source are mounted
// on workers for their respective distributed loading contracts.
func volumeMounts(o Options, isHead bool) []any {
	mounts := []any{}
	if !isHead && workerNeedsSourcePayload(o) {
		mounts = append(mounts,
			map[string]any{"name": "script", "mountPath": payloadTargetDir, "readOnly": true},
		)
	}
	if isHead {
		mounts = append(mounts,
			map[string]any{"name": "script", "mountPath": payloadTargetDir, "readOnly": true},
			raylogoffload.VolumeMount(false),
		)
		if o.MetricsOffload.Enabled() {
			mounts = append(mounts, metricsoffload.RuntimeMount())
		}
	}
	return append(mounts,
		map[string]any{"name": "data", "mountPath": storage.DurableRoot},
		map[string]any{"name": "tau-hot", "mountPath": storage.HotRoot},
		map[string]any{"name": "dshm", "mountPath": "/dev/shm"},
	)
}

func workerNeedsSourcePayload(o Options) bool {
	return len(o.ProjectArchive) > 0 || isTuneLauncher(o.Launcher)
}

func stringMapFromAnyMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for key, value := range m {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

func mergeSelectors(first, second map[string]string) map[string]string {
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

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == '/', r == ':', r == '=', r == '+', r == '@':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func marshal(obj any) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(obj); err != nil {
		return nil, fmt.Errorf("yaml encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
