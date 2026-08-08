package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/secretpreflight"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type runJobRequest struct {
	Name    string
	Options runDispatchOptions
}

const (
	directMetricsReadyFile    = "/var/run/tau/metrics-ready"
	directMetricsDoneFile     = "/var/run/tau/metrics-done"
	directMetricsReadyTimeout = 2 * time.Minute
	directMetricsDoneTimeout  = 2 * time.Minute
)

func newRunJobRequest(options runDispatchOptions, name string) (runJobRequest, error) {
	if strings.TrimSpace(name) == "" {
		return runJobRequest{}, fmt.Errorf("job runs require NAME")
	}
	if err := validateRunJobStorageConfig(options); err != nil {
		return runJobRequest{}, err
	}
	if strings.TrimSpace(options.workingDir) != "" {
		// working_dir is delivered through Ray's runtime_env, which a plain
		// Kubernetes Job has no equivalent of. Accepting it silently here
		// would suppress the submit-time import check and hand the user back
		// the ModuleNotFoundError-after-startup failure it exists to prevent.
		return runJobRequest{}, fmt.Errorf(
			"run.working_dir is not supported with engine: job.\n" +
				"It is delivered through Ray's runtime_env, so it applies to engine: ray only.\n" +
				"Either switch to engine: ray to ship the project directory, or remove run.working_dir " +
				"and keep the run to a single self-contained entrypoint.")
	}
	if options.output == "" && firstNonEmpty(options.dataPVC, options.resultPVC) != "" {
		options.output = defaultRunOutputPath(name)
	}
	if options.metricsOffloadEnabled && strings.TrimSpace(options.metricsSessionID) == "" {
		sessionID, err := newMetricsSessionID()
		if err != nil {
			return runJobRequest{}, err
		}
		options.metricsSessionID = sessionID
	}
	if options.metricsOffloadEnabled {
		if err := validateMetricsSessionID(options.metricsSessionID); err != nil {
			return runJobRequest{}, err
		}
	}
	if err := ensureArtifactPublicationID(&options); err != nil {
		return runJobRequest{}, err
	}
	return runJobRequest{Name: name, Options: options}, nil
}

