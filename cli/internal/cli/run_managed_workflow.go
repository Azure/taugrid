// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/kvspec"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/cli/internal/secretpreflight"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// runManagedWorkflowRequest is the resolved managed workflow (schema_version: 1)
// dispatch for `tau run --config`. Managed workflows keep the manifest as the
// source of truth for compute and storage; the run config only supplies the
// scheduling policy and the entrypoint.
type runManagedWorkflowRequest struct {
	Options resolvedRunManagedWorkflowOptions
}

// managedWorkflowGPUDemand reports the total number of GPUs a managed workflow
// asks Kueue to admit at once, which is what the quota preflight must compare
// against a queue's capacity. It is not the per-pod count: a training RayJob
// renders workers dedicated GPU worker replicas plus a CPU-only control head,
// so its demand is workers x gpus. Eval renders one dedicated GPU worker and
// CPU fanout workers; a plain Job is a single pod, so both stay at the
// per-execution-pod count.
func managedWorkflowGPUDemand(m *manifest.Manifest, workloadKind string) int {
	if m == nil || m.Compute.GPUs <= 0 {
		return 0
	}
	if workloadKind == manifest.WorkloadKindRayJob {
		return rayJobRequestedGPUCount(m.Compute.Workers, m.Compute.GPUs)
	}
	return m.Compute.GPUs
}

func newRunManagedWorkflowRequest(o unresolvedRunOptions) (runManagedWorkflowRequest, error) {
	if strings.TrimSpace(o.file) == "" {
		return runManagedWorkflowRequest{}, fmt.Errorf("--manifest is required")
	}
	if o.dryRun != "" && o.dryRun != "client" && o.dryRun != "server" {
		return runManagedWorkflowRequest{}, fmt.Errorf("--dry-run must be one of: client, server")
	}
	o.mainScript = firstNonEmpty(o.mainScript, o.script)
	if strings.TrimSpace(o.mainScript) == "" {
		return runManagedWorkflowRequest{}, fmt.Errorf("workflow config requires run.entrypoint")
	}
	switch o.workloadKind {
	case "", manifest.WorkloadKindJob, manifest.WorkloadKindRayJob, manifest.WorkloadKindRayJobEval:
	default:
		return runManagedWorkflowRequest{}, fmt.Errorf("workload_kind must be one of: %s, %s, %s", manifest.WorkloadKindJob, manifest.WorkloadKindRayJob, manifest.WorkloadKindRayJobEval)
	}
	return runManagedWorkflowRequest{Options: resolveRunManagedWorkflowOptions(o)}, nil
}

// managedWorkflowProfileOptions resolves the nsys profiling window from the run
// config. Profiling knobs are only meaningful with an explicit profiler.
func managedWorkflowProfileOptions(o resolvedRunManagedWorkflowOptions) (manifest.ProfileOptions, error) {
	resolvedProfiler, err := validateProfiler(o.profiler)
	if err != nil {
		return manifest.ProfileOptions{}, err
	}
	if resolvedProfiler != "" && resolvedProfiler != "nsys" {
		return manifest.ProfileOptions{}, fmt.Errorf("profile.mode %q: managed workflow RayJob profiling supports nsys only", resolvedProfiler)
	}
	warmup, err := parseRunJobDuration("profile.warmup", o.profileWarmup)
	if err != nil {
		return manifest.ProfileOptions{}, err
	}
	duration, err := parseRunJobDuration("profile.duration", o.profileDuration)
	if err != nil {
		return manifest.ProfileOptions{}, err
	}
	rank := strings.TrimSpace(o.profileRank)
	if resolvedProfiler == "" {
		for field, set := range map[string]bool{
			"profile.rank":     rank != "",
			"profile.warmup":   warmup != 0,
			"profile.duration": duration != 0,
		} {
			if set {
				return manifest.ProfileOptions{}, fmt.Errorf("%s requires profile.mode nsys", field)
			}
		}
		return manifest.ProfileOptions{}, nil
	}
	if rank == "" {
		rank = "0"
	}
	if duration == 0 {
		duration = 2 * time.Minute
	}
	return manifest.ProfileOptions{Mode: resolvedProfiler, Rank: rank, Warmup: warmup, Duration: duration}, nil
}

