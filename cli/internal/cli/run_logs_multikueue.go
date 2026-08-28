// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/kql"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type runLogsOptions struct {
	Namespace     string
	Follow        bool
	Tail          int
	Container     string
	AllContainers bool
	Previous      bool
	Timestamps    bool
	Prefix        bool
	KustoCluster  string
	KustoEndpoint string
	KustoDatabase string
}

type runLogsHooks struct {
	fetchSnapshot           func(context.Context) (status.Snapshot, error)
	rayJobLogs              func(context.Context, *kube.Runner, string, string, bool) (string, error)
	rayJobFollow            func(context.Context, *kube.Runner, string, string, int, io.Writer) error
	jobLogs                 func(context.Context, kubeRawRunner, string, string, bool, int) (string, error)
	jobFollow               func(context.Context, *kube.Runner, string, string, runLogsOptions, io.Writer) error
	jobPodsExist            func(context.Context, kubeRawRunner, string, string) (bool, error)
	resolveMultiKueueWorker func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error)
	queryADXLogs            func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error)
}

type multiKueueWorkerRef struct {
	Name        string
	Annotations map[string]string
}

func (r multiKueueWorkerRef) ADXClusterName() string {
	if name := strings.TrimSpace(r.Annotations[workloadmeta.AnnotationClusterName]); name != "" {
		return name
	}
	return ""
}

type kustoLogsQuery struct {
	Endpoint string
	Database string
	Query    string
}

