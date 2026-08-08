// Package jobrender renders a Tau Profile + typed options into a Kubernetes
// batch/v1 Job or multi-node Indexed Job manifest.
//
// The renderer is a pure function: (Profile, Options) → []byte YAML, no
// kubectl, no cluster. This keeps unit tests fast and lets the same code
// power `--dry-run=client` (print to stdout) and the apply path.
//
// Design notes:
//
//   - The Job is created `suspend: true` and labelled with
//     `kueue.x-k8s.io/queue-name` so Kueue admits it. This matches the
//     pattern used by earlier shell-based submission workflows.
//   - DRA ResourceClaim wiring: if the profile declares
//     spec.resources.dra.claimTemplate, we attach a single claim named
//     "gpu" and reference it from the container resources.claims slice.
//   - Script handling: if a script path is supplied, it's base64-encoded
//     into TAU_SCRIPT_B64 and the entrypoint decodes-and-runs it. No
//     ConfigMap or PVC mount is required.
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

	// HeadlessSuffix is the suffix appended to the Job name to form the
	// headless Service name for multi-node torchrun.
	HeadlessSuffix = "-headless"
)

// Options collects everything the renderer needs that isn't in the Profile.
type Options struct {
	// Name is the Job name. Required. Caller is responsible for collisions.
	Name string

	// Namespace is the target namespace. Required.
	Namespace string

	// Image overrides the container image. If empty, the profile's
	// spec.runtime.image is used; if that's empty too, the renderer
	// returns an error (we refuse to ship a "default image" footgun).
	Image string

	// ServiceAccountName selects the pod identity. Empty uses the namespace
	// default ServiceAccount.
	ServiceAccountName string
	// AzureWorkloadIdentity stamps the opt-in label required by the AKS
	// workload identity mutating webhook.
	AzureWorkloadIdentity bool

	// CPURequest, MemoryRequest, CPULimit, and MemoryLimit override the
	// corresponding resources on the main Job container.
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string

	// ScriptPath, if non-empty, is read from disk and base64-encoded into
	// TAU_SCRIPT_B64. The container entrypoint decodes and executes it.
	ScriptPath string

	// ScriptArgs, if non-empty, are forwarded to ScriptPath as positional
	// arguments ($1, $2, ... inside the script). Ignored when Command is set
	// (the caller already controls argv) or when ScriptPath is empty.
	ScriptArgs []string

	// CheckpointArtifact is storage.checkpoint: the file or directory,
	// relative to the run checkpoint dir, that this run produces as its
	// servable model. When set, the rendered command writes an artifact
	// index after a successful run. Empty means no index step is rendered.
	CheckpointArtifact string

	// Command, if non-empty, replaces the entrypoint entirely. Overrides
	// ScriptPath. Useful for tests and "run my image as-is" Jobs.
	Command []string

	// Launcher selects the process execution model. "python" forces Python
	// execution, "torchrun" wraps the script with torchrun, and empty detects
	// Python from a .py suffix or shebang while honoring other shebangs.
	Launcher string

	// ProcessesPerNode sets --nproc_per_node for torchrun. Validated against
	// the profile's GPU count when > 0. Ignored when Launcher is "python".
	ProcessesPerNode int

	// Nodes sets the number of pods for multi-node torchrun. When > 1, the
	// renderer emits a Kubernetes Indexed Job (completionMode: Indexed,
	// completions=N, parallelism=N) plus a headless Service for DNS-based
	// c10d rendezvous. JOB_COMPLETION_INDEX becomes --node_rank. Requires
	// Launcher == "torchrun".
	Nodes int

	// ExtraFlags are arbitrary flags appended to the rendered command.
	// Each key becomes --key=value (or bare --key when value is empty).
	// Keys are sorted alphabetically for deterministic output. Values
	// are shell-quoted. Validation is done upstream in runconfig.
	ExtraFlags map[string]string

	// Retry controls Job backoffLimit.
	Retry int

	// TerminationGracePeriodSeconds overrides the default pod grace
	// period. Default is 60s (north-star §5.6: give training jobs time
	// to flush checkpoints on SIGTERM; k8s default of 30s is too short
	// for multi-GB checkpoints to blob storage). Profile
	// spec.policy.terminationGracePeriodSeconds sets a per-profile
	// default; this option wins if > 0.
	TerminationGracePeriodSeconds int64

	// ActiveDeadlineSeconds, if > 0, bounds the job's total wall-clock
	// runtime. The k8s controller terminates the pod when exceeded. Used
	// by long-running `tau run` Jobs that want a hard cap regardless of
	// profile policy. Overrides any value
	// inherited from profile.spec.policy.activeDeadlineSeconds.
	ActiveDeadlineSeconds int64

	// TTLSecondsAfterFinished controls Kubernetes cleanup after completion.
	// Zero uses the renderer's normal eight-hour retention.
	TTLSecondsAfterFinished int64

	// PVCMount, if non-empty, mounts the named PVC at /data inside the main
	// container and adds an ephemeral /mnt hot-work volume. The PVC must
	// already exist in Namespace.
	PVCMount string

	// Volumes and VolumeMounts are run-time storage overrides. They are
	// additive with profile spec.resources.persistence and are intentionally a
	// small PVC-only subset for researcher data volumes.
	Volumes      []Volume
	VolumeMounts []VolumeMount
	ImageAssets  []runconfig.ImageAsset

	// Labels and Annotations carry workload-specific metadata that other
	// Tau commands can read back. V0 uses this for eval result paths and
	// run profiling; scheduling-critical labels are still owned by Render.
	Labels      map[string]string
	Annotations map[string]string

	// Env is a CLI-injected env-var map merged into the rendered container
	// env after profile-declared env. CLI keys win on conflict but Tau
	// reserves the TAU_ prefix for storage and profile-mode contracts;
	// Render rejects user-supplied keys with that prefix.
	Env map[string]string
	// EnvSecrets are secret-backed env vars rendered as valueFrom.secretKeyRef.
	// RedactSecrets replaces the referenced Secret name/key in client dry-run
	// output while preserving the dependency shape.
	EnvSecrets    []envspec.Var
	RedactSecrets bool

	// OutputDir, if set, advertises the durable result path on the pod via
	// the TAU_OUTPUT_DIR env var. Setting this does not otherwise affect
	// the rendered manifest; the result-path/result-pvc annotations live
	// in Annotations and are the contract `tau run get` reads.
	OutputDir string

	// Profile, if set, wraps the rendered entrypoint with a profiling harness.
	// The wrapper checks the Nsight tool is present at runtime, profiles only
	// the selected rank, and writes rank-scoped raw artifacts plus
	// metadata/summary files under OutputDir/profile (or /data/<Name>/profile
	// for direct Render callers that mounted /data).
	Profile ProfileOptions

	// MetricsOffload, when enabled by a pinned Image, tails append-only JSONL
	// histories into durable Stellar state and emits the terminal run marker.
	MetricsOffload metricsoffload.Runtime
	// ArtifactPublish optionally wraps the workload so closed artifacts staged
	// on local /mnt are copied and renamed into durable OutputDir after success.
	ArtifactPublish artifactpublish.Runtime

	// NodeSelector is a run-time placement override merged after profile and
	// topology selectors. ClearNodeSelector drops profile selectors before the
	// protected topology contract and NodeSelector are applied.
	NodeSelector      map[string]string
	ClearNodeSelector bool

	// Topology contract overrides. Profiles can declare spec.topology, while
	// CLI callers can override the researcher-facing intent here. Render turns
	// this into protected Kueue queue/topology metadata and rejects unsafe
	// combinations (for example elastic jobs without checkpoints, or eval on
	// scarce H200 NVLink capacity).
	Team            string
	Lane            string
	Mode            string
	Topology        string
	Shape           string
	GPUClass        string
	CheckpointEvery string
	QueueName       string
	// GPUResourceName constrains live queue discovery and validation to the
	// resource used by this workload (for example nvidia.com/gpu or
	// gpu.nvidia.com). It does not alter rendered pod resources.
	GPUResourceName                 string
	PriorityTier                    string
	RequiredTopology                string
	WorkloadPriorityClassName       string
	PodPriorityClassName            string
	DisableKueueTopologyAnnotations bool
	// DisableDefaultPriorities omits Tau-managed default priority classes.
	// It also suppresses built-in profile scheduling.priorityClassName values
	// that reference the same TauGrid defaults, so bring-your-own Kueue installs
	// without those classes can still enqueue Jobs.
	DisableDefaultPriorities bool
}