// managedWorkflowMetricsOffload resolves sidecar metrics offload options from
// the TAU_METRICS_OFFLOAD_* environment contract.
func managedWorkflowMetricsOffload(ctx context.Context) (manifest.MetricsOffloadOptions, error) {
	var opts manifest.MetricsOffloadOptions
	fromEnv := func(name string, dest *string) {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*dest = value
		}
	}
	fromEnv("TAU_METRICS_OFFLOAD_IMAGE", &opts.Image)
	fromEnv("TAU_METRICS_OFFLOAD_PROJECT", &opts.Project)
	fromEnv("TAU_METRICS_OFFLOAD_GROUP", &opts.Group)
	fromEnv("TAU_METRICS_OFFLOAD_SOURCE", &opts.Source)
	fromEnv("TAU_METRICS_OFFLOAD_STORE", &opts.Store)
	fromEnv("TAU_METRICS_OFFLOAD_OUT", &opts.Out)
	fromEnv("TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT", &opts.RemoteWriteEndpoint)
	if raw := strings.TrimSpace(os.Getenv("TAU_METRICS_OFFLOAD_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			return manifest.MetricsOffloadOptions{}, fmt.Errorf("TAU_METRICS_OFFLOAD_INTERVAL must be a positive duration (got %q)", raw)
		}
		opts.Interval = interval
	}
	return applyRunExperimentMetricsOffload(ctx, opts), nil
}