func runLogsCommandWithHooks(ctx context.Context, out io.Writer, r *kube.Runner, name string, opts runLogsOptions, hooks runLogsHooks) error {
	if err := validateRunLogsOptions(opts); err != nil {
		return err
	}
	hooks = normalizeRunLogsHooks(r, opts, name, hooks)

	snap, snapErr := hooks.fetchSnapshot(ctx)
	preferBatchJob := hasBatchLogOptions(opts) && snap.JobFound
	if snapErr == nil && shouldUseManagerMultiKueueLogs(snap) && !preferBatchJob {
		if hasBatchLogOptions(opts) {
			return batchLogOptionsError(name)
		}
		logs, err := managerMultiKueueRayJobLogs(ctx, r, name, opts, snap, hooks)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, logs)
		return err
	}
	if snapErr != nil && snap.RayJob.Found && snap.IsMultiKueue() && !preferBatchJob {
		return fmt.Errorf("resolve manager-side MultiKueue placement for RayJob %s: %w", name, snapErr)
	}
	if snap.RayJob.Found && !snap.JobFound && hasBatchLogOptions(opts) {
		return batchLogOptionsError(name)
	}

	// A positively identified RayJob remains authoritative even when an
	// ancillary Workload query made the snapshot partial. Never fall through to
	// the batch/v1 path: it selects pods by job-name=<name>, which a RayJob's
	// pods do not carry, so it matches nothing and reports empty success.
	if snap.RayJob.Found && !snap.JobFound {
		var logs string
		var err error
		if opts.Follow {
			countedRay := &countingWriter{out: out}
			err = hooks.rayJobFollow(ctx, r, opts.Namespace, name, opts.Tail, countedRay)
			if err != nil && countedRay.written > 0 {
				return fmt.Errorf("follow logs for RayJob %s: %w", name, err)
			}
		} else {
			logs, err = hooks.rayJobLogs(ctx, r, opts.Namespace, name, false)
		}
		if err != nil {
			if logs != "" {
				if _, writeErr := io.WriteString(out, logs); writeErr != nil {
					return writeErr
				}
				return fmt.Errorf("read logs for RayJob %s: %w", name, err)
			}
			if localRayJobTerminal(snap.RayJob) {
				centralLogs, centralErr := localTerminalRayJobLogs(ctx, name, opts, snap, hooks)
				if centralErr == nil {
					_, centralErr = io.WriteString(out, centralLogs)
					return centralErr
				}
				return fmt.Errorf("read logs for terminal RayJob %s: local pod logs unavailable: %w; central log fallback failed: %v", name, err, centralErr)
			}
			// Distinguish "not ready yet" from "no logs". Terse on purpose:
			// `tau run status` already renders the startup phases.
			hint := ""
			statusCommand := runRouteCommandHint(r, opts.Namespace, "run", "status", name)
			if strings.TrimSpace(snap.RayJob.JobID) == "" {
				hint = fmt.Sprintf("; the RayJob has not reported status.jobId yet, so no driver logs exist. Run `%s` to see startup progress, then retry", statusCommand)
			} else {
				hint = fmt.Sprintf("; run `%s` to inspect startup progress, then retry", statusCommand)
			}
			return fmt.Errorf("read logs for RayJob %s: %w%s", name, err, hint)
		}
		if opts.Follow {
			return nil
		}
		_, err = io.WriteString(out, logs)
		return err
	}

	// A successful snapshot that located neither workload kind does NOT by itself
	// prove the run is missing: the Job can be TTL-deleted while its pods linger,
	// so the label selector below may still find output. Attempt both paths and
	// decide on the OBSERVED result rather than on a prediction from the snapshot.
	var logs string
	var err error
	var rayErr error
	if !hasBatchLogOptions(opts) {
		if opts.Follow {
			countedRay := &countingWriter{out: out}
			if err = hooks.rayJobFollow(ctx, r, opts.Namespace, name, opts.Tail, countedRay); err == nil {
				return nil
			}
			if countedRay.written > 0 {
				return fmt.Errorf("follow logs for RayJob %s: %w", name, err)
			}
		} else {
			logs, err = hooks.rayJobLogs(ctx, r, opts.Namespace, name, false)
			if err == nil {
				_, err = io.WriteString(out, logs)
				return err
			}
			if logs != "" {
				if _, writeErr := io.WriteString(out, logs); writeErr != nil {
					return writeErr
				}
				return fmt.Errorf("read logs for RayJob %s: %w", name, err)
			}
		}
		if errors.Is(err, errRayJobNotReady) {
			return rayJobNotReadyError(r, name, opts.Namespace, err)
		}
		rayErr = err
	}

	if opts.Follow {
		counted := &countingWriter{out: out}
		if err := hooks.jobFollow(ctx, r, opts.Namespace, name, opts, counted); err != nil {
			if errors.Is(err, errJobLogTargetNotFound) {
				return missingRunLogsError(r, name, opts.Namespace, snapErr, rayErr)
			}
			return err
		}
		if counted.written == 0 && !snap.JobFound {
			found, findErr := hooks.jobPodsExist(ctx, r, opts.Namespace, name)
			if findErr != nil {
				return findErr
			}
			if !found {
				return missingRunLogsError(r, name, opts.Namespace, snapErr, rayErr)
			}
		}
		return nil
	}
	logs, err = hooks.jobLogs(ctx, r, opts.Namespace, name, opts.Follow, opts.Tail)
	if logs != "" {
		if _, writeErr := io.WriteString(out, logs); writeErr != nil {
			return writeErr
		}
		return err
	}
	if err == nil {
		if snap.JobFound {
			return nil
		}
		found, findErr := hooks.jobPodsExist(ctx, r, opts.Namespace, name)
		if findErr != nil {
			return findErr
		}
		if found {
			return nil
		}
		// Both paths came back empty with no error. `kubectl logs -l
		// job-name=<name>` matches nothing for an unknown name and reports
		// success, so returning nil here would exit 0 in silence and a typo
		// would read as "the run produced no output". Same empty-match hazard
		// the RayJob guard above documents, reached when no object was found.
		return missingRunLogsError(r, name, opts.Namespace, snapErr, rayErr)
	}
	return err
}

