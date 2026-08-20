// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/resume"
	"github.com/Azure/taugrid/cli/internal/storage"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func newRunResumeCmdWithConnectionFactory(connectionFactory runConnectionEnsurerFactory) *cobra.Command {
	return newRunResumeCmdWithDependencies(connectionFactory, resumeCommandHooks{})
}

func newRunResumeCmdWithDependencies(
	connectionFactory runConnectionEnsurerFactory,
	hooks resumeCommandHooks,
) *cobra.Command {
	var (
		configPath   string
		namespace    string
		kubeContext  string
		dryRun       string
		from         string
		force        bool
		betaFeatures []string
	)
	cmd := &cobra.Command{
		Use:   "resume <name> --config tau.yaml",
		Short: "Resume a failed job from its last checkpoint",
		Long: `Resume a training job that failed due to preemption, OOM, eviction, or
other transient errors. The command inspects the failed workload, discovers
the durable checkpoint directory, injects TAU_RESUME_FROM into the pod
env, deletes the old workload, and re-submits with the same config.

The trainer reads TAU_RESUME_FROM at startup and loads the latest
checkpoint from that directory to continue training.

For MultiKueue jobs, tau deletes the manager-cluster Job/RayJob and waits for
the manager-side Workload finalizer proof before resubmitting. It never calls
worker clusters directly during resume.

Requirements:
  - The workload must exist and be in a failed state.
  - --config is required (the original config cannot be recovered from
    the cluster without a lockfile registry).
  - If the failure was OOM, --force is required (same config = same OOM
    unless you also change compute resources).

Examples:
  tau run resume lora-7b-001 --config tau.yaml -n ray
  tau run resume lora-7b-001 --config tau.yaml --from /data/checkpoints/custom
  tau run resume lora-7b-001 --config tau.yaml --force -n ray
  tau run resume lora-7b-001 --config tau.yaml --dry-run=client`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if from != "" && !strings.HasPrefix(from, "/") {
				return fmt.Errorf("--from must be an absolute path (starts with /); got %q", from)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			projectName, err := cmd.Flags().GetString("project")
			if err != nil {
				return fmt.Errorf("read inherited --project flag: %w", err)
			}
			routing, restore, err := resolveResumeRouting(
				cmd,
				cwd,
				projectName,
				configPath,
				kubeContext,
				namespace,
				runContextExplicit(cmd),
				cmd.Flags().Changed("namespace"),
				connectionFactory(cmd),
			)
			if err != nil {
				return err
			}
			defer restore()
			routing.TargetOptions.betaFeatures, err = mergeBetaFeatureAcknowledgements(
				routing.TargetOptions.betaFeatures,
				betaFeatures,
			)
			if err != nil {
				return err
			}
			return runResumeCommand(
				cmd,
				name,
				routing,
				from,
				dryRun,
				force,
				hooks,
			)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to Tau experiment config (required)")
	_ = cmd.MarkFlagRequired("config")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&dryRun, "dry-run", "", "client|server (default: actually apply)")
	cmd.Flags().StringVar(&from, "from", "", "checkpoint directory override (default: /data/checkpoints/finetunes/<name>)")
	cmd.Flags().BoolVar(&force, "force", false, "resume even after OOM (use with resource increases)")
	cmd.Flags().StringSliceVar(&betaFeatures, "acknowledge-beta-feature", nil, "acknowledge a Beta execution feature for the replacement workload (supported: multikueue; repeatable)")
	return cmd
}

type resumeRouting struct {
	ConfigPath    string
	ConfigName    string
	KubeContext   string
	Namespace     string
	TargetOptions unresolvedRunOptions
}

func resolveResumeRouting(
	cmd *cobra.Command,
	cwd, projectName, configPath, kubeContext, namespace string,
	contextExplicit, namespaceExplicit bool,
	ensurer runConnectionEnsurer,
) (resumeRouting, func(), error) {
	kubeContext = strings.TrimSpace(kubeContext)
	resolution, err := resolveRunRequest(cwd, projectName, configPath, "", false)
	if err != nil {
		return resumeRouting{}, nil, err
	}
	targetOptions, configName, err := loadRunConfig(resolution.Input.ConfigPath)
	if err != nil {
		return resumeRouting{}, nil, err
	}
	if contextExplicit && kubeContext != "" {
		targetOptions.kubeContext = kubeContext
		targetOptions.kubeContextExplicit = true
	}
	if !resolution.Connection.Git {
		hasExplicitContext := contextExplicit && kubeContext != ""
		if !hasExplicitContext && !namespaceExplicit {
			return resumeRouting{}, nil, fmt.Errorf("lifecycle commands outside a Git repository require explicit --context or --namespace")
		}
		resolvedNamespace, err := requireWorkloadNamespace(namespace)
		if err != nil {
			return resumeRouting{}, nil, err
		}
		return resumeRouting{
			ConfigPath:    resolution.Input.ConfigPath,
			ConfigName:    configName,
			KubeContext:   kubeContext,
			Namespace:     resolvedNamespace,
			TargetOptions: targetOptions,
		}, func() {}, nil
	}
	resolvedOptions := targetOptions
	connection := workspaceconnection.ActiveConnection{}
	if resolution.Connection.Catalog {
		resolvedOptions, connection, err = applyAutomaticRunConnection(
			cmd.Context(),
			targetOptions,
			resolution.Connection,
			false,
			ensurer,
		)
		if err != nil {
			return resumeRouting{}, nil, err
		}
	} else if !targetOptions.kubeContextExplicit {
		connection, err = ensureRunConnection(cmd.Context(), ensurer, resolution.Connection)
		if err != nil {
			if !errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
				return resumeRouting{}, nil, err
			}
			connection = workspaceconnection.ActiveConnection{}
		}
		if connection.ContextName != "" {
			resolvedOptions.kubeContext = connection.ContextName
		}
	}
	resolvedContext := firstNonEmpty(resolvedOptions.kubeContext, kubeContext)
	resolvedNamespace := namespace
	if !namespaceExplicit && connection.Namespace != "" {
		resolvedNamespace = connection.Namespace
	}
	resolvedNamespace, err = requireWorkloadNamespace(resolvedNamespace)
	if err != nil {
		return resumeRouting{}, nil, err
	}
	restore, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return resumeRouting{}, nil, err
	}
	return resumeRouting{
		ConfigPath:    resolution.Input.ConfigPath,
		ConfigName:    configName,
		KubeContext:   resolvedContext,
		Namespace:     resolvedNamespace,
		TargetOptions: resolvedOptions,
	}, restore, nil
}

