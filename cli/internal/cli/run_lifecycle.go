// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

type watchStatusHooks struct {
	fetch       func(context.Context) (status.Snapshot, error)
	wait        func(context.Context, time.Duration) error
	clearScreen func(io.Writer)
}

func newRunStatusCmd() *cobra.Command {
	var (
		connection      runLifecycleConnectionFlags
		runProfile      bool
		watch           bool
		watchInterval   time.Duration
		maxIterations   int
		diagnosticHints bool
		output          string
	)
	cmd := &cobra.Command{
		Use:   "status <job-name>",
		Short: "Show job lifecycle and startup phases",
		Long: `Show the full lifecycle of a tau-submitted job:
  - The batch/v1 Job or RayJob (state, conditions, deployment status).
  - Kueue Workload(s) (queue, admission, phase, blocking reason if any).
  - Startup phases: Kueue admission, pod scheduling, DRA allocation, image pull,
    init containers, container start, readiness, and RayJob status.
  - Pods (phase, ready, restarts, node).

For RayJobs, shows the deployment status (New/Initializing/Running/Complete/
Failed/Suspended), the associated RayCluster name, and discovers pods via
the ray.io/cluster label.`,
		Example: `  tau run status lora-7b-001 -n ray
  tau run status my-job --context research-admin -n ray
  tau run status my-rayjob --context research-admin -n ray
  tau run status sample-finetune --watch -n ray`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedContext, ns, restore, err := resolveRunStatusConnection(
				cmd,
				output,
				connection.resolve,
			)
			if err != nil {
				return err
			}
			defer restore()
			opts := statusRunOptions{
				Namespace:       ns,
				KubeContext:     resolvedContext,
				Kubeconfig:      activeKubeconfigPath(),
				RunProfile:      runProfile,
				Watch:           watch,
				Interval:        watchInterval,
				MaxIterations:   maxIterations,
				DiagnosticHints: diagnosticHints,
				Output:          output,
			}
			return runStatusCommand(cmd, opts, name)
		},
	}
	connection.add(cmd)
	cmd.Flags().BoolVar(&runProfile, "run-profile", false, "include queue/runtime/artifact profiling for this run")
	cmd.Flags().BoolVar(&watch, "watch", false, "refresh startup phases until ready, failed, interrupted, or --max-iterations")
	cmd.Flags().DurationVar(&watchInterval, "interval", 2*time.Second, "poll interval when --watch is set")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum watch iterations; 0 runs until ready, failed, or interrupted")
	cmd.Flags().BoolVar(&diagnosticHints, "diagnostic-hints", false, "print scoped kubectl commands for deep pod diagnostics")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table (human-readable) or json (machine-readable)")
	return cmd
}

type runStatusConnectionResolver func(*cobra.Command) (string, string, func(), error)

func resolveRunStatusConnection(
	cmd *cobra.Command,
	output string,
	resolve runStatusConnectionResolver,
) (string, string, func(), error) {
	if output != "json" {
		return resolve(cmd)
	}
	stdout := cmd.OutOrStdout()
	cmd.SetOut(cmd.ErrOrStderr())
	resolvedContext, namespace, restore, err := resolve(cmd)
	cmd.SetOut(stdout)
	return resolvedContext, namespace, restore, err
}

func activeKubeconfigPath() string {
	paths := filepath.SplitList(os.Getenv("KUBECONFIG"))
	if len(paths) != 1 {
		return ""
	}
	return strings.TrimSpace(paths[0])
}

type statusRunOptions struct {
	Namespace       string
	KubeContext     string
	Kubeconfig      string
	Watch           bool
	Interval        time.Duration
	MaxIterations   int
	RunProfile      bool
	DiagnosticHints bool
	Output          string
}

