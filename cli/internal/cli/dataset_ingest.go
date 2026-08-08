// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/datasetingest"
	"github.com/Azure/taugrid/cli/internal/storage"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// ingestWorkerCmdName is the name of the hidden ingest-worker subcommand. It
// is a constant so the ingest command and the worker command stay in sync.
const ingestWorkerCmdName = "ingest-worker"

// ConfigMap size guard: manifests over this limit cannot be transported via a
// Kubernetes ConfigMap (the 1 MiB limit minus headroom for metadata).
const configMapPayloadLimit = 500 * 1024 // 500 KiB

// newDatasetIngestRegistryClient is kept as a seam so command tests can prove
// workspace mode never opens a caller-side registry. Only direct/file mode uses
// it; workspace workers construct their registry under workload identity.
var newDatasetIngestRegistryClient = func(rf registryFlags) (*dataset.Registry, error) {
	return rf.inClusterRegistryClient()
}

func newDatasetIngestCmd() *cobra.Command {
	var rf registryFlags
	var (
		sourceRoot  string
		destination string
		workerImage string
		workspace   string
		wait        bool
		dryRun      bool
		output      string
	)
	cmd := &cobra.Command{
		Use:   "ingest NAME@VERSION",
		Short: "Copy dataset bytes from source to destination and verify them",
		Long: `Ingest (copy + verify) dataset bytes from a source location to the
destination recorded in the dataset record.

Two modes:

  LOCAL (--source-root file://<dir>):
      Runs the worker logic in-process. No cluster or workspace is needed.
      Use this for local testing and the seed-catalog bootstrap workflow.

  WORKSPACE (--source-root az://<acct>/<ctr>[/<prefix>] or public https://... + --workspace <name>):
      Renders a one-shot batch/v1 Job in the TauWorkspace's namespace with the
      workspace ServiceAccount and workload-identity pod label. The Job invokes
      ` + "`tau data dataset ingest-worker`" + ` using the specified --worker-image
      (must be digest-pinned: image@sha256:<hash>). The worker reads the
      immutable record directly from the project registry using workload identity.

Security:
  - SAS tokens (?sig=, ?sv=, ?se=) are rejected in all URLs.
  - Storage account keys / shared-key credentials are rejected.
  - Plaintext http:// is rejected; az://, file://, and public https:// are accepted source schemes.
  - Worker images must be pinned with @sha256: (mutable tags are rejected).
  - The workspace must be in Ready state before the Job is submitted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			if ref.Version == "" {
				return fmt.Errorf("ingest requires NAME@VERSION (an exact version, not an alias)")
			}

			if sourceRoot == "" {
				return fmt.Errorf("--source-root is required")
			}
			// Security: reject SAS/key/http URLs.
			if err := datasetingest.ValidateAzureURL(sourceRoot); err != nil {
				return fmt.Errorf("--source-root: %w", err)
			}
			if destination != "" {
				if err := datasetingest.ValidateAzureURL(destination); err != nil {
					return fmt.Errorf("--destination: %w", err)
				}
				if !isFileIngestURI(destination) && !strings.HasPrefix(destination, "az://") {
					return fmt.Errorf("--destination must use file:// or az://")
				}
			}

			// Validate workspace-mode security flags before any network/disk I/O.
			isLocalSource := isFileIngestURI(sourceRoot)
			if !isLocalSource {
				if workspace == "" {
					return fmt.Errorf("--workspace is required for az:// or https:// sources (or use file:// for local mode)")
				}
				if workerImage == "" {
					return fmt.Errorf("--worker-image is required for workspace mode")
				}
				if err := validateDigestPinnedImage(workerImage); err != nil {
					return err
				}
				if !strings.HasPrefix(rf.registry, "az://") {
					return fmt.Errorf(
						"workspace ingest requires an az:// registry (--registry az://<account>/<container>); " +
							"the in-cluster worker reads the record and writes status via workload identity",
					)
				}
			}

			// Workspace mode intentionally does not construct or read a registry
			// in the caller process. The Job's workload identity loads the
			// immutable record, resolves the default destination, and validates an
			// explicit destination before copying bytes.
			if !isLocalSource {
				return runIngestWorkspace(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
					ref.Name, ref.Version, sourceRoot, destination, workerImage, workspace,
					rf.registry, rf.kubeContext, wait, dryRun, output)
			}

			// Direct/file mode may read the caller-selected registry.
			reg, err := newDatasetIngestRegistryClient(rf)
			if err != nil {
				return err
			}
			rec, err := reg.Get(cmd.Context(), ref.Name, ref.Version)
			if err != nil {
				return fmt.Errorf("load record %s@%s: %w", ref.Name, ref.Version, err)
			}
			destination, err = resolveIngestDestination(rec, destination)
			if err != nil {
				return err
			}
			return runIngestLocal(cmd.Context(), cmd.OutOrStdout(), ref.Name, ref.Version, reg, rec, sourceRoot, destination, dryRun, output)
		},
	}
	cmd.Flags().StringVar(&sourceRoot, "source-root", "", "source URI: file:///local/dir, az://account/container[/prefix], or public https://...")
	cmd.Flags().StringVar(&destination, "destination", "", "destination az:// URI (default: from record account/container/prefix)")
	cmd.Flags().StringVar(&workerImage, "worker-image", "", "digest-pinned container image for the ingest Job (workspace mode only, must contain @sha256:)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "TauWorkspace name to use for the ingest Job (workspace mode only)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the ingest Job to complete before returning (workspace mode only)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without creating any Kubernetes objects or writing bytes")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "pvc")
	return cmd
}

// datasetIngestResultSchemaVersion is the schema version of the stable
// datasetIngestResult emitted by a successful waited ingest.
const datasetIngestResultSchemaVersion = 1

// datasetIngestResult is the stable schema emitted by a successful waited
// ingest (local or workspace mode). It combines the ingest status (readiness
// evidence: state, per-file proofs, verified totals) with the immutable
// resolved reference so callers get both the verification proof AND a pinned,
// ready-to-consume dataset reference in one document.
type datasetIngestResult struct {
	SchemaVersion int                       `json:"schema_version"`
	Status        dataset.IngestStatus      `json:"status"`
	Reference     dataset.ResolvedReference `json:"reference"`
}

// buildDatasetIngestResult assembles the stable result from a record and its
// terminal ingest status. The resolved reference is derived from the immutable
// record, so it carries the pinned digest and every consumption mode.
func buildDatasetIngestResult(rec dataset.Record, status dataset.IngestStatus) datasetIngestResult {
	ref := dataset.BuildReference(rec, dataset.ReferenceOptions{
		DurableDatasetsDir: storage.DurableDatasetsDir,
		HotDatasetsDir:     storage.HotDatasetsDir,
		ManifestPath:       storage.DatasetRegistryRecordFile(rec.Name, rec.Version),
	})
	return datasetIngestResult{
		SchemaVersion: datasetIngestResultSchemaVersion,
		Status:        status,
		Reference:     ref,
	}
}

// printDatasetIngestResult renders the human-readable form: the ingest status
// followed by the resolved reference's pinned digest and recommended mode.
func printDatasetIngestResult(w io.Writer, res datasetIngestResult) error {
	if err := printIngestStatus(w, res.Status); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nResolved reference:\n")
	fmt.Fprintf(w, "  digest:      %s\n", res.Reference.Digest)
	fmt.Fprintf(w, "  manifest:    %s\n", res.Reference.Manifest)
	fmt.Fprintf(w, "  recommended: %s\n", res.Reference.Recommended)
	fmt.Fprintf(w, "  durable:     %s\n", res.Reference.Modes.DurableMount)
	return nil
}

// runIngestLocal runs the worker logic directly in-process for a local
// file:// source root. This is the primary E2E-testable path.
func runIngestLocal(ctx context.Context, out io.Writer, name, version string,
	reg *dataset.Registry, rec dataset.Record,
	sourceRoot, destination string, dryRun bool, output string,
) error {
	srcDir := strings.TrimPrefix(sourceRoot, "file://")
	if srcDir == "" {
		return fmt.Errorf("--source-root file:// path must be non-empty")
	}
	srcDir = strings.TrimRight(srcDir, "/")

	destDir, err := parseLocalDestination(destination)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprintf(out, "dry-run: would ingest %s@%s\n", name, version)
		fmt.Fprintf(out, "  source:      file://%s\n", srcDir)
		fmt.Fprintf(out, "  destination: file://%s\n", destDir)
		fmt.Fprintf(out, "  files: %d  total_bytes: %d\n", len(rec.Files), rec.TotalBytes)
		return nil
	}

	// EnsureRegister is idempotent; here the record is already loaded so we
	// just initialise the status if needed.
	_, _, err = reg.InitIngestStatus(ctx, rec)
	if err != nil {
		return fmt.Errorf("init ingest status: %w", err)
	}

	source := datasetingest.FileSource{Root: srcDir}
	sink := datasetingest.FileSink{Root: destDir}
	locker := datasetingest.FileLocker{Dir: destDir}

	result, err := datasetingest.RunWorker(ctx, name, version, datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    source,
		Sink:      sink,
		Locker:    locker,
		AttemptID: fmt.Sprintf("local-%d", time.Now().UnixNano()),
	})
	if err != nil {
		return err
	}

	ingestResult := buildDatasetIngestResult(rec, result.Status)
	if output == "json" {
		return writeJSON(out, ingestResult)
	}
	return printDatasetIngestResult(out, ingestResult)
}

// parseLocalDestination extracts the filesystem directory from a destination
// URI. Accepts "file:///path", "file://path", or a bare path. Rejects az://.
func parseLocalDestination(destination string) (string, error) {
	if strings.HasPrefix(destination, "az://") {
		return "", fmt.Errorf(
			"local ingest mode requires a file:// destination, got az://; " +
				"use --workspace for Azure destinations",
		)
	}
	dir := strings.TrimPrefix(destination, "file://")
	dir = strings.TrimRight(dir, "/")
	if dir == "" {
		return "", fmt.Errorf("--destination must be a non-empty file:// path for local mode")
	}
	return dir, nil
}

// runIngestWorkspace renders and applies a hardened batch/v1 Job in the
// TauWorkspace's target namespace that runs the ingest worker under the
// workspace's workload identity. The worker reads the registered record and
// writes ingest status itself, so no record ConfigMap is needed.
func runIngestWorkspace(
	ctx context.Context, out, errOut io.Writer,
	name, version string,
	sourceRoot, destination, workerImage, wsName,
	registry, kubeContext string,
	wait, dryRun bool, output string,
) error {
	// Fetch and validate the workspace identity (Ready, namespace, WI SA+client).
	ws, err := datasetFetchWorkspace(ctx, kubeContext, tauworkspace.PlatformNamespace, wsName)
	if err != nil {
		return fmt.Errorf("fetch workspace %q: %w", wsName, err)
	}
	ident, err := validateWorkspaceForJob(ws)
	if err != nil {
		return err
	}

	jobName := datasetRunJobName("tau-ds-ingest", name, version)
	command := []string{"tau", "data", "dataset", ingestWorkerCmdName, name + "@" + version}
	flagArgs := []string{
		"--registry", registry,
		"--source-root", sourceRoot,
	}
	if destination != "" {
		flagArgs = append(flagArgs, "--destination", destination)
	}
	manifest, err := renderDatasetWorkerJob(datasetWorkerJobSpec{
		JobName:               jobName,
		Namespace:             ident.Namespace,
		ServiceAcct:           ident.ServiceAccountName,
		Image:                 workerImage,
		DatasetName:           name,
		Version:               version,
		Command:               command,
		FlagArgs:              flagArgs,
		ActiveDeadlineSeconds: datasetIngestDeadline,
		Labels: map[string]string{
			"app":                     "tau-dataset",
			workloadmeta.LabelDataset: sanitizeLabelValue(name),
		},
	})
	if err != nil {
		return fmt.Errorf("render ingest Job: %w", err)
	}

	fmt.Fprintf(errOut, "workspace ingest: dataset %s@%s\n", name, version)
	fmt.Fprintf(errOut, "  workspace:   %s (namespace %s, sa %s)\n", wsName, ident.Namespace, ident.ServiceAccountName)
	fmt.Fprintf(errOut, "  worker:      %s\n", workerImage)
	fmt.Fprintf(errOut, "  source:      %s\n", sourceRoot)
	fmt.Fprintf(errOut, "  destination: %s\n", destination)

	if dryRun {
		fmt.Fprintf(errOut, "dry-run: rendered Job (not applied):\n")
		writeManifests(out, manifest)
		return nil
	}

	runner := newDatasetKubeRunner(kubeContext)
	if _, err := applyManifest(ctx, runner, manifest, ""); err != nil {
		return fmt.Errorf("apply ingest Job: %w", err)
	}
	fmt.Fprintf(errOut, "applied Job %s/%s\n", ident.Namespace, jobName)

	if !wait {
		fmt.Fprintf(errOut, "not waiting; check status with `tau data dataset status %s@%s --workspace %s`\n", name, version, wsName)
		return nil
	}

	phase, err := waitForJob(ctx, runner, ident.Namespace, jobName, datasetIngestWait)
	if err != nil {
		return err
	}
	logs, logErr := jobLogs(ctx, runner, ident.Namespace, jobName)
	if phase == "Failed" {
		if logErr == nil && strings.TrimSpace(logs) != "" {
			fmt.Fprintf(errOut, "worker logs:\n%s\n", logs)
		}
		return fmt.Errorf("ingest Job %s/%s failed", ident.Namespace, jobName)
	}

	// Success: the worker emits the complete stable result, including its
	// workspace-identity-derived immutable reference. The caller does not read
	// the registry to reconstruct either field.
	result, err := parseWorkerOutput(logs)
	if err != nil {
		return fmt.Errorf("read ingest result from worker logs: %w", err)
	}
	if result.SchemaVersion != datasetIngestResultSchemaVersion ||
		result.Status.Name != name || result.Status.Version != version ||
		result.Status.State != dataset.IngestStateReady ||
		result.Reference.Digest == "" || result.Reference.Digest != result.Status.RecordDigest {
		return fmt.Errorf("worker returned an incomplete or non-ready ingest result")
	}
	if output == "json" {
		return writeJSON(out, result)
	}
	return printDatasetIngestResult(out, result)
}

func isFileIngestURI(uri string) bool {
	return strings.HasPrefix(uri, "file://") || !strings.Contains(uri, "://")
}

func resolveIngestDestination(rec dataset.Record, destination string) (string, error) {
	if destination == "" {
		if rec.Account == "" || rec.Container == "" {
			return "", fmt.Errorf(
				"dataset %s@%s has no account/container in its record; "+
					"use --destination az://<account>/<container>[/<prefix>]",
				rec.Name, rec.Version,
			)
		}
		destination = "az://" + rec.Account + "/" + rec.Container
		if rec.Prefix != "" {
			destination += "/" + rec.Prefix
		}
		return destination, nil
	}
	if !strings.HasPrefix(destination, "az://") {
		return destination, nil
	}
	account, container, prefix, err := parseAzURL(destination)
	if err != nil {
		return "", fmt.Errorf("--destination: %w", err)
	}
	if account != rec.Account || container != rec.Container || prefix != rec.Prefix {
		return "", fmt.Errorf(
			"--destination %q does not match immutable record location az://%s/%s/%s for %s@%s; the record is authoritative",
			destination, rec.Account, rec.Container, rec.Prefix, rec.Name, rec.Version,
		)
	}
	return destination, nil
}

func newDatasetStatusCmd() *cobra.Command {
	var rf registryFlags
	var output string
	var workspace, workerImage string
	var wait bool
	cmd := &cobra.Command{
		Use:   "status NAME@VERSION",
		Short: "Show the ingest status for a dataset version",
		Long: `Read and display the mutable ingest-status.json companion for a dataset
version. The status tracks whether bytes have been ingested (registered,
ingesting, ready, or failed) and provides per-file proofs of committed content.

By default the status is read directly using the caller's registry access.
With --workspace, the status is read from inside the cluster under the named
TauWorkspace's workload identity, so a researcher without direct Azure RBAC can
still inspect a project's status. Workspace mode requires a digest-pinned
--worker-image and an az:// --registry.

The exit code is non-zero when the state is 'failed'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			if ref.Version == "" {
				return fmt.Errorf("status requires NAME@VERSION")
			}

			var status dataset.IngestStatus
			if workspace != "" {
				status, err = runStatusWorkspace(cmd.Context(), cmd.ErrOrStderr(),
					ref.Name, ref.Version, rf, workspace, workerImage, wait)
				if err != nil {
					return err
				}
			} else {
				reg, err := rf.registryClient()
				if err != nil {
					return err
				}
				status, err = reg.GetIngestStatus(cmd.Context(), ref.Name, ref.Version)
				if err != nil {
					if dataset.IsNotExist(err) {
						return fmt.Errorf("no ingest status found for %s@%s (run `tau data dataset ingest` first)", ref.Name, ref.Version)
					}
					return err
				}
			}
			if output == "json" {
				if err := writeJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			} else {
				if err := printIngestStatus(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			}
			if status.State == dataset.IngestStateFailed {
				return fmt.Errorf("ingest state is 'failed': %s", status.FailureSummary)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().StringVar(&workspace, "workspace", "", "read status from inside the cluster under this TauWorkspace's workload identity")
	cmd.Flags().StringVar(&workerImage, "worker-image", "", "digest-pinned worker image (image@sha256:...) for --workspace mode")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the status Job to complete (workspace mode)")
	rf.bind(cmd, "pvc")
	return cmd
}

// runStatusWorkspace renders a hidden status-worker Job under the workspace
// workload identity, waits for it, and parses the IngestStatus from its logs.
func runStatusWorkspace(
	ctx context.Context, errOut io.Writer,
	name, version string, rf registryFlags,
	wsName, workerImage string, wait bool,
) (dataset.IngestStatus, error) {
	if workerImage == "" {
		return dataset.IngestStatus{}, fmt.Errorf("--worker-image is required for workspace status")
	}
	if err := validateDigestPinnedImage(workerImage); err != nil {
		return dataset.IngestStatus{}, err
	}
	if !strings.HasPrefix(rf.registry, "az://") {
		return dataset.IngestStatus{}, fmt.Errorf(
			"workspace status requires an az:// registry (--registry az://<account>/<container>)")
	}
	if !wait {
		return dataset.IngestStatus{}, fmt.Errorf(
			"workspace status requires --wait so the worker result can be read from its logs")
	}

	ws, err := datasetFetchWorkspace(ctx, rf.kubeContext, tauworkspace.PlatformNamespace, wsName)
	if err != nil {
		return dataset.IngestStatus{}, fmt.Errorf("fetch workspace %q: %w", wsName, err)
	}
	ident, err := validateWorkspaceForJob(ws)
	if err != nil {
		return dataset.IngestStatus{}, err
	}

	jobName := datasetRunJobName("tau-ds-status", name, version)
	manifest, err := renderDatasetWorkerJob(datasetWorkerJobSpec{
		JobName:     jobName,
		Namespace:   ident.Namespace,
		ServiceAcct: ident.ServiceAccountName,
		Image:       workerImage,
		DatasetName: name,
		Version:     version,
		Command:     []string{"tau", "data", "dataset", statusWorkerCmdName, name + "@" + version},
		FlagArgs:    []string{"--registry", rf.registry},
		Labels: map[string]string{
			"app":                     "tau-dataset",
			workloadmeta.LabelDataset: sanitizeLabelValue(name),
		},
	})
	if err != nil {
		return dataset.IngestStatus{}, fmt.Errorf("render status Job: %w", err)
	}

	runner := newDatasetKubeRunner(rf.kubeContext)
	if _, err := applyManifest(ctx, runner, manifest, ""); err != nil {
		return dataset.IngestStatus{}, fmt.Errorf("apply status Job: %w", err)
	}
	fmt.Fprintf(errOut, "applied Job %s/%s\n", ident.Namespace, jobName)

	phase, err := waitForJob(ctx, runner, ident.Namespace, jobName, datasetJobDefaultWait)
	if err != nil {
		return dataset.IngestStatus{}, err
	}
	logs, logErr := jobLogs(ctx, runner, ident.Namespace, jobName)
	if phase == "Failed" {
		if logErr == nil && strings.TrimSpace(logs) != "" {
			fmt.Fprintf(errOut, "worker logs:\n%s\n", logs)
		}
		return dataset.IngestStatus{}, fmt.Errorf("status Job %s/%s failed", ident.Namespace, jobName)
	}
	raw, err := extractJSONObject(logs)
	if err != nil {
		return dataset.IngestStatus{}, fmt.Errorf("read status from worker logs: %w", err)
	}
	status, err := dataset.ParseIngestStatus(raw)
	if err != nil {
		return dataset.IngestStatus{}, fmt.Errorf("parse status: %w", err)
	}
	return status, nil
}

func printIngestStatus(w io.Writer, s dataset.IngestStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", s.Name)
	fmt.Fprintf(tw, "Version:\t%s\n", s.Version)
	fmt.Fprintf(tw, "State:\t%s\n", s.State)
	fmt.Fprintf(tw, "RecordDigest:\t%s\n", s.RecordDigest)
	fmt.Fprintf(tw, "VerifiedFiles:\t%d\n", s.VerifiedFiles)
	fmt.Fprintf(tw, "VerifiedBytes:\t%d\n", s.VerifiedBytes)
	if s.AttemptID != "" {
		fmt.Fprintf(tw, "AttemptID:\t%s\n", s.AttemptID)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(tw, "StartedAt:\t%s\n", s.StartedAt)
	}
	if s.UpdatedAt != "" {
		fmt.Fprintf(tw, "UpdatedAt:\t%s\n", s.UpdatedAt)
	}
	if s.FailureSummary != "" {
		fmt.Fprintf(tw, "FailureSummary:\t%s\n", s.FailureSummary)
	}
	return tw.Flush()
}

// validateDigestPinnedImage rejects a worker image that is not pinned by an
// immutable digest. A moved tag is a supply-chain hole for a privileged copy
// Job, so mutable references are refused.
func validateDigestPinnedImage(image string) error {
	if !strings.Contains(image, "@sha256:") {
		return fmt.Errorf(
			"--worker-image %q must be digest-pinned (image@sha256:<hash>) — "+
				"mutable tags are rejected for supply-chain safety",
			image,
		)
	}
	return nil
}