func executeRunJob(ctx context.Context, stdout, stderr io.Writer, request *runJobRequest, captureCommand string) error {
	if err := ensureSubmissionID(&request.Options); err != nil {
		return err
	}
	o := request.Options
	kubeContext := firstNonEmpty(o.kubeContext, defaultKubeContext())
	if o.dryRun != "" && o.dryRun != "client" && o.dryRun != "server" {
		return fmt.Errorf("--dry-run must be one of: client, server")
	}
	if o.script != "" {
		if _, err := os.Stat(o.script); err != nil {
			return fmt.Errorf("run.entrypoint: %w", err)
		}
	}

	profiler, err := validateProfiler(o.profiler)
	if err != nil {
		return err
	}
	profileWarmup, err := parseRunJobDuration("profiler.warmup", o.profileWarmup)
	if err != nil {
		return err
	}
	profileDuration, err := parseRunJobDuration("profiler.duration", o.profileDuration)
	if err != nil {
		return err
	}
	if profiler != "nsys" {
		if strings.TrimSpace(o.profileWarmup) != "" {
			return fmt.Errorf("profiler.warmup currently applies only to profiler.mode=nsys")
		}
		if strings.TrimSpace(o.profileDuration) != "" {
			return fmt.Errorf("profiler.duration currently applies only to profiler.mode=nsys")
		}
	} else if strings.TrimSpace(o.profileDuration) == "" {
		profileDuration = 2 * time.Minute
	}

	topo := runJobTopologyFlags(o)
	resolvedProfileName, preset, warnings, err := topo.resolvePreset(o.profileName)
	if err != nil {
		return err
	}
	if resolvedProfileName == "" && preset == nil {
		resolvedProfileName = "direct-job"
	}

	jobGPUCount, err := resolveDirectJobGPUCount(o.jobGPUs, topo.shape, preset)
	if err != nil {
		return err
	}
	p := resourceProfileForRender(resolvedProfileName, preset, topo.resourceProfileOptions(), jobGPUCount)
	namespaceExplicit := strings.TrimSpace(o.namespace) != ""
	ns := o.namespace
	if ns == "" {
		ns = defaultRenderNamespace
	}
	pvcMount, volumes, volumeMounts, err := resolveRunJobStorage(o)
	if err != nil {
		return err
	}
	env, err := parseEnvKV(o.env)
	if err != nil {
		return err
	}
	envSecrets, err := parseEnvSecretKV(o.envSecrets)
	if err != nil {
		return err
	}
	nodeSelector, err := parseRunJobNodeSelectors(o.nodeSelectors)
	if err != nil {
		return err
	}
	annotations, outputDir, outputWritable, err := buildRunJobOutputAnnotations(request.Name, o.output, pvcMount, volumes, volumeMounts, p)
	if err != nil {
		return err
	}
	artifactPublication := artifactpublish.Runtime{
		Mode:          o.outputPublish,
		OutputDir:     outputDir,
		StagingDir:    path.Join(storage.HotRoot, "tau-output", request.Name),
		PublicationID: o.artifactPublicationID,
	}
	if artifactPublication.Enabled() {
		if !outputWritable {
			return fmt.Errorf("storage.publish=staged requires storage.output backed by a writable PVC")
		}
		if err := artifactPublication.Validate(); err != nil {
			return err
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[workloadmeta.AnnotationArtifactPublication] = artifactPublication.Mode
		annotations[workloadmeta.AnnotationArtifactPublicationID] = artifactPublication.PublicationID
	}
	profileOptions := jobrender.ProfileOptions{
		Mode:     profiler,
		Rank:     firstNonEmpty(o.profileRank, "0"),
		Warmup:   profileWarmup,
		Duration: profileDuration,
	}
	if profiler != "" {
		if annotations == nil {
			annotations = map[string]string{}
		}
		if outputDir == "" || annotations[workloadmeta.AnnotationResultPVC] == "" {
			return fmt.Errorf("profiler requires durable PVC output; configure storage.data_pvc, storage.volumes/mounts, or profile spec.resources.persistence")
		}
		for k, v := range buildRunJobProfileAnnotations(outputDir, annotations[workloadmeta.AnnotationResultPVC], profileOptions) {
			annotations[k] = v
		}
	}

	opts := jobrender.Options{
		Name:                  request.Name,
		Namespace:             ns,
		Image:                 o.image,
		ServiceAccountName:    o.serviceAccountName,
		AzureWorkloadIdentity: o.azureWorkloadIdentity,
		ScriptPath:            o.script,
		Launcher:              o.launcher,
		ProcessesPerNode:      o.processesPerNode,
		Nodes:                 o.nodes,
		ExtraFlags:            stringifyConfigs(o.configs),
		CPURequest:            o.cpuRequest,
		MemoryRequest:         o.memoryRequest,
		CPULimit:              o.cpuLimit,
		MemoryLimit:           o.memoryLimit,
		PVCMount:              pvcMount,
		Volumes:               volumes,
		VolumeMounts:          volumeMounts,
		Env:                   env,
		EnvSecrets:            envSecrets,
		RedactSecrets:         o.dryRun == "client",
		Profile:               profileOptions,
		NodeSelector:          nodeSelector,
		ClearNodeSelector:     o.clearNodeSelector,
		OutputDir:             outputDir,
		ArtifactPublish:       artifactPublication,
		CheckpointArtifact:    o.checkpointArtifact,
		Annotations:           annotations,
	}
	topoWarnings, err := topo.applyWithChangedAndWorkspaceQueue(&opts, preset, func(flag string) bool {
		return runJobTopologyFieldSet(o, flag)
	}, o.workspaceQueueResolved)
	if err != nil {
		return err
	}
	if jobGPUCount > 0 {
		configureGPUQueueModeWithChanged("device-plugin", &opts, func(string) bool { return false })
	}
	if o.workspaceQueueResolved {
		makeWorkspaceQueueAuthoritative(&opts)
	}
	opts.GPUClass, _ = runtopology.ResolveGPUClass(p, opts.GPUClass)
	warnings = append(warnings, topoWarnings...)

	var runner *kube.Runner
	if o.dryRun != "client" {
		runner = kube.New(kubeContext)
	}
	allowImplicitAuto := preset == nil && resolvedProfileName == ""
	resolveWarnings, err := resolveAccessibleQueueNamespace(ctx, runner, namespaceExplicit, &ns, &opts, o.dryRun, "jobs.batch", allowImplicitAuto)
	if err != nil {
		return err
	}
	if o.dryRun == "client" {
		// Queue and namespace resolution both need a live cluster, so a client
		// dry-run can never carry the real values. jobrender fails closed on an
		// empty queue to stop permanently-suspended Jobs from being submitted;
		// substitute self-describing placeholders so the offline render still
		// works and no reader mistakes them for what the submit path resolves.
		var unresolved []string
		if strings.TrimSpace(opts.QueueName) == "" {
			opts.QueueName = clientDryRunQueuePlaceholder
			unresolved = append(unresolved, "queue")
		}
		if strings.TrimSpace(ns) == "" || (!namespaceExplicit && ns == defaultRenderNamespace) {
			ns = clientDryRunNamespacePlaceholder
			unresolved = append(unresolved, "namespace")
		}
		if len(unresolved) > 0 {
			warnings = append(warnings, clientDryRunPlaceholderWarning(unresolved...))
		}
	}
	ns, err = requireWorkloadNamespace(ns)
	if err != nil {
		return err
	}
	opts.Namespace = ns
	request.Options.namespace = ns
	warnings = append(warnings, resolveWarnings...)
	explicitAuto, implicitAuto := prepareAutoQueueRender(&opts, preset, allowImplicitAuto, o.dryRun)
	if o.dryRun != "client" {
		if err := secretpreflight.ValidateRequiredEnv(ctx, runner, ns, envSecrets); err != nil {
			return err
		}
	}
	if o.metricsOffloadEnabled {
		runtime, err := resolveMetricsOffload(o, p, request.Name, ns, kubeContext, outputDir, outputWritable, annotations)
		if err != nil {
			return err
		}
		opts.MetricsOffload = runtime
		if opts.Annotations == nil {
			opts.Annotations = map[string]string{}
		}
		opts.Annotations[experiment.AnnotationExperimentSource] = "stellar"
	}
	artifactBundle, err := resolveArtifactBundle(
		request.Name,
		ns,
		o.submissionID,
		outputDir,
		annotations[workloadmeta.AnnotationResultPVC],
		outputWritable,
		artifactPublication,
		opts.MetricsOffload,
		o.metricsSessionID,
		o.checkpointArtifact,
	)
	if err != nil {
		return err
	}
	if artifactBundle.Enabled() && strings.TrimSpace(o.script) == "" {
		warnings = append(warnings,
			"tau: complete bundle acknowledgement is unavailable for Jobs that use the image ENTRYPOINT/CMD")
		artifactBundle = artifactbundle.Runtime{}
	}
	opts.ArtifactBundle = artifactBundle
	if artifactBundle.Enabled() && opts.Nodes > 1 {
		warnings = append(warnings,
			"tau: complete bundle acknowledgement is unavailable for multi-node Indexed Jobs; no shared completion marker will be emitted")
		opts.ArtifactBundle = artifactbundle.Runtime{}
		artifactBundle = artifactbundle.Runtime{}
	}
	if artifactBundle.Enabled() {
		if opts.Annotations == nil {
			opts.Annotations = map[string]string{}
		}
		opts.Annotations[workloadmeta.AnnotationArtifactBundleID] = artifactBundle.BundleID
	}
	if o.dryRun == "" && !opts.DisableDefaultPriorities {
		disabled, warning := autoDisableMissingDefaultPriorities(ctx, runner)
		if disabled {
			opts.DisableDefaultPriorities = true
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}

	capture, err := buildJobCaptureMetadata(
		ctx,
		captureCommand,
		request.Name,
		ns,
		o.image,
		o.script,
		pvcMount,
		p,
		volumes,
		volumeMounts,
	)
	if err != nil {
		return err
	}
	capture = addRunWorkspaceMetadata(capture, o.workspace, o.workspaceResultScope)
	opts.Labels, opts.Annotations = experiment.MergeMetadata(opts.Labels, opts.Annotations, capture)
	opts.Labels = workloadmeta.StampWorkspace(opts.Labels, o.workspace)
	if o.submissionID != "" {
		opts.Annotations[workloadmeta.AnnotationSubmissionID] = o.submissionID
	}
	if o.metricsOffloadEnabled {
		opts.Annotations[workloadmeta.AnnotationMetricsSession] = o.metricsSessionID
	}
	manifest, err := jobrender.Render(p, opts)
	if err != nil {
		return err
	}
	autoWarnings, err := topo.resolveAutoQueueFromManifest(ctx, runner, ns, &opts, manifest, o.dryRun, explicitAuto, implicitAuto)
	if err != nil {
		return err
	}
	if explicitAuto || implicitAuto {
		manifest, err = jobrender.Render(p, opts)
		if err != nil {
			return err
		}
	}
	warnings = append(warnings, autoWarnings...)
	if o.dryRun != "client" {
		if err := validateRenderedQueue(ctx, kube.New(kubeContext), ns, manifest, opts, queueValidationPolicyFor(preset, o.workspaceQueueResolved)); err != nil {
			return err
		}
	}
	for _, warning := range warnings {
		fmt.Fprintln(stderr, warning)
	}

	if o.dryRun == "client" {
		_, err := stdout.Write(manifest)
		return err
	}
	if runner == nil {
		runner = kube.New(kubeContext)
	}
	if o.nodes > 1 && o.dryRun == "" {
		if err := createMultiNodeRunJobManifest(ctx, newKubernetesRunSubmissionRunner(runner), manifest, ns, request.Name, o.submissionID, stdout); err != nil {
			return err
		}
	} else {
		result, err := submitRunWorkload(ctx, runner, runSubmission{
			Resource:     "job",
			Name:         request.Name,
			Namespace:    ns,
			SubmissionID: o.submissionID,
			Manifest:     manifest,
			DryRun:       o.dryRun,
		})
		fmt.Fprint(stdout, result.Output)
		if err != nil {
			return err
		}
	}

	if o.dryRun == "" {
		fmt.Fprint(stdout, formatRunJobSubmission(request.Name, resolvedProfileName, ns, kubeContext))
		if profiler != "" {
			fmt.Fprintf(stdout, "profile artifacts: stored under %s on PVC %s\n", outputDir, annotations[workloadmeta.AnnotationResultPVC])
		}
		if preset != nil {
			fmt.Fprint(stdout, formatPresetHandoff(*preset))
		}
	}
	return nil
}

func resolveDirectJobGPUCount(explicit *int, shape string, preset *runtopology.ResolvedPreset) (int, error) {
	effectiveShape := strings.TrimSpace(shape)
	if effectiveShape == "" && preset != nil {
		effectiveShape = strings.TrimSpace(preset.Preset.Shape)
	}
	if effectiveShape != "" {
		shapeCount, ok, err := runtopology.GPUCountFromShape(effectiveShape)
		if err != nil {
			return 0, fmt.Errorf("policy.shape: %w", err)
		}
		if explicit != nil && ok && shapeCount != *explicit {
			return 0, fmt.Errorf("compute.gpus=%d conflicts with policy shape %q (%d GPUs)", *explicit, effectiveShape, shapeCount)
		}
		if explicit == nil && ok {
			return shapeCount, nil
		}
	}
	if explicit == nil {
		if preset != nil {
			return 0, fmt.Errorf("preset %q does not define a direct Job GPU count; set compute.gpus explicitly (0 for CPU, positive for GPU)", preset.Preset.Name)
		}
		return 0, nil
	}
	return *explicit, nil
}

func makeWorkspaceQueueAuthoritative(opts *jobrender.Options) {
	if opts.Annotations == nil {
		opts.Annotations = map[string]string{}
	}
	opts.Annotations[runtopology.AnnotationTopologyQueue] = opts.QueueName
	opts.Team = ""
	delete(opts.Labels, workloadmeta.LabelTeam)
	delete(opts.Annotations, workloadmeta.AnnotationClusterQueue)
	delete(opts.Annotations, workloadmeta.AnnotationResourceFlavor)
}

func queueValidationPolicyFor(preset *runtopology.ResolvedPreset, workspaceQueueResolved bool) queueValidationPolicy {
	if !workspaceQueueResolved || preset == nil {
		return queueValidationPolicy{Preset: preset}
	}
	if strings.TrimSpace(preset.Preset.TopologyName) == "" {
		return queueValidationPolicy{}
	}
	return queueValidationPolicy{
		TopologyName:            preset.Preset.TopologyName,
		CatalogTopologyContract: true,
	}
}

func parseRunJobDuration(field, value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return parsed, nil
}

func runJobTopologyFlags(o runDispatchOptions) topologyFlags {
	return topologyFlags{
		preset:                   o.preset,
		policyPath:               o.topologyPolicy,
		team:                     o.team,
		lane:                     o.lane,
		mode:                     o.mode,
		topology:                 o.topology,
		shape:                    o.shape,
		gpuClass:                 o.gpuClass,
		queue:                    o.queue,
		priorityTier:             o.priorityTier,
		workloadPriorityClass:    o.workloadPriorityClass,
		podPriorityClass:         o.podPriorityClass,
		disableDefaultPriorities: o.disableDefaultPriorities,
	}
}

func runJobTopologyFieldSet(o runDispatchOptions, flag string) bool {
	switch flag {
	case "team":
		return strings.TrimSpace(o.team) != ""
	case "lane":
		return strings.TrimSpace(o.lane) != ""
	case "mode":
		return strings.TrimSpace(o.mode) != ""
	case "topology":
		return strings.TrimSpace(o.topology) != ""
	case "shape":
		return strings.TrimSpace(o.shape) != ""
	case "gpu-class":
		return strings.TrimSpace(o.gpuClass) != ""
	case "queue":
		return strings.TrimSpace(o.queue) != ""
	case "priority-tier":
		return strings.TrimSpace(o.priorityTier) != ""
	case "workload-priority-class":
		return strings.TrimSpace(o.workloadPriorityClass) != ""
	case "pod-priority-class":
		return strings.TrimSpace(o.podPriorityClass) != ""
	case "disable-default-priorities":
		return o.disableDefaultPriorities
	default:
		return false
	}
}

func validateRunJobStorageConfig(o runDispatchOptions) error {
	if o.resultPVC != "" && o.dataPVC != "" && o.resultPVC != o.dataPVC {
		return fmt.Errorf("storage.result_pvc cannot differ from storage.data_pvc for job run configs")
	}
	if firstNonEmpty(o.dataPVC, o.resultPVC) != "" && len(o.volumeSpecs) > 0 {
		return fmt.Errorf("storage.data_pvc cannot be combined with storage.volumes for job run configs")
	}
	return nil
}

func resolveRunJobStorage(o runDispatchOptions) (string, []jobrender.Volume, []jobrender.VolumeMount, error) {
	if err := validateRunJobStorageConfig(o); err != nil {
		return "", nil, nil, err
	}
	volumeSpecs := append([]string{}, o.volumeSpecs...)
	if dataPVC := firstNonEmpty(o.dataPVC, o.resultPVC); dataPVC != "" {
		volumeSpecs = append([]string{"data=pvc:" + dataPVC}, volumeSpecs...)
	}
	return resolvePVCMounts(volumeSpecs, o.mountSpecs)
}

func resolveMetricsOffload(o runDispatchOptions, p profile.Profile, runID, namespace, cluster, outputDir string, outputWritable bool, annotations map[string]string) (metricsoffload.Runtime, error) {
	if strings.TrimSpace(o.workspace) == "" {
		return metricsoffload.Runtime{}, fmt.Errorf("metrics.offload.enabled requires a resolved TauWorkspace; set policy.workspace, --workspace, or a workspace connection")
	}
	outputDir = path.Clean(strings.TrimSpace(outputDir))
	if outputDir == "." || (outputDir != storage.DurableRoot && !strings.HasPrefix(outputDir, storage.DurableRoot+"/")) {
		return metricsoffload.Runtime{}, fmt.Errorf("metrics.offload.enabled requires storage.output on a writable PVC mounted under %s", storage.DurableRoot)
	}
	if !outputWritable || strings.TrimSpace(annotations[workloadmeta.AnnotationResultPVC]) == "" {
		return metricsoffload.Runtime{}, fmt.Errorf("metrics.offload.enabled requires storage.output backed by a writable PVC")
	}

	policy, err := metricsoffload.OptionsFromProfile(p)
	if err != nil {
		return metricsoffload.Runtime{}, err
	}
	if err := applyDirectMetricsOffloadEnvPolicy(&policy); err != nil {
		return metricsoffload.Runtime{}, err
	}
	if strings.TrimSpace(policy.Image) == "" {
		return metricsoffload.Runtime{}, fmt.Errorf("metrics.offload.enabled requires profile spec.metrics.offload.image or TAU_METRICS_OFFLOAD_IMAGE")
	}
	if err := metricsoffload.ValidatePinnedImage(policy.Image); err != nil {
		return metricsoffload.Runtime{}, err
	}

	history := make([]string, 0, len(o.metricsHistory))
	for _, raw := range o.metricsHistory {
		clean := path.Clean(strings.TrimSpace(raw))
		if !path.IsAbs(clean) {
			clean = path.Join(outputDir, clean)
		}
		if clean != storage.DurableRoot && !strings.HasPrefix(clean, storage.DurableRoot+"/") {
			return metricsoffload.Runtime{}, fmt.Errorf("metrics.history %q must resolve under %s", raw, storage.DurableRoot)
		}
		history = append(history, clean)
	}

	protected := map[string]string{
		exptelemetry.TauWorkspaceTag: o.workspace,
		exptelemetry.TauNamespaceTag: namespace,
	}
	if cluster = strings.TrimSpace(cluster); cluster != "" {
		protected[exptelemetry.TauClusterTag] = cluster
	}
	if attempt := runDispatchEnvValue(o.env, "TAU_RETRY_ATTEMPT"); attempt != "" {
		protected[exptelemetry.TauRetryAttemptTag] = attempt
	}
	tags := metricsoffload.MergeTags(policy.Tags, o.experiment.Tags, protected)
	interval := policy.Interval
	if interval == 0 {
		interval = metricsoffload.DefaultInterval
	}
	endpoint := firstNonEmpty(policy.RemoteWriteEndpoint, metricsoffload.DefaultRemoteWriteEndpoint)
	source := firstNonEmpty(policy.Source, metricsoffload.DefaultSource)
	group := firstNonEmpty(o.experiment.RunGroupID, policy.Group, "default")
	checkpointURI := strings.TrimSpace(o.checkpointPath)
	if err := validateMetricsSessionID(o.metricsSessionID); err != nil {
		return metricsoffload.Runtime{}, err
	}
	// The SQLite index (Store) is the only buffer that needs POSIX file locks;
	// BlobFuse-backed /data does not provide them, so SQLite fails schema init
	// with SQLITE_BUSY there. Root Store under the sidecar's /var/run/tau
	// emptyDir instead. The spool (Out) is written with WriteFileAtomic and
	// needs no locks, so it stays on durable /data — its JSONL checkpoint must
	// survive an ungraceful pod death so a retry (resilience.max_retries or
	// tau run resume) resumes from attempt 1's undrained rows instead of
	// re-baselining to end-of-file and silently dropping them.
	runtimeRoot := path.Join(metricsoffload.RuntimeMountPath, "metrics", o.metricsSessionID)
	durableRoot := path.Join(outputDir, ".tau", "metrics", o.metricsSessionID)

	return metricsoffload.Runtime{
		Image:                   policy.Image,
		RunID:                   runID,
		Project:                 o.experiment.Project,
		Experiment:              o.experiment.ExperimentID,
		Group:                   group,
		Tags:                    tags,
		Source:                  source,
		Store:                   path.Join(runtimeRoot, "expstore"),
		Out:                     path.Join(durableRoot, "offload"),
		History:                 history,
		CompletionFile:          "/var/run/tau/metrics-completion.json",
		RemoteWriteEndpoint:     endpoint,
		Interval:                interval,
		ArtifactURI:             outputDir,
		CheckpointURI:           checkpointURI,
		BaselineExistingHistory: true,
		ReadyFile:               directMetricsReadyFile,
		ReadyTimeout:            directMetricsReadyTimeout,
		DoneFile:                directMetricsDoneFile,
		DoneTimeout:             directMetricsDoneTimeout,
	}, nil
}

func validateMetricsSessionID(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("metrics offload session ID is required")
	}
	if len(sessionID) > 64 {
		return fmt.Errorf("metrics offload session ID must not exceed 64 characters")
	}
	for _, r := range sessionID {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("metrics offload session ID %q must contain only lowercase letters, digits, and '-'", sessionID)
		}
	}
	return nil
}

