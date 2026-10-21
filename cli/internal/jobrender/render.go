// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobrender

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/artifactindex"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/podsecurity"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/cli/internal/storageprobe"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	c10dPort = 29500

	HeadlessSuffix = "-headless"
)

type Options struct {
	Name string

	Namespace string

	Image string

	ServiceAccountName    string
	AzureWorkloadIdentity bool

	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string

	ScriptPath string
	Source     *runconfig.Source

	WorkingDir string

	ScriptArgs []string

	CheckpointArtifact string

	Command []string

	Launcher string

	ProcessesPerNode int

	Nodes int

	ExtraFlags map[string]string

	Retry int

	TerminationGracePeriodSeconds int64

	ActiveDeadlineSeconds int64

	TTLSecondsAfterFinished int64

	PVCMount string

	Volumes      []Volume
	VolumeMounts []VolumeMount
	ImageAssets  []runconfig.ImageAsset

	Labels      map[string]string
	Annotations map[string]string

	Env           map[string]string
	EnvSecrets    []envspec.Var
	RedactSecrets bool
	SecurityMode  string

	RDMA RDMAOptions

	OutputDir string

	Profile ProfileOptions

	MetricsOffload  metricsoffload.Runtime
	ArtifactPublish artifactpublish.Runtime

	NodeSelector      map[string]string
	ClearNodeSelector bool

	Team                            string
	Lane                            string
	Mode                            string
	Topology                        string
	Shape                           string
	GPUClass                        string
	CheckpointEvery                 string
	QueueName                       string
	GPUResourceName                 string
	PriorityTier                    string
	WorkloadPriorityClassName       string
	PodPriorityClassName            string
	DisableKueueTopologyAnnotations bool
	DisableDefaultPriorities        bool
}

type Volume struct {
	Name string
	PVC  string
}

type VolumeMount struct {
	Name      string
	MountPath string
	ReadOnly  bool
}

type RDMAOptions = runconfig.NormalizedRDMA

type ProfileOptions struct {
	Mode     string
	Rank     string
	Warmup   time.Duration
	Duration time.Duration
}

func Render(p profile.Profile, o Options) ([]byte, error) {
	o.Launcher = strings.ToLower(strings.TrimSpace(o.Launcher))
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.Retry < 0 {
		return nil, fmt.Errorf("--retry must be >= 0, got %d", o.Retry)
	}

	image, err := resolveImage(p, o)
	if err != nil {
		return nil, err
	}
	cmd, env, err := resolveCommand(o)
	if err != nil {
		return nil, err
	}
	topologyOptions := topology.Options{
		Team:                            o.Team,
		Lane:                            o.Lane,
		Mode:                            o.Mode,
		Placement:                       o.Topology,
		Shape:                           o.Shape,
		GPUClass:                        o.GPUClass,
		CheckpointEvery:                 o.CheckpointEvery,
		QueueName:                       o.QueueName,
		PriorityTier:                    o.PriorityTier,
		WorkloadPriorityClassName:       o.WorkloadPriorityClassName,
		PodPriorityClassName:            o.PodPriorityClassName,
		DisableKueueTopologyAnnotations: o.DisableKueueTopologyAnnotations,
		DisableDefaultPriorities:        o.DisableDefaultPriorities,
	}
	if o.RDMA.Enabled {
		topologyOptions = topology.WithNetworkDomainRequirement(p, topologyOptions)
	}
	topologyPlan, err := topology.Build(p, topologyOptions)
	if err != nil {
		return nil, err
	}

	storagePlan, err := buildStoragePlan(o)
	if err != nil {
		return nil, err
	}
	if err := validateImageAssetStorageCollisions(storagePlan, o.ImageAssets); err != nil {
		return nil, err
	}
	if err := validateSourceStorageCollisions(storagePlan, o.Source); err != nil {
		return nil, err
	}
	if storagePlan.HasDurableData {
		env = mergeEnv(storage.RuntimeEnv(), env)
		cmd = wrapCommandWithStoragePreflight(cmd)
	}

	if o.profileOptions().Mode != "" {
		profileEnv, profileCmd, err := applyProfiler(o, cmd, storagePlan)
		if err != nil {
			return nil, err
		}
		env = mergeEnv(env, profileEnv)
		cmd = profileCmd
	}

	if o.OutputDir != "" {
		env = mergeEnv(env, map[string]string{
			"TAU_OUTPUT_DIR": o.OutputDir,
		})
	}

	if len(o.Env) > 0 {
		if err := validateUserEnv(o.Env); err != nil {
			return nil, err
		}
		env = mergeEnv(env, o.Env)
	}
	if len(o.EnvSecrets) > 0 {
		if err := validateUserEnvVars(o.EnvSecrets); err != nil {
			return nil, err
		}
	}
	if o.ArtifactPublish.Enabled() {
		if o.Nodes > 1 {
			return nil, fmt.Errorf("staged artifact publication requires a single Job pod")
		}
		if !storagePlan.HasWritableDurableData {
			return nil, fmt.Errorf("staged artifact publication requires writable durable PVC storage")
		}
		cmd, err = artifactpublish.WrapCommand(cmd, o.ArtifactPublish)
		if err != nil {
			return nil, err
		}
		env = mergeEnv(env, o.ArtifactPublish.Env())
	}
	if strings.TrimSpace(o.CheckpointArtifact) != "" {
		if !storagePlan.HasWritableDurableData {
			return nil, fmt.Errorf("storage.checkpoint requires writable durable PVC storage to write the artifact index")
		}
		cmd = artifactindex.WrapCommand(cmd, artifactindex.Config{
			Artifact:     o.CheckpointArtifact,
			Run:          o.Name,
			ResourceName: o.Name,
			Namespace:    o.Namespace,
		})
	}
	if o.MetricsOffload.Enabled() {
		if err := o.MetricsOffload.Validate(); err != nil {
			return nil, err
		}
		if o.Nodes > 1 {
			return nil, fmt.Errorf("metrics offload requires a single Job pod")
		}
		if !storagePlan.HasDurableData {
			return nil, fmt.Errorf("metrics offload requires durable PVC storage")
		}
		if !storagePlan.HasWritableDurableData {
			return nil, fmt.Errorf("metrics offload requires writable durable PVC storage")
		}
		cmd, err = metricsoffload.WrapCommand(cmd, o.MetricsOffload)
		if err != nil {
			return nil, err
		}
	}

	gpu := p.Resources.GPU
	if o.Launcher == "torchrun" && o.ProcessesPerNode > 1 && gpu.Count == 0 {
		return nil, fmt.Errorf("processes_per_node (%d) with torchrun requires a known GPU count, but the profile does not declare one", o.ProcessesPerNode)
	}
	if o.Launcher == "torchrun" && o.ProcessesPerNode > 0 && gpu.Count > 0 && o.ProcessesPerNode > gpu.Count {
		return nil, fmt.Errorf("processes_per_node (%d) exceeds profile GPU count (%d)", o.ProcessesPerNode, gpu.Count)
	}

	job, err := buildJob(p, o, image, cmd, env, topologyPlan, storagePlan, gpu)
	if err != nil {
		return nil, err
	}
	if o.Nodes > 1 {
		svc := buildHeadlessService(o)
		return marshalMultiDoc(svc, job)
	}
	return marshal(job)
}

