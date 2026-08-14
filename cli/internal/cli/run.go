// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/cli/internal/onboarding"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/runconfig"
	runtopology "github.com/Azure/taugrid/core/topology"
)

func newRunCmd() *cobra.Command {
	return newRunCmdWithConnectionFactory(defaultRunConnectionEnsurer)
}

func newRunCmdWithConnectionFactory(connectionFactory runConnectionEnsurerFactory) *cobra.Command {
	var (
		configPath         string
		namespace          string
		workspace          string
		kubeContext        string
		dryRun             string
		keyVault           string
		kvTenantID         string
		kvClientID         string
		serviceAccountName string
		projectName        string
	)

	cmd := &cobra.Command{
		Use:   "run [TARGET] [--config tau.yaml]",
		Short: "Run a Tau workload",
		Long: `Run a Tau workload from an experiment config.

In a Tau-enabled repository, "smoke" runs the built-in platform onboarding
smoke and any other TARGET resolves tau/TARGET.yaml. The config describes the
entrypoint, runtime, compute, storage, and profiler intent; workspace
policy and cluster access come from tau/workspace.connection.yaml.

The built-in "smoke" target is a platform probe, not your workload: it runs a
public base image and a trivial command to prove the workspace, queue, and pod
path work. It does not exercise your container image, GPUs, or /data storage.
Run your own config for that.

Evaluation is not a separate command or config shape: an eval is just a run
whose image evaluates instead of trains. Point script/image at your eval
entrypoint, pass model and output paths through runtime.env, and set
storage.output so "tau run get" can fetch the results.
See: tau run explain-config

Common examples:
  tau run smoke
  tau run train
  tau run
  tau run mc-rl --config experiments/mc-rl/tau.yaml
  tau run --dry-run=client
  tau run --config examples/market-policy/tau.yaml --dry-run=client
  tau run --config examples/ray-tune-smoke/tau.yaml --dry-run=client
  tau run status mc-rl`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() {
				runErr = onboarding.Explain(runErr)
			}()
			requestedTarget := ""
			if len(args) == 1 {
				requestedTarget = args[0]
			}
			startDir, err := os.Getwd()
			if err != nil {
				return err
			}
			resolution, err := resolveRunRequest(
				startDir,
				projectName,
				configPath,
				requestedTarget,
				cmd.Flags().Changed("workspace") && strings.TrimSpace(workspace) != "",
			)
			if err != nil {
				return err
			}
			input := resolution.Input
			if input.BuiltinSmoke {
				return executeBuiltinSmoke(cmd, builtinSmokeCLIOptions{
					Workspace:           workspace,
					Namespace:           namespace,
					KubeContext:         kubeContext,
					KubeContextExplicit: runContextExplicit(cmd),
					KubeContextFromFlag: cmd.Flags().Changed("context"),
					DryRun:              dryRun,
					Connection:          resolution.Connection,
					ConnectionFactory:   connectionFactory,
				})
			}
			resolvedConfigPath := input.ConfigPath
			if resolvedConfigPath == "" {
				return fmt.Errorf("no Tau run config found; create tau.yaml or pass --config")
			}
			targetOptions, configName, err := loadRunConfig(resolvedConfigPath)
			if err != nil {
				return err
			}
			name := ""
			if input.ExplicitConfig {
				name = requestedTarget
			}
			if name == "" {
				name = configName
				targetOptions.nameFromConfig = name != ""
			}
			if cmd.Flags().Changed("namespace") {
				targetOptions.namespace = namespace
			}
			if cmd.Flags().Changed("workspace") {
				targetOptions.workspace = workspace
				targetOptions.workspaceExplicit = strings.TrimSpace(workspace) != ""
			}
			if runContextExplicit(cmd) {
				targetOptions.kubeContext = kubeContext
				targetOptions.kubeContextExplicit = strings.TrimSpace(kubeContext) != ""
				targetOptions.kubeContextFromFlag = cmd.Flags().Changed("context")
			}
			if cmd.Flags().Changed("dry-run") {
				targetOptions.dryRun = dryRun
			}
			targetOptions.keyVault = keyVault
			targetOptions.kvTenantID = kvTenantID
			targetOptions.kvClientID = kvClientID
			targetOptions.serviceAccountName = serviceAccountName
			connectionEnsurer := connectionFactory(cmd)
			targetOptions, connection, err := applyAutomaticRunConnection(
				cmd.Context(),
				targetOptions,
				resolution.Connection,
				false,
				connectionEnsurer,
			)
			if err != nil {
				return err
			}
			restoreKubeconfig, err := useKubeconfig(connection.KubeconfigPath)
			if err != nil {
				return err
			}
			defer restoreKubeconfig()
			// TauGrid v0 activates exactly one workspace per cluster, so a
			// researcher should not have to name it. When --workspace was not
			// given and the connection descriptor did not carry one, resolve
			// the cluster's primary workspace instead of silently running
			// without workspace defaults.
			//
			// Discovery is best-effort: clusters without TauGrid installed
			// legitimately have no TauWorkspace and ran fine before this, so a
			// failure here warns and falls through to the pre-existing
			// namespace handling rather than failing the run.
			// Workspace discovery is skipped for --dry-run=client so a
			// client-side render still works on a cluster without a
			// TauWorkspace. This is not an offline path: the connection step
			// above may already have contacted the cluster.
			if strings.TrimSpace(targetOptions.workspace) == "" && targetOptions.dryRun != "client" {
				discovered, derr := discoverPrimaryWorkspace(cmd, targetOptions.kubeContext)
				if derr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not resolve this cluster's workspace automatically: %v\n", derr)
				} else {
					targetOptions.workspace = discovered.Metadata.Name
				}
			}
			// A client render may retain an explicitly named workspace as
			// metadata, but it must not fetch that workspace. Values normally
			// inherited from the live object remain explicit config values or
			// unresolved client-side placeholders.
			if targetOptions.workspace != "" && targetOptions.dryRun != "client" {
				workspaceStatus, err := fetchWorkspace(cmd, targetOptions.kubeContext, tauworkspace.PlatformNamespace, targetOptions.workspace)
				if err != nil {
					return err
				}
				targetOptions, err = applyWorkspaceDefaults(targetOptions, workspaceStatus, name)
				if err != nil {
					return err
				}
			}
			// The workspace owns the target namespace. When the TauWorkspace
			// itself is unreachable, fall back to the namespace recorded in the
			// active connection descriptor, which is what the lifecycle
			// subcommands already use. Without this the render path dropped
			// connection.Namespace and inherited an unrelated flag default, so
			// `tau run` applied into a different namespace than `tau run status`
			// went on to query.
			if strings.TrimSpace(targetOptions.namespace) == "" {
				targetOptions.namespace = strings.TrimSpace(connection.Namespace)
			}
			if err := validateRunDispatchOptions(targetOptions); err != nil {
				return err
			}
			if targetOptions.metricsOffloadEnabled && strings.TrimSpace(targetOptions.metricsSessionID) == "" {
				targetOptions.metricsSessionID, err = newMetricsSessionID()
				if err != nil {
					return err
				}
			}
			if err := ensureArtifactPublicationID(&targetOptions); err != nil {
				return err
			}
			captureCommand := buildRunCaptureCommand(cmd, name, targetOptions.nameFromConfig, resolvedConfigPath)
			target, err := resolveRunTarget(targetOptions, name)
			if err != nil {
				return err
			}
			if err := executeRunTarget(cmd, target, captureCommand, targetOptions.experiment); err != nil {
				return err
			}
			if targetOptions.maxRetries > 0 && targetOptions.dryRun == "" {
				retryDispatch := targetOptions
				switch {
				case target.job != nil:
					retryDispatch = target.job.Options
				case target.ray != nil:
					retryDispatch = target.ray.Options
				case target.managedWorkflow != nil:
					retryDispatch = target.managedWorkflow.Options
				}
				retryNS := retryDispatch.namespace
				return retryLoop(cmd, retryLoopOptions{
					name:           name,
					namespace:      retryNS,
					kubeContext:    targetOptions.kubeContext,
					configPath:     resolvedConfigPath,
					maxRetries:     targetOptions.maxRetries,
					retryOn:        targetOptions.retryOn,
					checkpointPath: targetOptions.checkpointPath,
					backoffInitial: targetOptions.backoffInitial,
					backoffMax:     targetOptions.backoffMax,
					cleanup: managerCleanupOptions{
						Timeout:  defaultManagerCleanupTimeout,
						Interval: defaultManagerCleanupInterval,
					},
					dispatch: retryDispatch,
				})
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "explicit Tau experiment config (default: tau.yaml; named targets use tau/TARGET.yaml)")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: config, target command, or profile/preset)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "TauWorkspace name to use for namespace, queue, priority, output, and workload identity defaults")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&dryRun, "dry-run", "", "client|server (default: actually apply)")
	cmd.Flags().StringVar(&keyVault, "key-vault", "", "Azure Key Vault name for runtime.env_kv secret references")
	cmd.Flags().StringVar(&kvTenantID, "tenant-id", "", "Azure AD tenant ID for Key Vault workload identity")
	cmd.Flags().StringVar(&kvClientID, "workload-identity-client-id", "", "Managed identity client ID for Key Vault workload identity")
	cmd.Flags().StringVar(&serviceAccountName, "service-account", "", "pod ServiceAccount for workload cloud identity (overrides the TauWorkspace default; authorization remains server-side)")
	cmd.PersistentFlags().StringVar(&projectName, "project", "", "Tau project name from the repository's tau.projects.yaml")

	cmd.AddCommand(newRunValidateCmd(), newRunSchemaCmd(), newRunExplainConfigCmd(), newRunGetCmd(), newRunListCmd(), newRunStatusCmd(), newRunLogsCmd(), newRunCancelCmd(), newRunResumeCmdWithConnectionFactory(connectionFactory), newRunHistoryCmd())
	return cmd
}

