package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/projectzip"
	"github.com/Azure/taugrid/cli/internal/rayjobrender"
	"github.com/Azure/taugrid/cli/internal/secretpreflight"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kube"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// runRayRequest is the typed Ray dispatch for `tau run`.
//
// The script stays a normal Ray program: it imports ray.train, configures a
// TorchTrainer/ScalingConfig, and calls fit(). Tau owns only the cluster
// contract — RayJob rendering, Kueue queue/preset selection, storage, status
// handoff, and kubectl submission.
// maxProjectArchiveInputBytes bounds what run.working_dir may sweep up before
// compression. The archive is embedded in the workload spec, so an accidental
// dataset directory must fail fast with a readable message rather than
// producing a spec the API server will reject.
const maxProjectArchiveInputBytes = 8 << 20

type runRayRequest struct {
	Name    string
	Options runDispatchOptions
}

func newRunRayRequest(options runDispatchOptions, name string) (runRayRequest, error) {
	if name == "" {
		return runRayRequest{}, fmt.Errorf("ray runs require NAME")
	}
	if options.script == "" {
		return runRayRequest{}, fmt.Errorf("engine=ray requires run.entrypoint")
	}
	if options.resultPVC != "" && options.dataPVC != "" && options.resultPVC != options.dataPVC {
		return runRayRequest{}, fmt.Errorf("storage.result_pvc cannot differ from storage.data_pvc for Ray run configs")
	}
	if len(options.volumeSpecs) > 0 || len(options.mountSpecs) > 0 {
		return runRayRequest{}, fmt.Errorf("ray run configs support storage.data_pvc/output, but not storage.volumes/mounts")
	}
	return runRayRequest{Name: name, Options: options}, nil
}

