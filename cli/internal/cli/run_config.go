// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/runconfig"
)

var defaultRunConfigFiles = runconfig.DefaultFiles

type runInputDiscovery struct {
	ConfigPath     string
	ExplicitConfig bool
	BuiltinSmoke   bool
}

func discoverRunInput(startDir, explicit, target string) (runInputDiscovery, error) {
	if strings.TrimSpace(startDir) == "" {
		var err error
		startDir, err = os.Getwd()
		if err != nil {
			return runInputDiscovery{}, fmt.Errorf("get current directory: %w", err)
		}
	}
	if strings.TrimSpace(explicit) != "" {
		checkPath := explicit
		if !filepath.IsAbs(checkPath) {
			checkPath = filepath.Join(startDir, checkPath)
		}
		if _, err := os.Stat(checkPath); err != nil {
			return runInputDiscovery{}, fmt.Errorf("--config %s: %w", explicit, err)
		}
		return runInputDiscovery{ConfigPath: filepath.Clean(explicit), ExplicitConfig: true}, nil
	}
	target = strings.TrimSpace(target)
	if target == "smoke" {
		return runInputDiscovery{BuiltinSmoke: true}, nil
	}
	if target != "" {
		if err := validateRunTargetName(target); err != nil {
			return runInputDiscovery{}, err
		}
		var matches []string
		for _, extension := range []string{".yaml", ".yml"} {
			candidate := filepath.Join(startDir, "tau", target+extension)
			if _, err := os.Stat(candidate); err == nil {
				matches = append(matches, candidate)
			} else if !os.IsNotExist(err) {
				return runInputDiscovery{}, fmt.Errorf("check %s: %w", candidate, err)
			}
		}
		if len(matches) > 1 {
			return runInputDiscovery{}, fmt.Errorf(
				"run target %q has both tau/%s.yaml and tau/%s.yml; keep exactly one extension",
				target,
				target,
				target,
			)
		}
		if len(matches) == 1 {
			return runInputDiscovery{ConfigPath: matches[0]}, nil
		}
		return runInputDiscovery{}, fmt.Errorf("run target %q not found; expected tau/%s.yaml or pass --config", target, target)
	}
	for _, candidate := range defaultRunConfigFiles {
		path := filepath.Join(startDir, candidate)
		if _, err := os.Stat(path); err == nil {
			return runInputDiscovery{ConfigPath: path}, nil
		} else if !os.IsNotExist(err) {
			return runInputDiscovery{}, fmt.Errorf("check %s: %w", path, err)
		}
	}
	return runInputDiscovery{}, nil
}

func discoverRunConfig(explicit string) (string, bool, error) {
	discovery, err := discoverRunInput("", explicit, "")
	if err != nil {
		return "", strings.TrimSpace(explicit) != "", err
	}
	return discovery.ConfigPath, discovery.ExplicitConfig, nil
}

func loadRunConfig(path string) (unresolvedRunOptions, string, error) {
	_, options, name, warnings, err := readRunConfig(path)
	if err != nil {
		return unresolvedRunOptions{}, "", err
	}
	emitConfigWarnings(os.Stderr, warnings)
	return options, name, nil
}

// emitConfigWarnings surfaces non-fatal config diagnostics. Unknown keys in a
// managed manifest are ignored by the parser, so this is the only signal the
// author gets that a directive they wrote is doing nothing.
func emitConfigWarnings(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintln(w, warning)
	}
}

func readRunConfig(path string) (runconfig.Config, unresolvedRunOptions, string, []string, error) {
	cfg, warnings, err := runconfig.LoadWithDiagnostics(path)
	if err != nil {
		return runconfig.Config{}, unresolvedRunOptions{}, "", nil, err
	}
	engine := firstNonEmpty(cfg.Run.Engine, cfg.Engine)
	if err := cfg.ValidateExecution(engine); err != nil {
		return runconfig.Config{}, unresolvedRunOptions{}, "", nil, err
	}
	opts, err := configToDispatch(cfg, path)
	if err != nil {
		return runconfig.Config{}, unresolvedRunOptions{}, "", nil, err
	}
	return cfg, opts, firstNonEmpty(cfg.Run.Name, cfg.Name), warnings, nil
}