func applyDirectMetricsOffloadEnvPolicy(opts *metricsoffload.Options) error {
	for env, target := range map[string]*string{
		"TAU_METRICS_OFFLOAD_IMAGE":                 &opts.Image,
		"TAU_METRICS_OFFLOAD_SOURCE":                &opts.Source,
		"TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT": &opts.RemoteWriteEndpoint,
	} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			*target = value
		}
	}
	if value := strings.TrimSpace(os.Getenv("TAU_METRICS_OFFLOAD_INTERVAL")); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("TAU_METRICS_OFFLOAD_INTERVAL: %w", err)
		}
		opts.Interval = interval
	}
	return nil
}

func runDispatchEnvValue(values []string, name string) string {
	prefix := name + "="
	for i := len(values) - 1; i >= 0; i-- {
		if strings.HasPrefix(values[i], prefix) {
			return strings.TrimSpace(strings.TrimPrefix(values[i], prefix))
		}
	}
	return ""
}

func resolvePVCMounts(volumeSpecs, mountSpecs []string) (string, []jobrender.Volume, []jobrender.VolumeMount, error) {
	volumes, err := parseVolumeSpecs(volumeSpecs)
	if err != nil {
		return "", nil, nil, err
	}
	mounts, err := parseMountSpecs(mountSpecs)
	if err != nil {
		return "", nil, nil, err
	}
	if len(volumes) == 0 {
		if len(mounts) > 0 {
			return "", nil, nil, fmt.Errorf("--mount requires at least one --volume")
		}
		return "", nil, nil, nil
	}
	for _, volume := range volumes {
		if volume.PVC == "" {
			return "", nil, nil, fmt.Errorf("--volume currently supports pvc volumes only; %q is not pvc-backed", volume.Name)
		}
	}
	if len(mounts) == 0 {
		if len(volumes) != 1 {
			return "", nil, nil, fmt.Errorf("--volume without --mount requires exactly one pvc volume")
		}
		return volumes[0].PVC, nil, nil, nil
	}
	if err := validateMountsAgainstVolumes(mounts, volumes); err != nil {
		return "", nil, nil, err
	}
	outVolumes := make([]jobrender.Volume, 0, len(volumes))
	for _, volume := range volumes {
		outVolumes = append(outVolumes, jobrender.Volume{Name: volume.Name, PVC: volume.PVC})
	}
	outMounts := make([]jobrender.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
		outMounts = append(outMounts, jobrender.VolumeMount{Name: mount.Name, MountPath: mount.MountPath, ReadOnly: mount.ReadOnly})
	}
	return "", outVolumes, outMounts, nil
}

