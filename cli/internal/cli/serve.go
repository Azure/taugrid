package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/serve"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/kube"
	runtopology "github.com/Azure/taugrid/core/topology"
)

// newServeCmd: north-star §1 / §5 — deploy a model endpoint as a
// KubeRay RayService.
//
// V0 implementation:
//   - deploy: real. Renders RayService CR with Kueue queue label, DRA
//     claim, profile scheduling, and Serve v2 config. Reuses submit's
//     --dry-run=client contract for offline inspection.
//   - status: real thin wrapper around `kubectl get rayservice`.
//   - delete: real thin wrapper around `kubectl delete rayservice`.
//   - scale:  stub. num_replicas live-edit on a RayService requires
//     careful CR patching (serveConfigV2 is a string, not structured)
//     and is deferred until there's a real traffic reason to scale.
//
// Closes anti-pattern #6: five core commands, all load-bearing surfaces
// now real (deploy is the load-bearing one; status/delete are hygiene).
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Deploy a model endpoint",
		Long: `Deploy a model as a RayService.

Examples:
  tau serve deploy my-7b --image vllm/vllm-openai:v0.6.3 --profile model-serve \
      --args "--model /data/checkpoints/my-7b --quantize awq"
  tau serve status my-7b
  tau serve delete my-7b`,
	}
	cmd.AddCommand(
		newServeDeployCmd(),
		newServeStatusCmd(),
		newServeScaleCmd(),
		newServeDeleteCmd(),
	)
	return cmd
}