func configToDispatch(c runconfig.Config, configPath string) (unresolvedRunOptions, error) {
	o := defaultRunDispatchOptions()
	baseDir := filepath.Dir(configPath)
	o.engine = firstNonEmpty(c.Run.Engine, c.Engine)
	entrypoint := firstNonEmpty(c.Run.Entrypoint, c.Run.Script, c.Entrypoint, c.Script)
	if c.Run.Source != nil {
		source := *c.Run.Source
		o.source = &source
		o.script = strings.TrimSpace(entrypoint)
	} else {
		o.script = configRelativePath(baseDir, entrypoint)
	}
	if c.Run.TTLSecondsAfterFinished != nil {
		o.ttlSecondsAfterFinished = *c.Run.TTLSecondsAfterFinished
	}
	if mainScript := firstNonEmpty(c.Run.MainScript, c.Workflow.MainScript, c.Workflow.Script); mainScript != "" {
		o.mainScript = configRelativePath(baseDir, mainScript)
	} else {
		o.mainScript = o.script
	}
	o.image = firstNonEmpty(c.Runtime.Image, c.Run.Image, c.Image)
	o.file = configRelativePath(baseDir, c.Workflow.File)
	if o.file == "" && c.LooksLikeManagedWorkflow() {
		o.file = configPath
	}

	o.namespace = c.Policy.Namespace
	o.workspace = c.Policy.Workspace
	o.profileName = c.Policy.Profile
	o.queue = c.Policy.Queue
	o.preset = c.Policy.Preset
	o.team = c.Policy.Team
	o.lane = c.Policy.Lane
	o.gpuClass = c.Policy.GPUClass
	o.mode = c.Policy.Mode
	o.topology = c.Policy.Topology
	o.shape = c.Policy.Shape
	o.priorityTier = firstNonEmpty(c.Policy.PriorityTier, c.Policy.Priority)
	o.topologyPolicy = c.Policy.TopologyPolicy
	o.workloadPriorityClass = c.Policy.WorkloadPriorityClass
	o.podPriorityClass = c.Policy.PodPriorityClass
	o.nodeSelectors = mapToKeyValueList(c.Policy.NodeSelector)
	o.clearNodeSelector = c.Policy.ClearNodeSelector
	o.disableDefaultPriorities = c.Policy.DisableDefaultPriorities

	o.dataPVC = c.Storage.DataPVC
	o.resultPVC = c.Storage.ResultPVC
	o.output = c.Storage.Output
	o.outputPublish = c.Storage.Publish
	o.checkpointArtifact = c.Storage.Checkpoint
	o.volumeSpecs = append([]string{}, c.Storage.Volumes...)
	o.mountSpecs = append([]string{}, c.Storage.Mounts...)
	o.imageAssets = append([]runconfig.ImageAsset{}, c.Storage.ImageAssets...)

	if c.Compute.Workers != nil {
		o.workers = *c.Compute.Workers
	}
	if c.Compute.GPUs != nil {
		gpus := *c.Compute.GPUs
		o.jobGPUs = &gpus
	}
	if c.Compute.GPUsPerWorker != nil {
		o.gpusPerWorker = *c.Compute.GPUsPerWorker
		o.gpusPerWorkerExplicit = true
	}
	if c.Compute.CPUWorkers != nil {
		o.cpuWorkers = *c.Compute.CPUWorkers
	}
	o.workloadKind = firstNonEmpty(c.Workflow.WorkloadKind, c.Run.WorkloadKind, c.Compute.WorkloadKind)
	o.gpuResourceMode = c.Compute.GPUResourceMode
	o.migProfile = c.Compute.MIGProfile
	o.cpuRequest = c.Compute.CPURequest
	o.memoryRequest = c.Compute.MemoryRequest
	o.cpuLimit = c.Compute.CPULimit
	o.memoryLimit = c.Compute.MemoryLimit
	o.headCPURequest = c.Compute.HeadCPURequest
	o.headMemoryRequest = c.Compute.HeadMemRequest
	o.headCPULimit = c.Compute.HeadCPULimit
	o.headMemoryLimit = c.Compute.HeadMemLimit
	o.workerCPURequest = c.Compute.WorkerCPUReq
	o.workerMemoryRequest = c.Compute.WorkerMemReq
	o.workerCPULimit = c.Compute.WorkerCPULimit
	o.workerMemoryLimit = c.Compute.WorkerMemLimit
	o.runtimePip = append([]string{}, c.Runtime.Pip...)
	o.env = mapToKeyValueList(c.Runtime.Env)
	o.envSecrets = mapToKeyValueList(c.Runtime.EnvSecret)
	o.envKV = mapToKeyValueList(c.Runtime.EnvKV)

	o.upstreamCheckpoint = c.Workflow.UpstreamCheckpoint
	o.secretPayloadPath = configRelativePath(baseDir, c.Workflow.SecretPayload)
	o.extraScripts = configRelativeExtraScripts(baseDir, c.Workflow.ExtraScripts)
	o.configDir = baseDir
	workingDir, err := resolveConfigWorkingDirectory(c, baseDir)
	if err != nil {
		return unresolvedRunOptions{}, err
	}
	o.workingDir = workingDir
	if c.Run.SmokePairs != nil {
		o.smokePairs = *c.Run.SmokePairs
	}
	if c.Workflow.SmokePairs != nil {
		o.smokePairs = *c.Workflow.SmokePairs
	}

	o.profiler = c.Profiler.Mode
	o.profileRank = c.Profiler.Rank
	o.profileWarmup = c.Profiler.Warmup
	o.profileDuration = c.Profiler.Duration
	o.experiment = runConfigExperimentMetadata(c.Experiment)
	if !c.LooksLikeManagedWorkflow() {
		o.metricsHistory = append([]string{}, c.Metrics.History...)
		o.metricsOffloadEnabled = c.Metrics.Offload.Enabled
	}

	if c.Execution.Launcher != nil {
		o.launcher = *c.Execution.Launcher
	}
	if c.Execution.ProcessesPerNode != nil {
		o.processesPerNode = *c.Execution.ProcessesPerNode
	}
	if c.Execution.Nodes != nil {
		o.nodes = *c.Execution.Nodes
	}
	o.tuneMetric = c.Execution.Metric
	o.tuneMode = c.Execution.Mode
	if c.Execution.NumSamples != nil {
		o.tuneNumSamples = *c.Execution.NumSamples
	}
	if c.Execution.MaxConcurrentTrials != nil {
		o.tuneMaxConcurrentTrials = *c.Execution.MaxConcurrentTrials
	}
	if len(c.Execution.Configs) > 0 {
		o.configs = c.Execution.Configs
		// For ray-tune, also pre-serialize to tuneParamSpace for the CLI wire.
		effectiveLauncher := o.launcher
		if effectiveLauncher == "" && strings.EqualFold(o.engine, "ray") {
			effectiveLauncher = "ray-train"
		}
		if effectiveLauncher == "ray-tune" {
			raw, err := json.Marshal(c.Execution.Configs)
			if err != nil {
				return unresolvedRunOptions{}, fmt.Errorf("execution.configs: %w", err)
			}
			o.tuneParamSpace = string(raw)
		}
	}
	o.allowNCCLOverride = c.Execution.AllowNCCLOverride

	o.maxRetries = c.Resilience.MaxRetries
	o.retryOn = append([]string{}, c.Resilience.RetryOn...)
	o.checkpointPath = c.Resilience.CheckpointPath
	if c.Resilience.BackoffInitial.Duration > 0 {
		o.backoffInitial = c.Resilience.BackoffInitial.Duration
	} else {
		o.backoffInitial = 30 * time.Second
	}
	if c.Resilience.BackoffMax.Duration > 0 {
		o.backoffMax = c.Resilience.BackoffMax.Duration
	} else {
		o.backoffMax = 5 * time.Minute
	}
	if len(o.retryOn) == 0 && o.maxRetries > 0 {
		o.retryOn = []string{"Preempted", "Evicted"}
	}

	return o, nil
}