func executeRunManagedWorkflow(ctx context.Context, stdout, stderr io.Writer, request *runManagedWorkflowRequest, captureCommand string) error {
	if err := ensureSubmissionIDValue(request.Options.dryRun, &request.Options.submissionID); err != nil {
		return err
	}
	o := request.Options
	manifestPath := o.file
	workloadKind := o.workloadKind
	namespace := strings.TrimSpace(o.namespace)
	namespaceExplicit := strings.TrimSpace(o.namespace) != ""
	kubeContext := firstNonEmpty(o.kubeContext, defaultKubeContext())
	dryRun := o.dryRun

	profileOptions, err := managedWorkflowProfileOptions(o)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("workflow.file: %w", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", manifestPath, err)
	}
	dataPVC, err := cleanStorageFlag("storage.data_pvc", o.dataPVC)
	if err != nil {
		return err
	}
	if raw, err = applyDataPVCOverride(raw, m, dataPVC); err != nil {
		return err
	}
	if raw, err = applyRuntimeImageOverride(raw, m, o.image); err != nil {
		return err
	}
	// compute.workers from the run config overrides the manifest so the
	// manifest stays the source of truth while one-off submits can still
	// resize the gang.
	if o.workers > 1 {
		m.Compute.Workers = o.workers
	}
	if m.IsCPUOnly() {
		switch workloadKind {
		case manifest.WorkloadKindJob:
			return fmt.Errorf("workload_kind=job cannot host a CPU-only manifest (compute.gpus=0); omit workload_kind or set workload_kind: rayjob")
		case "":
			workloadKind = manifest.WorkloadKindRayJob
			fmt.Fprintf(stderr, "tau: inferred workload_kind=rayjob (compute.gpus=0 requests the CPU RayJob path; set workload_kind: rayjob explicitly to silence this warning)\n")
		}
	}
	if m.IsMultiNode() {
		switch workloadKind {
		case manifest.WorkloadKindJob:
			return fmt.Errorf("workload_kind=job cannot be multi-node (compute.workers=%d); omit workload_kind or set workload_kind: rayjob", m.Compute.Workers)
		case "":
			workloadKind = manifest.WorkloadKindRayJob
			fmt.Fprintf(stderr, "tau: inferred workload_kind=rayjob (compute.workers=%d requires multi-node; set workload_kind: rayjob explicitly to silence this warning)\n", m.Compute.Workers)
		}
	}
	// eval.cpu_workers from the run config overrides the manifest. Apply it
	// before the eval dispatch inference below so IsEval() sees the override.
	if o.cpuWorkers > 0 {
		m.Eval.CPUWorkers = o.cpuWorkers
	}
	if m.IsEval() {
		switch workloadKind {
		case manifest.WorkloadKindJob, manifest.WorkloadKindRayJob:
			return fmt.Errorf("workload_kind=%s cannot host an eval manifest (eval.cpu_workers=%d, eval.upstream=%q); omit workload_kind or set workload_kind: rayjob-eval", workloadKind, m.Eval.CPUWorkers, m.Eval.Upstream)
		case "":
			workloadKind = manifest.WorkloadKindRayJobEval
			fmt.Fprintf(stderr, "tau: inferred workload_kind=rayjob-eval (eval.cpu_workers=%d set in manifest; set workload_kind: rayjob-eval explicitly to silence this warning)\n", m.Eval.CPUWorkers)
		}
	} else if workloadKind == manifest.WorkloadKindRayJobEval {
		return fmt.Errorf("workload_kind=rayjob-eval requires eval.cpu_workers > 0 (set it in the manifest or the run config)")
	}
	if workloadKind == manifest.WorkloadKindRayJob || workloadKind == manifest.WorkloadKindRayJobEval {
		if err := m.ValidateRayJobResourceName(); err != nil {
			return err
		}
	}
	if profileOptions.Mode != "" {
		if workloadKind != manifest.WorkloadKindRayJob {
			return fmt.Errorf("profile.mode nsys currently requires rayjob training (got %q)", workloadKind)
		}
		if profileOptions.Duration <= 0 {
			return fmt.Errorf("profile.duration must be > 0 for RayJob actor profiling")
		}
	}
	extras, err := loadExtraScripts(o.extraScripts)
	if err != nil {
		return err
	}
	mainScriptBytes, err := os.ReadFile(o.mainScript)
	if err != nil {
		return fmt.Errorf("run.entrypoint: %w", err)
	}

	gpuResourceMode, err := manifest.NormalizeGPUResourceMode(firstNonEmpty(o.gpuResourceMode, defaultGPUResourceMode()))
	if err != nil {
		return fmt.Errorf("compute.gpu_resource_mode: %w", err)
	}
	if o.migProfile != "" && gpuResourceMode != manifest.GPUResourceModeMIG {
		gpuResourceMode = manifest.GPUResourceModeMIG
	}
	nodeSelector, err := parseNodeSelectors(o.nodeSelectors)
	if err != nil {
		return err
	}
	jobSecret, err := loadJobSecretPayload(o.secretPayloadPath)
	if err != nil {
		return err
	}
	envSecrets, err := parseEnvSecretKV(o.envSecrets)
	if err != nil {
		return err
	}
	cliEnv, err := parseEnvKV(o.env)
	if err != nil {
		return err
	}
	var cliVars []envspec.Var
	if len(cliEnv) > 0 {
		cliVars = envspec.FromMap(cliEnv)
	}
	cliVars = append(cliVars, envSecrets...)
	if len(cliVars) > 0 {
		mergedEnv, err := envspec.Merge(m.Runtime.Env, cliVars)
		if err != nil {
			return err
		}
		m.Runtime.Env = mergedEnv
	}

	topo := resolvedRunTopologyFlags(o.runPlacement)
	changed := func(flag string) bool { return resolvedRunTopologyFieldSet(o.runPlacement, flag) }
	resolvedProfileName, preset, warnings, err := topo.resolvePreset(o.profileName)
	if err != nil {
		return err
	}
	// Eval workloads belong on the eval lane (separate Kueue queue, separate
	// priority class, doesn't compete with training). Pick the inferred lane up
	// front so both preset suggestion and the validate step agree on the target.
	inferLane := "training"
	if workloadKind == manifest.WorkloadKindRayJobEval {
		inferLane = "eval"
	}
	explicitQueueOnlyDevicePlugin := gpuResourceMode == manifest.GPUResourceModeDevicePlugin && strings.TrimSpace(topo.queue) != ""
	if preset == nil && resolvedProfileName == "" && !explicitQueueOnlyDevicePlugin {
		inferred, source, ierr := suggestManagedWorkflowPreset(topo, m.Compute.GPUs, m.Compute.Workers, inferLane)
		if ierr == nil {
			preset = &inferred
			resolvedProfileName = inferred.Preset.Profile
			warnings = append(warnings, fmt.Sprintf("inferred preset: %s (team=%s from %s, lane=%s, gpus=%d, workers=%d; set policy.preset to override or policy.team to switch teams)", inferred.Preset.Name, inferred.Preset.Team, source, inferLane, m.Compute.GPUs, m.Compute.Workers))
		}
	}
	if preset != nil && !namespaceExplicit && preset.Preset.Namespace != "" {
		namespace = preset.Preset.Namespace
	}
	if preset != nil && gpuResourceMode == manifest.GPUResourceModeDRA {
		draPreset := runtopology.WithDRAQueue(*preset)
		preset = &draPreset
	}
	topologyHolder := jobrender.Options{}
	topoWarnings, err := topo.applyWithChangedAndWorkspaceQueue(&topologyHolder, preset, changed, o.workspaceQueueResolved)
	if err != nil {
		return err
	}
	configureGPUQueueModeWithChanged(gpuResourceMode, &topologyHolder, changed)
	if o.workspaceQueueResolved {
		makeWorkspaceQueueAuthoritative(&topologyHolder)
	}
	if nodeSelector, err = mergeNodeSelectors(nodeSelector, topologyHolder.NodeSelector); err != nil {
		return err
	}
	if gpuResourceMode == manifest.GPUResourceModeDevicePlugin && preset == nil {
		topologyHolder.DisableDefaultPriorities = true
	}
	warnings = append(warnings, topoWarnings...)
	if err := validateManagedWorkflowTopologyIntent(m, topologyHolder, preset, workloadKind); err != nil {
		return err
	}
	var topologyProfile *profile.Profile
	if resolvedProfileName != "" || preset != nil {
		p := resourceProfileForRender(resolvedProfileName, preset, topo.resourceProfileOptions(), m.Compute.GPUs)
		topologyProfile = &p
		topologyHolder.GPUClass, _ = runtopology.ResolveGPUClass(p, topologyHolder.GPUClass)
	}
	var kvSpec *kvspec.Spec
	if len(m.Runtime.EnvKV) > 0 || o.keyVault != "" {
		entries, err := kvspec.ParseEntries(m.Runtime.EnvKV, o.keyVault)
		if err != nil {
			return fmt.Errorf("runtime.env_kv: %w", err)
		}
		if len(entries) > 0 {
			if o.kvTenantID == "" {
				return fmt.Errorf("runtime.env_kv_tenant is required when runtime.env_kv is set")
			}
			if o.kvClientID == "" {
				return fmt.Errorf("runtime.env_kv_client is required when runtime.env_kv is set")
			}
			if o.serviceAccountName == "" {
				return fmt.Errorf("a ServiceAccount is required when runtime.env_kv is set; use the ServiceAccount reconciled from TauWorkspace spec.workloadIdentity")
			}
			if kvSpec, err = kvspec.NewSpec(entries, o.kvTenantID, o.kvClientID); err != nil {
				return fmt.Errorf("key vault spec: %w", err)
			}
		}
	}

	metricsOffloadOptions, err := managedWorkflowMetricsOffload(ctx)
	if err != nil {
		return err
	}
	var r *kube.Runner
	if dryRun != "client" {
		r = kube.New(kubeContext)
	}
	allowImplicitAuto := preset == nil && resolvedProfileName == ""
	resolveWarnings, err := resolveAccessibleQueueNamespace(ctx, r, namespaceExplicit, &namespace, &topologyHolder, dryRun, workloadKindK8sResource(workloadKind), allowImplicitAuto)
	if err == nil {
		namespace, err = requireWorkloadNamespace(namespace)
	}
	if err != nil {
		return err
	}
	request.Options.namespace = namespace
	warnings = append(warnings, resolveWarnings...)
	if dryRun != "client" {
		var available []secretpreflight.AvailableSecret
		if jobSecret != nil {
			keys := make([]string, 0, len(jobSecret.StringData))
			for key := range jobSecret.StringData {
				keys = append(keys, key)
			}
			available = append(available, secretpreflight.AvailableSecret{Name: jobSecret.Name, Keys: keys})
		}
		if err := secretpreflight.ValidateRequiredEnv(ctx, r, namespace, m.RuntimeEnv(), available...); err != nil {
			return err
		}
		preflightNodeSelector, err := storagePreflightNodeSelector(topologyProfile, topologyHolder, nodeSelector)
		if err != nil {
			return err
		}
		if err := validateStorageNodeCompatibility(ctx, r, namespace, m, preflightNodeSelector); err != nil {
			return err
		}
	}
	explicitAuto, implicitAuto := prepareAutoQueueRender(&topologyHolder, preset, allowImplicitAuto, dryRun)
	capture := buildManagedWorkflowCaptureMetadata(ctx, captureCommand, m, raw, namespace, workloadKind)
	capture = addRunWorkspaceMetadata(capture, o.workspace, o.workspaceResultScope)
	labels, annotations := experiment.MergeMetadata(topologyHolder.Labels, topologyHolder.Annotations, capture)
	labels = workloadmeta.StampWorkspace(labels, o.workspace)
	if o.submissionID != "" {
		annotations[workloadmeta.AnnotationSubmissionID] = o.submissionID
	}

	if jobSecret != nil {
		jobSecret.OwnerName = m.ResourceName()
		jobSecret.OwnerKind = workloadKindToK8sKind(workloadKind)
	}

	renderManagedWorkflow := func() ([]byte, error) {
		return manifest.Render(manifest.RenderOptions{
			Manifest:           m,
			ManifestRaw:        raw,
			ManifestFilename:   filepath.Base(manifestPath),
			Namespace:          namespace,
			SmokePairs:         o.smokePairs,
			ExtraScripts:       extras,
			TopologyProfile:    topologyProfile,
			TopologyOptions:    topologyOptionsFromSubmit(topologyHolder),
			ProfileName:        resolvedProfileName,
			Labels:             labels,
			Annotations:        annotations,
			WorkloadKind:       workloadKind,
			GPUResourceMode:    gpuResourceMode,
			MIGProfile:         o.migProfile,
			NodeSelector:       nodeSelector,
			MainScript:         mainScriptBytes,
			UpstreamCheckpoint: o.upstreamCheckpoint,
			JobSecret:          jobSecret,
			RedactSecrets:      dryRun == "client",
			Profile:            profileOptions,
			MetricsOffload:     metricsOffloadOptions,
			KVSpec:             kvSpec,
			ServiceAccountName: o.serviceAccountName,
		})
	}
	rendered, err := renderManagedWorkflow()
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	autoWarnings, err := topo.resolveAutoQueueFromManifest(ctx, r, namespace, &topologyHolder, rendered, dryRun, explicitAuto, implicitAuto)
	if err != nil {
		return err
	}
	if explicitAuto || implicitAuto {
		rendered, err = renderManagedWorkflow()
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
	}
	warnings = append(warnings, autoWarnings...)
	if dryRun != "client" {
		rendered, err = prepareGeneratedQueueTopology(ctx, r, namespace, rendered, &topologyHolder, queueValidationPolicyFor(preset, o.workspaceQueueResolved), renderManagedWorkflow)
		if err != nil {
			return err
		}
	}
	for _, warning := range warnings {
		fmt.Fprintln(stderr, warning)
	}

	if dryRun == "client" {
		_, err := stdout.Write(rendered)
		return err
	}

	if r == nil {
		r = kube.New(kubeContext)
	}
	submissionRunner := newKubernetesRunSubmissionRunner(r)
	submission := runSubmission{
		Resource:     workloadKindK8sResource(workloadKind),
		Name:         m.ResourceName(),
		Namespace:    namespace,
		SubmissionID: o.submissionID,
		Manifest:     rendered,
		DryRun:       dryRun,
	}
	if dryRun != "" {
		result, err := submitRunWorkload(ctx, submissionRunner, submission)
		fmt.Fprint(stdout, result.Output)
		return err
	}
	prepared, err := prepareManagedSubmission(rendered, workloadKindToK8sKind(workloadKind), m.ResourceName(), o.submissionID)
	if err != nil {
		return err
	}
	submission.Manifest = prepared.Primary
	result, err := submitRunWorkload(ctx, submissionRunner, submission)
	fmt.Fprint(stdout, result.Output)
	if err != nil {
		return err
	}
	cleanupSubmissions := []runSubmission{submission}
	if jobSecret != nil {
		cleanupSubmissions = append([]runSubmission{{
			Resource:     "secret",
			Name:         jobSecret.Name,
			Namespace:    namespace,
			SubmissionID: o.submissionID,
		}}, cleanupSubmissions...)
	}
	if len(prepared.Ancillary) > 0 {
		out, ancillaryErr := submissionRunner.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, prepared.Ancillary)
		fmt.Fprint(stdout, out)
		if ancillaryErr != nil {
			return withRunSubmissionCleanup(fmt.Errorf("reconcile ancillary workload resources: %w", ancillaryErr), submissionRunner, cleanupSubmissions...)
		}
	}
	if jobSecret != nil {
		if err := patchSecretOwnerRef(ctx, submissionRunner.Runner, namespace, jobSecret, workloadKind); err != nil {
			return withRunSubmissionCleanup(fmt.Errorf("ownerRef patch failed: %w", err), submissionRunner, cleanupSubmissions...)
		}
	}
	out, activationErr := activateRunSubmissionQueue(ctx, submissionRunner, submission, prepared.QueueName)
	fmt.Fprint(stdout, out)
	if activationErr != nil {
		return withRunSubmissionCleanup(activationErr, submissionRunner, cleanupSubmissions...)
	}
	printManagedWorkflowSubmission(stdout, m, namespace, kubeContext, workloadKind, resolvedProfileName, extras, preset)
	return nil
}