func runStatusCommand(cmd *cobra.Command, opts statusRunOptions, name string) error {
	if opts.Watch && opts.DiagnosticHints {
		return fmt.Errorf("--diagnostic-hints cannot be used with --watch")
	}
	if opts.Output != "table" && opts.Output != "json" {
		return fmt.Errorf("--output must be table or json")
	}
	if opts.Watch && opts.Output == "json" {
		return fmt.Errorf("--output json cannot be used with --watch; poll status for one JSON document per request")
	}
	if opts.DiagnosticHints && opts.Output == "json" {
		return fmt.Errorf("--diagnostic-hints cannot be used with --output json; JSON output already includes diagnostic commands")
	}
	resolvedNamespace, err := resolveWorkloadNamespace(cmd, opts.KubeContext, opts.Namespace)
	if err != nil {
		return err
	}
	opts.Namespace = resolvedNamespace
	if opts.Watch {
		return watchStatusCommand(cmd, opts, name)
	}
	r := kube.NewWithKubeconfig(opts.KubeContext, opts.Kubeconfig)
	snap, err := fetchRunStatusSnapshot(cmd.Context(), r, opts.Namespace, name, opts.RunProfile)
	if err != nil {
		return err
	}
	doc := newRunStatusDocument(snap, opts.RunProfile, opts.KubeContext, opts.Kubeconfig)
	if opts.Output == "json" {
		return writeRunStatusJSONDocument(cmd.OutOrStdout(), doc)
	}
	if err := writeRunStatusHuman(cmd.OutOrStdout(), doc); err != nil {
		return err
	}
	if opts.DiagnosticHints {
		fmt.Fprint(cmd.OutOrStdout(), renderKubectlDiagnosticHints(opts.KubeContext, opts.Kubeconfig, opts.Namespace, name, snap))
	}
	return nil
}

func watchStatusCommand(cmd *cobra.Command, opts statusRunOptions, name string) error {
	if opts.Interval <= 0 {
		return fmt.Errorf("--interval must be > 0")
	}
	if opts.MaxIterations < 0 {
		return fmt.Errorf("--max-iterations must be >= 0")
	}
	r := kube.NewWithKubeconfig(opts.KubeContext, opts.Kubeconfig)
	hooks := watchStatusHooks{
		fetch: func(ctx context.Context) (status.Snapshot, error) {
			return fetchRunStatusSnapshot(ctx, r, opts.Namespace, name, opts.RunProfile)
		},
		wait:        waitStatusInterval,
		clearScreen: clearStatusScreen,
	}
	return watchStatusCommandWithHooks(cmd, opts, name, hooks)
}

func fetchRunStatusSnapshot(ctx context.Context, r *kube.Runner, namespace, name string, runProfile bool) (status.Snapshot, error) {
	snap, err := status.Fetch(ctx, r, namespace, name)
	if err != nil {
		return snap, err
	}
	if runProfile {
		snap.GPURuntime = status.FetchGPURuntime(ctx, r, snap)
	}
	return snap, nil
}

func watchStatusCommandWithHooks(cmd *cobra.Command, opts statusRunOptions, name string, hooks watchStatusHooks) error {
	for iteration := 0; ; iteration++ {
		snap, err := hooks.fetch(cmd.Context())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		hooks.clearScreen(out)
		fmt.Fprintf(out, "watching %s/%s every %s\n\n", opts.Namespace, name, opts.Interval)
		doc := newRunStatusDocument(snap, opts.RunProfile, opts.KubeContext, opts.Kubeconfig)
		if err := writeRunStatusHuman(out, doc); err != nil {
			return err
		}
		if status.WatchFailed(snap) {
			return fmt.Errorf("startup phase failed for %s/%s", opts.Namespace, name)
		}
		if status.WatchComplete(snap) {
			return nil
		}
		if opts.MaxIterations > 0 && iteration+1 >= opts.MaxIterations {
			return nil
		}
		if err := hooks.wait(cmd.Context(), opts.Interval); err != nil {
			return err
		}
	}
}

func clearStatusScreen(w io.Writer) {
	fmt.Fprint(w, "\033[H\033[2J")
}

func renderKubectlDiagnosticHints(kubeContext, kubeconfig, namespace, name string, snap status.Snapshot) string {
	commands := kubectlDiagnosticCommands(kubeContext, kubeconfig, namespace, name, snap)
	var b strings.Builder
	b.WriteString("\nDeep diagnostics (kubectl escape hatches):\n")
	for _, command := range commands {
		b.WriteString("  ")
		b.WriteString(renderShellCommand(command))
		b.WriteByte('\n')
	}
	return b.String()
}