type resumePlan struct {
	Reason        resume.FailureReason
	CheckpointDir string
	ConfigWarning string // non-empty if config hash changed
}

func resumePreflight(snap status.Snapshot, name, configHash, from string, force bool) (resumePlan, error) {
	if !snap.JobFound && !snap.RayJob.Found {
		return resumePlan{}, fmt.Errorf("no Job or RayJob %q found in namespace %q", name, snap.Namespace)
	}
	if snap.JobActive > 0 || isRayJobRunning(snap) {
		return resumePlan{}, fmt.Errorf("workload %q is still running — cancel it first or wait for it to fail", name)
	}
	if status.StartupComplete(snap) {
		return resumePlan{}, fmt.Errorf("workload %q completed successfully — nothing to resume", name)
	}
	if !status.StartupFailed(snap) {
		return resumePlan{}, fmt.Errorf("workload %q is still running — cancel it first or wait for it to fail", name)
	}

	reason := resume.ClassifyFailure(snap)
	if !resume.IsRetryable(reason) {
		return resumePlan{}, fmt.Errorf(
			"workload %q failed with non-retryable reason %s — inspect logs/status before resubmitting manually",
			name, reason)
	}
	if resume.IsOOM(reason) && !force {
		return resumePlan{}, fmt.Errorf(
			"workload %q failed with OOM — the same config will likely OOM again.\n"+
				"Increase compute resources in your config, then resume with --force",
			name)
	}

	var configWarning string
	if configHash != "" {
		if oldHash, ok := snap.Annotations[experiment.AnnotationConfigHash]; ok && oldHash != "" {
			if configHash != oldHash {
				oldPrefix := oldHash
				if len(oldPrefix) > 8 {
					oldPrefix = oldPrefix[:8]
				}
				newPrefix := configHash
				if len(newPrefix) > 8 {
					newPrefix = newPrefix[:8]
				}
				configWarning = fmt.Sprintf("config hash changed (%s → %s) — the resumed workload may behave differently",
					oldPrefix, newPrefix)
			}
		}
	}

	checkpointDir := from
	if checkpointDir == "" {
		checkpointDir = storage.DurableFinetuneDir(name)
	}

	return resumePlan{
		Reason:        reason,
		CheckpointDir: checkpointDir,
		ConfigWarning: configWarning,
	}, nil
}

func isRayJobRunning(snap status.Snapshot) bool {
	if !snap.RayJob.Found {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(snap.RayJob.JobDeploymentStatus))
	return s == "running"
}

type resumeCommandHooks struct {
	fetchStatus    func(context.Context, string, string, string) (status.Snapshot, error)
	fetchWorkspace smokeWorkspaceFetcher
	validateTarget func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error
	deleteOld      func(context.Context, string, string, string, io.Writer) error
	executeTarget  func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error
	resolveProfile func(context.Context, unresolvedRunOptions) (unresolvedRunOptions, error)
}