func normalizeRunLogsHooks(r *kube.Runner, opts runLogsOptions, name string, hooks runLogsHooks) runLogsHooks {
	if hooks.fetchSnapshot == nil {
		hooks.fetchSnapshot = func(ctx context.Context) (status.Snapshot, error) {
			return status.FetchRunLogs(ctx, r, opts.Namespace, name)
		}
	}
	if hooks.rayJobLogs == nil {
		hooks.rayJobLogs = func(ctx context.Context, r *kube.Runner, namespace, name string, follow bool) (string, error) {
			if r == nil {
				return "", fmt.Errorf("not a RayJob or jobId not available")
			}
			return rayJobLogs(ctx, r, namespace, name, follow, opts.Tail)
		}
	}
	if hooks.rayJobFollow == nil {
		hooks.rayJobFollow = func(ctx context.Context, r *kube.Runner, namespace, name string, tail int, out io.Writer) error {
			if r == nil {
				return fmt.Errorf("not a RayJob or jobId not available")
			}
			return rayJobFollow(ctx, r, namespace, name, tail, out)
		}
	}
	if hooks.jobLogs == nil {
		hooks.jobLogs = func(ctx context.Context, r kubeRawRunner, namespace, name string, _ bool, _ int) (string, error) {
			return localJobLogsWithOptions(ctx, r, namespace, name, opts)
		}
	}
	if hooks.jobFollow == nil {
		hooks.jobFollow = localJobLogsFollow
	}
	if hooks.jobPodsExist == nil {
		hooks.jobPodsExist = func(ctx context.Context, _ kubeRawRunner, namespace, name string) (bool, error) {
			if r == nil {
				return false, nil
			}
			return localJobPodsExist(ctx, r, namespace, name)
		}
	}

	if hooks.resolveMultiKueueWorker == nil {
		hooks.resolveMultiKueueWorker = fetchMultiKueueWorkerRef
	}
	if hooks.queryADXLogs == nil {
		hooks.queryADXLogs = queryADXLogs
	}
	return hooks
}

func rayJobNotReadyError(r *kube.Runner, name, namespace string, cause error) error {
	return fmt.Errorf(
		"read logs for RayJob %s: %w; the RayJob is not ready for driver logs. Run `%s` to see startup progress, then retry",
		name,
		cause,
		runRouteCommandHint(r, namespace, "run", "status", name),
	)
}

func missingRunLogsError(r *kube.Runner, name, namespace string, lookupErrors ...error) error {
	result := []error{fmt.Errorf(
		"no logs found for run %q in namespace %s; it may not exist or may not have started — run `%s` to see available runs",
		name,
		namespace,
		runRouteCommandHint(r, namespace, "run", "list"),
	)}
	for _, lookupErr := range lookupErrors {
		if lookupErr == nil ||
			errors.Is(lookupErr, errRayJobNotReady) ||
			isExactObjectNotFound(lookupErr, name, "rayjob.ray.io", "rayjobs.ray.io") {
			continue
		}
		result = append(result, fmt.Errorf("run lookup failed: %w", lookupErr))
	}
	return errors.Join(result...)
}

func runRouteCommandHint(r *kube.Runner, namespace string, args ...string) string {
	parts := make([]string, 0, len(args)+7)
	if r != nil && strings.TrimSpace(r.Kubeconfig) != "" {
		parts = append(parts, "KUBECONFIG="+commandHintArg(r.Kubeconfig))
	}
	parts = append(parts, "tau")
	for _, arg := range args {
		parts = append(parts, commandHintArg(arg))
	}
	if strings.TrimSpace(namespace) != "" {
		parts = append(parts, "-n", commandHintArg(namespace))
	}
	if r != nil && strings.TrimSpace(r.Context) != "" {
		parts = append(parts, "--context", commandHintArg(r.Context))
	}
	return strings.Join(parts, " ")
}

func commandHintArg(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune("-._/:@", r))
	}) == -1 {
		return value
	}
	return strconv.Quote(value)
}

func validateRunLogsOptions(opts runLogsOptions) error {
	if strings.TrimSpace(opts.Container) != "" && opts.AllContainers {
		return fmt.Errorf("--container and --all-containers cannot be used together")
	}
	return nil
}

func hasBatchLogOptions(opts runLogsOptions) bool {
	return strings.TrimSpace(opts.Container) != "" || opts.AllContainers || opts.Previous || opts.Timestamps || opts.Prefix
}

func batchLogOptionsError(name string) error {
	return fmt.Errorf("container selection, --previous, --timestamps, and --prefix are supported only for batch/v1 Job logs; %s is a RayJob", name)
}