type runDispatchOptions struct {
	engine, file, script, mainScript, image, profileName, namespace, workspace, kubeContext, dryRun string
	dataPVC, resultPVC, output, queue, preset, team, lane, gpuClass, mode, topology, shape          string
	outputPublish                                                                                   string
	checkpointArtifact                                                                              string
	artifactPublicationID                                                                           string
	workspaceResultScope                                                                            string
	priorityTier, workloadPriorityClass, podPriorityClass                                           string
	cpuRequest, memoryRequest, cpuLimit, memoryLimit                                                string
	headCPURequest, headMemoryRequest, headCPULimit, headMemoryLimit                                string
	workerCPURequest, workerMemoryRequest, workerCPULimit, workerMemoryLimit                        string
	volumeSpecs, mountSpecs                                                                         []string
	imageAssets                                                                                     []runconfig.ImageAsset
	topologyPolicy, workloadKind, upstreamCheckpoint                                                string
	configDir                                                                                       string
	workingDir                                                                                      string
	workingDirExcludes                                                                              []string
	source                                                                                          *runconfig.Source
	nodeSelectors, runtimePip, env, envSecrets, envKV, extraScripts                                 []string
	metricsHistory                                                                                  []string
	metricsSessionID                                                                                string
	submissionID                                                                                    string
	clearNodeSelector, disableDefaultPriorities, nameFromConfig                                     bool
	metricsOffloadEnabled                                                                           bool
	workspaceExplicit, workspaceQueueResolved, kubeContextExplicit                                  bool
	// kubeContextFromFlag separates a typed --context from one inherited via
	// $TAU_CONTEXT. Both name a cluster, but only the flag is unambiguously
	// about this invocation, so only the flag settles a disagreement with a
	// checked-in workspace connection descriptor.
	kubeContextFromFlag                                                                                   bool
	azureWorkloadIdentity                                                                                 bool
	workers, gpusPerWorker, nConcurrent, cpuWorkers                                                       int
	jobGPUs                                                                                               *int
	gpusPerWorkerExplicit                                                                                 bool
	profiler, profileRank, profileWarmup, profileDuration, gpuResourceMode, migProfile, secretPayloadPath string
	keyVault, kvTenantID, kvClientID                                                                      string
	serviceAccountName                                                                                    string
	experiment                                                                                            runExperimentMetadata
	smokePairs                                                                                            int
	maxRetries                                                                                            int
	retryOn                                                                                               []string
	checkpointPath                                                                                        string
	backoffInitial, backoffMax                                                                            time.Duration
	launcher                                                                                              string
	processesPerNode                                                                                      int
	nodes                                                                                                 int
	executionDeadlineSeconds                                                                              int64
	ttlSecondsAfterFinished                                                                               int64
	tuneMetric, tuneMode, tuneParamSpace                                                                  string
	tuneNumSamples, tuneMaxConcurrentTrials                                                               int
	configs                                                                                               map[string]any
	allowNCCLOverride                                                                                     bool
}