func (h resumeCommandHooks) fetch(
	ctx context.Context,
	kubeContext, namespace, name string,
) (status.Snapshot, error) {
	if h.fetchStatus != nil {
		return h.fetchStatus(ctx, kubeContext, namespace, name)
	}
	return status.Fetch(ctx, kube.New(kubeContext), namespace, name)
}

func (h resumeCommandHooks) workspace(
	cmd *cobra.Command,
	kubeContext, name string,
) (tauworkspace.Workspace, error) {
	if h.fetchWorkspace != nil {
		return h.fetchWorkspace(cmd, kubeContext, systemNamespaceFromCommand(cmd), name)
	}
	return fetchWorkspace(cmd, kubeContext, systemNamespaceFromCommand(cmd), name)
}

func (h resumeCommandHooks) delete(
	ctx context.Context,
	kubeContext, namespace, name string,
	output io.Writer,
) error {
	if h.deleteOld != nil {
		return h.deleteOld(ctx, kubeContext, namespace, name, output)
	}
	runner := kube.New(kubeContext)
	return deleteWorkloadAndWaitForManagerCleanup(ctx, runner, name, namespace, output, managerCleanupOptions{
		Timeout:  defaultManagerCleanupTimeout,
		Interval: defaultManagerCleanupInterval,
	}, newManagerCleanupHooks(runner, namespace, name))
}

func (h resumeCommandHooks) execute(
	parent *cobra.Command,
	target resolvedRunTarget,
	captureCommand string,
	experiment runExperimentMetadata,
) error {
	if h.executeTarget != nil {
		return h.executeTarget(parent, target, captureCommand, experiment)
	}
	return executeRunTarget(parent, target, captureCommand, experiment)
}

func (h resumeCommandHooks) validate(
	parent *cobra.Command,
	target resolvedRunTarget,
	captureCommand string,
	experiment runExperimentMetadata,
) error {
	if h.validateTarget != nil {
		return h.validateTarget(parent, target, captureCommand, experiment)
	}
	return executeRunTarget(parent, target, captureCommand, experiment)
}

func (h resumeCommandHooks) profile(
	ctx context.Context,
	options unresolvedRunOptions,
) (unresolvedRunOptions, error) {
	if h.resolveProfile != nil {
		return h.resolveProfile(ctx, options)
	}
	if strings.TrimSpace(options.workloadProfileSnapshot) != "" {
		return resolveSnapshotRunWorkloadProfile(ctx, options)
	}
	return resolveClusterRunWorkloadProfile(ctx, options)
}