func shouldUseManagerMultiKueueLogs(snap status.Snapshot) bool {
	// Kueue's MultiKueue adapter clears spec.managedBy on the remote RayJob copy
	// (v0.18.2 ray_multikueue_adapter.go SyncJob -> setManagedBy(remoteJob, nil)),
	// so a worker-local snapshot with no manager Workload placement markers must
	// stay on the local logs path instead of querying ADX. FetchRunLogs
	// intentionally skips Pods to match the exact read-only manager viewer RBAC,
	// so run-logs routing keys off manager-visible MultiKueue signals alone.
	return snap.RayJob.Found && snap.IsMultiKueue()
}

func managerMultiKueueRayJobLogs(ctx context.Context, r *kube.Runner, name string, opts runLogsOptions, snap status.Snapshot, hooks runLogsHooks) (string, error) {
	statusCommand := runRouteCommandHint(r, opts.Namespace, "run", "status", name)
	if worker := strings.TrimSpace(snap.PlacementWorkerCluster()); worker != "" {
		if opts.Follow {
			return "", fmt.Errorf("--follow is not supported for manager-side MultiKueue RayJob logs; tau queries ADX and does not implement cursor-based polling or de-duplication for centrally offloaded driver logs")
		}
		if strings.TrimSpace(opts.KustoEndpoint) == "" || strings.TrimSpace(opts.KustoDatabase) == "" {
			return "", fmt.Errorf("manager-side MultiKueue RayJob logs require both --kusto-endpoint and --kusto-database so tau can query ADX Logs.ContainerLogs")
		}
		clusterRef, err := hooks.resolveMultiKueueWorker(ctx, r, worker)
		if err != nil {
			return "", err
		}
		adxCluster := clusterRef.ADXClusterName()
		if adxCluster == "" {
			return "", fmt.Errorf("selected MultiKueueCluster %q is missing required metadata.annotations[%q]; set that annotation to the actual AKS/ADX Cluster value used in Logs.ContainerLogs before using manager-side `tau logs`", clusterRef.Name, workloadmeta.AnnotationClusterName)
		}
		rayClusterName := strings.TrimSpace(snap.RayJob.RayClusterName)
		if rayClusterName == "" {
			return "", fmt.Errorf("RayJob %s has not reported its mirrored RayCluster name yet; wait for `%s` to show the worker-side RayCluster before reading manager-side logs", name, statusCommand)
		}
		logs, err := centralRayDriverLogs(ctx, name, adxCluster, rayClusterName, opts, hooks)
		if err != nil {
			return "", fmt.Errorf("query ADX Logs.ContainerLogs for RayJob %s on worker %s: %w", name, worker, err)
		}
		if logs == "" && opts.Tail != 0 {
			return "", fmt.Errorf("no manager-side driver logs were found in ADX for RayJob %s on worker %s yet; wait for the %s sidecar to ingest logs and try again", name, worker, raylogoffload.SidecarContainerName)
		}
		return logs, nil
	}
	state := snap.MultiKueueState()
	switch state {
	case status.MultiKueueStatePending, status.MultiKueueStateNominated:
		nominated := snap.NominatedWorkerClusters()
		if len(nominated) > 0 {
			return "", fmt.Errorf("RayJob %s has not been assigned to a MultiKueue worker yet; manager-side placement is still nominated for %s", name, strings.Join(nominated, ", "))
		}
		return "", fmt.Errorf("RayJob %s has not been assigned to a MultiKueue worker yet; wait for `%s` to show the selected worker before reading manager-side logs", name, statusCommand)
	default:
		return "", fmt.Errorf("RayJob %s does not have an unambiguous selected MultiKueue worker yet; run `%s` to inspect manager-side placement", name, statusCommand)
	}
}

func localRayJobTerminal(rj status.RayJob) bool {
	for _, state := range []string{rj.JobDeploymentStatus, rj.JobStatus} {
		switch strings.ToUpper(strings.TrimSpace(state)) {
		case "COMPLETE", "FAILED", "SUCCEEDED", "STOPPED":
			return true
		}
	}
	return !rj.FinishedAt.IsZero()
}