func defaultRunDispatchOptions() runDispatchOptions {
	return runDispatchOptions{
		workers:       1,
		gpusPerWorker: 1,
		nConcurrent:   1,
	}
}

// resolvedRunTarget is the dispatch `tau run` resolved from a run config.
// Exactly one engine request is set.
type resolvedRunTarget struct {
	job             *runJobRequest
	ray             *runRayRequest
	managedWorkflow *runManagedWorkflowRequest
}

// dispatchOptions returns the resolved options for whichever engine was chosen.
func (t resolvedRunTarget) dispatchOptions() (runDispatchOptions, bool) {
	switch {
	case t.job != nil:
		return t.job.Options, true
	case t.ray != nil:
		return t.ray.Options, true
	case t.managedWorkflow != nil:
		return t.managedWorkflow.Options, true
	default:
		return runDispatchOptions{}, false
	}
}

func resolveRunTarget(o runDispatchOptions, name string) (resolvedRunTarget, error) {
	if err := ensureSubmissionID(&o); err != nil {
		return resolvedRunTarget{}, err
	}
	if o.file != "" {
		if o.outputPublish != "" {
			return resolvedRunTarget{}, fmt.Errorf("storage.publish is not supported for managed workflow configs")
		}
		if name != "" && !o.nameFromConfig {
			return resolvedRunTarget{}, fmt.Errorf("managed workflow configs take the run name from the config file; remove positional NAME")
		}
		managed, err := newRunManagedWorkflowRequest(o)
		if err != nil {
			return resolvedRunTarget{}, err
		}
		return resolvedRunTarget{managedWorkflow: &managed}, nil
	}

	if len(o.envKV) > 0 || o.keyVault != "" || o.kvTenantID != "" || o.kvClientID != "" {
		return resolvedRunTarget{}, fmt.Errorf("runtime.env_kv is only supported for workflow.file managed configs; direct job/ray configs must remove runtime.env_kv or use workflow.file")
	}
	if err := ensureArtifactPublicationID(&o); err != nil {
		return resolvedRunTarget{}, err
	}

	dispatch, explicitDispatch, err := explicitRunDispatch(o)
	if err != nil {
		return resolvedRunTarget{}, err
	}
	if err := validateDirectMetricsOffloadDispatch(o, dispatch, explicitDispatch); err != nil {
		return resolvedRunTarget{}, err
	}
	inferRay := !explicitDispatch && (o.workers > 1 || o.gpusPerWorker != 1 || o.gpusPerWorkerExplicit || len(o.runtimePip) > 0)
	if dispatch == "ray" || inferRay {
		if o.jobGPUs != nil {
			return resolvedRunTarget{}, fmt.Errorf("compute.gpus is only supported for engine: job; use compute.gpus_per_worker for Ray")
		}
	}
	if (dispatch == "ray" || inferRay) && o.nodes > 1 {
		return resolvedRunTarget{}, fmt.Errorf("execution.nodes > 1 requires engine: job; cannot combine with Ray dispatch")
	}
	if dispatch == "ray" || inferRay {
		ray, err := newRunRayRequest(o, name)
		if err != nil {
			return resolvedRunTarget{}, err
		}
		return resolvedRunTarget{ray: &ray}, nil
	}
	if err := validateExplicitJobRunConfig(o); err != nil {
		return resolvedRunTarget{}, err
	}
	if o.metricsOffloadEnabled && strings.TrimSpace(o.metricsSessionID) == "" {
		o.metricsSessionID, err = newMetricsSessionID()
		if err != nil {
			return resolvedRunTarget{}, err
		}
	}
	job, err := newRunJobRequest(o, name)
	if err != nil {
		return resolvedRunTarget{}, err
	}
	return resolvedRunTarget{job: &job}, nil
}