func (o Options) validate() error {
	if o.Name == "" {
		return errors.New("Options.Name is required")
	}
	if err := runconfig.ValidateRunName(o.Name, runconfig.EngineJob, o.Nodes); err != nil {
		return err
	}
	if o.Namespace == "" {
		return errors.New("Options.Namespace is required")
	}
	if err := (runconfig.Storage{ImageAssets: o.ImageAssets}).ValidateImageAssets(); err != nil {
		return err
	}
	if err := o.Source.Validate(); err != nil {
		return err
	}
	if err := o.Source.ValidateEntrypoint(o.ScriptPath); err != nil {
		return err
	}
	if err := runconfig.ValidateContainerWorkingDir(o.WorkingDir); err != nil {
		return err
	}
	if o.Source != nil && o.WorkingDir != "" {
		return fmt.Errorf("runtime.working_dir and run.source cannot be used together; run.source fixes the container working directory at %s", runconfig.SourceMountPath)
	}
	if o.TTLSecondsAfterFinished < 0 {
		return errors.New("Options.TTLSecondsAfterFinished must be >= 0")
	}
	if o.TTLSecondsAfterFinished > runconfig.MaxTTLSecondsAfterFinished {
		return fmt.Errorf(
			"Options.TTLSecondsAfterFinished must be <= %d (Kubernetes int32 maximum)",
			runconfig.MaxTTLSecondsAfterFinished,
		)
	}
	switch strings.ToLower(o.Launcher) {
	case "", "python", "torchrun":
	default:
		return fmt.Errorf("Options.Launcher %q: must be one of python|torchrun (or empty)", o.Launcher)
	}
	if o.ProcessesPerNode > 1 && o.Launcher != "torchrun" {
		return fmt.Errorf("processes_per_node > 1 requires launcher=torchrun (got launcher=%q)", o.Launcher)
	}
	if o.ProcessesPerNode < 0 {
		return fmt.Errorf("processes_per_node must be >= 0 (got %d)", o.ProcessesPerNode)
	}
	if o.Nodes < 0 {
		return fmt.Errorf("nodes must be >= 0 (got %d)", o.Nodes)
	}
	if o.Nodes > 1 && o.Launcher != "torchrun" {
		return fmt.Errorf("nodes > 1 requires launcher=torchrun (got launcher=%q)", o.Launcher)
	}
	if o.Launcher == "torchrun" {
		reserved := []string{"MASTER_ADDR", "MASTER_PORT"}
		if o.Nodes > 1 {
			reserved = append(reserved, "JOB_COMPLETION_INDEX")
		}
		for _, key := range reserved {
			if _, ok := o.Env[key]; ok {
				return fmt.Errorf("%s is managed by torchrun and must not be set in runtime.env", key)
			}
			for _, s := range o.EnvSecrets {
				if s.Name == key {
					return fmt.Errorf("%s is managed by torchrun and must not be set in runtime.env_secret", key)
				}
			}
		}
	}
	prof := o.profileOptions()
	if o.Launcher == "torchrun" && prof.Mode != "" {
		return fmt.Errorf("--launcher=torchrun cannot be combined with --profiler=%s (torchrun manages worker processes; profiler wrapping is not compatible)", prof.Mode)
	}
	switch prof.Mode {
	case "", "ncu", "nsys":
	default:
		return fmt.Errorf("Options.Profile.Mode %q: must be one of ncu|nsys (or empty)", prof.Mode)
	}
	if prof.Rank != "" && prof.Rank != "all" {
		rank, err := strconv.Atoi(prof.Rank)
		if err != nil || rank < 0 {
			return fmt.Errorf("Options.Profile.Rank %q: must be a non-negative integer or all", prof.Rank)
		}
	}
	if prof.Warmup < 0 {
		return fmt.Errorf("Options.Profile.Warmup must be >= 0")
	}
	if prof.Duration < 0 {
		return fmt.Errorf("Options.Profile.Duration must be >= 0")
	}
	return nil
}

func (o Options) profileOptions() ProfileOptions {
	prof := o.Profile
	prof.Mode = strings.TrimSpace(strings.ToLower(prof.Mode))
	prof.Rank = strings.TrimSpace(strings.ToLower(prof.Rank))
	if prof.Rank == "" {
		prof.Rank = "0"
	}
	return prof
}

func resolveImage(p profile.Profile, o Options) (string, error) {
	if o.Image != "" {
		return o.Image, nil
	}
	if p.Runtime.Image != "" {
		return p.Runtime.Image, nil
	}
	return "", fmt.Errorf("no image: profile %q declares no runtime image and --image was not set", p.Name)
}

func RequirePythonEntrypoint(scriptPath string) error {
	scriptPath = strings.TrimSpace(scriptPath)
	if scriptPath == "" || strings.HasSuffix(scriptPath, ".py") {
		return nil
	}
	return fmt.Errorf("run.entrypoint %q must be a .py file with execution.launcher: torchrun, which runs it under python3; point it at a Python entrypoint or drop the launcher", scriptPath)
}