func runConfigExperimentMetadata(e runconfig.Experiment) runExperimentMetadata {
	return runconfig.ExperimentRunMetadata(e)
}

func resolveConfigWorkingDirectory(c runconfig.Config, baseDir string) (dispatchWorkingDir, error) {
	rayProjectDir := strings.TrimSpace(c.Run.WorkingDir)
	jobContainerDir := strings.TrimSpace(c.Runtime.WorkingDir)
	if rayProjectDir != "" && jobContainerDir != "" {
		return dispatchWorkingDir{}, fmt.Errorf(
			"runtime.working_dir and run.working_dir cannot be used together; runtime.working_dir sets a Job container path while run.working_dir ships a local project through Ray",
		)
	}
	if jobContainerDir != "" {
		return jobContainerWorkingDirectory(c.Runtime.WorkingDir), nil
	}
	if rayProjectDir != "" {
		return rayProjectWorkingDirectory(
			configRelativePath(baseDir, c.Run.WorkingDir),
			c.Run.WorkingDirExcludes,
		), nil
	}
	return dispatchWorkingDir{}, nil
}

func configRelativePath(baseDir, value string) string {
	if strings.TrimSpace(value) == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func defaultRunOutputPath(name string) string {
	return path.Join(storage.DurableCheckpointsDir, "workflows", name)
}

func configRelativeExtraScripts(baseDir string, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		pathPart, dest, hasDest := strings.Cut(value, ":")
		resolved := configRelativePath(baseDir, pathPart)
		if hasDest {
			resolved += ":" + dest
		}
		out = append(out, resolved)
	}
	return out
}

func mapToKeyValueList(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}