func newMetricsSessionID() (string, error) {
	var value [12]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate metrics session ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func ensureSubmissionID(options *runDispatchOptions) error {
	if options.dryRun != "" || strings.TrimSpace(options.submissionID) != "" {
		return nil
	}
	submissionID, err := newMetricsSessionID()
	if err != nil {
		return fmt.Errorf("generate submission ID: %w", err)
	}
	options.submissionID = submissionID
	return nil
}

func ensureArtifactPublicationID(options *runDispatchOptions) error {
	if options.outputPublish != artifactpublish.ModeStaged || strings.TrimSpace(options.artifactPublicationID) != "" {
		return nil
	}
	publicationID, err := newMetricsSessionID()
	if err != nil {
		return err
	}
	options.artifactPublicationID = publicationID
	return nil
}

func validateDirectMetricsOffloadDispatch(o runDispatchOptions, dispatch string, explicitDispatch bool) error {
	if !o.metricsOffloadEnabled {
		return nil
	}
	inferRay := !explicitDispatch && (o.workers > 1 || o.gpusPerWorker != 1 || o.gpusPerWorkerExplicit || len(o.runtimePip) > 0)
	if dispatch != "ray" && !inferRay && o.nodes > 1 {
		return fmt.Errorf("metrics.offload.enabled requires a single Job pod; execution.nodes must be unset or 1")
	}
	if strings.TrimSpace(o.script) == "" {
		return fmt.Errorf("metrics.offload.enabled requires run.entrypoint so Tau can record the workload exit status")
	}
	return nil
}

func explicitRunDispatch(o runDispatchOptions) (target string, explicit bool, err error) {
	engine := strings.ToLower(strings.TrimSpace(o.engine))
	workloadKind := strings.ToLower(strings.TrimSpace(o.workloadKind))

	engineTarget := ""
	switch engine {
	case "":
	case "job":
		engineTarget = "job"
	case "ray":
		engineTarget = "ray"
	default:
		return "", false, fmt.Errorf("engine must be one of: job, ray")
	}

	kindTarget := ""
	switch workloadKind {
	case "":
	case "job":
		kindTarget = "job"
	case "ray", "rayjob", "ray-train", "ray_train":
		kindTarget = "ray"
	default:
		return "", false, fmt.Errorf("workload_kind must be one of: job, rayjob, ray-train for non-managed run configs")
	}

	if engineTarget != "" && kindTarget != "" && engineTarget != kindTarget {
		return "", false, fmt.Errorf("engine=%s conflicts with workload_kind=%s", engine, workloadKind)
	}
	target = firstNonEmpty(engineTarget, kindTarget)
	return target, target != "", nil
}

func validateExplicitJobRunConfig(o runDispatchOptions) error {
	intent := dispatchIntent(o)
	if o.workers > 1 {
		return fmt.Errorf("%s cannot set compute.workers=%d; use engine: ray or remove compute.workers", intent, o.workers)
	}
	if o.gpusPerWorkerExplicit {
		return fmt.Errorf("%s cannot set compute.gpus_per_worker; use compute.gpus for a direct Job or switch to engine: ray", intent)
	}
	if len(o.runtimePip) > 0 {
		return fmt.Errorf("%s cannot set runtime.pip; use engine: ray or bake dependencies into the image", intent)
	}
	for field, value := range map[string]string{
		"compute.head_cpu_request":      o.headCPURequest,
		"compute.head_memory_request":   o.headMemoryRequest,
		"compute.head_cpu_limit":        o.headCPULimit,
		"compute.head_memory_limit":     o.headMemoryLimit,
		"compute.worker_cpu_request":    o.workerCPURequest,
		"compute.worker_memory_request": o.workerMemoryRequest,
		"compute.worker_cpu_limit":      o.workerCPULimit,
		"compute.worker_memory_limit":   o.workerMemoryLimit,
	} {
		if strings.TrimSpace(value) != "" {
			return fmt.Errorf("%s cannot set %s; jobs are single-pod, so use compute.cpu_request, compute.memory_request, compute.cpu_limit, and compute.memory_limit or switch to engine: ray", intent, field)
		}
	}
	if o.nodes > 1 && strings.ToLower(strings.TrimSpace(o.launcher)) != "torchrun" {
		return fmt.Errorf("%s with execution.nodes=%d requires execution.launcher=torchrun", intent, o.nodes)
	}
	if strings.TrimSpace(o.migProfile) != "" {
		return fmt.Errorf("%s cannot set compute.mig_profile; switch to engine: ray", intent)
	}
	gpuResourceMode, err := manifest.NormalizeGPUResourceMode(o.gpuResourceMode)
	if err != nil {
		return fmt.Errorf("%s has invalid compute.gpu_resource_mode: %w", intent, err)
	}
	if gpuResourceMode != manifest.GPUResourceModeDevicePlugin {
		return fmt.Errorf("%s cannot use compute.gpu_resource_mode=%q; direct Jobs support device-plugin GPUs", intent, o.gpuResourceMode)
	}
	if o.jobGPUs == nil && strings.TrimSpace(o.preset) == "" {
		shape := strings.TrimSpace(o.shape)
		if shape == "" {
			return fmt.Errorf("%s has ambiguous resources: set compute.gpus explicitly (0 for CPU, positive for GPU) or use a preset with a GPU shape; policy.profile does not define Job resources", intent)
		}
		if _, ok, err := runtopology.GPUCountFromShape(shape); err != nil {
			return fmt.Errorf("policy.shape: %w", err)
		} else if !ok {
			return fmt.Errorf("policy.shape %q does not prove a direct Job GPU count; set compute.gpus explicitly", shape)
		}
	}
	return nil
}

func dispatchIntent(o runDispatchOptions) string {
	engine := strings.TrimSpace(o.engine)
	workloadKind := strings.TrimSpace(o.workloadKind)
	switch {
	case engine != "" && workloadKind != "":
		return fmt.Sprintf("engine=%s/workload_kind=%s", engine, workloadKind)
	case engine != "":
		return "engine=" + engine
	case workloadKind != "":
		return "workload_kind=" + workloadKind
	default:
		return "implicit dispatch"
	}
}

func executeRunTarget(parent *cobra.Command, target resolvedRunTarget, captureCommand string, experiment runExperimentMetadata) error {
	ctx := withRunExperimentMetadata(parent.Context(), experiment)
	if target.job != nil {
		return executeRunJob(ctx, parent.OutOrStdout(), parent.ErrOrStderr(), target.job, captureCommand)
	}
	if target.ray != nil {
		return executeRunRay(ctx, parent.OutOrStdout(), parent.ErrOrStderr(), target.ray, captureCommand)
	}
	if target.managedWorkflow != nil {
		return executeRunManagedWorkflow(ctx, parent.OutOrStdout(), parent.ErrOrStderr(), target.managedWorkflow, captureCommand)
	}
	return fmt.Errorf("resolved run target has no executor")
}

func buildRunCaptureCommand(cmd *cobra.Command, name string, nameFromConfig bool, configPath string) string {
	parts := strings.Fields(cmd.CommandPath())
	if name != "" && !nameFromConfig {
		parts = append(parts, name)
	}
	if configPath != "" {
		parts = append(parts, "--config", configPath)
	}
	for _, flagName := range []string{"namespace", "context", "dry-run"} {
		flag := cmd.Flags().Lookup(flagName)
		if flag == nil || !flag.Changed {
			continue
		}
		parts = append(parts, "--"+flagName, flag.Value.String())
	}
	return experiment.RedactCommandArgs(parts)
}