func localTerminalRayJobLogs(ctx context.Context, name string, opts runLogsOptions, snap status.Snapshot, hooks runLogsHooks) (string, error) {
	if opts.Follow {
		return "", fmt.Errorf("--follow is not supported after a RayJob's head pod is deleted; tau queries ADX and does not implement cursor-based polling or de-duplication for centrally offloaded driver logs")
	}
	missing := make([]string, 0, 3)
	if strings.TrimSpace(opts.KustoCluster) == "" {
		missing = append(missing, "--kusto-cluster")
	}
	if strings.TrimSpace(opts.KustoEndpoint) == "" {
		missing = append(missing, "--kusto-endpoint")
	}
	if strings.TrimSpace(opts.KustoDatabase) == "" {
		missing = append(missing, "--kusto-database")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("head pod was deleted and terminal local RayJob logs require %s to query ADX Logs.ContainerLogs", strings.Join(missing, ", "))
	}
	rayClusterName := strings.TrimSpace(snap.RayJob.RayClusterName)
	if rayClusterName == "" {
		return "", fmt.Errorf("RayJob %s has no recorded RayCluster name for the ADX pod-prefix query", name)
	}
	logs, err := centralRayDriverLogs(ctx, name, opts.KustoCluster, rayClusterName, opts, hooks)
	if err != nil {
		return "", err
	}
	if logs == "" && opts.Tail != 0 {
		return "", fmt.Errorf("no centrally offloaded driver logs were found in ADX for terminal RayJob %s", name)
	}
	return logs, nil
}

func centralRayDriverLogs(ctx context.Context, name, clusterName, rayClusterName string, opts runLogsOptions, hooks runLogsHooks) (string, error) {
	if opts.Follow {
		return "", fmt.Errorf("--follow is not supported for centrally offloaded RayJob logs")
	}
	if opts.Tail == 0 {
		return "", nil
	}
	rows, err := hooks.queryADXLogs(ctx, kustoLogsQuery{
		Endpoint: strings.TrimSpace(opts.KustoEndpoint),
		Database: strings.TrimSpace(opts.KustoDatabase),
		Query:    buildMultiKueueRayDriverLogsQuery(clusterName, opts.Namespace, rayClusterName, opts.Tail),
	})
	if err != nil {
		return "", fmt.Errorf("query ADX Logs.ContainerLogs for RayJob %s: %w", name, err)
	}
	return formatCentralLogRows(rows, opts.Tail), nil
}

func localJobLogs(ctx context.Context, r kubeRawRunner, namespace, name string, follow bool, tail int) (string, error) {
	return localJobLogsWithOptions(ctx, r, namespace, name, runLogsOptions{Follow: follow, Tail: tail})
}

func localJobLogsWithOptions(ctx context.Context, r kubeRawRunner, namespace, name string, opts runLogsOptions) (string, error) {
	return r.Raw(ctx, localJobLogArgs(namespace, name, opts), nil)
}

var (
	errRayJobNotReady       = errors.New("RayJob is not ready for logs")
	errJobLogTargetNotFound = errors.New("batch Job and matching pods were not found")
)

func localJobLogsFollow(ctx context.Context, r *kube.Runner, namespace, name string, opts runLogsOptions, out io.Writer) error {
	if err := waitForJobLogPod(ctx, r, namespace, name, 500*time.Millisecond); err != nil {
		return err
	}
	return r.RawStream(ctx, localJobLogArgs(namespace, name, opts), nil, out)
}