func kubectlDiagnosticCommands(kubeContext, kubeconfig, namespace, name string, snap status.Snapshot) [][]string {
	if snap.JobFound && snap.RayJob.Found {
		// Status gives a same-name batch Job precedence. The fetched pod set can
		// contain both Job and Ray pods, so use the Job selector rather than
		// emitting pod-specific commands that could cross workload boundaries.
		snap.Pods = nil
	}
	base := []string{"kubectl"}
	if strings.TrimSpace(kubeContext) != "" {
		base = append(base, "--context", kubeContext)
	}
	if strings.TrimSpace(kubeconfig) != "" {
		base = append(base, "--kubeconfig", kubeconfig)
	}
	base = append(base, "-n", namespace)
	selector := diagnosticPodSelector(name, snap)

	commands := make([][]string, 0, len(snap.Pods)+2)
	if len(snap.Pods) > 0 {
		for _, pod := range snap.Pods {
			topArgs := append(append([]string{}, base...), "top", "pod", pod.Name, "--containers")
			commands = append(commands, topArgs)
		}
	} else {
		topArgs := append(append([]string{}, base...), "top", "pod", "-l", selector, "--containers")
		commands = append(commands, topArgs)
	}

	logArgs := append(append([]string{}, base...), "logs", "-l", selector, "--all-containers=true", "--prefix=true", "--timestamps=true")
	if pod, container, previous, ok := firstDiagnosticContainer(snap); ok {
		logArgs = append(append([]string{}, base...), "logs", pod, "-c", container)
		if previous {
			logArgs = append(logArgs, "--previous")
		}
		logArgs = append(logArgs, "--timestamps=true")
	}
	commands = append(commands, logArgs)

	if pod, container, ok := firstRunnableContainer(snap); ok {
		execArgs := append(append([]string{}, base...), "exec", "-it", pod, "-c", container, "--", "/bin/sh")
		commands = append(commands, execArgs)
	}
	return commands
}

func diagnosticPodSelector(name string, snap status.Snapshot) string {
	if !snap.JobFound && snap.RayJob.Found && strings.TrimSpace(snap.RayJob.RayClusterName) != "" {
		return "ray.io/cluster=" + snap.RayJob.RayClusterName
	}
	return "job-name=" + name
}

func firstDiagnosticContainer(snap status.Snapshot) (string, string, bool, bool) {
	for _, pod := range snap.Pods {
		for _, container := range pod.InitContainers {
			if container.ExitCode != nil && *container.ExitCode != 0 {
				return pod.Name, container.Name, false, true
			}
		}
		for _, container := range pod.Containers {
			if container.ExitCode != nil && *container.ExitCode != 0 {
				return pod.Name, container.Name, false, true
			}
		}
	}
	for _, pod := range snap.Pods {
		for _, container := range pod.InitContainers {
			if containerNeedsPrevious(container) {
				return pod.Name, container.Name, true, true
			}
		}
		for _, container := range pod.Containers {
			if containerNeedsPrevious(container) {
				return pod.Name, container.Name, true, true
			}
		}
	}
	for _, pod := range snap.Pods {
		if len(pod.Containers) > 0 {
			return pod.Name, pod.Containers[0].Name, false, true
		}
		if len(pod.InitContainers) > 0 {
			return pod.Name, pod.InitContainers[0].Name, false, true
		}
	}
	return "", "", false, false
}

func containerNeedsPrevious(container status.Container) bool {
	return container.RestartCount > 0 || container.LastExitCode != nil || strings.TrimSpace(container.LastReason) != ""
}

func firstRunnableContainer(snap status.Snapshot) (string, string, bool) {
	for _, pod := range snap.Pods {
		for _, container := range pod.Containers {
			if container.State == "running" {
				return pod.Name, container.Name, true
			}
		}
	}
	return "", "", false
}

func renderShellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}

func newRunLogsCmd() *cobra.Command {
	return newLogsCmd(false)
}

func newRootLogsCmd() *cobra.Command {
	return newLogsCmd(true)
}