func newServeDeployCmd() *cobra.Command {
	var (
		profileName   string
		image         string
		replicas      int
		importPath    string
		port          int
		rayVersion    string
		argsStr       string
		namespace     string
		dryRun        string
		kubeContext   string
		kind          string // rayservice | deployment
		ports         []int
		envKV         []string
		initSpecs     []string // --init NAME=IMAGE (repeatable)
		sideSpecs     []string // --sidecar NAME=IMAGE (repeatable)
		volSpecs      []string // --volume NAME=KIND[:src] (repeatable)
		mountSpecs    []string // --mount NAME:PATH[:ro] (repeatable)
		envSecretKV   []string // --env-secret KEY=SECRET:KEY (repeatable)
		runtimePip    []string // --runtime-pip package spec (repeatable)
		checkpoint    string
		checkpointPVC string
		fromFinetune  string
		checkpointRef string
		fromModel     string
		modelRef      string
		readinessPath string
		startupPath   string
		livenessPath  string
		startupFails  int
		servicePort   int
		serviceTarget int
		gpus          int
		minReplicas   int
		maxReplicas   int
		targetQPS     int
		scaleDownSec  int
	)
	cmd := &cobra.Command{
		Use:   "deploy [name]",
		Short: "Deploy or update a serve endpoint",
		Long: `Deploy a model endpoint. Two kinds supported:

  --kind=rayservice (default): KubeRay RayService. For Ray Serve apps.
  --kind=deployment:           plain k8s Deployment with Kueue
                               pod-integration. For non-Ray serving
                               (vLLM raw, TGI, triton, custom HTTP servers,
                               multi-container shapes like fish-speech-tts).

Examples:
  tau serve deploy my-7b --profile model-serve \
      --image vllm/vllm-openai:v0.6.3 \
      --args "--model /ckpt --quantize awq"

  tau serve deploy sample-compiled-demo --kind=rayservice --profile ai-serve-gpu-l \
      --image sampleprojectcr.azurecr.io/sample-demo:v7 \
      --import-path experiments.sample_serving.app:app \
      --checkpoint demo-sample-1738/last.safetensors \
      --env SAMPLE_INFER_BACKEND=compile \
      --env SAMPLE_COMPILE_MODE=reduce-overhead

  tau serve deploy gura-llm --kind=deployment --profile sample-project-llm-a100 \
      --image sampleprojectcr.azurecr.io/llm:v1 --dry-run=client

  tau serve deploy tts --kind=deployment --profile model-serve \
      --image my-reg/tts-api:v1 --deployment-port 8080 \
      --readiness-path /health --service-port 8080 \
      --env MODEL_DIR=/models --replicas 1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if profileName == "" {
				return fmt.Errorf("--profile is required (e.g. model-serve)")
			}
			if dryRun != "" && dryRun != "client" && dryRun != "server" {
				return fmt.Errorf("--dry-run must be one of: client, server")
			}
			if kind == "" {
				kind = "rayservice"
			}
			if kind != "rayservice" && kind != "deployment" {
				return fmt.Errorf("--kind must be one of: rayservice, deployment")
			}
			if gpus < 0 {
				return fmt.Errorf("--gpus must be >= 0")
			}
			if maxReplicas < 0 {
				return fmt.Errorf("--max-replicas must be >= 0")
			}
			if maxReplicas > 0 && cmd.Flags().Changed("replicas") {
				return fmt.Errorf("--max-replicas and --replicas are mutually exclusive; use --min-replicas to set a floor")
			}
			if maxReplicas == 0 {
				for _, f := range []string{"min-replicas", "target-qps", "scale-down-delay"} {
					if cmd.Flags().Changed(f) {
						return fmt.Errorf("--%s requires --max-replicas to enable autoscaling", f)
					}
				}
			}

			var autoscaling *serve.AutoscalingOptions
			if maxReplicas > 0 {
				autoscaling = &serve.AutoscalingOptions{
					MinReplicas:    int32(minReplicas),
					MaxReplicas:    int32(maxReplicas),
					TargetQPS:      targetQPS,
					ScaleDownDelay: scaleDownSec,
				}
			}

			var runner *kube.Runner
			if dryRun != "client" {
				runner = kube.New(kubeContext)
			}
			target, warning, err := resolveServeTarget(
				cmd.Context(), runner, namespace, dryRun, serveWorkloadResource(kind),
			)
			if err != nil {
				return err
			}
			if warning != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
			}
			ns := target.Namespace

			p := resourceProfileForRender(profileName, nil, runtopology.Options{Lane: "serve", QueueName: target.Queue}, gpus)
			env, envErr := parseEnvKV(envKV)
			if envErr != nil {
				return envErr
			}
			envSecrets, envSecretErr := parseEnvSecretKV(envSecretKV)
			if envSecretErr != nil {
				return envSecretErr
			}
			vols, verr := parseVolumeSpecs(volSpecs)
			if verr != nil {
				return verr
			}
			mounts, merr := parseMountSpecs(mountSpecs)
			if merr != nil {
				return merr
			}
			if checkpoint != "" && (fromFinetune != "" || checkpointRef != "" || fromModel != "" || modelRef != "") {
				return fmt.Errorf("--checkpoint conflicts with --from-finetune/--checkpoint-ref/--from-model/--model-ref")
			}
			if (fromFinetune != "" || checkpointRef != "") && (fromModel != "" || modelRef != "") {
				return fmt.Errorf("finetune checkpoint refs conflict with model refs")
			}
			ref, hasRef, refErr := parseServeCheckpointRef(fromFinetune, checkpointRef)
			if refErr != nil {
				return refErr
			}
			modelRefValue, hasModelRef, modelRefErr := parseServeModelRef(fromModel, modelRef)
			if modelRefErr != nil {
				return modelRefErr
			}
			if hasRef {
				if dryRun == "client" {
					return fmt.Errorf("--from-finetune/--checkpoint-ref require reading artifacts.json from Kubernetes; use --dry-run=server, omit --dry-run, or pass a resolved --checkpoint path")
				}
				raw, _, err := fetchManagedWorkflowArtifacts(cmd.Context(), kubeContext, ns, ref.Run, checkpointPVC)
				if err != nil {
					return err
				}
				artifact, err := selectManagedWorkflowArtifact(raw, ref.Artifact)
				if err != nil {
					return err
				}
				if err := validatePVCArtifactVisible(cmd.Context(), kubeContext, ns, ref.Run, checkpointPVC, artifact.DurablePath, artifact.FileCount); err != nil {
					return err
				}
				checkpoint = artifact.DurablePath
			}
			if hasModelRef {
				if dryRun == "client" {
					return fmt.Errorf("--from-model/--model-ref require reading model registry metadata from Kubernetes; use --dry-run=server, omit --dry-run, or pass a resolved --checkpoint path")
				}
				record, err := resolveModelRef(cmd.Context(), kubeContext, ns, checkpointPVC, modelRefValue)
				if err != nil {
					return err
				}
				artifact, err := selectModelArtifact(record, "checkpoint")
				if err != nil {
					return err
				}
				if err := validatePVCArtifactVisible(cmd.Context(), kubeContext, ns, record.Run, checkpointPVC, artifact.DurablePath, artifact.FileCount); err != nil {
					return err
				}
				checkpoint = artifact.DurablePath
			}
			env, vols, mounts, err = applyCheckpointMount(env, vols, mounts, checkpoint, checkpointPVC)
			if err != nil {
				return err
			}
			if mverr := validateMountsAgainstVolumes(mounts, vols); mverr != nil {
				return mverr
			}

			var manifest []byte
			switch kind {
			case "rayservice":
				manifest, err = serve.Render(p, serve.Options{
					Name:         name,
					Namespace:    ns,
					Image:        image,
					Replicas:     replicas,
					ReplicasSet:  cmd.Flags().Changed("replicas"),
					ImportPath:   importPath,
					ServePort:    port,
					RayVersion:   rayVersion,
					Args:         splitShellish(argsStr),
					Env:          env,
					EnvVars:      envSecrets,
					RuntimePip:   runtimePip,
					Volumes:      vols,
					VolumeMounts: mounts,
					Autoscaling:  autoscaling,
				})
			case "deployment":
				inits, ierr := parseContainerSpecs(initSpecs, "--init")
				if ierr != nil {
					return ierr
				}
				sides, serr := parseContainerSpecs(sideSpecs, "--sidecar")
				if serr != nil {
					return serr
				}
				manifest, err = serve.RenderDeployment(p, serve.DeploymentOptions{
					Name:              name,
					Namespace:         ns,
					Image:             image,
					Replicas:          deployReplicas(replicas, autoscaling),
					Args:              splitShellish(argsStr),
					Env:               env,
					EnvVars:           envSecrets,
					RuntimePip:        runtimePip,
					Ports:             ports,
					InitContainers:    inits,
					Sidecars:          sides,
					Volumes:           vols,
					VolumeMounts:      mounts,
					ReadinessProbe:    serve.HTTPProbe{Path: readinessPath},
					StartupProbe:      serve.HTTPProbe{Path: startupPath, FailureThreshold: startupFails},
					LivenessProbe:     serve.HTTPProbe{Path: livenessPath},
					ServicePort:       servicePort,
					ServiceTargetPort: serviceTarget,
					Autoscaling:       autoscaling,
				})
			}
			if err != nil {
				return err
			}

			if dryRun == "client" {
				_, err := cmd.OutOrStdout().Write(manifest)
				return err
			}

			extra := []string{"apply", "-n", ns, "-f", "-"}
			if dryRun != "" {
				extra = append(extra, "--dry-run="+dryRun)
			}
			out, err := runner.Raw(cmd.Context(), extra, manifest)
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			if err != nil {
				return err
			}
			if dryRun == "" {
				fmt.Fprintf(cmd.OutOrStdout(),
					"deployed service %s (kind=%s, profile=%s, image=%s, ns=%s)\ntrack: tau serve status %s -n %s --kind %s\n",
					name, kind, profileName, image, ns, name, ns, kind)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "", "profile name (required, e.g. model-serve)")
	cmd.Flags().StringVar(&kind, "kind", "rayservice", "serving kind: rayservice|deployment")
	cmd.Flags().StringVar(&image, "image", "", "container image (overrides profile image)")
	cmd.Flags().IntVar(&replicas, "replicas", 1, "override Ray Serve deployment replicas")
	cmd.Flags().StringVar(&importPath, "import-path", "", "Ray Serve import path (rayservice only; default: serve:app)")
	cmd.Flags().IntVar(&port, "port", 0, "serve HTTP port (rayservice: single port; default 8000)")
	cmd.Flags().IntSliceVar(&ports, "deployment-port", nil, "container port(s) for --kind=deployment (repeatable)")
	cmd.Flags().StringArrayVar(&envKV, "env", nil, "env var KEY=VAL for the serve container and Ray Serve runtime_env (repeatable)")
	cmd.Flags().StringArrayVar(&envSecretKV, "env-secret", nil, "env var KEY=SECRET:KEY from a Kubernetes Secret; rendered as valueFrom.secretKeyRef (repeatable)")
	cmd.Flags().StringArrayVar(&runtimePip, "runtime-pip", nil, "Python package to install through Ray Serve runtime_env.pip (--kind=rayservice only; repeatable)")
	cmd.Flags().StringArrayVar(&initSpecs, "init", nil, "init container NAME=IMAGE for --kind=deployment (repeatable)")
	cmd.Flags().StringArrayVar(&sideSpecs, "sidecar", nil, "sidecar container NAME=IMAGE for --kind=deployment (repeatable)")
	cmd.Flags().StringArrayVar(&volSpecs, "volume", nil, "volume NAME=KIND[:src] for the serve pod (repeatable). KIND ∈ pvc|emptyDir|configMap|secret. e.g. --volume data=pvc:blob-training, --volume shm=emptyDir, --volume creds=secret:hf-token")
	cmd.Flags().StringArrayVar(&mountSpecs, "mount", nil, "mount NAME:PATH[:ro] on the serve container (repeatable). NAME must match a --volume.")
	cmd.Flags().StringVar(&checkpoint, "checkpoint", "", "checkpoint path to serve; relative paths resolve under /data/checkpoints and set TAU_MODEL_PATH")
	cmd.Flags().StringVar(&checkpointPVC, "checkpoint-pvc", "blob-training", "PVC mounted at /data when --checkpoint is set")
	cmd.Flags().StringVar(&fromFinetune, "from-finetune", "", "completed finetune run whose ready checkpoint artifact should be served")
	cmd.Flags().StringVar(&checkpointRef, "checkpoint-ref", "", "checkpoint reference to serve, e.g. finetune/RUN[:artifact]")
	cmd.Flags().StringVar(&fromModel, "from-model", "", "model registry ref to serve, e.g. MODEL:alias or MODEL@run")
	cmd.Flags().StringVar(&modelRef, "model-ref", "", "alias for --from-model")
	cmd.Flags().StringVar(&readinessPath, "readiness-path", "", "HTTP path for the main container readiness probe (--kind=deployment)")
	cmd.Flags().StringVar(&startupPath, "startup-path", "", "HTTP path for the main container startup probe (--kind=deployment)")
	cmd.Flags().StringVar(&livenessPath, "liveness-path", "", "HTTP path for the main container liveness probe (--kind=deployment)")
	cmd.Flags().IntVar(&startupFails, "startup-failure-threshold", 0, "failureThreshold for --startup-path (default: Kubernetes default)")
	cmd.Flags().IntVar(&servicePort, "service-port", 0, "ClusterIP Service port to render for --kind=deployment (0 disables Service)")
	cmd.Flags().IntVar(&serviceTarget, "service-target-port", 0, "ClusterIP Service targetPort for --kind=deployment (default: first --deployment-port or --service-port)")
	cmd.Flags().StringVar(&rayVersion, "ray-version", "", "Ray version (default: 2.40.0)")
	cmd.Flags().StringVar(&argsStr, "args", "", "extra container args (space-separated; e.g. \"--model /ckpt --quantize awq\")")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&dryRun, "dry-run", "", "client|server (default: actually apply)")
	cmd.Flags().IntVar(&gpus, "gpus", 1, "GPU count per replica (0 for CPU-only serving)")
	cmd.Flags().IntVar(&minReplicas, "min-replicas", 1, "minimum replica count for autoscaling (requires --max-replicas)")
	cmd.Flags().IntVar(&maxReplicas, "max-replicas", 0, "maximum replica count; >0 enables autoscaling (mutually exclusive with --replicas)")
	cmd.Flags().IntVar(&targetQPS, "target-qps", 0, "target QPS per replica for autoscaling (0 = CPU-utilization-only for deployment, default 5 for rayservice)")
	cmd.Flags().IntVar(&scaleDownSec, "scale-down-delay", 300, "scale-down stabilization window in seconds")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

type serveCheckpointRef struct {
	Run      string
	Artifact string
}

func parseServeCheckpointRef(fromFinetune, checkpointRef string) (serveCheckpointRef, bool, error) {
	fromFinetune = strings.TrimSpace(fromFinetune)
	checkpointRef = strings.TrimSpace(checkpointRef)
	if fromFinetune != "" && checkpointRef != "" {
		return serveCheckpointRef{}, false, fmt.Errorf("--from-finetune conflicts with --checkpoint-ref")
	}
	if fromFinetune != "" {
		return serveCheckpointRef{Run: fromFinetune, Artifact: "checkpoint"}, true, nil
	}
	if checkpointRef == "" {
		return serveCheckpointRef{}, false, nil
	}
	if !strings.HasPrefix(checkpointRef, "finetune/") {
		return serveCheckpointRef{}, false, fmt.Errorf("--checkpoint-ref: only finetune/RUN[:artifact] is supported, got %q", checkpointRef)
	}
	rest := strings.TrimPrefix(checkpointRef, "finetune/")
	parts := strings.SplitN(rest, ":", 2)
	run := strings.TrimSpace(parts[0])
	if run == "" {
		return serveCheckpointRef{}, false, fmt.Errorf("--checkpoint-ref: finetune run name is required")
	}
	artifact := "checkpoint"
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		artifact = strings.TrimSpace(parts[1])
	}
	return serveCheckpointRef{Run: run, Artifact: artifact}, true, nil
}

func parseServeModelRef(fromModel, modelRef string) (string, bool, error) {
	fromModel = strings.TrimSpace(fromModel)
	modelRef = strings.TrimSpace(modelRef)
	if fromModel != "" && modelRef != "" {
		return "", false, fmt.Errorf("--from-model conflicts with --model-ref")
	}
	ref := fromModel
	if ref == "" {
		ref = modelRef
	}
	if ref == "" {
		return "", false, nil
	}
	if _, err := parseModelRef(ref); err != nil {
		return "", false, err
	}
	return ref, true, nil
}

func selectManagedWorkflowArtifact(raw []byte, artifactName string) (managedWorkflowArtifact, error) {
	var idx managedWorkflowArtifactIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return managedWorkflowArtifact{}, fmt.Errorf("parse artifacts.json: %w", err)
	}
	if artifactName == "" {
		artifactName = "checkpoint"
	}
	var names []string
	for _, artifact := range idx.Artifacts {
		names = append(names, artifact.Name)
		if artifact.Name != artifactName && artifact.ManifestPath != artifactName {
			continue
		}
		if artifact.Status != "ready" {
			return managedWorkflowArtifact{}, fmt.Errorf("artifact %q for run %q is not ready (status=%q)", artifactName, idx.Run, artifact.Status)
		}
		if artifact.DurablePath == "" {
			return managedWorkflowArtifact{}, fmt.Errorf("artifact %q for run %q has no durable_path", artifactName, idx.Run)
		}
		return artifact, nil
	}
	return managedWorkflowArtifact{}, fmt.Errorf("artifact %q not found in run %q (available: %s)", artifactName, idx.Run, strings.Join(names, ", "))
}

func parseEnvKV(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		i := indexEq(kv)
		if i <= 0 {
			return nil, fmt.Errorf("--env: expected KEY=VAL, got %q", kv)
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out, nil
}

func parseEnvSecretKV(kvs []string) ([]envspec.Var, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make([]envspec.Var, 0, len(kvs))
	for _, kv := range kvs {
		i := indexEq(kv)
		if i <= 0 || i >= len(kv)-1 {
			return nil, fmt.Errorf("--env-secret: expected KEY=SECRET:KEY, got %q", kv)
		}
		name := kv[:i]
		secretSpec := kv[i+1:]
		ref, err := envspec.ParseSecretKeyRefSpec(secretSpec)
		if err != nil {
			return nil, fmt.Errorf("--env-secret %q: %w", kv, err)
		}
		out = append(out, envspec.Secret(name, ref.Name, ref.Key))
	}
	if err := envspec.Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

func applyCheckpointMount(env map[string]string, volumes []serve.Volume, mounts []serve.VolumeMount, checkpointPath, checkpointPVC string) (map[string]string, []serve.Volume, []serve.VolumeMount, error) {
	if checkpointPath == "" {
		return env, volumes, mounts, nil
	}
	if checkpointPVC == "" {
		return nil, nil, nil, fmt.Errorf("--checkpoint-pvc is required when --checkpoint is set")
	}
	normalizedCheckpointPath := storage.NormalizeCheckpointPath(checkpointPath)
	if existing, ok := env["TAU_MODEL_PATH"]; ok && existing != "" && existing != normalizedCheckpointPath {
		return nil, nil, nil, fmt.Errorf("--checkpoint conflicts with --env TAU_MODEL_PATH=%s", existing)
	}
	for _, v := range volumes {
		if v.Name == "tau-data" {
			return nil, nil, nil, fmt.Errorf("--checkpoint reserves volume name %q", v.Name)
		}
	}
	for _, m := range mounts {
		if m.Name == "tau-data" || m.MountPath == storage.DurableRoot {
			return nil, nil, nil, fmt.Errorf("--checkpoint reserves mount name %q and path %s", m.Name, storage.DurableRoot)
		}
	}
	if env == nil {
		env = map[string]string{}
	}
	env["TAU_MODEL_PATH"] = normalizedCheckpointPath
	env["TAU_DATA_DIR"] = storage.DurableRoot
	env["TAU_DURABLE_CHECKPOINTS_DIR"] = storage.DurableCheckpointsDir
	volumes = append(volumes, serve.Volume{Name: "tau-data", PVC: checkpointPVC})
	mounts = append(mounts, serve.VolumeMount{Name: "tau-data", MountPath: storage.DurableRoot})
	return env, volumes, mounts, nil
}

func indexEq(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return i
		}
	}
	return -1
}

// parseContainerSpecs parses NAME=IMAGE repeated flags into serve.Container.
// V0 only sets name+image; users wanting command/args should bake them
// into the image (ENTRYPOINT/CMD).
func parseContainerSpecs(specs []string, flag string) ([]serve.Container, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]serve.Container, 0, len(specs))
	for _, s := range specs {
		i := indexEq(s)
		if i <= 0 || i >= len(s)-1 {
			return nil, fmt.Errorf("%s: expected NAME=IMAGE, got %q", flag, s)
		}
		out = append(out, serve.Container{Name: s[:i], Image: s[i+1:]})
	}
	return out, nil
}

// parseVolumeSpecs parses "NAME=KIND[:src]" strings into serve.Volume.
// Accepted forms:
//
//	NAME=emptyDir
//	NAME=pvc:<claimName>
//	NAME=configMap:<cmName>
//	NAME=secret:<secretName>
func parseVolumeSpecs(specs []string) ([]serve.Volume, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]serve.Volume, 0, len(specs))
	for _, s := range specs {
		i := indexEq(s)
		if i <= 0 || i >= len(s)-1 {
			return nil, fmt.Errorf("--volume: expected NAME=KIND[:src], got %q", s)
		}
		name := s[:i]
		body := s[i+1:]
		var v serve.Volume
		v.Name = name
		kind, src, hasSrc := strings.Cut(body, ":")
		switch strings.ToLower(kind) {
		case "emptydir":
			if hasSrc {
				return nil, fmt.Errorf("--volume %q: emptyDir takes no source", s)
			}
			v.EmptyDir = true
		case "pvc", "persistentvolumeclaim":
			if !hasSrc || src == "" {
				return nil, fmt.Errorf("--volume %q: pvc requires :<claimName>", s)
			}
			v.PVC = src
		case "configmap", "cm":
			if !hasSrc || src == "" {
				return nil, fmt.Errorf("--volume %q: configMap requires :<name>", s)
			}
			v.ConfigMap = src
		case "secret":
			if !hasSrc || src == "" {
				return nil, fmt.Errorf("--volume %q: secret requires :<name>", s)
			}
			v.Secret = src
		default:
			return nil, fmt.Errorf("--volume %q: unknown kind %q (want pvc|emptyDir|configMap|secret)", s, kind)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseMountSpecs parses "NAME:PATH[:ro]" strings into serve.VolumeMount.
func parseMountSpecs(specs []string) ([]serve.VolumeMount, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]serve.VolumeMount, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--mount: expected NAME:PATH[:ro], got %q", s)
		}
		vm := serve.VolumeMount{Name: parts[0], MountPath: parts[1]}
		if len(parts) == 3 {
			switch strings.ToLower(parts[2]) {
			case "ro", "readonly":
				vm.ReadOnly = true
			case "rw":
				// default; explicit OK
			default:
				return nil, fmt.Errorf("--mount %q: 3rd field must be ro|rw", s)
			}
		}
		if len(parts) > 3 {
			return nil, fmt.Errorf("--mount %q: too many colon-separated fields", s)
		}
		out = append(out, vm)
	}
	return out, nil
}

// validateMountsAgainstVolumes checks every mount references a volume
// declared via --volume. Fail-fast at CLI layer so users see a clear
// error instead of a kubectl rejection later.
func validateMountsAgainstVolumes(mounts []serve.VolumeMount, vols []serve.Volume) error {
	names := make(map[string]bool, len(vols))
	for _, v := range vols {
		names[v.Name] = true
	}
	for _, m := range mounts {
		if !names[m.Name] {
			return fmt.Errorf("--mount %q: no matching --volume with name %q", m.Name, m.Name)
		}
	}
	return nil
}

func newServeStatusCmd() *cobra.Command {
	var namespace, kubeContext, kind string
	cmd := &cobra.Command{
		Use:   "status [name]",
		Short: "Show serve endpoint status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			if kind == "" {
				kind = "rayservice"
			}
			r := kube.New(kubeContext)
			var extra []string
			switch kind {
			case "rayservice":
				extra = []string{"get", "rayservice", args[0], "-n", ns,
					"-o", "custom-columns=NAME:.metadata.name,STATUS:.status.serviceStatus,READY:.status.numServeEndpoints,AGE:.metadata.creationTimestamp"}
			case "deployment":
				extra = []string{"get", "deployment", args[0], "-n", ns,
					"-o", "custom-columns=NAME:.metadata.name,READY:.status.readyReplicas,DESIRED:.spec.replicas,AVAILABLE:.status.availableReplicas,AGE:.metadata.creationTimestamp"}
			default:
				return fmt.Errorf("--kind must be one of: rayservice, deployment")
			}
			out, err := r.Raw(cmd.Context(), extra, nil)
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "rayservice", "serving kind: rayservice|deployment")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newServeScaleCmd() *cobra.Command {
	var (
		kind        string
		replicas    int
		namespace   string
		kubeContext string
	)
	cmd := &cobra.Command{
		Use:   "scale [name]",
		Short: "Scale serve endpoint replicas",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "" {
				kind = "rayservice"
			}
			if replicas < 0 {
				return fmt.Errorf("--replicas must be >= 0")
			}
			ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			switch kind {
			case "deployment":
				r := kube.New(kubeContext)
				out, err := r.Raw(cmd.Context(),
					[]string{"scale", "deployment", args[0], "-n", ns,
						fmt.Sprintf("--replicas=%d", replicas)}, nil)
				if out != "" {
					fmt.Fprint(cmd.OutOrStdout(), out)
				}
				return err
			case "rayservice":
				return fmt.Errorf("serve scale --kind=rayservice: not yet implemented " +
					"(serveConfigV2 is a string blob; redeploy with --replicas for now)")
			default:
				return fmt.Errorf("--kind must be one of: rayservice, deployment")
			}
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "rayservice", "serving kind: rayservice|deployment")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "target replica count")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newServeDeleteCmd() *cobra.Command {
	var namespace, kubeContext, kind string
	cmd := &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete a serve endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "" {
				kind = "rayservice"
			}
			if kind != "rayservice" && kind != "deployment" {
				return fmt.Errorf("--kind must be one of: rayservice, deployment")
			}
			ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			r := kube.New(kubeContext)
			out, err := r.Raw(cmd.Context(), []string{
				"delete", kind, args[0], "-n", ns, "--ignore-not-found",
			}, nil)
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "rayservice", "serving kind: rayservice|deployment")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func deployReplicas(replicas int, autoscaling *serve.AutoscalingOptions) int32 {
	if autoscaling != nil {
		return 0
	}
	return int32(replicas)
}

// splitShellish splits a simple "--a b --c d" arg string on whitespace.
// No quote handling in V0 — users with exotic args use a custom image
// or a config file. Keeping this naive is deliberate; Python-style
// shlex parsing in the CLI is a footgun we don't want.
func splitShellish(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