func parseRunJobNodeSelectors(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		i := indexEq(value)
		if i <= 0 || i == len(value)-1 {
			return nil, fmt.Errorf("policy.node_selector: expected KEY=VALUE, got %q", value)
		}
		out[value[:i]] = value[i+1:]
	}
	return out, nil
}

type priorityChecker interface {
	Raw(context.Context, []string, []byte) (string, error)
}

func autoDisableMissingDefaultPriorities(ctx context.Context, runner priorityChecker) (bool, string) {
	workloadOK := priorityResourceExists(ctx, runner, "workloadpriorityclass.kueue.x-k8s.io", "taugrid-default")
	podOK := priorityResourceExists(ctx, runner, "priorityclass", "taugrid-default")
	if workloadOK && podOK {
		return false, ""
	}
	missing := []string{}
	if !workloadOK {
		missing = append(missing, "Kueue WorkloadPriorityClass taugrid-default")
	}
	if !podOK {
		missing = append(missing, "Kubernetes PriorityClass taugrid-default")
	}
	return true, fmt.Sprintf("warning: cluster is missing %s; omitting Tau default priority classes for this run (set policy.disable_default_priorities: true to make this explicit)", strings.Join(missing, " and "))
}

func priorityResourceExists(ctx context.Context, runner priorityChecker, kind, name string) bool {
	out, err := runner.Raw(ctx, []string{"get", kind, name, "-o", "name"}, nil)
	return err == nil && strings.TrimSpace(out) != ""
}