func resolveCommand(o Options) ([]string, map[string]string, error) {
	if len(o.Command) > 0 {
		if o.Launcher == "torchrun" {
			return nil, nil, fmt.Errorf("--launcher=torchrun cannot be combined with an explicit command override")
		}
		return o.Command, nil, nil
	}
	if o.Source != nil {
		return resolveSourceCommand(o)
	}
	if o.ScriptPath != "" {
		data, err := os.ReadFile(o.ScriptPath)
		if err != nil {
			return nil, nil, fmt.Errorf("reading --script %s: %w", o.ScriptPath, err)
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		var argList strings.Builder
		for _, a := range o.ScriptArgs {
			argList.WriteString(" ")
			argList.WriteString(shellQuote(a))
		}

		env := map[string]string{
			"TAU_SCRIPT_B64": b64,
			"TAU_SCRIPT_SRC": filepath.Base(o.ScriptPath),
		}

		extraFlagStr := renderExtraFlags(o.ExtraFlags)

		if o.Launcher == "torchrun" {
			if err := RequirePythonEntrypoint(o.ScriptPath); err != nil {
				return nil, nil, err
			}
			nproc := max(o.ProcessesPerNode, 1)
			nodes := max(o.Nodes, 1)

			var torchArgs string
			if nodes > 1 {
				headlessHost := o.Name + "-0." + o.Name + HeadlessSuffix
				torchArgs = fmt.Sprintf("--nnodes=%d --node_rank=$JOB_COMPLETION_INDEX --rdzv-backend=c10d --rdzv-endpoint=%s:%d --rdzv-id=%s --nproc_per_node=%d",
					nodes, headlessHost, c10dPort, o.Name, nproc)
				env["MASTER_ADDR"] = headlessHost
				env["MASTER_PORT"] = strconv.Itoa(c10dPort)
			} else {
				torchArgs = fmt.Sprintf("--standalone --nproc_per_node=%d", nproc)
				env["MASTER_ADDR"] = "localhost"
				env["MASTER_PORT"] = strconv.Itoa(c10dPort)
			}
			torchArgs += extraFlagStr

			entrypoint := []string{
				"bash", "-c",
				fmt.Sprintf(`echo "$TAU_SCRIPT_B64" | base64 -d > /tmp/tau-entrypoint.py && exec python3 -m torch.distributed.run %s /tmp/tau-entrypoint.py%s`, torchArgs, argList.String()),
			}
			if nproc > 1 || nodes > 1 {
				env["OMP_NUM_THREADS"] = "1"
			}
			return entrypoint, env, nil
		}

		runWithPython, err := shouldRunScriptWithPython(o.ScriptPath, data, o.Launcher)
		if err != nil {
			return nil, nil, err
		}
		if runWithPython {
			return []string{
				"bash", "-c",
				`echo "$TAU_SCRIPT_B64" | base64 -d > /tmp/tau-entrypoint.py && exec python3 /tmp/tau-entrypoint.py` + argList.String() + extraFlagStr,
			}, env, nil
		}
		return []string{
			"bash", "-c",
			`echo "$TAU_SCRIPT_B64" | base64 -d > /tmp/run.sh && chmod +x /tmp/run.sh && exec /tmp/run.sh` + argList.String() + extraFlagStr,
		}, env, nil
	}
	if o.Launcher == "torchrun" {
		return nil, nil, fmt.Errorf("--launcher=torchrun requires --script (no script or command was provided)")
	}
	return nil, nil, nil
}

func resolveSourceCommand(o Options) ([]string, map[string]string, error) {
	entrypoint := path.Join(runconfig.SourceMountPath, o.ScriptPath)
	var argList strings.Builder
	for _, a := range o.ScriptArgs {
		argList.WriteString(" ")
		argList.WriteString(shellQuote(a))
	}
	extraFlagStr := renderExtraFlags(o.ExtraFlags)

	if o.Launcher == "torchrun" {
		if err := RequirePythonEntrypoint(o.ScriptPath); err != nil {
			return nil, nil, err
		}
		nproc := max(o.ProcessesPerNode, 1)
		nodes := max(o.Nodes, 1)
		env := map[string]string{}
		var torchArgs string
		if nodes > 1 {
			headlessHost := o.Name + "-0." + o.Name + HeadlessSuffix
			torchArgs = fmt.Sprintf("--nnodes=%d --node_rank=$JOB_COMPLETION_INDEX --rdzv-backend=c10d --rdzv-endpoint=%s:%d --rdzv-id=%s --nproc_per_node=%d",
				nodes, headlessHost, c10dPort, o.Name, nproc)
			env["MASTER_ADDR"] = headlessHost
			env["MASTER_PORT"] = strconv.Itoa(c10dPort)
		} else {
			torchArgs = fmt.Sprintf("--standalone --nproc_per_node=%d", nproc)
			env["MASTER_ADDR"] = "localhost"
			env["MASTER_PORT"] = strconv.Itoa(c10dPort)
		}
		if nproc > 1 || nodes > 1 {
			env["OMP_NUM_THREADS"] = "1"
		}
		return []string{
			"bash", "-c",
			fmt.Sprintf("exec python3 -m torch.distributed.run %s%s %s%s", torchArgs, extraFlagStr, shellQuote(entrypoint), argList.String()),
		}, env, nil
	}

	if o.Launcher == "python" || strings.EqualFold(filepath.Ext(o.ScriptPath), ".py") {
		return []string{
			"bash", "-c",
			"exec python3 " + shellQuote(entrypoint) + argList.String() + extraFlagStr,
		}, nil, nil
	}
	return []string{
		"bash", "-c",
		"exec " + shellQuote(entrypoint) + argList.String() + extraFlagStr,
	}, nil, nil
}

func shouldRunScriptWithPython(path string, data []byte, launcher string) (bool, error) {
	shebang, hasShebang := parseShebang(data)
	if hasShebang && shebang == "" {
		return false, fmt.Errorf("script %q has an empty shebang; add an interpreter after #!", path)
	}
	if launcher == "python" {
		if hasShebang && !isPythonShebang(shebang) {
			return false, fmt.Errorf("script %q has a non-Python shebang but execution.launcher is python", path)
		}
		return true, nil
	}
	if hasShebang {
		return false, nil
	}
	if strings.EqualFold(filepath.Ext(path), ".py") {
		return true, nil
	}
	return false, fmt.Errorf(
		"script %q has no shebang and is not a .py file; add a shebang or set execution.launcher: python",
		path,
	)
}

func parseShebang(data []byte) (string, bool) {
	if !strings.HasPrefix(string(data), "#!") {
		return "", false
	}
	line := string(data[2:])
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return strings.TrimSpace(line), true
}

func isPythonShebang(shebang string) bool {
	fields := strings.Fields(shebang)
	if len(fields) == 0 {
		return false
	}
	command := filepath.Base(fields[0])
	if command == "env" {
		command = ""
		for i := 1; i < len(fields); i++ {
			token := fields[i]
			switch token {
			case "-u", "--unset", "-C", "--chdir":
				i++
				continue
			}
			if strings.HasPrefix(token, "-") || strings.Contains(token, "=") {
				continue
			}
			command = filepath.Base(token)
			break
		}
	}
	command = strings.ToLower(command)
	return command == "python" || command == "python3" || strings.HasPrefix(command, "python3.")
}

func buildJob(p profile.Profile, o Options, image string, cmd []string, extraEnv map[string]string, topologyPlan topology.Plan, storagePlan storagePlan, gpu profile.GPUContract) (map[string]any, error) {
	if err := topology.ValidateGPUClassNodeSelector(topologyPlan.Labels[workloadmeta.LabelGPUClass], o.NodeSelector); err != nil {
		return nil, err
	}
	labels := map[string]any{}
	for k, v := range o.Labels {
		if k != "" && v != "" {
			labels[k] = v
		}
	}
	for k, v := range topologyPlan.Labels {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			labels[k] = v
		}
	}
	for k, v := range gpu.Labels() {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			labels[k] = v
		}
	}
	queueName := topologyPlan.QueueName
	if queueName == "" {
		queueName = p.Queue
	}
	if strings.TrimSpace(queueName) == "" {
		return nil, fmt.Errorf("render suspended Job: Kueue LocalQueue is required")
	}
	labels[topology.QueueLabel] = queueName
	labels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue

	pod := map[string]any{
		"restartPolicy": "Never",
	}
	if o.ServiceAccountName != "" {
		pod["serviceAccountName"] = o.ServiceAccountName
	}
	grace := int64(600)
	if o.TerminationGracePeriodSeconds > 0 {
		grace = o.TerminationGracePeriodSeconds
	}
	if o.MetricsOffload.Enabled() {
		grace = metricsoffload.ShutdownGracePeriodSeconds(o.MetricsOffload.DoneTimeout, grace)
	}
	pod["terminationGracePeriodSeconds"] = grace
	if tols := gpuTolerations(gpu.Count); len(tols) > 0 {
		pod["tolerations"] = tols
	}
	if topologyPlan.PodPriorityClassName != "" {
		pod["priorityClassName"] = topologyPlan.PodPriorityClassName
	}
	if o.ClearNodeSelector {
		delete(pod, "nodeSelector")
	}
	if len(topologyPlan.NodeSelector) > 0 {
		selector := map[string]any{}
		if existing, ok := pod["nodeSelector"].(map[string]any); ok {
			for k, v := range existing {
				selector[k] = v
			}
		}
		for k, v := range topologyPlan.NodeSelector {
			if k != "" && v != "" {
				selector[k] = v
			}
		}
		pod["nodeSelector"] = selector
	}
	if len(o.NodeSelector) > 0 {
		selector := map[string]any{}
		if existing, ok := pod["nodeSelector"].(map[string]any); ok {
			for k, v := range existing {
				selector[k] = v
			}
		}
		for k, v := range o.NodeSelector {
			if k != "" && v != "" {
				selector[k] = v
			}
		}
		pod["nodeSelector"] = selector
	}
	container := map[string]any{
		"name":  "main",
		"image": image,
	}
	if p.Runtime.ImagePullPolicy != "" {
		container["imagePullPolicy"] = p.Runtime.ImagePullPolicy
	}
	if o.RDMA.Enabled {
		sc := map[string]any{}
		if p.Runtime.SecurityContext != nil {
			for k, v := range p.Runtime.SecurityContext {
				sc[k] = v
			}
		}
		if runAsNonRoot, _ := sc["runAsNonRoot"].(bool); runAsNonRoot {
			return nil, fmt.Errorf("RDMA requires runAsUser=0 and conflicts with profile securityContext runAsNonRoot=true")
		}
		sc["runAsUser"] = int64(0)
		sc["runAsGroup"] = int64(0)
		sc["allowPrivilegeEscalation"] = false
		if _, exists := sc["seccompProfile"]; !exists {
			sc["seccompProfile"] = map[string]any{"type": "RuntimeDefault"}
		}
		sc["capabilities"] = map[string]any{
			"drop": []any{"ALL"},
			"add":  []any{"IPC_LOCK", "SYS_RESOURCE", "DAC_OVERRIDE"},
		}
		container["securityContext"] = sc
	} else if p.Runtime.SecurityContext != nil {
		container["securityContext"] = p.Runtime.SecurityContext
	}
	if len(cmd) > 0 {
		container["command"] = cmd
	}

	envList, err := buildEnvList(p, extraEnv, o.EnvSecrets, o.RedactSecrets)
	if err != nil {
		return nil, err
	}
	if len(envList) > 0 {
		container["env"] = envList
	}

	resources := map[string]any{}
	if p.Resources.Requests != nil {
		resources["requests"] = p.Resources.Requests
	}
	if p.Resources.Limits != nil {
		resources["limits"] = p.Resources.Limits
	}
	applyContainerResourceOverrides(resources, o)
	profile.AddGPUResources(resources, gpu.Count)
	if o.Launcher == "torchrun" && o.Nodes > 1 {
		addIntegerResource(resources, workloadmeta.ResourceTorchrunHostSlot, 1)
	}
	if len(resources) > 0 {
		container["resources"] = resources
	}
	if len(storagePlan.Volumes) > 0 {
		volumes := make([]any, 0, len(storagePlan.Volumes))
		for _, v := range storagePlan.Volumes {
			if v.PVC != "" {
				volumes = append(volumes, map[string]any{
					"name": v.Name,
					"persistentVolumeClaim": map[string]any{
						"claimName": v.PVC,
					},
				})
				continue
			}
			volumes = append(volumes, map[string]any{
				"name":     v.Name,
				"emptyDir": map[string]any{},
			})
		}
		pod["volumes"] = volumes

		mounts := make([]any, 0, len(storagePlan.VolumeMounts))
		for _, vm := range storagePlan.VolumeMounts {
			m := map[string]any{"name": vm.Name, "mountPath": vm.MountPath}
			if vm.ReadOnly {
				m["readOnly"] = true
			}
			mounts = append(mounts, m)
		}
		container["volumeMounts"] = mounts
	}
	if o.WorkingDir != "" {
		container["workingDir"] = o.WorkingDir
	} else if storagePlan.HasDurableData {
		container["workingDir"] = storage.DurableRoot
	}

	if o.Launcher == "torchrun" && (o.ProcessesPerNode > 1 || o.Nodes > 1) && !hasVolume(pod, "dshm") {
		shmSize := "16Gi"
		if o.RDMA.Enabled {
			shmSize = runconfig.DefaultRDMAShmSize
		}
		shmVol := map[string]any{
			"name":     "dshm",
			"emptyDir": map[string]any{"medium": "Memory", "sizeLimit": shmSize},
		}
		shmMount := map[string]any{"name": "dshm", "mountPath": "/dev/shm"}
		if existing, ok := pod["volumes"].([]any); ok {
			pod["volumes"] = append(existing, shmVol)
		} else {
			pod["volumes"] = []any{shmVol}
		}
		if existing, ok := container["volumeMounts"].([]any); ok {
			container["volumeMounts"] = append(existing, shmMount)
		} else {
			container["volumeMounts"] = []any{shmMount}
		}
	}
	applyImageAssets(pod, container, o.ImageAssets)
	applySource(pod, container, o.Source, o.ScriptPath)

	containers := []any{container}
	if o.MetricsOffload.Enabled() {
		runtimeVolume := metricsoffload.RuntimeVolume()
		if existing, ok := pod["volumes"].([]any); ok {
			pod["volumes"] = append(existing, runtimeVolume)
		} else {
			pod["volumes"] = []any{runtimeVolume}
		}

		runtimeMount := metricsoffload.RuntimeMount()
		if existing, ok := container["volumeMounts"].([]any); ok {
			container["volumeMounts"] = append(existing, runtimeMount)
		} else {
			container["volumeMounts"] = []any{runtimeMount}
		}
		containers = append(containers, metricsoffload.BuildContainer(o.MetricsOffload, metricsOffloadMounts(storagePlan)))
	}
	pod["containers"] = containers
	if o.SecurityMode == runconfig.SecurityModeRestricted {
		if err := podsecurity.ApplyRestricted(pod, nil); err != nil {
			return nil, err
		}
	} else if o.SecurityMode != "" {
		return nil, fmt.Errorf("unsupported runtime security mode %q", o.SecurityMode)
	}

	podLabels := map[string]any{}
	for k, v := range o.Labels {
		if k != "" && v != "" {
			podLabels[k] = v
		}
	}
	for k, v := range topologyPlan.Labels {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			podLabels[k] = v
		}
	}
	for k, v := range gpu.Labels() {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			podLabels[k] = v
		}
	}
	podLabels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue
	if o.AzureWorkloadIdentity && o.ServiceAccountName != "" {
		podLabels[workloadmeta.LabelAzureWorkloadIdentityUse] = "true"
	}
	podMetadata := map[string]any{
		"labels": podLabels,
	}
	correlationAnnotations := workloadmeta.PodCorrelationAnnotations(o.Annotations)
	if len(correlationAnnotations) > 0 || len(topologyPlan.Annotations) > 0 || len(gpu.Annotations()) > 0 {
		podAnnotations := map[string]any{}
		for k, v := range correlationAnnotations {
			podAnnotations[k] = v
		}
		for k, v := range topologyPlan.Annotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				podAnnotations[k] = v
			}
		}
		for k, v := range gpu.Annotations() {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				podAnnotations[k] = v
			}
		}
		if len(podAnnotations) > 0 {
			podMetadata["annotations"] = podAnnotations
		}
	}
	if o.Nodes > 1 {
		pod["subdomain"] = o.Name + HeadlessSuffix
	}
	if o.Launcher == "torchrun" && o.Nodes > 1 {
		pod["affinity"] = torchrunHostAntiAffinity(o.Name)
	}

	jobSpec := map[string]any{
		"suspend":                 true,
		"backoffLimit":            int64(o.Retry),
		"ttlSecondsAfterFinished": int64(28800),
		"template": map[string]any{
			"metadata": podMetadata,
			"spec":     pod,
		},
	}
	if o.TTLSecondsAfterFinished > 0 {
		jobSpec["ttlSecondsAfterFinished"] = o.TTLSecondsAfterFinished
	}
	if o.Nodes > 1 {
		jobSpec["completionMode"] = "Indexed"
		jobSpec["completions"] = int64(o.Nodes)
		jobSpec["parallelism"] = int64(o.Nodes)
	}
	if p.ActiveDeadlineSeconds > 0 {
		jobSpec["activeDeadlineSeconds"] = p.ActiveDeadlineSeconds
	}
	if o.ActiveDeadlineSeconds > 0 {
		jobSpec["activeDeadlineSeconds"] = o.ActiveDeadlineSeconds
	}

	metadata := map[string]any{
		"name":      o.Name,
		"namespace": o.Namespace,
		"labels":    labels,
	}
	hasExecution := o.Launcher == "torchrun"
	checkpointArtifact := strings.TrimSpace(o.CheckpointArtifact)
	if len(o.Annotations) > 0 || len(topologyPlan.Annotations) > 0 || len(gpu.Annotations()) > 0 || hasExecution || checkpointArtifact != "" {
		annotations := map[string]any{}
		if checkpointArtifact != "" {
			annotations[workloadmeta.AnnotationCheckpointArtifact] = checkpointArtifact
		}
		for k, v := range o.Annotations {
			if k != "" && v != "" {
				annotations[k] = v
			}
		}
		for k, v := range topologyPlan.Annotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				annotations[k] = v
			}
		}
		for k, v := range gpu.Annotations() {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				annotations[k] = v
			}
		}
		if hasExecution {
			nproc := max(o.ProcessesPerNode, 1)
			nodes := max(o.Nodes, 1)
			annotations[workloadmeta.AnnotationSpecExecution] = fmt.Sprintf(`{"launcher":%q,"processes_per_node":%d,"nodes":%d}`, o.Launcher, nproc, nodes)
		}
		if len(annotations) > 0 {
			metadata["annotations"] = annotations
		}
	}

	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   metadata,
		"spec":       jobSpec,
	}, nil
}