func printManagedWorkflowSubmission(stdout io.Writer, m *manifest.Manifest, namespace, kubeContext, workloadKind, resolvedProfileName string, extras []manifest.ExtraScript, preset *runtopology.ResolvedPreset) {
	profileDetail := ""
	if resolvedProfileName != "" {
		profileDetail = fmt.Sprintf(", profile=%s", resolvedProfileName)
	}
	kindLabel := "job"
	resourceName := m.ResourceName()
	isRay := workloadKind == manifest.WorkloadKindRayJob || workloadKind == manifest.WorkloadKindRayJobEval
	followUp := fmt.Sprintf(
		"status:  tau run status %s -n %s%s\n"+
			"logs:    tau run logs %s -n %s -f%s\n"+
			"profile: tau run status %s -n %s --run-profile%s\n",
		resourceName, namespace, contextFlag(kubeContext),
		resourceName, namespace, contextFlag(kubeContext),
		resourceName, namespace, contextFlag(kubeContext))
	if isRay {
		if workloadKind == manifest.WorkloadKindRayJob {
			kindLabel = "rayjob"
		} else {
			kindLabel = "rayjob-eval"
		}
		// tau run get reads batch/v1 Job result annotations today. Surface
		// RayJob-native lifecycle commands and a Kueue lookup without
		// advertising unsupported retrieval.
		followUp = fmt.Sprintf(
			"status:  tau run status %s -n %s%s\n"+
				"rayjob:  kubectl get rayjob %s -n %s%s\n"+
				"head:    cluster=$(kubectl get rayjob %s -n %s -o jsonpath='{.status.rayClusterName}'%s) && kubectl get pod -n %s -l ray.io/cluster=$cluster,ray.io/node-type=head%s\n"+
				"logs:    tau run logs %s -n %s -f%s\n"+
				"kueue:   uid=$(kubectl get rayjob %s -n %s -o jsonpath='{.metadata.uid}'%s) && kubectl get workload -n %s -l kueue.x-k8s.io/job-uid=$uid%s\n",
			resourceName, namespace, contextFlag(kubeContext),
			resourceName, namespace, contextFlag(kubeContext),
			resourceName, namespace, contextFlag(kubeContext), namespace, contextFlag(kubeContext),
			resourceName, namespace, contextFlag(kubeContext),
			resourceName, namespace, contextFlag(kubeContext),
			namespace, contextFlag(kubeContext))
	}
	fmt.Fprintf(stdout, "\nsubmitted %s (kind=%s, gpus=%d%s, namespace=%s)\n%s",
		resourceName, kindLabel, m.Compute.GPUs, profileDetail, namespace, followUp)
	if len(extras) > 0 {
		fmt.Fprintf(stdout, "scripts: %s\n", extraScriptPaths(extras))
	}
	dataPVC := m.DataPVC()
	if isRay {
		if dataPVC != "" {
			fmt.Fprintf(stdout, "artifacts: %s/metrics.json (durable on %s PVC mounted at /data)\n", storage.DurableFinetuneDir(m.Name), dataPVC)
		} else {
			fmt.Fprintln(stdout, "artifacts: /data is ephemeral (emptyDir); set storage.data_pvc to persist files beyond RayJob teardown")
		}
	} else {
		resultPath := storage.DurableFinetuneDir(m.Name) + "/metrics.json"
		if dataPVC != "" {
			fmt.Fprintf(stdout,
				"results: tau run get %s -n %s%s --path %s --pvc %s\n"+
					"output:  %s (durable copy on %s PVC mounted at /data)\n",
				resourceName, namespace, contextFlag(kubeContext), resultPath, dataPVC, resultPath, dataPVC)
		} else {
			fmt.Fprintf(stdout, "output:  %s (ephemeral; set storage.data_pvc to persist and fetch results)\n", resultPath)
		}
	}
	if preset != nil {
		fmt.Fprint(stdout, formatPresetHandoff(*preset))
	}
}