func validateProfiler(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "ncu", "nsys":
		return mode, nil
	default:
		return "", fmt.Errorf("profiler %q: must be nsys|ncu", mode)
	}
}

func formatRunJobSubmission(name, profileName, namespace, kubeContext string) string {
	return fmt.Sprintf(
		"submitted %s (profile=%s, ns=%s)\nstatus:  tau run status %s -n %s%s\nlogs:    tau run logs %s -n %s -f%s\nprofile: tau run status %s -n %s --run-profile%s\n",
		name, profileName, namespace,
		name, namespace, contextFlag(kubeContext),
		name, namespace, contextFlag(kubeContext),
		name, namespace, contextFlag(kubeContext),
	)
}

func buildRunJobProfileAnnotations(outputDir, pvc string, profileOptions jobrender.ProfileOptions) map[string]string {
	path := strings.TrimRight(outputDir, "/") + "/profile"
	annotations := map[string]string{
		workloadmeta.AnnotationProfilerMode: profileOptions.Mode,
		workloadmeta.AnnotationProfilerPath: path,
		workloadmeta.AnnotationProfilerPVC:  pvc,
		workloadmeta.AnnotationProfilerRank: profileOptions.Rank,
	}
	if profileOptions.Warmup > 0 {
		annotations[workloadmeta.AnnotationProfilerWarmup] = profileOptions.Warmup.String()
	}
	if profileOptions.Duration > 0 {
		annotations[workloadmeta.AnnotationProfilerDuration] = profileOptions.Duration.String()
	}
	return annotations
}