type storagePlan struct {
	Volumes                []Volume
	VolumeMounts           []VolumeMount
	HasDurableData         bool
	HasWritableDurableData bool
}

func applyImageAssets(pod, container map[string]any, assets []runconfig.ImageAsset) {
	if len(assets) == 0 {
		return
	}

	initContainers, _ := pod["initContainers"].([]any)
	volumes, _ := pod["volumes"].([]any)
	mounts, _ := container["volumeMounts"].([]any)
	for _, asset := range assets {
		name := "tau-asset-" + asset.Name
		initContainers = append(initContainers, map[string]any{
			"name":    name,
			"image":   asset.Image,
			"command": []string{"/bin/cp"},
			"args":    []string{"-a", "--", asset.SourcePath + "/.", "/tau-asset/"},
			"volumeMounts": []any{map[string]any{
				"name": name, "mountPath": "/tau-asset",
			}},
		})
		volumes = append(volumes, map[string]any{
			"name": name, "emptyDir": map[string]any{},
		})
		mounts = append(mounts, map[string]any{
			"name": name, "mountPath": asset.MountPath, "readOnly": true,
		})
	}
	pod["initContainers"] = initContainers
	pod["volumes"] = volumes
	container["volumeMounts"] = mounts
}

func applySource(pod, container map[string]any, source *runconfig.Source, entrypoint string) {
	if source == nil {
		return
	}
	const stagingPath = "/tau-source"
	initContainers, _ := pod["initContainers"].([]any)
	volumes, _ := pod["volumes"].([]any)
	mounts, _ := container["volumeMounts"].([]any)
	initContainers = append(initContainers, map[string]any{
		"name":    "tau-source",
		"image":   source.Image,
		"command": []string{"/bin/sh"},
		"args": []string{
			"-c",
			`cp -R -- "$1"/. /tau-source/ && chmod -R a+rwX /tau-source && chmod a+x "/tau-source/$2"`,
			"tau-source",
			source.Path,
			entrypoint,
		},
		"volumeMounts": []any{map[string]any{
			"name": "tau-source", "mountPath": stagingPath,
		}},
	})
	volumes = append(volumes, map[string]any{
		"name": "tau-source", "emptyDir": map[string]any{},
	})
	mounts = append(mounts, map[string]any{
		"name": "tau-source", "mountPath": runconfig.SourceMountPath,
	})
	pod["initContainers"] = initContainers
	pod["volumes"] = volumes
	container["volumeMounts"] = mounts
	container["workingDir"] = runconfig.SourceMountPath
}