// Volume is a PVC-backed volume attached to a rendered Job.
type Volume struct {
	Name string
	PVC  string
}

// VolumeMount is a mount for a rendered Job volume.
type VolumeMount struct {
	Name      string
	MountPath string
	ReadOnly  bool
}

// ProfileOptions configures the opt-in profiler wrapper for Job workloads.
// Mode is "nsys" or "ncu"; empty disables profiling. Rank defaults to "0"
// (profile one representative rank) and may be set to "all". Warmup and
// Duration are currently applied to Nsight Systems only.
type ProfileOptions struct {
	Mode     string
	Rank     string
	Warmup   time.Duration
	Duration time.Duration
}

// Render turns a resolved Profile + Options into a single-document YAML
// manifest ready to feed to `kubectl apply -f -`.
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
	topologyPlan, err := topology.Build(p, topology.Options{
		Team:                            o.Team,
		Lane:                            o.Lane,
		Mode:                            o.Mode,
		Placement:                       o.Topology,
		Shape:                           o.Shape,
		GPUClass:                        o.GPUClass,
		CheckpointEvery:                 o.CheckpointEvery,
		QueueName:                       o.QueueName,
		PriorityTier:                    o.PriorityTier,
		RequiredTopology:                o.RequiredTopology,
		WorkloadPriorityClassName:       o.WorkloadPriorityClassName,
		PodPriorityClassName:            o.PodPriorityClassName,
		DisableKueueTopologyAnnotations: o.DisableKueueTopologyAnnotations,
		DisableDefaultPriorities:        o.DisableDefaultPriorities,
	})
	if err != nil {
		return nil, err
	}

	storageContract, hasStorageContract, err := profile.StorageContractFromProfile(p)
	if err != nil {
		return nil, err
	}
	storagePlan, err := buildStoragePlan(p, o, storageContract, hasStorageContract)
	if err != nil {
		return nil, err
	}
	if err := validateImageAssetStorageCollisions(storagePlan, o.ImageAssets); err != nil {
		return nil, err
	}
	if storagePlan.HasDurableData {
		env = mergeEnv(storage.ContractEnv(storageContract, hasStorageContract), env)
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
	// Index the declared checkpoint after a successful run so
	// `tau serve deploy --from-finetune` can resolve this run by name.
	// Wrapped before the metrics offloader so the index exists by the time
	// the offloader reports terminal status.
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

	gpuPlan, err := profile.BuildGPUSchedulingPlan(p)
	if err != nil {
		return nil, err
	}

	if o.Launcher == "torchrun" && o.ProcessesPerNode > 1 && gpuPlan.Contract.Count == 0 {
		return nil, fmt.Errorf("processes_per_node (%d) with torchrun requires a known GPU count, but the profile does not declare one; set spec.resources.gpu.count in the profile", o.ProcessesPerNode)
	}
	if o.Launcher == "torchrun" && o.ProcessesPerNode > 0 && gpuPlan.Contract.Count > 0 && o.ProcessesPerNode > gpuPlan.Contract.Count {
		return nil, fmt.Errorf("processes_per_node (%d) exceeds profile GPU count (%d)", o.ProcessesPerNode, gpuPlan.Contract.Count)
	}

	job, err := buildJob(p, o, image, cmd, env, topologyPlan, storagePlan, gpuPlan, storageContract, hasStorageContract)
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
	if o.Namespace == "" {
		return errors.New("Options.Namespace is required")
	}
	if err := (runconfig.Storage{ImageAssets: o.ImageAssets}).ValidateImageAssets(); err != nil {
		return err
	}
	if o.TTLSecondsAfterFinished < 0 {
		return errors.New("Options.TTLSecondsAfterFinished must be >= 0")
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
	if o.Nodes > 1 && len(o.Name)+len(HeadlessSuffix) > 63 {
		return fmt.Errorf("job name %q is too long for multi-node: %q must fit DNS label limit (63 chars); max name length is %d", o.Name, o.Name+HeadlessSuffix, 63-len(HeadlessSuffix))
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
	if rt, ok := p.Spec["runtime"].(map[string]any); ok {
		if img, ok := rt["image"].(string); ok && img != "" {
			return img, nil
		}
	}
	return "", fmt.Errorf("no image: profile %q declares no spec.runtime.image and --image was not set", p.Name)
}

// RequirePythonEntrypoint rejects an entrypoint torchrun cannot execute.
// torchrun spawns Python interpreters, so the rendered command decodes the
// entrypoint to a .py file and passes it to `python3 -m torch.distributed.run`.
// A shell script there reaches CPython and dies with a SyntaxError once the pod
// is already running. The default launcher has no such constraint: it execs the
// decoded script, so the shebang picks the interpreter.
//
// An empty path is left alone: torchrun with no entrypoint at all is a missing
// field, which the caller reports by name.
func RequirePythonEntrypoint(scriptPath string) error {
	scriptPath = strings.TrimSpace(scriptPath)
	if scriptPath == "" || strings.HasSuffix(scriptPath, ".py") {
		return nil
	}
	return fmt.Errorf("run.entrypoint %q must be a .py file with execution.launcher: torchrun, which runs it under python3; point it at a Python entrypoint or drop the launcher", scriptPath)
}

// resolveCommand returns the container command + extra env vars.
// Precedence: Options.Command > Options.ScriptPath > nil (image's own
// ENTRYPOINT/CMD takes over — common when --image points to a container
// that knows what to do).
func resolveCommand(o Options) ([]string, map[string]string, error) {
	if len(o.Command) > 0 {
		if o.Launcher == "torchrun" {
			return nil, nil, fmt.Errorf("--launcher=torchrun cannot be combined with an explicit command override")
		}
		return o.Command, nil, nil
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
	return nil, nil, nil // let the image's ENTRYPOINT/CMD run
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

// buildJob constructs the Job manifest as a typed map[string]any tree.
// We deliberately avoid k8s.io/api/batch/v1 to keep the binary small —
// shelling out to kubectl with rendered YAML is the contract; we don't
// need typed objects for V0.
func buildJob(p profile.Profile, o Options, image string, cmd []string, extraEnv map[string]string, topologyPlan topology.Plan, storagePlan storagePlan, gpuPlan profile.GPUSchedulingPlan, storageContract profile.StorageContract, hasStorageContract bool) (map[string]any, error) {
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
	for k, v := range gpuPlan.Labels {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			labels[k] = v
		}
	}
	if hasStorageContract {
		for k, v := range storageContract.Labels() {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				labels[k] = v
			}
		}
	}
	queueName := topologyPlan.QueueName
	if q, ok := p.Spec["queue"].(map[string]any); ok {
		if queueName == "" {
			if lq, ok := q["localQueue"].(string); ok && lq != "" {
				queueName = lq
			}
		}
	}
	if strings.TrimSpace(queueName) == "" {
		return nil, fmt.Errorf("render suspended Job: Kueue LocalQueue is required")
	}
	labels[topology.QueueLabel] = queueName
	labels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue

	// Pod spec scaffolding.
	pod := map[string]any{
		"restartPolicy": "Never",
	}
	if o.ServiceAccountName != "" {
		pod["serviceAccountName"] = o.ServiceAccountName
	}
	// Grace period: default 600s for checkpoint flush on SIGTERM; profile
	// may override via spec.policy.terminationGracePeriodSeconds; the
	// CLI option (o.TerminationGracePeriodSeconds) wins over both.
	grace := int64(600)
	if pol, ok := p.Spec["policy"].(map[string]any); ok {
		if g, ok := pol["terminationGracePeriodSeconds"]; ok {
			if gi, ok := toInt64(g); ok && gi >= 0 {
				grace = gi
			}
		}
	}
	if o.TerminationGracePeriodSeconds > 0 {
		grace = o.TerminationGracePeriodSeconds
	}
	pod["terminationGracePeriodSeconds"] = grace
	var profileTolerations any
	if sched, ok := p.Spec["scheduling"].(map[string]any); ok {
		if ns, ok := sched["nodeSelector"]; ok {
			if selector := profileNodeSelector(ns, topologyPlan); len(selector) > 0 {
				pod["nodeSelector"] = selector
			}
		}
		if tols, ok := sched["tolerations"]; ok {
			profileTolerations = tols
		}
		if pc, ok := sched["priorityClassName"].(string); ok && pc != "" {
			if !(o.DisableDefaultPriorities && isTauDefaultPriorityClass(pc)) {
				pod["priorityClassName"] = pc
			}
		}
	}
	if tols := gpuTolerations(profileTolerations, profile.GPURequestPlanFromProfile(p)); len(tols) > 0 {
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
	if gpuPlan.PackingAffinity != nil {
		pod["affinity"] = gpuPlan.PackingAffinity
	}
	if rt, ok := p.Spec["runtime"].(map[string]any); ok {
		if secrets := normalizeImagePullSecrets(rt["imagePullSecrets"]); len(secrets) > 0 {
			pod["imagePullSecrets"] = secrets
		}
	}

	// Container spec.
	container := map[string]any{
		"name":  "main",
		"image": image,
	}
	if rt, ok := p.Spec["runtime"].(map[string]any); ok {
		if pp, ok := rt["imagePullPolicy"].(string); ok && pp != "" {
			container["imagePullPolicy"] = pp
		}
		if securityContext, ok := rt["securityContext"]; ok {
			container["securityContext"] = securityContext
		}
	}
	if len(cmd) > 0 {
		container["command"] = cmd
	}

	// Env: profile-declared env first, then extraEnv (script-injected) wins.
	envList, err := buildEnvList(p, extraEnv, o.EnvSecrets, o.RedactSecrets)
	if err != nil {
		return nil, err
	}
	if len(envList) > 0 {
		container["env"] = envList
	}

	// Resources: CPU/memory requests, plus GPU via device-plugin or DRA.
	resources := map[string]any{}
	if res, ok := p.Spec["resources"].(map[string]any); ok {
		if req, ok := res["requests"]; ok {
			resources["requests"] = req
		}
		if lim, ok := res["limits"]; ok {
			resources["limits"] = lim
		}
	}
	applyContainerResourceOverrides(resources, o)
	resourceClaims, err := profile.ApplyGPUResources(resources, profile.GPURequestPlanFromProfile(p))
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", p.Name, err)
	}

	if len(resourceClaims) > 0 {
		pod["resourceClaims"] = resourceClaims
	}
	if len(resources) > 0 {
		container["resources"] = resources
	}
	// Optional persistent storage. Profile persistence and run overrides can
	// mount arbitrary PVCs; durable /data also gets hot /mnt scratch with a
	// command preflight that falls back to /data when /mnt is unavailable.
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
	if storagePlan.HasDurableData {
		container["workingDir"] = storage.DurableRoot
	}

	// /dev/shm: PyTorch DDP uses shared memory for inter-process IPC. The
	// default 64MB is too small for multi-GPU training; mount an emptyDir
	// backed by memory.
	if o.Launcher == "torchrun" && (o.ProcessesPerNode > 1 || o.Nodes > 1) && !hasVolume(pod, "dshm") {
		shmVol := map[string]any{
			"name":     "dshm",
			"emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "16Gi"},
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

	// Job spec.
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
	for k, v := range gpuPlan.Labels {
		if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
			podLabels[k] = v
		}
	}
	if hasStorageContract {
		for k, v := range storageContract.Labels() {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				podLabels[k] = v
			}
		}
	}
	podLabels[workloadmeta.LabelManagedBy] = workloadmeta.ManagedByValue
	if o.AzureWorkloadIdentity && o.ServiceAccountName != "" {
		podLabels[workloadmeta.LabelAzureWorkloadIdentityUse] = "true"
	}
	podMetadata := map[string]any{
		"labels": podLabels,
	}
	storageAnnotations := map[string]string{}
	if hasStorageContract {
		storageAnnotations = storageContract.Annotations()
	}
	correlationAnnotations := workloadmeta.PodCorrelationAnnotations(o.Annotations)
	if len(correlationAnnotations) > 0 || len(topologyPlan.Annotations) > 0 || len(gpuPlan.Annotations) > 0 || len(storageAnnotations) > 0 {
		podAnnotations := map[string]any{}
		for k, v := range correlationAnnotations {
			podAnnotations[k] = v
		}
		for k, v := range topologyPlan.Annotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				podAnnotations[k] = v
			}
		}
		for k, v := range gpuPlan.Annotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				podAnnotations[k] = v
			}
		}
		for k, v := range storageAnnotations {
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

	jobSpec := map[string]any{
		// Suspended so Kueue admits it; Kueue flips suspend=false on admission.
		"suspend":      true,
		"backoffLimit": int64(o.Retry),
		// Defense in depth: GC the Job + Pods 8h after finish, matching
		// the cluster Kueue retention policy.
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
	if pol, ok := p.Spec["policy"].(map[string]any); ok {
		if d, ok := pol["activeDeadlineSeconds"]; ok {
			jobSpec["activeDeadlineSeconds"] = d
		}
	}
	// Options override wins over profile policy.
	if o.ActiveDeadlineSeconds > 0 {
		jobSpec["activeDeadlineSeconds"] = o.ActiveDeadlineSeconds
	}

	metadata := map[string]any{
		"name":      o.Name,
		"namespace": o.Namespace,
		"labels":    labels,
	}
	hasExecution := o.Launcher == "torchrun"
	// Records the declaration so `tau run get` can tell a run that produced
	// nothing from one whose promised artifact is missing. Written into the
	// local map below, not o.Annotations: Options is passed by value but its
	// map header is shared, so stamping there leaks onto the next render.
	checkpointArtifact := strings.TrimSpace(o.CheckpointArtifact)
	if len(o.Annotations) > 0 || len(topologyPlan.Annotations) > 0 || len(gpuPlan.Annotations) > 0 || len(storageAnnotations) > 0 || hasExecution || checkpointArtifact != "" {
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
		for k, v := range gpuPlan.Annotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				annotations[k] = v
			}
		}
		for k, v := range storageAnnotations {
			if k != "" && v != "" && !isGeneratedTauMetadataKey(k) {
				annotations[k] = v
			}
		}
		if hasExecution {
			nproc := max(o.ProcessesPerNode, 1)
			nodes := max(o.Nodes, 1)
			annotations[workloadmeta.AnnotationSpecExecution] = fmt.Sprintf(`{"launcher":%q,"processes_per_node":%d,"nodes":%d}`, o.Launcher, nproc, nodes)
		}
		if o.Nodes > 1 {
			annotations[workloadmeta.AnnotationMultiKueueIncompatible] = "indexed-job-headless-service"
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

func copyResourceQuantities(existing any) map[string]any {
	out := map[string]any{}
	if values, ok := existing.(map[string]any); ok {
		for key, value := range values {
			out[key] = value
		}
	}
	return out
}

func buildStoragePlan(p profile.Profile, o Options, contract profile.StorageContract, hasContract bool) (storagePlan, error) {
	var plan storagePlan
	if err := addProfilePersistence(&plan, p); err != nil {
		return storagePlan{}, err
	}

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
		if hasContract {
			if hot, ok := contract.Role(profile.StorageRoleHotScratch); ok && hot.Type != "" && hot.Type != profile.StorageTypeEmptyDir {
				return storagePlan{}, fmt.Errorf("profile %q storage.hot.type=%q at %s requires an explicit mount; the Job path only synthesizes empty-dir hot scratch", p.Name, hot.Type, storage.HotRoot)
			}
		}
		if err := plan.addVolumeMount(
			Volume{Name: "tau-hot"},
			VolumeMount{Name: "tau-hot", MountPath: storage.HotRoot},
		); err != nil {
			return storagePlan{}, err
		}
	}
	return plan, nil
}

// gpuTolerations returns the pod tolerations for a workload, combining the
// profile's spec.scheduling.tolerations with the two taints AKS GPU node pools
// carry: the "sku=gpu" pool taint and the device plugin's "nvidia.com/gpu".
// Kueue unions a pod set's own tolerations with the admitting ResourceFlavor's
// before filtering nodes, so injecting these lets a GPU workload schedule onto
// a tainted pool even when the flavor omits them. Tolerations only ever widen
// where a pod may land, so profile entries are kept rather than replaced.
//
// The GPU test mirrors ApplyGPUResources: a device-plugin profile requests GPUs
// through Count, a DRA profile through ClaimTemplate. Gating on Count alone
// would leave DRA workloads without tolerations.
func gpuTolerations(profileRaw any, gpu profile.GPURequestPlan) []any {
	var out []any
	seen := map[string]bool{}
	add := func(t any) {
		key := tolerationIdentity(t)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, t)
	}
	switch tols := profileRaw.(type) {
	case nil:
	case []any:
		for _, t := range tols {
			add(t)
		}
	default:
		// An unrecognized shape is passed through so the API server rejects it
		// loudly, rather than being dropped here where nothing reports it.
		add(tols)
	}
	if gpu.Count > 0 || gpu.ClaimTemplate != "" {
		add(map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"})
		add(map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"})
	}
	return out
}

// tolerationIdentity keys a toleration by the fields Kubernetes matches on, so
// a profile that already declares one of the GPU tolerations does not get a
// duplicate. tolerationSeconds is part of the key because two entries differing
// only in it are genuinely different. Entries that are not maps key on their
// rendered value, so an unrecognized shape is passed through rather than
// collapsed into another.
func tolerationIdentity(t any) string {
	m, ok := t.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", t)
	}
	return fmt.Sprintf("%v|%v|%v|%v|%v", m["key"], m["operator"], m["value"], m["effect"], m["tolerationSeconds"])
}

func profileNodeSelector(raw any, topologyPlan topology.Plan) map[string]any {
	out := map[string]any{}
	switch selector := raw.(type) {
	case map[string]any:
		for k, v := range selector {
			if k != "" && v != nil && !strings.HasPrefix(k, workloadmeta.Domain) {
				out[k] = v
			}
		}
	case map[string]string:
		for k, v := range selector {
			if k != "" && v != "" && !strings.HasPrefix(k, workloadmeta.Domain) {
				out[k] = v
			}
		}
	default:
		return nil
	}
	return out
}

func isGeneratedTauMetadataKey(key string) bool {
	return strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelGPUClass
}

func isTauDefaultPriorityClass(name string) bool {
	return name == topology.DefaultTrainPodPriority || name == topology.DefaultElasticWorkloadPrio
}

func addProfilePersistence(plan *storagePlan, p profile.Profile) error {
	res, ok := p.Spec["resources"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := res["persistence"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case map[string]any:
		if err := addPersistenceEntry(plan, v, 0); err != nil {
			return fmt.Errorf("profile %q spec.resources.persistence: %w", p.Name, err)
		}
		return nil
	case []any:
		for i, item := range v {
			entry, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("profile %q spec.resources.persistence[%d] must be a map", p.Name, i)
			}
			if err := addPersistenceEntry(plan, entry, i); err != nil {
				return fmt.Errorf("profile %q spec.resources.persistence[%d]: %w", p.Name, i, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("profile %q spec.resources.persistence must be a map or list", p.Name)
	}
}

func addPersistenceEntry(plan *storagePlan, entry map[string]any, idx int) error {
	pvc, ok := entry["pvcName"].(string)
	if !ok || pvc == "" {
		return errors.New("pvcName is required")
	}
	mountPath, ok := entry["mountPath"].(string)
	if !ok || mountPath == "" {
		return errors.New("mountPath is required")
	}
	readOnly := false
	if raw, ok := entry["readOnly"]; ok {
		var boolOK bool
		readOnly, boolOK = raw.(bool)
		if !boolOK {
			return errors.New("readOnly must be boolean")
		}
	}
	name := generatedPersistenceVolumeName(mountPath, idx)
	return plan.addVolumeMount(
		Volume{Name: name, PVC: pvc},
		VolumeMount{Name: name, MountPath: mountPath, ReadOnly: readOnly},
	)
}

func generatedPersistenceVolumeName(mountPath string, idx int) string {
	if mountPath == storage.DurableRoot {
		return "data"
	}
	return fmt.Sprintf("persistence-%d", idx)
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

// validateUserEnv guards the Tau-owned TAU_ namespace. The rule lives in
// core/runconfig so this gate and the one `tau run --config` applies at load
// cannot drift apart.
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

// applyProfiler wraps the rendered command with the requested profiler
// (ncu or nsys), arranging for rank-scoped profile artifacts to land under a
// durable output directory. The wrapper checks at runtime that the tool is
// installed in the image and exits 127 with a clear message if it isn't.
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

// profileModeWrapperScript returns a shell snippet that rewrites $@ to
// invoke the requested profiler (if installed) before exec'ing the original
// entrypoint. It is concatenated with whatever wrapping is already on cmd
// (storage preflight, script base64 entrypoint, etc) — those already form
// a single argv that becomes "$@" here.
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

func normalizeImagePullSecrets(raw any) []any {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []any{map[string]any{"name": v}}
	case []string:
		out := make([]any, 0, len(v))
		for _, name := range v {
			if name != "" {
				out = append(out, map[string]any{"name": name})
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			switch s := item.(type) {
			case string:
				if s != "" {
					out = append(out, map[string]any{"name": s})
				}
			case map[string]any:
				if name, ok := s["name"].(string); ok && name != "" {
					out = append(out, map[string]any{"name": name})
				}
			}
		}
		return out
	default:
		return nil
	}
}

func buildEnvList(p profile.Profile, extra map[string]string, secrets []envspec.Var, redactSecrets bool) ([]any, error) {
	merged := map[string]string{
		"TAU_PROFILE": p.Name,
	}
	if rt, ok := p.Spec["runtime"].(map[string]any); ok {
		if envMap, ok := rt["env"].(map[string]any); ok {
			for k, v := range envMap {
				if s, ok := v.(string); ok {
					merged[k] = s
				}
			}
		}
	}
	for k, v := range extra {
		merged[k] = v
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

// shellQuote wraps s so it can be embedded as a single token in a bash
// command string. POSIX-safe: single-quote everything, and escape embedded
// single quotes via the standard '"'"' trick.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Fast path: only safe chars → no quoting needed.
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

// renderExtraFlags builds a shell fragment from a key-value map. Keys are
// sorted alphabetically for deterministic output. Each entry becomes
// " --key=value" (value shell-quoted) or " --key" when value is empty.
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

// toInt64 coerces numeric values that YAML/JSON decoders produce into
// int64. YAML gives us `int`, `int64`, or `float64` depending on size
// and source. Returns (0, false) for anything non-numeric.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}