func buildRunJobOutputAnnotations(name, output, pvcMount string, volumes []jobrender.Volume, mounts []jobrender.VolumeMount, p profile.Profile) (map[string]string, string, bool, error) {
	type mountedPVC struct {
		path     string
		pvc      string
		readOnly bool
	}
	var mounted []mountedPVC
	if pvcMount != "" {
		mounted = append(mounted, mountedPVC{path: storage.DurableRoot, pvc: pvcMount})
	}
	if len(volumes) > 0 && len(mounts) > 0 {
		pvcByName := make(map[string]string, len(volumes))
		for _, volume := range volumes {
			pvcByName[volume.Name] = volume.PVC
		}
		for _, mount := range mounts {
			if pvc := pvcByName[mount.Name]; pvc != "" {
				mounted = append(mounted, mountedPVC{path: mount.MountPath, pvc: pvc, readOnly: mount.ReadOnly})
			}
		}
	}
	for _, mount := range profilePersistenceMounts(p) {
		mounted = append(mounted, mountedPVC{path: mount.MountPath, pvc: mount.PVC, readOnly: mount.ReadOnly})
	}
	if output == "" {
		if len(mounted) == 0 {
			return nil, "", false, nil
		}
		mount := mounted[0]
		defaultOutput := filepath.Clean(filepath.Join(mount.path, name))
		return map[string]string{
			workloadmeta.AnnotationResultPath: defaultOutput,
			workloadmeta.AnnotationResultPVC:  mount.pvc,
		}, defaultOutput, !mount.readOnly, nil
	}
	if !filepath.IsAbs(output) {
		return nil, "", false, fmt.Errorf("storage.output %q: must be an absolute path", output)
	}
	if len(mounted) == 0 {
		return nil, "", false, fmt.Errorf("storage.output %q: no PVC supplied by storage.data_pvc, storage.volumes/mounts, or profile persistence; output cannot be retrieved by `tau run get`", output)
	}
	cleanOutput := filepath.Clean(output)
	for _, mount := range mounted {
		base := filepath.Clean(mount.path)
		if cleanOutput == base || strings.HasPrefix(cleanOutput, base+"/") {
			return map[string]string{
				workloadmeta.AnnotationResultPath: cleanOutput,
				workloadmeta.AnnotationResultPVC:  mount.pvc,
			}, cleanOutput, !mount.readOnly, nil
		}
	}
	paths := make([]string, 0, len(mounted))
	for _, mount := range mounted {
		paths = append(paths, mount.path)
	}
	return nil, "", false, fmt.Errorf("storage.output %q: not under any mounted PVC path %v", output, paths)
}