func validateSourceStorageCollisions(plan storagePlan, source *runconfig.Source) error {
	if source == nil {
		return nil
	}
	if plan.hasVolumeName("tau-source") {
		return fmt.Errorf("run.source generates volume name %q already used by storage", "tau-source")
	}
	for _, mount := range plan.VolumeMounts {
		if mountPathsOverlap(runconfig.SourceMountPath, mount.MountPath) {
			return fmt.Errorf("run.source mount path %q overlaps storage mount %q", runconfig.SourceMountPath, mount.MountPath)
		}
	}
	return nil
}

func validateImageAssetStorageCollisions(plan storagePlan, assets []runconfig.ImageAsset) error {
	for _, asset := range assets {
		name := "tau-asset-" + asset.Name
		if plan.hasVolumeName(name) {
			return fmt.Errorf("storage.image_assets[%s] generates volume name %q already used by storage", asset.Name, name)
		}
		for _, mount := range plan.VolumeMounts {
			if mountPathsOverlap(asset.MountPath, mount.MountPath) {
				return fmt.Errorf("storage.image_assets[%s] mount_path %q overlaps storage mount %q", asset.Name, asset.MountPath, mount.MountPath)
			}
		}
	}
	return nil
}

func mountPathsOverlap(a, b string) bool {
	a = path.Clean(a)
	b = path.Clean(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func metricsOffloadMounts(storage storagePlan) []metricsoffload.Mount {
	mounts := make([]metricsoffload.Mount, 0, len(storage.VolumeMounts))
	for _, mount := range storage.VolumeMounts {
		mounts = append(mounts, metricsoffload.Mount{
			Name:     mount.Name,
			Path:     mount.MountPath,
			ReadOnly: mount.ReadOnly,
		})
	}
	return mounts
}

func applyContainerResourceOverrides(resources map[string]any, o Options) {
	requests := copyResourceQuantities(resources["requests"])
	limits := copyResourceQuantities(resources["limits"])
	if value := strings.TrimSpace(o.CPURequest); value != "" {
		requests["cpu"] = value
	}
	if value := strings.TrimSpace(o.MemoryRequest); value != "" {
		requests["memory"] = value
	}
	if value := strings.TrimSpace(o.CPULimit); value != "" {
		limits["cpu"] = value
	}
	if value := strings.TrimSpace(o.MemoryLimit); value != "" {
		limits["memory"] = value
	}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
}

func addIntegerResource(resources map[string]any, name string, value int) {
	for _, key := range []string{"requests", "limits"} {
		values := copyResourceQuantities(resources[key])
		values[name] = value
		resources[key] = values
	}
}

func torchrunHostAntiAffinity(jobName string) map[string]any {
	return map[string]any{
		"podAntiAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": []any{
				map[string]any{
					"labelSelector": map[string]any{
						"matchLabels": map[string]any{
							"batch.kubernetes.io/job-name": jobName,
						},
					},
					"topologyKey": "kubernetes.io/hostname",
				},
			},
		},
	}
}

func copyResourceQuantities(existing any) map[string]any {
	out := map[string]any{}
	if values, ok := existing.(map[string]any); ok {
		for key, value := range values {
			out[key] = value
		}
	}
	return out
}

func buildStoragePlan(o Options) (storagePlan, error) {
	var plan storagePlan
	if o.PVCMount != "" {
		plan.removeMountPath(storage.DurableRoot)
		plan.removeVolumeName("data")
		if err := plan.addVolumeMount(Volume{Name: "data", PVC: o.PVCMount}, VolumeMount{Name: "data", MountPath: storage.DurableRoot}); err != nil {
			return storagePlan{}, err
		}
	}

	for _, v := range o.Volumes {
		if err := plan.addVolume(v); err != nil {
			return storagePlan{}, err
		}
	}
	for _, vm := range o.VolumeMounts {
		if err := plan.addMount(vm); err != nil {
			return storagePlan{}, err
		}
	}
	if err := plan.validateReferences(); err != nil {
		return storagePlan{}, err
	}

	for _, vm := range plan.VolumeMounts {
		if vm.MountPath == storage.DurableRoot {
			plan.HasDurableData = true
			plan.HasWritableDurableData = !vm.ReadOnly
			break
		}
	}
	if plan.HasDurableData && !plan.hasMountPath(storage.HotRoot) {
		if err := plan.addVolumeMount(
			Volume{Name: "tau-hot"},
			VolumeMount{Name: "tau-hot", MountPath: storage.HotRoot},
		); err != nil {
			return storagePlan{}, err
		}
	}
	return plan, nil
}

func gpuTolerations(count int) []any {
	if count <= 0 {
		return nil
	}
	return []any{
		map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
		map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
	}
}

func isGeneratedTauMetadataKey(key string) bool {
	return strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelGPUClass
}

func (p *storagePlan) addVolumeMount(v Volume, vm VolumeMount) error {
	if err := p.addVolume(v); err != nil {
		return err
	}
	return p.addMount(vm)
}

func (p *storagePlan) addVolume(v Volume) error {
	if v.Name == "" {
		return errors.New("volume name is required")
	}
	if v.PVC == "" && v.Name != "tau-hot" {
		return fmt.Errorf("volume %q: PVC is required", v.Name)
	}
	if p.hasVolumeName(v.Name) {
		return fmt.Errorf("volume %q declared more than once", v.Name)
	}
	p.Volumes = append(p.Volumes, v)
	return nil
}

func (p *storagePlan) addMount(vm VolumeMount) error {
	if vm.Name == "" {
		return errors.New("mount volume name is required")
	}
	if vm.MountPath == "" {
		return fmt.Errorf("mount %q: mountPath is required", vm.Name)
	}
	if !strings.HasPrefix(vm.MountPath, "/") {
		return fmt.Errorf("mount %q: mountPath must be absolute", vm.Name)
	}
	if p.hasMountPath(vm.MountPath) {
		return fmt.Errorf("mountPath %q declared more than once", vm.MountPath)
	}
	p.VolumeMounts = append(p.VolumeMounts, vm)
	return nil
}

func (p *storagePlan) validateReferences() error {
	for _, vm := range p.VolumeMounts {
		if !p.hasVolumeName(vm.Name) {
			return fmt.Errorf("mount %q references unknown volume %q", vm.MountPath, vm.Name)
		}
	}
	return nil
}

func (p *storagePlan) hasVolumeName(name string) bool {
	for _, v := range p.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (p *storagePlan) hasMountPath(path string) bool {
	for _, vm := range p.VolumeMounts {
		if vm.MountPath == path {
			return true
		}
	}
	return false
}

func (p *storagePlan) removeVolumeName(name string) {
	filtered := p.Volumes[:0]
	for _, v := range p.Volumes {
		if v.Name != name {
			filtered = append(filtered, v)
		}
	}
	p.Volumes = filtered
}

func (p *storagePlan) removeMountPath(path string) {
	filtered := p.VolumeMounts[:0]
	for _, vm := range p.VolumeMounts {
		if vm.MountPath != path {
			filtered = append(filtered, vm)
		}
	}
	p.VolumeMounts = filtered
}

func mergeEnv(first, second map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range first {
		out[k] = v
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

func wrapCommandWithStoragePreflight(cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}
	return append([]string{"bash", "-lc", storageprobe.Script() + "\nexec \"$@\"", "tau-entrypoint"}, cmd...)
}

func validateUserEnv(env map[string]string) error {
	for k := range env {
		if runconfig.ReservedTauEnvKey(k) {
			return fmt.Errorf("--env: %w", runconfig.ReservedTauEnvKeyError(k))
		}
	}
	return nil
}

func validateUserEnvVars(vars []envspec.Var) error {
	if err := envspec.Validate(vars); err != nil {
		return err
	}
	for _, v := range vars {
		if runconfig.ReservedTauEnvKey(v.Name) {
			return fmt.Errorf("env var: %w", runconfig.ReservedTauEnvKeyError(v.Name))
		}
	}
	return nil
}

func applyProfiler(o Options, cmd []string, storagePlan storagePlan) (map[string]string, []string, error) {
	if len(cmd) == 0 {
		return nil, nil, fmt.Errorf("--profiler requires --script or --command (image-only entrypoints can't be wrapped)")
	}
	prof := o.profileOptions()
	ext := ""
	tool := ""
	switch prof.Mode {
	case "ncu":
		ext = "ncu-rep"
		tool = "ncu"
	case "nsys":
		ext = "nsys-rep"
		tool = "nsys"
	default:
		return nil, nil, fmt.Errorf("--profiler %q: must be ncu|nsys", prof.Mode)
	}
	outRoot := o.OutputDir
	if outRoot == "" && storagePlan.HasDurableData {
		outRoot = storage.DurableRoot + "/" + o.Name
	}
	if outRoot == "" {
		return nil, nil, fmt.Errorf("--profiler requires a durable output path; mount a PVC with --volume/--mount or declare profile persistence so artifacts survive the pod")
	}
	outDir := strings.TrimRight(outRoot, "/") + "/profile"
	outFile := outDir + "/rank-" + prof.Rank + "." + ext
	envOut := map[string]string{
		"TAU_PROFILE_MODE":       prof.Mode,
		"TAU_PROFILE_TOOL":       tool,
		"TAU_PROFILE_RANK":       prof.Rank,
		"TAU_PROFILE_OUT_DIR":    outDir,
		"TAU_PROFILE_RUN_ID":     o.Name,
		"TAU_PROFILE_NAMESPACE":  o.Namespace,
		"TAU_PROFILE_EXT":        ext,
		"TAU_PROFILE_WARMUP_SEC": profileDurationSeconds(prof.Warmup),
		"TAU_PROFILE_ACTIVE_SEC": profileDurationSeconds(prof.Duration),
	}
	if prof.Rank == "all" {
		envOut["TAU_PROFILE_OUT_PATTERN"] = outDir + "/rank-<rank>." + ext
	} else {
		envOut["TAU_PROFILE_OUT"] = outFile
	}
	wrapper := profileModeWrapperScript()
	wrapped := append([]string{"bash", "-lc", wrapper, "tau-entrypoint"}, cmd...)
	return envOut, wrapped, nil
}

func profileDurationSeconds(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	sec := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(sec, 10)
}

func profileModeWrapperScript() string {
	return `
tau_profile_rank() {
  for name in TAU_RANK RANK WORLD_RANK OMPI_COMM_WORLD_RANK PMI_RANK LOCAL_RANK; do
    value="${!name:-}"
    if [ -n "$value" ]; then
      printf '%s\n' "$value"
      return 0
    fi
  done
  printf '0\n'
}
tau_profile_json_escape() {
  local s="$1"
  local out="" ch escaped code i
  for ((i=0; i<${#s}; i++)); do
    ch="${s:i:1}"
    case "$ch" in
      $'\\') out="${out}\\\\"
        ;;
      '"') out="${out}\\\""
        ;;
      $'\n') out="${out}\\n"
        ;;
      $'\r') out="${out}\\r"
        ;;
      $'\t') out="${out}\\t"
        ;;
      $'\b') out="${out}\\b"
        ;;
      $'\f') out="${out}\\f"
        ;;
      [[:cntrl:]])
        printf -v code '%d' "'$ch"
        printf -v escaped '\\u%04x' "$code"
        out="${out}${escaped}"
        ;;
      *) out="${out}${ch}"
        ;;
    esac
  done
  printf '%s' "$out"
}
tau_profile_write_metadata() {
  local path="$1"
  local status="$2"
  local target_status="$3"
  local completion_reason="$4"
  local error_msg="$5"
  local completed_at="$6"
  local export_status="$7"
  local command_json tool_json host_json reason_json error_json export_json
  command_json="$(tau_profile_json_escape "$TAU_PROFILE_COMMAND")"
  tool_json="$(tau_profile_json_escape "$TAU_PROFILE_TOOL_VERSION")"
  host_json="$(tau_profile_json_escape "$TAU_PROFILE_HOST")"
  reason_json="$(tau_profile_json_escape "$completion_reason")"
  error_json="$(tau_profile_json_escape "$error_msg")"
  export_json="$(tau_profile_json_escape "$export_status")"
  cat > "${path}.tmp" <<EOF
{
  "schema_version": 1,
  "run_id": "$(tau_profile_json_escape "$TAU_PROFILE_RUN_ID")",
  "namespace": "$(tau_profile_json_escape "$TAU_PROFILE_NAMESPACE")",
  "mode": "$(tau_profile_json_escape "$TAU_PROFILE_MODE")",
  "rank": "$(tau_profile_json_escape "$TAU_PROFILE_RUNTIME_RANK")",
  "rank_filter": "$(tau_profile_json_escape "$TAU_PROFILE_RANK")",
  "host": "$host_json",
  "command": "$command_json",
  "warmup_seconds": $TAU_PROFILE_WARMUP_SEC,
  "active_seconds": $TAU_PROFILE_ACTIVE_SEC,
  "tool_version": "$tool_json",
  "raw_profile_path": "$(tau_profile_json_escape "$TAU_PROFILE_RAW")",
  "sqlite_path": "$(tau_profile_json_escape "$TAU_PROFILE_SQLITE")",
  "summary_path": "$(tau_profile_json_escape "$TAU_PROFILE_SUMMARY")",
  "started_at": "$(tau_profile_json_escape "$TAU_PROFILE_STARTED_AT")",
  "completed_at": "$(tau_profile_json_escape "$completed_at")",
  "exit_status": $status,
  "target_exit_status": $target_status,
  "completion_reason": "$reason_json",
  "export_status": "$export_json",
  "error": "$error_json"
}
EOF
  mv "${path}.tmp" "$path"
}
tau_profile_write_summary() {
  local path="$1"
  local status="$2"
  local target_status="$3"
  local completion_reason="$4"
  local error_msg="$5"
  local completed_at="$6"
  local export_status="$7"
  cat > "${path}.tmp" <<EOF
# Tau profile artifact

- run_id: $TAU_PROFILE_RUN_ID
- namespace: $TAU_PROFILE_NAMESPACE
- mode: $TAU_PROFILE_MODE
- rank: $TAU_PROFILE_RUNTIME_RANK (filter: $TAU_PROFILE_RANK)
- host: $TAU_PROFILE_HOST
- command: $TAU_PROFILE_COMMAND
- warmup_seconds: $TAU_PROFILE_WARMUP_SEC
- active_seconds: $TAU_PROFILE_ACTIVE_SEC
- tool_version: $TAU_PROFILE_TOOL_VERSION
- raw_profile_path: $TAU_PROFILE_RAW
- sqlite_path: $TAU_PROFILE_SQLITE
- metadata_path: $TAU_PROFILE_METADATA
- started_at: $TAU_PROFILE_STARTED_AT
- completed_at: $completed_at
- exit_status: $status
- target_exit_status: $target_status
- completion_reason: $completion_reason
- export_status: $export_status
- error: $error_msg
EOF
  mv "${path}.tmp" "$path"
}

TAU_PROFILE_RUNTIME_RANK="$(tau_profile_rank)"
if [ "$TAU_PROFILE_RANK" != "all" ] && [ "$TAU_PROFILE_RUNTIME_RANK" != "$TAU_PROFILE_RANK" ]; then
  echo "tau profiler: skipping rank $TAU_PROFILE_RUNTIME_RANK (target rank $TAU_PROFILE_RANK)" >&2
  exec "$@"
fi
case "$TAU_PROFILE_RUNTIME_RANK" in
  *[!A-Za-z0-9_.-]*|"") TAU_PROFILE_SAFE_RANK="unknown" ;;
  *) TAU_PROFILE_SAFE_RANK="$TAU_PROFILE_RUNTIME_RANK" ;;
esac
mkdir -p "$TAU_PROFILE_OUT_DIR"
TAU_PROFILE_BASE="$TAU_PROFILE_OUT_DIR/rank-$TAU_PROFILE_SAFE_RANK"
TAU_PROFILE_RAW="$TAU_PROFILE_BASE.$TAU_PROFILE_EXT"
TAU_PROFILE_SQLITE=""
if [ "$TAU_PROFILE_MODE" = "nsys" ]; then
  TAU_PROFILE_SQLITE="$TAU_PROFILE_BASE.sqlite"
fi
TAU_PROFILE_METADATA="$TAU_PROFILE_BASE.metadata.json"
TAU_PROFILE_SUMMARY="$TAU_PROFILE_BASE.summary.md"
TAU_PROFILE_HOST="${HOSTNAME:-}"
if [ -z "$TAU_PROFILE_HOST" ] && [ -r /etc/hostname ]; then
  IFS= read -r TAU_PROFILE_HOST < /etc/hostname || true
fi
TAU_PROFILE_HOST="${TAU_PROFILE_HOST:-unknown}"
TAU_PROFILE_COMMAND="$(printf '%q ' "$@")"
TAU_PROFILE_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if ! command -v "$TAU_PROFILE_TOOL" >/dev/null 2>&1; then
  TAU_PROFILE_TOOL_VERSION=""
  completed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  err="tau --profiler=$TAU_PROFILE_MODE: $TAU_PROFILE_TOOL not found in image"
  echo "$err" >&2
  mkdir -p "$TAU_PROFILE_OUT_DIR"
  tau_profile_write_metadata "$TAU_PROFILE_METADATA" 127 127 "tool-not-found" "$err" "$completed_at" "not-run" || true
  tau_profile_write_summary "$TAU_PROFILE_SUMMARY" 127 127 "tool-not-found" "$err" "$completed_at" "not-run" || true
  exit 127
fi
TAU_PROFILE_TOOL_VERSION="$($TAU_PROFILE_TOOL --version 2>&1 | { IFS= read -r line || true; printf '%s' "$line"; } || true)"

orig=( "$@" )
case "$TAU_PROFILE_MODE" in
  ncu)
    profiler=( ncu --target-processes all -f -o "$TAU_PROFILE_BASE" )
    ;;
  nsys)
    profiler=( nsys profile --force-overwrite=true -o "$TAU_PROFILE_BASE" )
    if [ "$TAU_PROFILE_WARMUP_SEC" != "0" ]; then
      profiler+=( --delay "$TAU_PROFILE_WARMUP_SEC" )
    fi
    if [ "$TAU_PROFILE_ACTIVE_SEC" != "0" ]; then
      profiler+=( --duration "$TAU_PROFILE_ACTIVE_SEC" )
    fi
    ;;
esac
"${profiler[@]}" -- "${orig[@]}"
target_status=$?
status=$target_status
completed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
export_status="not-run"
if [ "$TAU_PROFILE_MODE" = "nsys" ] && [ -f "$TAU_PROFILE_RAW" ]; then
  if nsys export --type sqlite --force-overwrite=true --output "$TAU_PROFILE_SQLITE" "$TAU_PROFILE_RAW" >/dev/null 2>&1; then
    export_status="ok"
  else
    export_status="failed"
    echo "tau profiler: nsys sqlite export failed for $TAU_PROFILE_RAW; raw .nsys-rep is still available" >&2
  fi
fi
completion_reason="target-exited"
if [ "$TAU_PROFILE_MODE" = "nsys" ] && [ "$TAU_PROFILE_ACTIVE_SEC" != "0" ] && [ "$target_status" -eq 143 ] && [ "$export_status" = "ok" ] && [ -f "$TAU_PROFILE_RAW" ]; then
  status=0
  completion_reason="nsys-duration-capture-complete"
fi
if ! tau_profile_write_metadata "$TAU_PROFILE_METADATA" "$status" "$target_status" "$completion_reason" "" "$completed_at" "$export_status"; then
  echo "tau profiler: failed to write metadata $TAU_PROFILE_METADATA" >&2
  if [ "$status" -eq 0 ]; then status=1; fi
fi
if ! tau_profile_write_summary "$TAU_PROFILE_SUMMARY" "$status" "$target_status" "$completion_reason" "" "$completed_at" "$export_status"; then
  echo "tau profiler: failed to write summary $TAU_PROFILE_SUMMARY" >&2
  if [ "$status" -eq 0 ]; then status=1; fi
fi
exit "$status"
`
}

func buildEnvList(p profile.Profile, extra map[string]string, secrets []envspec.Var, redactSecrets bool) ([]any, error) {
	merged := map[string]string{
		"TAU_PROFILE": p.Name,
	}
	for k, v := range extra {
		merged[k] = v
	}
	if err := runconfig.ValidateLiteralEnvPayloads(merged); err != nil {
		return nil, err
	}
	if redactSecrets {
		secrets = envspec.RedactSecretRefs(secrets)
	}
	vars, err := envspec.Merge(envspec.FromMap(merged), secrets)
	if err != nil {
		return nil, err
	}
	return envspec.K8sList(vars), nil
}

func hasVolume(pod map[string]any, name string) bool {
	vols, ok := pod["volumes"].([]any)
	if !ok {
		return false
	}
	for _, v := range vols {
		if m, ok := v.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
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
			r == '_', r == '-', r == '.', r == '/', r == ':', r == '=', r == '+':
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

func renderExtraFlags(flags map[string]string) string {
	if len(flags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(" --")
		b.WriteString(k)
		if v := flags[k]; v != "" {
			b.WriteString("=")
			b.WriteString(shellQuote(v))
		}
	}
	return b.String()
}

func marshal(obj any) ([]byte, error) {
	return marshalMultiDoc(obj)
}

func marshalMultiDoc(objects ...any) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, obj := range objects {
		if err := enc.Encode(obj); err != nil {
			return nil, fmt.Errorf("yaml encode: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func buildHeadlessService(o Options) map[string]any {
	metadata := map[string]any{
		"name":      o.Name + HeadlessSuffix,
		"namespace": o.Namespace,
		"labels": map[string]any{
			workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
		},
	}
	if submissionID := o.Annotations[workloadmeta.AnnotationSubmissionID]; submissionID != "" {
		metadata["annotations"] = map[string]any{
			workloadmeta.AnnotationSubmissionID: submissionID,
		}
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   metadata,
		"spec": map[string]any{
			"clusterIP":                "None",
			"publishNotReadyAddresses": true,
			"selector": map[string]any{
				"batch.kubernetes.io/job-name": o.Name,
			},
			"ports": []any{
				map[string]any{
					"name":     "c10d",
					"port":     int64(c10dPort),
					"protocol": "TCP",
				},
			},
		},
	}
}
