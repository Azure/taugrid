package cli

import (
	"context"
	"fmt"
	"io"
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
	)
	cmd := &cobra.Command{
		Use:   "status [job-name]",
		Short: "Show job lifecycle and startup phases",
		Long: `Show the full lifecycle of a tau-submitted job:
  - The batch/v1 Job or RayJob (state, conditions, deployment status).
  - Kueue Workload(s) (queue, admission, phase, blocking reason if any).
  - Startup phases: Kueue admission, pod scheduling, DRA allocation, image pull,
    init containers, container start, readiness, and RayJob status.
  - Pods (phase, ready, restarts, node).

For RayJobs, shows the deployment status (New/Initializing/Running/Complete/
Failed/Suspended), the associated RayCluster name, and discovers pods via
the ray.io/cluster label.

Examples:
  tau run status lora-7b-001 -n ray
  tau run status my-job --context research-admin -n ray
  tau run status my-rayjob --context research-admin -n ray
  tau run status sample-finetune --watch -n ray`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedContext, ns, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			opts := statusRunOptions{
				Namespace:       ns,
				KubeContext:     resolvedContext,
				RunProfile:      runProfile,
				Watch:           watch,
				Interval:        watchInterval,
				MaxIterations:   maxIterations,
				DiagnosticHints: diagnosticHints,
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
	return cmd
}

type statusRunOptions struct {
	Namespace       string
	KubeContext     string
	Watch           bool
	Interval        time.Duration
	MaxIterations   int
	RunProfile      bool
	DiagnosticHints bool
}

func runStatusCommand(cmd *cobra.Command, opts statusRunOptions, name string) error {
	resolvedNamespace, err := resolveWorkloadNamespace(cmd, opts.KubeContext, opts.Namespace)
	if err != nil {
		return err
	}
	opts.Namespace = resolvedNamespace
	if opts.Watch && opts.DiagnosticHints {
		return fmt.Errorf("--diagnostic-hints cannot be used with --watch")
	}
	if opts.Watch {
		return watchStatusCommand(cmd, opts, name)
	}
	r := kube.New(opts.KubeContext)
	snap, err := status.Fetch(cmd.Context(), r, opts.Namespace, name)
	if err != nil {
		return err
	}
	writeStatusSnapshot(cmd.OutOrStdout(), snap, opts.RunProfile)
	if opts.DiagnosticHints {
		fmt.Fprint(cmd.OutOrStdout(), renderKubectlDiagnosticHints(opts.KubeContext, opts.Namespace, name, snap))
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
	r := kube.New(opts.KubeContext)
	hooks := watchStatusHooks{
		fetch: func(ctx context.Context) (status.Snapshot, error) {
			return status.Fetch(ctx, r, opts.Namespace, name)
		},
		wait:        waitStatusInterval,
		clearScreen: clearStatusScreen,
	}
	return watchStatusCommandWithHooks(cmd, opts, name, hooks)
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
		writeStatusSnapshot(out, snap, opts.RunProfile)
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

func writeStatusSnapshot(w io.Writer, snap status.Snapshot, runProfile bool) {
	fmt.Fprint(w, status.Render(snap))
	if runProfile {
		fmt.Fprint(w, status.RenderRunProfile(snap, status.CostProfile{}))
	}
}

func renderKubectlDiagnosticHints(kubeContext, namespace, name string, snap status.Snapshot) string {
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
	base = append(base, "-n", namespace)
	selector := diagnosticPodSelector(name, snap)

	var b strings.Builder
	b.WriteString("\nDeep diagnostics (kubectl escape hatches):\n")
	if len(snap.Pods) > 0 {
		for _, pod := range snap.Pods {
			topArgs := append(append([]string{}, base...), "top", "pod", pod.Name, "--containers")
			b.WriteString("  ")
			b.WriteString(renderShellCommand(topArgs))
			b.WriteByte('\n')
		}
	} else {
		topArgs := append(append([]string{}, base...), "top", "pod", "-l", selector, "--containers")
		b.WriteString("  ")
		b.WriteString(renderShellCommand(topArgs))
		b.WriteByte('\n')
	}

	logArgs := append(append([]string{}, base...), "logs", "-l", selector, "--all-containers=true", "--prefix=true", "--timestamps=true")
	if pod, container, previous, ok := firstDiagnosticContainer(snap); ok {
		logArgs = append(append([]string{}, base...), "logs", pod, "-c", container)
		if previous {
			logArgs = append(logArgs, "--previous")
		}
		logArgs = append(logArgs, "--timestamps=true")
	}
	b.WriteString("  ")
	b.WriteString(renderShellCommand(logArgs))
	b.WriteByte('\n')

	if pod, container, ok := firstRunnableContainer(snap); ok {
		execArgs := append(append([]string{}, base...), "exec", "-it", pod, "-c", container, "--", "/bin/sh")
		b.WriteString("  ")
		b.WriteString(renderShellCommand(execArgs))
		b.WriteByte('\n')
	}
	return b.String()
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
	for _, pod := range snap.Pods {
		if len(pod.Containers) > 0 {
			return pod.Name, pod.Containers[0].Name, true
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
	cmd := &cobra.Command{
		Use:   "logs <job-name>",
		Short: "Stream logs for a job",
		Args:  cobra.ExactArgs(1),
		Long: `Stream logs for a tau-submitted job.

For RayJobs with local worker access, fetches the actual job execution logs via
the Ray Dashboard API (ray job logs) instead of the head pod container logs.
For manager-side MultiKueue RayJobs, reads centrally offloaded driver logs from
ADX Logs.ContainerLogs and requires --kusto-endpoint plus --kusto-database once
the selected worker is known. For batch/v1 Jobs, streams pod logs via the
Kubernetes job-name=<name> label selector.

Examples:
  tau run logs lora-7b-001 -n ray
  tau run logs lora-7b-001 -n ray -f --tail=100
  tau run logs lora-7b-001 -n ray --kusto-endpoint=https://... --kusto-database=Logs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedContext, ns, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			return runLogsCommandWithHooks(cmd.Context(), cmd.OutOrStdout(), r, name, runLogsOptions{
				Namespace:     ns,
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
			}, runLogsHooks{})
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

// rayJobLogs fetches Ray Job execution logs by exec-ing into the head pod.
// Returns an error if the workload is not a RayJob or the logs cannot be retrieved.
func rayJobLogs(ctx context.Context, r *kube.Runner, namespace, name string, follow bool) (string, error) {
	// 1. Get the RayJob's jobId.
	jobID, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "rayjob", name,
		"-o", "jsonpath={.status.jobId}",
	}, nil)
	if err != nil || jobID == "" {
		return "", fmt.Errorf("not a RayJob or jobId not available")
	}

	// 2. Find the head pod through the KubeRay RayCluster name.
	clusterName, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "rayjob", name,
		"-o", "jsonpath={.status.rayClusterName}",
	}, nil)
	clusterName = strings.TrimSpace(clusterName)
	if err != nil || clusterName == "" {
		return "", fmt.Errorf("RayJob %s has no rayClusterName yet", name)
	}
	headPod, err := r.Raw(ctx, []string{
		"-n", namespace, "get", "pods",
		"-l", "ray.io/cluster=" + clusterName + ",ray.io/node-type=head",
		"-o", "jsonpath={.items[0].metadata.name}",
	}, nil)
	if err != nil || headPod == "" {
		return "", fmt.Errorf("head pod not found for RayJob %s", name)
	}

	// 3. Exec ray job logs inside the head pod.
	execArgs := []string{"-n", namespace, "exec", headPod, "--", "ray", "job", "logs", jobID}
	if follow {
		execArgs = append(execArgs, "--follow")
	}
	out, execErr := r.Raw(ctx, execArgs, nil)
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
	sidecarOut, sidecarErr := rayDriverSidecarLogs(ctx, r, namespace, headPod, follow)
	if sidecarErr == nil {
		return sidecarOut, nil
	}
	return "", fmt.Errorf("ray job logs: %w (fallback to %s sidecar on pod %s also failed: %v)",
		execErr, raylogoffload.SidecarContainerName, headPod, sidecarErr)
}

// rayDriverSidecarLogs reads driver output from the log-offload sidecar, which
// survives the ray-head container's teardown on job completion.
func rayDriverSidecarLogs(ctx context.Context, r *kube.Runner, namespace, headPod string, follow bool) (string, error) {
	args := []string{"-n", namespace, "logs", headPod, "-c", raylogoffload.SidecarContainerName}
	if follow {
		args = append(args, "-f")
	}
	out, err := r.Raw(ctx, args, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("sidecar %s produced no output", raylogoffload.SidecarContainerName)
	}
	return out, nil
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
clusters directly during cancel.

Examples:
  tau run cancel lora-7b-001 -n ray
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