func buildPrimaryPVCOutputAnnotations(name, output, pvc string) (map[string]string, string, bool, error) {
	return buildRunJobOutputAnnotations(name, output, pvc, nil, nil, profile.Profile{})
}

func patchRunJobHeadlessServiceOwnerRef(ctx context.Context, runner kubeRawRunner, namespace, jobName string) error {
	serviceName := jobName + jobrender.HeadlessSuffix
	uidOut, err := runner.Raw(ctx, []string{
		"get", "job", jobName, "-n", namespace,
		"-o", "jsonpath={.metadata.uid}",
	}, nil)
	if err != nil {
		return fmt.Errorf("get Job uid: %w", err)
	}
	uid := strings.TrimSpace(uidOut)
	if uid == "" {
		return fmt.Errorf("empty uid for Job/%s", jobName)
	}
	patch := fmt.Sprintf(`{"metadata":{"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":%q,"uid":%q,"controller":true,"blockOwnerDeletion":true}]}}`,
		jobName, uid)
	_, err = runner.Raw(ctx, []string{
		"patch", "service", serviceName, "-n", namespace,
		"--type=merge", "-p", patch,
	}, nil)
	if err != nil {
		return fmt.Errorf("patch service %s: %w", serviceName, err)
	}
	return nil
}

func createMultiNodeRunJobManifest(ctx context.Context, runner runSubmissionCleanupRunner, manifest []byte, namespace, name, submissionID string, stdout io.Writer) error {
	serviceName := name + jobrender.HeadlessSuffix
	separator := []byte("\n---\n")
	index := bytes.Index(manifest, separator)
	if index < 0 {
		return fmt.Errorf("multi-node manifest missing YAML document separator")
	}
	serviceDocument := manifest[:index+1]
	jobDocument := manifest[index+len(separator):]
	jobResult, err := submitRunWorkload(ctx, runner, runSubmission{
		Resource:     "job",
		Name:         name,
		Namespace:    namespace,
		SubmissionID: submissionID,
		Manifest:     jobDocument,
	})
	fmt.Fprint(stdout, jobResult.Output)
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	serviceResult, err := submitRunWorkload(ctx, runner, runSubmission{
		Resource:     "service",
		Name:         serviceName,
		Namespace:    namespace,
		SubmissionID: submissionID,
		Manifest:     serviceDocument,
	})
	fmt.Fprint(stdout, serviceResult.Output)
	if err != nil {
		return withRunSubmissionCleanup(fmt.Errorf("create headless service: %w", err), runner, runSubmission{
			Resource:     "job",
			Name:         name,
			Namespace:    namespace,
			SubmissionID: submissionID,
		})
	}
	if err := patchRunJobHeadlessServiceOwnerRef(ctx, runner, namespace, name); err != nil {
		return withRunSubmissionCleanup(fmt.Errorf("headless service ownerRef patch failed: %w", err), runner,
			runSubmission{Resource: "service", Name: serviceName, Namespace: namespace, SubmissionID: submissionID},
			runSubmission{Resource: "job", Name: name, Namespace: namespace, SubmissionID: submissionID},
		)
	}
	return nil
}

func stringifyConfigs(configs map[string]any) map[string]string {
	if len(configs) == 0 {
		return nil
	}
	out := make(map[string]string, len(configs))
	for k, v := range configs {
		out[k] = fmt.Sprint(v)
	}
	return out
}
