// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	"github.com/Azure/taugrid/cli/internal/podsecurity"
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

	rayJobTTLSecondsAfterFinished = 15
)

var (
	resourceNamePartRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	rayImageVersionRE  = regexp.MustCompile(`-ray((\d+)\.(\d+)\.\d+)`)
	migProfileRE       = regexp.MustCompile(`^\d+g\.\d+gb$`)
	pythonModuleNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const payloadTargetDir = "/script"

const projectArchiveFilename = "_tau_project.zip"

const MaxProjectArchiveBytes = 45 << 10

type Options struct {
	Name               string
	Namespace          string
	ServiceAccountName string
	ScriptName         string
	Script             []byte
	ProjectArchive     []byte
	Image              string
	Workers            int
	GPUsPerWorker      int
	Launcher           string
	GPUResourceMode    string
	MIGProfile         string
	RuntimePip         []string
	Env                map[string]string
	EnvSecrets         []envspec.Var
	RedactSecrets      bool
	SecurityMode       string
	RDMA               runconfig.NormalizedRDMA
	DataPVC            string
	Profile            profile.Profile
	TopologyOptions    topology.Options
	NodeSelector       map[string]string
	Labels             map[string]string
	Annotations        map[string]string
	Resources          Resources
	OutputDir          string
	ArtifactPublish    artifactpublish.Runtime

	CheckpointArtifact string
	MetricsOffload     metricsoffload.Runtime

	TuneMetric              string
	TuneMode                string
	TuneNumSamples          int
	TuneMaxConcurrentTrials int
	TuneParamSpace          string

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
		files = map[string][]byte{projectArchiveFilename: o.ProjectArchive}
		if isTuneLauncher(o.Launcher) {
			files[tuneDriverFilename] = []byte(tuneDriverScript)
		}
	}
	encodedPayload, payloadDigest, err := payload.Encode(payload.New(files))
	if err != nil {
		return nil, fmt.Errorf("script payload: %w", err)
	}
	topologyOptions := o.TopologyOptions
	if o.RDMA.Enabled {
		topologyOptions = topology.WithNetworkDomainRequirement(o.Profile, topologyOptions)
	}
	plan, err := topology.Build(o.Profile, topologyOptions)
	if err != nil {
		return nil, err
	}
	if plan.QueueName == "" {
		plan.QueueName = o.Profile.Queue
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
		return fmt.Errorf("gpu-resource-mode %q is not supported by the RayJob engine (supported: device-plugin, mig)", o.GPUResourceMode)
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
		"ttlSecondsAfterFinished":  int64(rayJobTTLSecondsAfterFinished),
		"submissionMode":           "HTTPMode",
		"entrypoint":               jobEntrypoint,
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
	if !isHead && o.RDMA.Enabled {
		container["securityContext"] = map[string]any{
			"runAsUser":                int64(0),
			"runAsGroup":               int64(0),
			"allowPrivilegeEscalation": false,
			"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
				"add":  []any{"IPC_LOCK", "SYS_RESOURCE", "DAC_OVERRIDE"},
			},
		}
	}
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

	grace := int64(600)
	if isHead && o.MetricsOffload.Enabled() {
		grace = metricsoffload.ShutdownGracePeriodSeconds(o.MetricsOffload.DoneTimeout, grace)
	}
	pod := map[string]any{
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": grace,
		"containers":                    containers,
		"volumes":                       volumes(o, isHead),
	}
	if o.ServiceAccountName != "" {
		pod["serviceAccountName"] = o.ServiceAccountName
	}
	if isHead {
		initContainers := []any{payloadInitContainer(image, encodedPayload, payloadDigest)}
		if o.SecurityMode != runconfig.SecurityModeRestricted {
			initContainers = append([]any{raylogoffload.PrepareInitContainer(image)}, initContainers...)
		}
		pod["initContainers"] = initContainers
	} else if workerNeedsSourcePayload(o) {
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
	if o.SecurityMode == runconfig.SecurityModeRestricted {
		group := int64(65532)
		if err := podsecurity.ApplyRestricted(pod, &group); err != nil {
			return nil, err
		}
	} else if o.SecurityMode != "" {
		return nil, fmt.Errorf("unsupported runtime security mode %q", o.SecurityMode)
	}
	return pod, nil
}

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

func rayVersionString(image string) string {
	if m := rayImageVersionRE.FindStringSubmatch(image); m != nil {
		return m[1]
	}
	return RayVersion
}

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
	b.WriteString("if [ -s /script/requirements.txt ]; then python3 -m pip install --quiet --no-cache-dir -r /script/requirements.txt || python3 -m pip install --quiet --no-cache-dir --user -r /script/requirements.txt; fi\n")
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
	if strings.TrimSpace(o.CheckpointArtifact) != "" {
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