func runResumeCommand(
	cmd *cobra.Command,
	name string,
	routing resumeRouting,
	from, dryRun string,
	force bool,
	hooks resumeCommandHooks,
) error {
	w := cmd.OutOrStdout()
	targetOptions := routing.TargetOptions
	if routing.ConfigName != "" && routing.ConfigName != name {
		fmt.Fprintf(w, "note: config names job %q but resuming %q\n", routing.ConfigName, name)
	}

	snap, err := hooks.fetch(cmd.Context(), routing.KubeContext, routing.Namespace, name)
	if err != nil {
		return fmt.Errorf("fetching workload status: %w", err)
	}
	if targetOptions.metricsOffloadEnabled {
		targetOptions.metricsSessionID = strings.TrimSpace(snap.Annotations[workloadmeta.AnnotationMetricsSession])
		if targetOptions.metricsSessionID == "" {
			targetOptions.metricsSessionID, err = newMetricsSessionID()
			if err != nil {
				return err
			}
		}
	}

	configHash, hashErr := experiment.HashFile(routing.ConfigPath)
	if hashErr != nil {
		fmt.Fprintf(w, "warning: cannot hash config file: %v\n", hashErr)
	}

	plan, err := resumePreflight(snap, name, configHash, from, force)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "failure reason: %s\n", plan.Reason)
	if plan.ConfigWarning != "" {
		fmt.Fprintf(w, "warning: %s\n", plan.ConfigWarning)
	}
	fmt.Fprintf(w, "checkpoint: %s\n", plan.CheckpointDir)

	filtered := targetOptions.env[:0]
	for _, e := range targetOptions.env {
		if !strings.HasPrefix(e, "TAU_RESUME_FROM=") {
			filtered = append(filtered, e)
		}
	}
	targetOptions.env = append(filtered, "TAU_RESUME_FROM="+plan.CheckpointDir)
	targetOptions.nameFromConfig = true

	if targetOptions.workspace != "" {
		currentWorkspace, err := hooks.workspace(cmd, routing.KubeContext, targetOptions.workspace)
		if err != nil {
			return fmt.Errorf("refreshing TauWorkspace %q before resume: %w", targetOptions.workspace, err)
		}
		targetOptions, err = applyWorkspaceDefaults(targetOptions, currentWorkspace, name)
		if err != nil {
			return fmt.Errorf("applying current TauWorkspace routing before resume: %w", err)
		}
		if routing.Namespace != "" && routing.Namespace != targetOptions.namespace {
			return fmt.Errorf(
				"failed workload is in namespace %q but TauWorkspace %q now targets namespace %q; refusing to delete it",
				routing.Namespace,
				targetOptions.workspace,
				targetOptions.namespace,
			)
		}
	}
	if err := validateMetricsResumeStateLocation(snap, targetOptions, name); err != nil {
		return err
	}
	applyResumeOverrides(&targetOptions, routing.Namespace, routing.KubeContext, dryRun)
	// Status and failure inspection above remain available regardless of the
	// current profile/Beta gate. Resolve the replacement profile only after
	// observation, and always before deleting the failed workload.
	productionExecution := hooks.executeTarget == nil && hooks.validateTarget == nil
	if hooks.resolveProfile != nil || productionExecution {
		targetOptions, err = hooks.profile(cmd.Context(), targetOptions)
		if err != nil {
			return fmt.Errorf("resolving current workload profile for replacement: %w", err)
		}
	}
	if err := validateRunDispatchOptions(targetOptions); err != nil {
		return err
	}

	captureCommand := fmt.Sprintf("tau run resume %s --config %s", name, routing.ConfigPath)
	if force {
		captureCommand += " --force"
	}
	if from != "" {
		captureCommand += " --from " + from
	}

	if dryRun == "" {
		preflightOptions := targetOptions
		preflightOptions.dryRun = "client"
		preflightTarget, err := resolveRunTarget(preflightOptions, name)
		if err != nil {
			return fmt.Errorf("resolving replacement workload for validation: %w", err)
		}
		preflightParent := &cobra.Command{}
		preflightParent.SetContext(cmd.Context())
		preflightParent.SetIn(cmd.InOrStdin())
		preflightParent.SetOut(io.Discard)
		preflightParent.SetErr(io.Discard)
		if err := hooks.validate(preflightParent, preflightTarget, captureCommand, targetOptions.experiment); err != nil {
			return fmt.Errorf("validating replacement workload before deleting old workload: %w", err)
		}

		fmt.Fprintf(w, "deleting failed workload %s/%s...\n", routing.Namespace, name)
		if err := hooks.delete(cmd.Context(), routing.KubeContext, routing.Namespace, name, w); err != nil {
			return fmt.Errorf("deleting old workload: %w", err)
		}
	}

	target, err := resolveRunTarget(targetOptions, name)
	if err != nil {
		return fmt.Errorf("resolving run target: %w", err)
	}
	return hooks.execute(cmd, target, captureCommand, targetOptions.experiment)
}

func validateMetricsResumeStateLocation(snapshot status.Snapshot, options unresolvedRunOptions, name string) error {
	if !options.metricsOffloadEnabled ||
		strings.TrimSpace(snapshot.Annotations[workloadmeta.AnnotationMetricsSession]) == "" {
		return nil
	}
	previousOutput := strings.TrimSpace(snapshot.Annotations[experiment.AnnotationResultPath])
	currentOutput := strings.TrimSpace(options.output)
	if currentOutput == "" && firstNonEmpty(options.dataPVC, options.resultPVC) != "" {
		currentOutput = defaultRunOutputPath(name)
	}
	if previousOutput != "" && currentOutput != "" && path.Clean(previousOutput) != path.Clean(currentOutput) {
		return fmt.Errorf(
			"metrics-enabled resume cannot change storage.output from %q to %q because telemetry checkpoints are session-scoped beneath the original output; start a fresh run instead",
			previousOutput,
			currentOutput,
		)
	}
	previousPVC := strings.TrimSpace(snapshot.Annotations[experiment.AnnotationResultPVC])
	currentPVC := strings.TrimSpace(firstNonEmpty(options.dataPVC, options.resultPVC))
	if previousPVC != "" && currentPVC != "" && previousPVC != currentPVC {
		return fmt.Errorf(
			"metrics-enabled resume cannot change the output PVC from %q to %q because telemetry checkpoints must remain on the original volume; start a fresh run instead",
			previousPVC,
			currentPVC,
		)
	}
	return nil
}

func applyResumeOverrides(o *unresolvedRunOptions, ns, kubeContext, dryRun string) {
	if ns != "" {
		o.namespace = ns
	}
	if kubeContext != "" {
		o.kubeContext = kubeContext
	}
	if dryRun != "" {
		o.dryRun = dryRun
	}
}