func executeRunRay(ctx context.Context, stdout, stderr io.Writer, request *runRayRequest, captureCommand string) error {
	if err := ensureSubmissionID(&request.Options); err != nil {
		return err
	}
	o := request.Options
	name := request.Name
	kubeContext := firstNonEmpty(o.kubeContext, defaultKubeContext())
	if o.dryRun != "" && o.dryRun != "client" && o.dryRun != "server" {
		return fmt.Errorf("--dry-run must be one of: client, server")
	}

	scriptBytes, err := os.ReadFile(o.script)
	if err != nil {
		return fmt.Errorf("run.entrypoint: %w", err)
	}
	env, err := parseEnvKV(o.env)
	if err != nil {
		return err
	}
	envSecrets, err := parseEnvSecretKV(o.envSecrets)
	if err != nil {
		return err
	}
	nodeSelector, err := parseNodeSelectors(o.nodeSelectors)
	if err != nil {
		return err
	}
	dataPVC, err := cleanStorageFlag("storage.data_pvc", firstNonEmpty(o.dataPVC, o.resultPVC))
	if err != nil {
		return err
	}

	gpuResourceMode := o.gpuResourceMode
	if o.migProfile != "" && gpuResourceMode != "mig" {
		// compute.mig_profile implies MIG mode. Force it even when another
		// mode was requested so the renderer never silently emits
		// nvidia.com/gpu for a MIG request.
		gpuResourceMode = "mig"
	}
	normalizedGPUResourceMode, err := manifest.NormalizeGPUResourceMode(gpuResourceMode)
	if err != nil {
		return fmt.Errorf("compute.gpu_resource_mode: %w", err)
	}

	namespaceExplicit := strings.TrimSpace(o.namespace) != ""
	namespace := strings.TrimSpace(o.namespace)

	topo := runJobTopologyFlags(o)
	topoChanged := func(flag string) bool { return runJobTopologyFieldSet(o, flag) }
	resolvedProfileName, preset, warnings, err := topo.resolvePreset(o.profileName)
	if err != nil {
		return err
	}
	if preset != nil && !namespaceExplicit && preset.Preset.Namespace != "" {
		namespace = preset.Preset.Namespace
	}
	if preset != nil && normalizedGPUResourceMode == manifest.GPUResourceModeDRA {
		draPreset := runtopology.WithDRAQueue(*preset)
		preset = &draPreset
	}
	topologyHolder := jobrender.Options{}
	topoWarnings, err := topo.applyWithChangedAndWorkspaceQueue(&topologyHolder, preset, topoChanged, o.workspaceQueueResolved)
	if err != nil {
		return err
	}
	configureGPUQueueModeWithChanged(normalizedGPUResourceMode, &topologyHolder, topoChanged)
	if o.workspaceQueueResolved {
		makeWorkspaceQueueAuthoritative(&topologyHolder)
	}
	nodeSelector, err = mergeNodeSelectors(nodeSelector, topologyHolder.NodeSelector)
	if err != nil {
		return err
	}
	warnings = append(warnings, topoWarnings...)

	gpuDemand := rayRequestedGPUCount(o.workers, o.gpusPerWorker)
	p := resourceProfileForRender(resolvedProfileName, preset, topo.resourceProfileOptions(), gpuDemand)
	var runner *kube.Runner
	if o.dryRun != "client" {
		runner = kube.New(kubeContext)
	}
	resolveWarnings, err := resolveAccessibleQueueNamespace(ctx, runner, namespaceExplicit, &namespace, &topologyHolder, o.dryRun, "rayjobs.ray.io")
	if err == nil && o.dryRun == "client" && strings.TrimSpace(namespace) == "" {
		// Namespace resolution needs a live cluster, so an offline client
		// dry-run cannot have one. Substitute a self-describing placeholder
		// rather than failing: the render is the whole point of the command,
		// and the value is visibly not the one the submit path resolves.
		namespace = clientDryRunNamespacePlaceholder
		warnings = append(warnings, clientDryRunPlaceholderWarning("namespace"))
	}
	if err == nil {
		namespace, err = requireWorkloadNamespace(namespace)
	}
	if err != nil {
		return err
	}
	request.Options.namespace = namespace
	if o.dryRun != "client" {
		if err := secretpreflight.ValidateRequiredEnv(ctx, runner, namespace, envSecrets); err != nil {
			return err
		}
	}
	warnings = append(warnings, resolveWarnings...)
	autoWarnings, err := topo.resolveAutoQueue(ctx, runner, namespace, &topologyHolder, preset, gpuDemand, nodeSelector, o.dryRun, preset == nil && resolvedProfileName == "")
	if err != nil {
		return err
	}
	warnings = append(warnings, autoWarnings...)

	capture, err := buildRayCaptureMetadata(ctx, captureCommand, name, namespace, o.image, o.script, o.workers, o.gpusPerWorker, dataPVC)
	if err != nil {
		return err
	}
	capture = addRunWorkspaceMetadata(capture, o.workspace, o.workspaceResultScope)
	labels, annotations := experiment.MergeMetadata(topologyHolder.Labels, topologyHolder.Annotations, capture)
	labels = workloadmeta.StampWorkspace(labels, o.workspace)
	if o.submissionID != "" {
		annotations[workloadmeta.AnnotationSubmissionID] = o.submissionID
	}
	resultAnnotations, outputDir, outputWritable, err := buildPrimaryPVCOutputAnnotations(name, o.output, dataPVC)
	if err != nil {
		return err
	}
	if o.workspaceResultScope != "" && outputDir != "" {
		if err := validateRunOutputScope(outputDir, o.workspaceResultScope); err != nil {
			return fmt.Errorf("storage.output %q is outside TauWorkspace %q result scope %q", outputDir, o.workspace, o.workspaceResultScope)
		}
	}
	for key, value := range resultAnnotations {
		annotations[key] = value
	}
	artifactPublication := artifactpublish.Runtime{
		Mode:          o.outputPublish,
		OutputDir:     outputDir,
		StagingDir:    filepath.ToSlash(filepath.Join(storage.HotRoot, "tau-output", name)),
		PublicationID: o.artifactPublicationID,
	}
	if artifactPublication.Enabled() {
		if !outputWritable {
			return fmt.Errorf("storage.publish=staged requires storage.output backed by a writable PVC")
		}
		if err := artifactPublication.Validate(); err != nil {
			return err
		}
		annotations[workloadmeta.AnnotationArtifactPublication] = artifactPublication.Mode
		annotations[workloadmeta.AnnotationArtifactPublicationID] = artifactPublication.PublicationID
	}
	var metricsRuntime metricsoffload.Runtime
	if o.metricsOffloadEnabled {
		offload := o
		offload.checkpointPath = runDispatchEnvValue(o.env, "TAU_RESUME_FROM")
		metricsRuntime, err = resolveMetricsOffload(offload, p, name, namespace, kubeContext, outputDir, outputWritable, annotations)
		if err != nil {
			return err
		}
		annotations[experiment.AnnotationExperimentSource] = "stellar"
		annotations[workloadmeta.AnnotationMetricsSession] = o.metricsSessionID
	}

	projectArchive, scriptName, err := buildProjectArchive(o)
	if err != nil {
		return err
	}

	rendered, err := rayjobrender.Render(rayjobrender.Options{
		Name:               name,
		Namespace:          namespace,
		ServiceAccountName: o.serviceAccountName,
		ScriptName:         scriptName,
		Script:             scriptBytes,
		ProjectArchive:     projectArchive,
		Image:              o.image,
		Workers:            o.workers,
		GPUsPerWorker:      o.gpusPerWorker,
		Launcher:           o.launcher,
		GPUResourceMode:    normalizedGPUResourceMode,
		MIGProfile:         o.migProfile,
		RuntimePip:         o.runtimePip,
		Env:                env,
		EnvSecrets:         envSecrets,
		RedactSecrets:      o.dryRun == "client",
		DataPVC:            dataPVC,
		Profile:            p,
		TopologyOptions:    topologyOptionsFromSubmit(topologyHolder),
		NodeSelector:       nodeSelector,
		Labels:             labels,
		Annotations:        annotations,
		OutputDir:          outputDir,
		ArtifactPublish:    artifactPublication,
		CheckpointArtifact: o.checkpointArtifact,
		MetricsOffload:     metricsRuntime,
		Resources: rayjobrender.Resources{
			CPURequest:    o.cpuRequest,
			MemoryRequest: o.memoryRequest,
			CPULimit:      o.cpuLimit,
			MemoryLimit:   o.memoryLimit,
			Head: rayjobrender.ResourceOverrides{
				CPURequest:    o.headCPURequest,
				MemoryRequest: o.headMemoryRequest,
				CPULimit:      o.headCPULimit,
				MemoryLimit:   o.headMemoryLimit,
			},
			Worker: rayjobrender.ResourceOverrides{
				CPURequest:    o.workerCPURequest,
				MemoryRequest: o.workerMemoryRequest,
				CPULimit:      o.workerCPULimit,
				MemoryLimit:   o.workerMemoryLimit,
			},
		},

		TuneMetric:              o.tuneMetric,
		TuneMode:                o.tuneMode,
		TuneNumSamples:          o.tuneNumSamples,
		TuneMaxConcurrentTrials: o.tuneMaxConcurrentTrials,
		TuneParamSpace:          o.tuneParamSpace,

		AllowNCCLOverride: o.allowNCCLOverride,

		RayTrainConfig:        rayTrainConfigForRender(o),
		AzureWorkloadIdentity: o.azureWorkloadIdentity,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if o.dryRun != "client" {
		if err := validateRenderedQueue(ctx, runner, namespace, rendered, topologyHolder, queueValidationPolicyFor(preset, o.workspaceQueueResolved)); err != nil {
			return err
		}
	}
	for _, warning := range warnings {
		fmt.Fprintln(stderr, warning)
	}
	if o.dryRun == "client" {
		_, err := stdout.Write(rendered)
		return err
	}
	if runner == nil {
		runner = kube.New(kubeContext)
	}
	result, err := submitRunWorkload(ctx, runner, runSubmission{
		Resource:     "rayjob.ray.io",
		Name:         name,
		Namespace:    namespace,
		SubmissionID: o.submissionID,
		Manifest:     rendered,
		DryRun:       o.dryRun,
	})
	fmt.Fprint(stdout, result.Output)
	if err != nil {
		return err
	}
	if o.dryRun == "" {
		fmt.Fprint(stdout, formatRaySubmitHandoff(name, namespace, kubeContext, preset))
	}
	return nil
}

// rayTrainConfigForRender passes execution.configs straight through. The
// argv path used to JSON-encode it and re-parse it on the other side; the
// typed path keeps the map.
func rayTrainConfigForRender(o runDispatchOptions) map[string]any {
	if o.tuneParamSpace != "" || len(o.configs) == 0 {
		return nil
	}
	return o.configs
}

// buildProjectArchive packages run.working_dir for Ray's runtime_env when the
// config asks for it, returning the archive bytes and the entrypoint path
// relative to the project root. With no working_dir configured it returns nil
// and the plain entrypoint base name, which is the pre-existing single-file
// behaviour.
func buildProjectArchive(o runDispatchOptions) ([]byte, string, error) {
	dir := strings.TrimSpace(o.workingDir)
	if dir == "" {
		return nil, filepath.Base(o.script), nil
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", fmt.Errorf("run.working_dir: %w", err)
	}
	scriptAbs, err := filepath.Abs(o.script)
	if err != nil {
		return nil, "", fmt.Errorf("run.entrypoint: %w", err)
	}
	rel, err := filepath.Rel(root, scriptAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, "", fmt.Errorf(
			"run.entrypoint %s must live inside run.working_dir %s; Ray unpacks the project archive as the working directory, so an entrypoint outside it would not be shipped",
			o.script, dir)
	}

	archive, files, err := projectzip.Build(projectzip.Options{
		Dir:      root,
		Excludes: o.workingDirExcludes,
		MaxBytes: maxProjectArchiveInputBytes,
	})
	if err != nil {
		if len(files) > 0 {
			return nil, "", fmt.Errorf("run.working_dir: %w\n\nLargest files:%s\n\nExclude what the run does not need with run.working_dir_excludes, or keep large data on a PVC instead of in the project directory.",
				err, projectzip.DescribeLargest(files, 5))
		}
		return nil, "", fmt.Errorf("run.working_dir: %w", err)
	}
	// Check the packaged size here rather than letting the renderer reject it,
	// so the error can name the files actually responsible.
	if len(archive) > rayjobrender.MaxProjectArchiveBytes {
		return nil, "", fmt.Errorf(
			"run.working_dir %s packages to %d bytes, which exceeds the limit of %d bytes (%d KiB).\n"+
				"The archive ships in the head and every worker pod template, so it counts more than once "+
				"against the Kubernetes object-size budget.\n\nLargest files:%s\n\n"+
				"Exclude what the run does not need with run.working_dir_excludes, keep large inputs on a PVC, "+
				"or bake heavy dependencies into a custom Ray image.",
			dir, len(archive), rayjobrender.MaxProjectArchiveBytes, rayjobrender.MaxProjectArchiveBytes>>10,
			projectzip.DescribeLargest(files, 5))
	}
	return archive, filepath.ToSlash(rel), nil
}