func waitForJobLogPod(ctx context.Context, r kubeRawRunner, namespace, name string, interval time.Duration) error {
	for {
		found, err := localJobPodsExist(ctx, r, namespace, name)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		job, err := r.Raw(ctx, []string{
			"-n", namespace,
			"get", "job", name,
			"-o", "name",
			"--ignore-not-found",
		}, nil)
		if err != nil {
			return fmt.Errorf("check batch Job %s/%s before following logs: %w", namespace, name, err)
		}
		if strings.TrimSpace(job) == "" {
			return fmt.Errorf("%w: %s/%s", errJobLogTargetNotFound, namespace, name)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func localJobPodsExist(ctx context.Context, r kubeRawRunner, namespace, name string) (bool, error) {
	pods, err := r.Raw(ctx, []string{
		"-n", namespace,
		"get", "pods",
		"-l", "job-name=" + name,
		"-o", "name",
	}, nil)
	if err != nil {
		return false, fmt.Errorf("list batch Job pods for %s/%s: %w", namespace, name, err)
	}
	return strings.TrimSpace(pods) != "", nil
}

func localJobLogArgs(namespace, name string, opts runLogsOptions) []string {
	selector := "job-name=" + name
	args := []string{"-n", namespace, "logs", "-l", selector}
	if container := strings.TrimSpace(opts.Container); container != "" {
		args = append(args, "-c", container)
	}
	if opts.AllContainers {
		args = append(args, "--all-containers=true")
	}
	if opts.Previous {
		args = append(args, "--previous")
	}
	if opts.Timestamps {
		args = append(args, "--timestamps=true")
	}
	if opts.Prefix {
		args = append(args, "--prefix=true")
	}
	if opts.Follow {
		args = append(args, "-f")
	}
	args = append(args, fmt.Sprintf("--tail=%d", opts.Tail))
	return args
}

func fetchMultiKueueWorkerRef(ctx context.Context, r kubeRawRunner, name string) (multiKueueWorkerRef, error) {
	raw, err := r.Raw(ctx, []string{"get", "multikueuecluster", name, "-o", "json"}, nil)
	if err != nil {
		return multiKueueWorkerRef{}, fmt.Errorf("get selected MultiKueueCluster %q (needs manager-side get on multikueueclusters.kueue.x-k8s.io): %w", name, err)
	}
	var doc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return multiKueueWorkerRef{}, fmt.Errorf("decode selected MultiKueueCluster %q: %w", name, err)
	}
	return multiKueueWorkerRef{Name: strings.TrimSpace(name), Annotations: doc.Metadata.Annotations}, nil
}

func queryADXLogs(ctx context.Context, spec kustoLogsQuery) ([]kustoquery.Row, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	client, err := azkustodata.New(
		azkustodata.NewConnectionStringBuilder(spec.Endpoint).WithTokenCredential(cred),
	)
	if err != nil {
		return nil, fmt.Errorf("create ADX client for endpoint %s: %w", spec.Endpoint, err)
	}
	defer client.Close()

	raw, err := client.QueryToJson(ctx, spec.Database, kql.New("").AddUnsafe(spec.Query))
	if err != nil {
		return nil, fmt.Errorf("endpoint=%s database=%s: %w", spec.Endpoint, spec.Database, err)
	}
	rows, err := kustoquery.ParseRows([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse ADX response: %w", err)
	}
	return rows, nil
}

func buildMultiKueueRayDriverLogsQuery(clusterName, namespace, rayClusterName string, tail int) string {
	headPodPrefix := strings.TrimSpace(rayClusterName) + "-head"
	lines := []string{
		"ContainerLogs",
		"| where Cluster == " + kustoquery.QuoteString(strings.TrimSpace(clusterName)),
		"| where Namespace == " + kustoquery.QuoteString(strings.TrimSpace(namespace)),
		"| where Container == " + kustoquery.QuoteString(raylogoffload.SidecarContainerName),
		"| where Pod startswith " + kustoquery.QuoteString(headPodPrefix),
		"| project Timestamp, Body=tostring(Body)",
	}
	if tail >= 0 {
		lines = append(lines,
			"| order by Timestamp desc",
			fmt.Sprintf("| take %d", tail),
			"| order by Timestamp asc",
		)
	} else {
		lines = append(lines, "| order by Timestamp asc")
	}
	return strings.Join(lines, "\n")
}

func formatCentralLogRows(rows []kustoquery.Row, tail int) string {
	if tail >= 0 && len(rows) > tail {
		rows = rows[len(rows)-tail:]
	}
	var b strings.Builder
	for _, row := range rows {
		body := row.Str("Body")
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