func newLogsCmd(discover bool) *cobra.Command {
	var (
		connection    runLifecycleConnectionFlags
		follow        bool
		tail          int
		container     string
		allContainers bool
		previous      bool
		timestamps    bool
		prefix        bool
		kustoCluster  string
		kustoEndpoint string
		kustoDatabase string
	)
	use := "logs <run-name>"
	short := "Stream logs for a run"
	invocation := "tau logs"
	if !discover {
		short = "Stream logs for a job"
		invocation = "tau run logs"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		Long: `Stream logs for a tau-submitted job.

When invoked as ` + "`tau logs`" + ` without routing flags, Tau searches the
connected workspace first, then the locally configured Tau workspace connections.
Use --workspace, or --context and --namespace, to select an exact target.

For RayJobs with local worker access, fetches the actual job execution logs via
the Ray Dashboard API (ray job logs) instead of the head pod container logs.
For manager-side MultiKueue RayJobs, reads centrally offloaded driver logs from
ADX Logs.ContainerLogs and requires --kusto-endpoint plus --kusto-database once
the selected worker is known. For batch/v1 Jobs, streams pod logs via the
Kubernetes job-name=<name> label selector.`,
		Example: `  ` + invocation + ` lora-7b-001
  ` + invocation + ` lora-7b-001 -f --tail=100
  ` + invocation + ` lora-7b-001 --context research-admin -n ray
  ` + invocation + ` lora-7b-001 --kusto-endpoint=https://cluster.kusto.windows.net --kusto-database=Logs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			opts := runLogsOptions{
				Follow:        follow,
				Tail:          tail,
				Container:     container,
				AllContainers: allContainers,
				Previous:      previous,
				Timestamps:    timestamps,
				Prefix:        prefix,
				KustoCluster:  kustoCluster,
				KustoEndpoint: kustoEndpoint,
				KustoDatabase: kustoDatabase,
			}
			explicitRoute := runContextExplicit(cmd) || cmd.Flags().Changed("namespace")
			if discover && !explicitRoute && cmd.Flags().Changed("workspace") {
				hooks := defaultRunLogsDiscoveryHooks(cmd, &connection)
				route, found, err := cachedRunLogsWorkspaceRoute(cmd.Context(), connection.workspace, name, hooks)
				if err != nil {
					return err
				}
				if found {
					return hooks.execute(cmd.Context(), cmd.OutOrStdout(), route, name, opts)
				}
				explicitRoute = true
			}
			if discover && !explicitRoute {
				return runLogsWithDiscovery(
					cmd.Context(),
					cmd.OutOrStdout(),
					cmd.ErrOrStderr(),
					name,
					opts,
					defaultRunLogsDiscoveryHooks(cmd, &connection),
				)
			}
			resolvedContext, ns, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			opts.Namespace = ns
			return runLogsCommandWithHooks(cmd.Context(), cmd.OutOrStdout(), r, name, opts, runLogsHooks{})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new logs")
	cmd.Flags().IntVar(&tail, "tail", 200, "lines from end (-1 = all)")
	cmd.Flags().StringVarP(&container, "container", "c", "", "container or init-container name (batch Jobs only)")
	cmd.Flags().BoolVar(&allContainers, "all-containers", false, "include every container (batch Jobs only)")
	cmd.Flags().BoolVar(&previous, "previous", false, "show logs for the previous container instance (batch Jobs only)")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "include timestamps on each line (batch Jobs only)")
	cmd.Flags().BoolVar(&prefix, "prefix", false, "prefix each line with pod and container names (batch Jobs only)")
	cmd.Flags().StringVar(&kustoCluster, "kusto-cluster", "", "cluster identifier in ADX Logs.ContainerLogs for terminal local RayJob logs")
	cmd.Flags().StringVar(&kustoEndpoint, "kusto-endpoint", "", "ADX endpoint for centrally offloaded RayJob logs")
	cmd.Flags().StringVar(&kustoDatabase, "kusto-database", "", "ADX database for centrally offloaded RayJob logs")
	connection.add(cmd)
	return cmd
}

// rayJobLogs fetches Ray Job execution logs from the head pod.
// Returns an error if the workload is not a RayJob or the logs cannot be retrieved.
func rayJobLogs(ctx context.Context, r *kube.Runner, namespace, name string, follow bool, tail int) (string, error) {
	jobID, headPod, err := resolveRayJobLogTarget(ctx, r, namespace, name)
	if err != nil {
		return "", err
	}
	if tail == 0 && !follow {
		return "", nil
	}

	// The log-offload sidecar supports Kubernetes' native tail and follow
	// semantics. Prefer it for bounded reads; older RayJobs without the sidecar
	// fall back to the Ray Dashboard CLI below.
	var sidecarOut string
	var sidecarErr error
	if tail >= 0 {
		sidecarOut, err = rayDriverSidecarLogs(ctx, r, namespace, headPod, follow, tail)
		if err == nil {
			return sidecarOut, nil
		}
		sidecarErr = err
	}

	execArgs := rayJobLogExecArgs(namespace, headPod, jobID, follow)
	var out string
	var execErr error
	if tail >= 0 {
		tailed := &lineTailWriter{limit: tail}
		execErr = r.RawStream(ctx, execArgs, nil, tailed)
		out = tailed.String()
	} else {
		out, execErr = r.Raw(ctx, execArgs, nil)
	}
	if execErr == nil {
		return out, nil
	}

	// The exec path only works while the ray-head container is alive. KubeRay
	// tears it down when the RayJob reaches a terminal state, so on a job that
	// merely SUCCEEDED this fails with "container not found (ray-head)" and the
	// researcher cannot read the output of their own completed run.
	//
	// The head pod also runs a log-offload sidecar that holds the same driver
	// output and outlives the ray-head container, so fall back to reading it.
	if tail < 0 {
		sidecarOut, err := rayDriverSidecarLogs(ctx, r, namespace, headPod, follow, tail)
		if err == nil {
			return sidecarOut, nil
		}
		sidecarErr = err
	}
	if out == "" {
		out = sidecarOut
	}
	return out, fmt.Errorf("ray job logs: %w (fallback to %s sidecar on pod %s also failed: %v)",
		execErr, raylogoffload.SidecarContainerName, headPod, sidecarErr)
}

func rayJobFollow(ctx context.Context, r *kube.Runner, namespace, name string, tail int, out io.Writer) error {
	jobID, headPod, err := resolveRayJobLogTarget(ctx, r, namespace, name)
	if err != nil {
		return err
	}

	counted := &countingWriter{out: out}
	sidecarErr := r.RawStream(ctx, rayDriverSidecarLogArgs(namespace, headPod, true, tail), nil, counted)
	if sidecarErr == nil {
		return nil
	}
	if counted.written > 0 {
		return sidecarErr
	}
	if tail >= 0 {
		return fmt.Errorf("bounded --follow requires the %s sidecar rendered by current Tau versions; retry with --tail=-1 for this legacy RayJob: %w",
			raylogoffload.SidecarContainerName, sidecarErr)
	}
	if err := r.RawStream(ctx, rayJobLogExecArgs(namespace, headPod, jobID, true), nil, out); err != nil {
		return fmt.Errorf(
			"follow RayJob logs via Ray CLI: %w; prior %s sidecar attempt also failed: %v",
			err,
			raylogoffload.SidecarContainerName,
			sidecarErr,
		)
	}
	return nil
}

func resolveRayJobLogTarget(ctx context.Context, r *kube.Runner, namespace, name string) (string, string, error) {
	jobID, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "rayjob", name,
		"-o", "jsonpath={.status.jobId}",
	}, nil)
	if err != nil {
		return "", "", fmt.Errorf("read RayJob %s/%s: %w", namespace, name, err)
	}
	if strings.TrimSpace(jobID) == "" {
		return "", "", fmt.Errorf("%w: %s/%s has not reported status.jobId", errRayJobNotReady, namespace, name)
	}

	clusterName, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "rayjob", name,
		"-o", "jsonpath={.status.rayClusterName}",
	}, nil)
	clusterName = strings.TrimSpace(clusterName)
	if err != nil {
		return "", "", errors.Join(
			errRayJobNotReady,
			fmt.Errorf("read RayJob %s/%s rayClusterName: %w", namespace, name, err),
		)
	}
	if clusterName == "" {
		return "", "", fmt.Errorf("%w: RayJob %s/%s has no rayClusterName yet", errRayJobNotReady, namespace, name)
	}
	headPod, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "pods",
		"-l", "ray.io/cluster=" + clusterName + ",ray.io/node-type=head",
		"-o", "jsonpath={.items[0].metadata.name}",
	}, nil)
	if err != nil {
		return "", "", errors.Join(
			errRayJobNotReady,
			fmt.Errorf("list head pods for RayJob %s/%s: %w", namespace, name, err),
		)
	}
	if strings.TrimSpace(headPod) == "" {
		return "", "", fmt.Errorf("%w: head pod not found for RayJob %s", errRayJobNotReady, name)
	}
	return strings.TrimSpace(jobID), strings.TrimSpace(headPod), nil
}

func rayJobLogExecArgs(namespace, headPod, jobID string, follow bool) []string {
	args := []string{"-n", namespace, "exec", headPod, "--", "ray", "job", "logs", jobID}
	if follow {
		args = append(args, "--follow")
	}
	return args
}

// rayDriverSidecarLogs reads driver output from the log-offload sidecar, which
// survives the ray-head container's teardown on job completion.
func rayDriverSidecarLogs(ctx context.Context, r *kube.Runner, namespace, headPod string, follow bool, tail int) (string, error) {
	out, err := r.Raw(ctx, rayDriverSidecarLogArgs(namespace, headPod, follow, tail), nil)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("sidecar %s produced no output", raylogoffload.SidecarContainerName)
	}
	return out, nil
}

func rayDriverSidecarLogArgs(namespace, headPod string, follow bool, tail int) []string {
	args := []string{
		"-n", namespace, "logs", headPod,
		"-c", raylogoffload.SidecarContainerName,
		fmt.Sprintf("--tail=%d", tail),
	}
	if follow {
		args = append(args, "-f")
	}
	return args
}

func tailLogOutput(logs string, tail int) string {
	if tail < 0 {
		return logs
	}
	if tail == 0 || logs == "" {
		return ""
	}
	lines := strings.SplitAfter(logs, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= tail {
		return logs
	}
	return strings.Join(lines[len(lines)-tail:], "")
}

type countingWriter struct {
	out     io.Writer
	written int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.written += int64(n)
	return n, err
}

type lineTailWriter struct {
	limit   int
	lines   []string
	next    int
	pending []byte
}

func (w *lineTailWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return len(p), nil
	}
	start := 0
	for i, b := range p {
		if b != '\n' {
			continue
		}
		w.pending = append(w.pending, p[start:i+1]...)
		w.addLine(string(w.pending))
		w.pending = w.pending[:0]
		start = i + 1
	}
	w.pending = append(w.pending, p[start:]...)
	return len(p), nil
}

func (w *lineTailWriter) String() string {
	ordered := w.lines
	if len(w.lines) == w.limit && w.next > 0 {
		ordered = append(append(make([]string, 0, len(w.lines)), w.lines[w.next:]...), w.lines[:w.next]...)
	}
	logs := strings.Join(ordered, "") + string(w.pending)
	return tailLogOutput(logs, w.limit)
}

func (w *lineTailWriter) addLine(line string) {
	if len(w.lines) < w.limit {
		w.lines = append(w.lines, line)
		return
	}
	w.lines[w.next] = line
	w.next = (w.next + 1) % w.limit
}

func newRunCancelCmd() *cobra.Command {
	var (
		connection      runLifecycleConnectionFlags
		timeout         time.Duration
		interval        time.Duration
		wait            bool
		teardownTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "cancel <job-name>",
		Short: "Cancel a running job (deletes Job/RayJob; Kueue cleans up Workload)",
		Args:  cobra.ExactArgs(1),
		Long: `Delete the named Job or RayJob, then wait until the compute is actually
released.

Deleting a RayJob does not free the node. KubeRay's RayJob deletion path only
stops the Ray job and removes its finalizer; the RayCluster goes away through
Kubernetes owner-reference garbage collection, and its pods then drain through
a 600s termination grace period. "kubectl delete rayjob" returns seconds before
any of that completes.

That matters because those still-running pods are no longer attached to a
tracked Kueue Workload, so quota looks free while the node is not. Resubmitting
immediately gets admitted, lands on an occupied node, and Ray's autoscaler
wedges with "No available node types can fulfill cluster constraint" with no
error and no timeout. So cancel waits by default, and reports what it is
waiting on. Pass --wait=false to skip the wait; tau will then say explicitly
that teardown is incomplete.

If the RayCluster outlives its owner, nothing in KubeRay will collect it, so
the wait times out with a non-zero exit and the command to remove it by hand.

Kueue's Job-integration controller observes the deletion and cleans up the
associated Workload object automatically; we don't need to delete the Workload
explicitly.

For MultiKueue jobs, tau waits for the manager-cluster Workload objects to
disappear after the delete request. This is a manager-only proof that the
MultiKueue finalizer finished its remote cleanup path; tau never calls worker
clusters directly during cancel.`,
		Example: `  tau run cancel lora-7b-001 -n ray
  tau run cancel lora-7b-001 -n ray --wait=false`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedContext, ns, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			return cancelWorkload(cmd.Context(), r, name, ns, cmd.OutOrStdout(), cancelWorkloadOptions{
				Timeout:  timeout,
				Interval: interval,
				Release: computeReleaseOptions{
					Enabled:  true,
					Wait:     wait,
					Timeout:  teardownTimeout,
					Interval: interval,
				},
			}, newManagerCleanupHooks(r, ns, name))
		},
	}
	connection.add(cmd)
	cmd.Flags().DurationVar(&timeout, "timeout", defaultManagerCleanupTimeout, "maximum time to wait for MultiKueue manager Workloads to disappear after delete")
	cmd.Flags().DurationVar(&interval, "interval", defaultManagerCleanupInterval, "poll interval while waiting for MultiKueue cleanup and for the RayCluster and its pods to be released")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait until the RayCluster and its pods are gone before reporting success")
	cmd.Flags().DurationVar(&teardownTimeout, "teardown-timeout", defaultComputeReleaseTimeout, "maximum time to wait for the RayCluster and its pods to be released (covers the 600s pod termination grace period)")
	return cmd
}

func deleteWorkload(ctx context.Context, r kubeRawRunner, name, ns string, w io.Writer) error {
	out, rayErr := r.Raw(ctx,
		[]string{"-n", ns, "delete", "rayjob.ray.io", name, "--ignore-not-found"}, nil)
	if out != "" {
		fmt.Fprint(w, out)
	}
	if rayErr != nil && isUnknownResourceError(rayErr) {
		rayErr = nil
	}
	if rayErr != nil && isExactObjectNotFound(rayErr, name, "rayjob.ray.io", "rayjobs.ray.io") {
		rayErr = nil
	}

	out, jobErr := r.Raw(ctx,
		[]string{"-n", ns, "delete", "job", name, "--ignore-not-found"}, nil)
	if out != "" {
		fmt.Fprint(w, out)
	}
	if jobErr != nil && isExactObjectNotFound(jobErr, name, "job.batch", "jobs.batch") {
		jobErr = nil
	}

	// Clean up headless Service used by multi-node torchrun jobs.
	svcName := name + "-headless"
	out, _ = r.Raw(ctx,
		[]string{"-n", ns, "delete", "service", svcName, "--ignore-not-found"}, nil)
	if out != "" {
		fmt.Fprint(w, out)
	}

	if rayErr != nil {
		return rayErr
	}
	return jobErr
}

func cancelWorkload(ctx context.Context, r kubeRawRunner, name, ns string, w io.Writer, opts cancelWorkloadOptions, hooks cancelWorkloadHooks) error {
	return deleteWorkloadAndWaitForManagerCleanup(ctx, r, name, ns, w, opts, hooks)
}

func describeAdmissionStates(snap status.Snapshot, workloadNames []string) string {
	if len(workloadNames) == 0 {
		return ""
	}
	remaining := make(map[string]bool, len(workloadNames))
	for _, name := range workloadNames {
		remaining[name] = true
	}
	parts := make([]string, 0)
	for _, summary := range snap.AdmissionCheckSummaries() {
		if !remaining[summary.WorkloadName] {
			continue
		}
		part := summary.WorkloadName + "/" + summary.Name + "=" + summary.State
		if msg := strings.TrimSpace(summary.Message); msg != "" {
			part += "(" + msg + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func isUnknownResourceError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "server doesn't have a resource type") ||
		strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "resource type") && strings.Contains(msg, "not found")
}

func isExactObjectNotFound(err error, name string, resourceKinds ...string) bool {
	if err == nil {
		return false
	}
	if name == "" {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	quotedName := `"` + strings.ToLower(name) + `"`
	for _, resourceKind := range resourceKinds {
		pattern := strings.ToLower(resourceKind) + " " + quotedName + " not found"
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
