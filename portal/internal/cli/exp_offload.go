// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/expimport"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/jsonlutil"
)

const (
	metricsOffloadRunEnv                 = "TAU_METRICS_OFFLOAD_RUN"
	metricsOffloadProjectEnv             = "TAU_METRICS_OFFLOAD_PROJECT"
	metricsOffloadExperimentEnv          = "TAU_METRICS_OFFLOAD_EXPERIMENT"
	metricsOffloadGroupEnv               = "TAU_METRICS_OFFLOAD_GROUP"
	metricsOffloadSourceEnv              = "TAU_METRICS_OFFLOAD_SOURCE"
	metricsOffloadTagsEnv                = "TAU_METRICS_OFFLOAD_TAGS"
	metricsOffloadOutEnv                 = "TAU_METRICS_OFFLOAD_OUT"
	metricsOffloadCompletionFileEnv      = "TAU_METRICS_OFFLOAD_COMPLETION_FILE"
	metricsOffloadArtifactURIEnv         = "TAU_METRICS_OFFLOAD_ARTIFACT_URI"
	metricsOffloadCheckpointURIEnv       = "TAU_METRICS_OFFLOAD_CHECKPOINT_URI"
	metricsOffloadIntervalEnv            = "TAU_METRICS_OFFLOAD_INTERVAL"
	metricsOffloadRemoteWriteEndpointEnv = "TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT"
)

func newExpOffloadCmd(storePath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "offload",
		Short: "Prepare offline experiment data for downstream ingestion",
	}
	cmd.AddCommand(newExpOffloadMetricsCmd(storePath))
	cmd.AddCommand(newExpOffloadMetricsAgentCmd())
	cmd.AddCommand(newExpOffloadArtifactsCmd(storePath))
	cmd.AddCommand(newExpOffloadArtifactsAgentCmd())
	return cmd
}

func newExpOffloadMetricsCmd(storePath *string) *cobra.Command {
	var output string
	var jsonOutput bool
	var opts metricsOffloadOptions
	var watch bool
	var interval time.Duration
	var maxIterations int
	var remoteWriteEndpoint string
	var remoteWriteBatchSize int
	var remoteWriteMaxAttempts int
	var remoteWriteRetryBackoff time.Duration
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Project Tau expstore metrics and optionally remote-write them to adx-mon",
		Long: `Project Tau expstore metrics to TauExpMetrics.jsonl.

Without --history, this is the offline/recovery path: the full local expstore
metric projection is written to --out and can be replayed to adx-mon.

With --history, this is the online sidecar path: Tau tails complete appended
JSONL lines, imports only new chunks into expstore, then remote-writes only the
newly-created metric chunks.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyExperimentDefaults(cmd, &opts.RunID, &opts.Project, &opts.ExperimentID, &opts.RunGroupID); err != nil {
				return err
			}
			if watch {
				if err := applyMetricsOffloadWatchEnvDefaults(cmd, &opts, &interval, &remoteWriteEndpoint); err != nil {
					return err
				}
				if strings.TrimSpace(opts.CompletionFile) != "" && opts.ShutdownCompletionWait == 0 {
					opts.ShutdownCompletionWait = 2 * time.Minute
				}
			}
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			store, err := openMetricsOffloadStore(cmd.Context(), storePath, opts)
			if err != nil {
				return err
			}
			defer store.Close()
			opts.RemoteWrite = remoteWriteConfig{
				Endpoint:     os.ExpandEnv(remoteWriteEndpoint),
				BatchSize:    remoteWriteBatchSize,
				MaxAttempts:  remoteWriteMaxAttempts,
				RetryBackoff: remoteWriteRetryBackoff,
			}
			if watch {
				if err := prepareMetricsOffloadWatch(opts); err != nil {
					return err
				}
				shutdown, stopShutdown := newMetricsOffloadSignalShutdown()
				defer stopShutdown()
				results, err := runMetricsOffloadWatch(cmd.Context(), store, opts, interval, maxIterations, shutdown)
				if err != nil {
					return err
				}
				if err := publishMetricsOffloadDone(opts.DoneFile, results); err != nil {
					return err
				}
				if out == "json" {
					return writeExpJSON(cmd.OutOrStdout(), results)
				}
				return writeMetricsOffloadWatchTable(cmd.OutOrStdout(), results)
			}
			result, err := runMetricsOffloadOnce(cmd.Context(), store, opts)
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeMetricsOffloadTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&opts.Out, "out", "", "spool/checkpoint output directory for TauExpMetrics.jsonl (required)")
	cmd.Flags().StringArrayVar(&opts.History, "history", nil, "online JSONL history path or glob to import before offloading (repeatable)")
	cmd.Flags().StringVar(&opts.RunID, "run", "", "run name for online JSONL imports (default: tau.yaml name/run.name; required with --history when no config name)")
	cmd.Flags().StringVar(&opts.Project, "project", "", "project id for online JSONL imports (default: store manifest project)")
	cmd.Flags().StringVar(&opts.ExperimentID, "experiment", "", "experiment id for online JSONL imports (default: tau.yaml experiment.name)")
	cmd.Flags().StringVar(&opts.RunGroupID, "group", "", "run group id for online JSONL imports (default: default)")
	cmd.Flags().StringVar(&opts.Source, "source", "stellar-online", "metric source label for online JSONL imports")
	cmd.Flags().StringArrayVar(&opts.Tags, "tag", nil, "run discovery tag key=value stamped into imported metrics (repeatable)")
	cmd.Flags().StringVar(&opts.CompletionFile, "completion-file", "", "sentinel file; when present, watch mode performs one final drain and exits")
	cmd.Flags().BoolVar(&opts.BaselineExistingHistory, "baseline-existing-history", false, "checkpoint existing history at its current end before watch mode starts")
	cmd.Flags().StringVar(&opts.ReadyFile, "ready-file", "", "atomically publish sidecar readiness after the history baseline is durable")
	cmd.Flags().StringVar(&opts.DoneFile, "done-file", "", "atomically confirm that terminal metrics and status were published successfully")
	cmd.Flags().StringVar(&opts.ArtifactURI, "status-artifact-uri", "", "authoritative artifact reference added to the terminal status marker")
	cmd.Flags().StringVar(&opts.CheckpointURI, "status-checkpoint-uri", "", "authoritative checkpoint reference added to the terminal status marker")
	cmd.Flags().BoolVar(&watch, "watch", false, "poll for new metrics until interrupted, --max-iterations, or --completion-file")
	cmd.Flags().DurationVar(&interval, "interval", 1*time.Minute, "poll interval when --watch is set")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum watch iterations; 0 runs until interrupted or completion sentinel")
	cmd.Flags().StringVar(&remoteWriteEndpoint, "remote-write-endpoint", "", "adx-mon/Prometheus remote-write endpoint to replay TauExpMetrics batches to")
	cmd.Flags().IntVar(&remoteWriteBatchSize, "remote-write-batch-size", 5000, "samples per remote-write request")
	cmd.Flags().IntVar(&remoteWriteMaxAttempts, "remote-write-max-attempts", 3, "max attempts per remote-write request; 0 uses the built-in default")
	cmd.Flags().DurationVar(&remoteWriteRetryBackoff, "remote-write-retry-backoff", time.Second, "initial backoff between retryable remote-write failures")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func publishMetricsOffloadDone(path string, results []metricsOffloadResult) error {
	path = strings.TrimSpace(path)
	if path == "" || len(results) == 0 {
		return nil
	}
	last := results[len(results)-1]
	if !last.Completed && strings.TrimSpace(last.StatusState) == "" {
		return nil
	}
	if err := fileutil.WriteFileAtomic(path, []byte("done\n"), 0o644); err != nil {
		return fmt.Errorf("publish metrics offload completion: %w", err)
	}
	return nil
}

func prepareMetricsOffloadWatch(opts metricsOffloadOptions) error {
	if !opts.BaselineExistingHistory && strings.TrimSpace(opts.ReadyFile) == "" {
		return nil
	}
	if opts.BaselineExistingHistory {
		if strings.TrimSpace(opts.Out) == "" {
			return fmt.Errorf("--out is required with --baseline-existing-history")
		}
		if len(opts.History) == 0 {
			return fmt.Errorf("--history is required with --baseline-existing-history")
		}
		if strings.TrimSpace(opts.ReadyFile) == "" {
			return fmt.Errorf("--ready-file is required with --baseline-existing-history")
		}
		checkpointFile := filepath.Join(strings.TrimSpace(opts.Out), "metrics_jsonl_checkpoint.json")
		if _, err := jsonlutil.InitializeFileCheckpointSetAtEnd(
			checkpointFile,
			metricsJSONLCheckpointSchemaVersion,
			opts.History,
		); err != nil {
			return err
		}
	}
	if readyFile := strings.TrimSpace(opts.ReadyFile); readyFile != "" {
		if err := fileutil.WriteFileAtomic(readyFile, []byte("ready\n"), 0o644); err != nil {
			return fmt.Errorf("publish metrics offload readiness: %w", err)
		}
	}
	return nil
}

func applyMetricsOffloadWatchEnvDefaults(cmd *cobra.Command, opts *metricsOffloadOptions, interval *time.Duration, remoteWriteEndpoint *string) error {
	if len(opts.History) == 0 && !cmd.Flags().Changed("history") {
		if value := strings.TrimSpace(os.Getenv("TAU_METRICS_HISTORY")); value != "" {
			opts.History = []string{value}
		}
	}
	applyStringEnvDefault(cmd, "run", &opts.RunID, metricsOffloadRunEnv)
	applyStringEnvDefault(cmd, "project", &opts.Project, metricsOffloadProjectEnv)
	applyStringEnvDefault(cmd, "experiment", &opts.ExperimentID, metricsOffloadExperimentEnv)
	applyStringEnvDefault(cmd, "group", &opts.RunGroupID, metricsOffloadGroupEnv)
	applyStringEnvDefault(cmd, "source", &opts.Source, metricsOffloadSourceEnv)
	applyStringArrayEnvDefault(cmd, "tag", &opts.Tags, metricsOffloadTagsEnv)
	applyStringEnvDefault(cmd, "out", &opts.Out, metricsOffloadOutEnv)
	applyStringEnvDefault(cmd, "completion-file", &opts.CompletionFile, metricsOffloadCompletionFileEnv)
	applyStringEnvDefault(cmd, "status-artifact-uri", &opts.ArtifactURI, metricsOffloadArtifactURIEnv)
	applyStringEnvDefault(cmd, "status-checkpoint-uri", &opts.CheckpointURI, metricsOffloadCheckpointURIEnv)
	applyStringEnvDefault(cmd, "remote-write-endpoint", remoteWriteEndpoint, metricsOffloadRemoteWriteEndpointEnv)
	if !cmd.Flags().Changed("interval") {
		if value := strings.TrimSpace(os.Getenv(metricsOffloadIntervalEnv)); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("%s: %w", metricsOffloadIntervalEnv, err)
			}
			*interval = parsed
		}
	}
	return nil
}

func applyStringEnvDefault(cmd *cobra.Command, flag string, target *string, env string) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		*target = value
	}
}

func applyStringArrayEnvDefault(cmd *cobra.Command, flag string, target *[]string, env string) {
	if cmd.Flags().Changed(flag) {
		return
	}
	values := parseEnvTagList(os.Getenv(env))
	if len(values) > 0 {
		*target = values
	}
}

func parseEnvTagList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "{") {
		var tags map[string]string
		if err := json.Unmarshal([]byte(value), &tags); err == nil {
			keys := make([]string, 0, len(tags))
			for key := range tags {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			out := make([]string, 0, len(keys))
			for _, key := range keys {
				out = append(out, key+"="+tags[key])
			}
			return out
		}
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func openMetricsOffloadStore(ctx context.Context, storePath *string, opts metricsOffloadOptions) (*expstore.Store, error) {
	if len(opts.History) == 0 {
		return openExpStore(ctx, storePath)
	}
	experimentID := metricsOffloadExperimentID(opts)
	if experimentID == "" {
		return nil, fmt.Errorf("--experiment is required with --history")
	}
	store, _, err := expstore.Init(ctx, storePathValue(storePath), expstore.InitOptions{
		Name:    experimentID,
		Project: opts.Project,
		Group:   firstNonEmpty(opts.RunGroupID, "default"),
	})
	return store, err
}

func metricsOffloadExperimentID(opts metricsOffloadOptions) string {
	return firstNonEmpty(opts.ExperimentID, opts.RunGroupID)
}

func newMetricsOffloadSignalShutdown() (<-chan metricsCompletionStatus, func()) {
	signals := make(chan os.Signal, 1)
	shutdown := make(chan metricsCompletionStatus, 1)
	done := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-signals:
			signal.Stop(signals)
			shutdown <- metricsSidecarShutdownStatus(sig.String())
		case <-done:
		}
	}()
	return shutdown, func() {
		signal.Stop(signals)
		close(done)
	}
}

func metricsSidecarShutdownStatus(signalName string) metricsCompletionStatus {
	signalName = strings.TrimSpace(signalName)
	if signalName == "" {
		signalName = "termination signal"
	}
	return metricsCompletionStatus{
		State:   "cancelled",
		Reason:  "sidecar-shutdown",
		Message: fmt.Sprintf("metrics sidecar received %s before the completion sentinel appeared", signalName),
	}
}

func runMetricsOffloadWatch(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions, interval time.Duration, maxIterations int, shutdown <-chan metricsCompletionStatus) ([]metricsOffloadResult, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("--interval must be positive")
	}
	if maxIterations < 0 {
		return nil, fmt.Errorf("--max-iterations must be non-negative")
	}
	var results []metricsOffloadResult
	for iteration := 0; ; iteration++ {
		select {
		case status := <-shutdown:
			result, err := runMetricsOffloadShutdown(ctx, store, opts, status)
			if err != nil {
				return nil, err
			}
			return append(results, result), nil
		default:
		}
		completed, err := metricsCompletionExists(opts.CompletionFile)
		if err != nil {
			return nil, err
		}
		runOpts := opts
		runOpts.FinalizeHistory = completed
		result, err := runMetricsOffloadOnce(ctx, store, runOpts)
		if err != nil {
			return nil, err
		}
		result.Completed = completed
		if completed {
			status, err := emitMetricsCompletionStatus(ctx, store, opts, result)
			if err != nil {
				return nil, err
			}
			result.addStatusMarker(status)
		}
		results = append(results, result)
		if completed || (maxIterations > 0 && iteration+1 >= maxIterations) {
			return results, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return results, ctx.Err()
		case status := <-shutdown:
			timer.Stop()
			result, err := runMetricsOffloadShutdown(ctx, store, opts, status)
			if err != nil {
				return nil, err
			}
			return append(results, result), nil
		case <-timer.C:
		}
	}
}

func runMetricsOffloadShutdown(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions, status metricsCompletionStatus) (metricsOffloadResult, error) {
	completed, err := waitForMetricsCompletion(ctx, opts.CompletionFile, opts.ShutdownCompletionWait)
	if err != nil {
		return metricsOffloadResult{}, err
	}

	opts.FinalizeHistory = completed
	result, err := runMetricsOffloadOnce(ctx, store, opts)
	if err != nil {
		return metricsOffloadResult{}, err
	}
	result.Completed = completed
	var statusResult metricsStatusMarkerResult
	if completed {
		statusResult, err = emitMetricsCompletionStatus(ctx, store, opts, result)
	} else {
		statusResult, err = emitMetricsRunStatus(ctx, store, opts, status, result)
	}
	if err != nil {
		return metricsOffloadResult{}, err
	}
	result.addStatusMarker(statusResult)
	return result, nil
}

func waitForMetricsCompletion(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	if timeout <= 0 || strings.TrimSpace(path) == "" {
		return metricsCompletionExists(path)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		completed, err := metricsCompletionExists(path)
		if err != nil || completed {
			return completed, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

func runMetricsOffloadOnce(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions) (metricsOffloadResult, error) {
	if err := opts.RemoteWrite.validate(); err != nil {
		return metricsOffloadResult{}, err
	}
	if len(opts.History) > 0 {
		return runMetricsOnlineOffloadOnce(ctx, store, opts)
	}
	return runMetricsFullOffloadOnce(ctx, store, opts)
}

func runMetricsFullOffloadOnce(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions) (metricsOffloadResult, error) {
	out := strings.TrimSpace(opts.Out)
	if out == "" {
		return metricsOffloadResult{}, fmt.Errorf("--out is required")
	}
	checkpointFile := filepath.Join(out, "metrics_offload_checkpoint.json")
	exportedAt, err := metricsOffloadExportedAt(checkpointFile)
	if err != nil {
		return metricsOffloadResult{}, err
	}
	export, err := store.ExportADXMetrics(ctx, expstore.ADXMetricsExportOptions{
		Out:        out,
		Format:     "jsonl",
		ExportedAt: exportedAt,
	})
	if err != nil {
		return metricsOffloadResult{}, err
	}
	checkpoint := metricsOffloadCheckpoint{
		SchemaVersion: "tau.metrics.offload.v1",
		SourceStoreID: export.SourceStoreID,
		ExportedAt:    export.ExportedAt,
		MetricsFile:   export.MetricsFile,
		Rows:          export.Rows,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := fileutil.WriteJSONFileAtomic(checkpointFile, checkpoint); err != nil {
		return metricsOffloadResult{}, err
	}
	result := metricsOffloadResult{
		Mode:           "offline",
		Rows:           export.Rows,
		MetricsFile:    export.MetricsFile,
		CheckpointFile: checkpointFile,
		SourceStoreID:  export.SourceStoreID,
		ExportedAt:     export.ExportedAt,
	}
	if strings.TrimSpace(opts.RemoteWrite.Endpoint) != "" {
		remote, err := expimport.ReplayMetricsRemoteWrite(ctx, expimport.RemoteWriteOptions{
			MetricsFile:     export.MetricsFile,
			Endpoint:        opts.RemoteWrite.Endpoint,
			BatchSize:       opts.RemoteWrite.BatchSize,
			MaxAttempts:     opts.RemoteWrite.MaxAttempts,
			RetryBackoff:    opts.RemoteWrite.RetryBackoff,
			CheckpointFile:  filepath.Join(out, "metrics_remote_write_checkpoint.json"),
			SkipIfCompleted: true,
		})
		if err != nil {
			return metricsOffloadResult{}, err
		}
		result.RemoteWrite = &remote
		result.RemoteWriteSamples = remote.Samples
	}
	return result, nil
}

func runMetricsOnlineOffloadOnce(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions) (metricsOffloadResult, error) {
	out := strings.TrimSpace(opts.Out)
	if out == "" {
		return metricsOffloadResult{}, fmt.Errorf("--out is required")
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return metricsOffloadResult{}, fmt.Errorf("--run is required with --history")
	}
	paths, err := jsonlutil.ExpandInputs(opts.History)
	if err != nil {
		return metricsOffloadResult{}, err
	}
	checkpointFile := filepath.Join(out, "metrics_jsonl_checkpoint.json")
	checkpoint, err := jsonlutil.ReadFileCheckpointSet(checkpointFile, metricsJSONLCheckpointSchemaVersion)
	if err != nil {
		return metricsOffloadResult{}, err
	}
	result := metricsOffloadResult{
		Mode:                 "online",
		OnlineCheckpointFile: checkpointFile,
	}
	initialCheckpoints := make(map[string]jsonlutil.FileCheckpoint, len(checkpoint.Files))
	for path, fileCheckpoint := range checkpoint.Files {
		initialCheckpoints[path] = fileCheckpoint
	}
	for _, path := range paths {
		fileCheckpoint, _, err := jsonlutil.ResolveFileCheckpoint(path, initialCheckpoints)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return metricsOffloadResult{}, err
		}
		var chunk metricsHistoryChunk
		if opts.FinalizeHistory {
			chunk, err = jsonlutil.ReadFinalHistoryChunk(path, fileCheckpoint)
		} else {
			chunk, err = jsonlutil.ReadHistoryChunk(path, fileCheckpoint)
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return metricsOffloadResult{}, err
		}
		if fileCheckpoint.Offset > 0 && chunk.StartOffset == 0 {
			checkpoint.Files[retainedMetricsCheckpointKey(fileCheckpoint)] = fileCheckpoint
		}
		checkpoint.Files[path] = jsonlutil.CheckpointForChunk(chunk)
		if !jsonlutil.HasJSONL(chunk) {
			continue
		}
		chunkResult, err := processMetricsHistoryChunk(ctx, store, out, opts, chunk)
		if err != nil {
			return metricsOffloadResult{}, err
		}
		result.addOnlineChunk(chunkResult)
	}
	if err := jsonlutil.WriteFileCheckpointSet(checkpointFile, metricsJSONLCheckpointSchemaVersion, checkpoint); err != nil {
		return metricsOffloadResult{}, err
	}
	return result, nil
}

func retainedMetricsCheckpointKey(checkpoint jsonlutil.FileCheckpoint) string {
	return fmt.Sprintf(
		"tau://retained-history/%d/%d/%d/%s",
		checkpoint.Device,
		checkpoint.Inode,
		checkpoint.Offset,
		checkpoint.PrefixSHA256,
	)
}

func processMetricsHistoryChunk(ctx context.Context, store *expstore.Store, out string, opts metricsOffloadOptions, chunk metricsHistoryChunk) (metricsOnlineChunkResult, error) {
	if !jsonlutil.HasJSONL(chunk) {
		return metricsOnlineChunkResult{}, nil
	}
	if err := validateOnlineMetricsChunk(chunk); err != nil {
		return metricsOnlineChunkResult{}, err
	}
	chunkPath, chunkKey, err := jsonlutil.WriteHistoryChunk(filepath.Join(out, "metrics-jsonl-chunks"), opts.RunID, chunk)
	if err != nil {
		return metricsOnlineChunkResult{}, err
	}
	tags, err := parseExpTags(opts.Tags)
	if err != nil {
		return metricsOnlineChunkResult{}, err
	}
	importResult, err := expimport.ImportJSONL(ctx, store, expimport.JSONLImportOptions{
		RunID:          opts.RunID,
		Project:        opts.Project,
		ExperimentID:   metricsOffloadExperimentID(opts),
		RunGroupID:     opts.RunGroupID,
		History:        []string{chunkPath},
		Source:         opts.Source,
		Tags:           tags,
		IdempotencyKey: "metrics-jsonl-" + opts.RunID + "-" + chunkKey,
		SkipArtifacts:  true,
	})
	if err != nil {
		if errors.Is(err, expimport.ErrNoJSONLScalarMetrics) {
			return metricsOnlineChunkResult{}, nil
		}
		return metricsOnlineChunkResult{}, err
	}
	result := metricsOnlineChunkResult{
		Imported:   true,
		ImportRows: importResult.Rows,
	}
	if importResult.MetricFile == nil {
		return result, nil
	}
	metricFileID := importResult.MetricFile.FileID
	result.ImportedMetricFileID = metricFileID
	export, err := store.ExportADXMetrics(ctx, expstore.ADXMetricsExportOptions{
		Out:           filepath.Join(out, "metrics-chunks"),
		Format:        "jsonl",
		FileName:      "TauExpMetrics-" + fileutil.SafePathComponent(metricFileID) + ".jsonl",
		ExportedAt:    chunk.ExportedAt,
		MetricFileIDs: []string{metricFileID},
	})
	if err != nil {
		return metricsOnlineChunkResult{}, err
	}
	result.ExportRows = export.Rows
	result.MetricsFile = export.MetricsFile
	result.SourceStoreID = export.SourceStoreID
	result.ExportedAt = export.ExportedAt
	if strings.TrimSpace(opts.RemoteWrite.Endpoint) == "" {
		return result, nil
	}
	remote, err := expimport.ReplayMetricsRemoteWrite(ctx, expimport.RemoteWriteOptions{
		MetricsFile:     export.MetricsFile,
		Endpoint:        opts.RemoteWrite.Endpoint,
		BatchSize:       opts.RemoteWrite.BatchSize,
		MaxAttempts:     opts.RemoteWrite.MaxAttempts,
		RetryBackoff:    opts.RemoteWrite.RetryBackoff,
		CheckpointFile:  filepath.Join(out, "metrics-chunks", fileutil.SafePathComponent(metricFileID)+"_remote_write_checkpoint.json"),
		SkipIfCompleted: true,
	})
	if err != nil {
		return metricsOnlineChunkResult{}, err
	}
	result.RemoteWrite = &remote
	return result, nil
}

func validateOnlineMetricsChunk(chunk metricsHistoryChunk) error {
	for index, raw := range bytes.Split(chunk.Data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		line := index + 1
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var payload map[string]any
		if err := dec.Decode(&payload); err != nil {
			return fmt.Errorf("direct online metrics history %s line %d: invalid JSON: %w", chunk.Path, line, err)
		}
		if err := requireOnlineMetricStep(payload); err != nil {
			return fmt.Errorf("direct online metrics history %s line %d: %w", chunk.Path, line, err)
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("direct online metrics history %s line %d: row must contain exactly one JSON object", chunk.Path, line)
		}
		timestamp, err := requiredOnlineMetricNumber(payload, "_timestamp")
		if err != nil {
			return fmt.Errorf("direct online metrics history %s line %d: %w", chunk.Path, line, err)
		}
		micros := timestamp * 1_000_000
		if timestamp <= 0 || math.IsInf(micros, 0) || micros >= math.Exp2(63) {
			return fmt.Errorf("direct online metrics history %s line %d: _timestamp must be a valid positive Unix epoch-seconds value", chunk.Path, line)
		}
		wallTime := time.UnixMicro(int64(micros)).UTC()
		if wallTime.Year() < 1 || wallTime.Year() > 9999 {
			return fmt.Errorf("direct online metrics history %s line %d: _timestamp must be representable as an RFC3339 wall time", chunk.Path, line)
		}
		for key, value := range payload {
			if strings.HasPrefix(key, "_") {
				continue
			}
			number, ok := value.(json.Number)
			if !ok {
				continue
			}
			parsed, err := number.Float64()
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
				return fmt.Errorf("direct online metrics history %s line %d: metric %q must be a finite number", chunk.Path, line, key)
			}
		}
	}
	return nil
}

func requireOnlineMetricStep(payload map[string]any) error {
	value, ok := payload["_step"]
	if !ok {
		return fmt.Errorf("missing required numeric _step field")
	}
	number, ok := value.(json.Number)
	if !ok {
		return fmt.Errorf("_step must be numeric")
	}
	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return fmt.Errorf("_step must be an integer representable as int64")
	}
	return nil
}

func requiredOnlineMetricNumber(payload map[string]any, field string) (float64, error) {
	value, ok := payload[field]
	if !ok {
		return 0, fmt.Errorf("missing required numeric %s field", field)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be numeric", field)
	}
	parsed, err := number.Float64()
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s must be a finite number", field)
	}
	return parsed, nil
}

func metricsOffloadExportedAt(checkpointFile string) (time.Time, error) {
	raw, err := os.ReadFile(checkpointFile)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Now().UTC(), nil
		}
		return time.Time{}, err
	}
	var checkpoint metricsOffloadCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return time.Time{}, fmt.Errorf("read metrics offload checkpoint %s: %w", checkpointFile, err)
	}
	exportedAt, err := time.Parse(time.RFC3339Nano, checkpoint.ExportedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("read metrics offload checkpoint %s exported_at: %w", checkpointFile, err)
	}
	return exportedAt.UTC(), nil
}

func metricsCompletionExists(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func writeMetricsOffloadTable(w io.Writer, result metricsOffloadResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	remoteSamples := ""
	if result.RemoteWriteSamples > 0 {
		remoteSamples = fmt.Sprint(result.RemoteWriteSamples)
	}
	fmt.Fprintf(tw, "MODE\tROWS\tIMPORT_ROWS\tREMOTE_WRITE_SAMPLES\tMETRICS_FILE\tCHECKPOINT\n")
	fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n", result.Mode, result.Rows, result.ImportRows, remoteSamples, result.MetricsFile, resultCheckpoint(result))
	return tw.Flush()
}

func writeMetricsOffloadWatchTable(w io.Writer, results []metricsOffloadResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ITERATION\tMODE\tROWS\tIMPORT_ROWS\tREMOTE_WRITE_SAMPLES\tCOMPLETED\tSTATUS\tMETRICS_FILE\tSTATUS_METRICS_FILE\tCHECKPOINT\n")
	for i, result := range results {
		remoteSamples := ""
		if result.RemoteWriteSamples > 0 {
			remoteSamples = fmt.Sprint(result.RemoteWriteSamples)
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\t%t\t%s\t%s\t%s\t%s\n", i+1, result.Mode, result.Rows, result.ImportRows, remoteSamples, result.Completed, result.StatusState, result.MetricsFile, result.StatusMetricsFile, resultCheckpoint(result))
	}
	return tw.Flush()
}

func resultCheckpoint(result metricsOffloadResult) string {
	if result.CheckpointFile != "" {
		return result.CheckpointFile
	}
	return result.OnlineCheckpointFile
}

func newExpOffloadMetricsAgentCmd() *cobra.Command {
	var name, namespace, image, pvc, mountPath, store, history, run, out, project, experimentID, group, source, interval, completionFile, remoteWriteEndpoint, serviceAccount string
	var tags []string
	var cpuRequest, memoryRequest, cpuLimit, memoryLimit string
	var nodeSelectors []string
	var maxIterations int
	var remoteWriteMaxAttempts int
	var remoteWriteRetryBackoff string
	cmd := &cobra.Command{
		Use:   "metrics-agent",
		Short: "Render a Kubernetes worker that ingests live Tau metrics into expstore and adx-mon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if image == "" {
				return fmt.Errorf("--image is required: specify the container image containing the tau binary")
			}
			manifest, err := renderMetricsOffloadAgentManifest(metricsOffloadAgentManifestOptions{
				Name:                    name,
				Namespace:               namespace,
				Image:                   image,
				PVC:                     pvc,
				MountPath:               mountPath,
				Store:                   store,
				History:                 history,
				Run:                     run,
				Out:                     out,
				Project:                 project,
				Experiment:              experimentID,
				Group:                   group,
				Source:                  source,
				Tags:                    tags,
				Interval:                interval,
				MaxIterations:           maxIterations,
				CompletionFile:          completionFile,
				RemoteWriteEndpoint:     remoteWriteEndpoint,
				RemoteWriteMaxAttempts:  remoteWriteMaxAttempts,
				RemoteWriteRetryBackoff: remoteWriteRetryBackoff,
				ServiceAccount:          serviceAccount,
				CPURequest:              cpuRequest,
				MemoryRequest:           memoryRequest,
				CPULimit:                cpuLimit,
				MemoryLimit:             memoryLimit,
				NodeSelectors:           nodeSelectors,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), manifest)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "tau-metrics-agent", "Deployment name")
	cmd.Flags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace")
	cmd.Flags().StringVar(&image, "image", "", "container image containing the tau binary (required)")
	cmd.Flags().StringVar(&pvc, "pvc", "blob-training", "PVC name that contains live Tau/Stellar outputs")
	cmd.Flags().StringVar(&mountPath, "mount-path", "/data", "PVC mount path inside the metrics agent")
	cmd.Flags().StringVar(&store, "store", "/data/tau-exp", "Tau expstore path inside the mounted PVC")
	cmd.Flags().StringVar(&history, "history", "/data/outputs/*/history.jsonl", "live JSONL history file path/glob inside the mounted PVC")
	cmd.Flags().StringVar(&run, "run", "", "run id stamped into imported metrics (required)")
	cmd.Flags().StringVar(&out, "out", "/data/tau-metrics-offload", "spool/checkpoint output root inside the mounted PVC")
	cmd.Flags().StringVar(&project, "project", "", "project id stamped into imported metrics (required)")
	cmd.Flags().StringVar(&experimentID, "experiment", "", "experiment id stamped into imported metrics (required)")
	cmd.Flags().StringVar(&group, "group", "", "run group id stamped into imported metrics (required)")
	cmd.Flags().StringVar(&source, "source", "stellar-online", "metric source label stamped into imported metrics")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "run discovery tag key=value stamped into imported metrics (repeatable)")
	cmd.Flags().StringVar(&interval, "interval", "10s", "watch interval")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum watch iterations; 0 runs until completion sentinel or interruption")
	cmd.Flags().StringVar(&completionFile, "completion-file", "/data/tau-metrics.done", "sentinel file that triggers one final drain and exit")
	cmd.Flags().StringVar(&remoteWriteEndpoint, "remote-write-endpoint", "http://${NODE_IP}:3100/receive", "adx-mon remote-write endpoint")
	cmd.Flags().IntVar(&remoteWriteMaxAttempts, "remote-write-max-attempts", 3, "max attempts per remote-write request")
	cmd.Flags().StringVar(&remoteWriteRetryBackoff, "remote-write-retry-backoff", "1s", "initial backoff between retryable remote-write failures")
	cmd.Flags().StringVar(&serviceAccount, "service-account", "default", "service account for the metrics agent")
	cmd.Flags().StringVar(&cpuRequest, "cpu-request", "100m", "CPU request for the metrics agent container")
	cmd.Flags().StringVar(&memoryRequest, "memory-request", "256Mi", "memory request for the metrics agent container")
	cmd.Flags().StringVar(&cpuLimit, "cpu-limit", "1", "CPU limit for the metrics agent container")
	cmd.Flags().StringVar(&memoryLimit, "memory-limit", "1Gi", "memory limit for the metrics agent container")
	cmd.Flags().StringArrayVar(&nodeSelectors, "node-selector", nil, "node selector key=value for the metrics agent pod (repeatable)")
	return cmd
}

func renderMetricsOffloadAgentManifest(opts metricsOffloadAgentManifestOptions) (string, error) {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Namespace = strings.TrimSpace(opts.Namespace)
	opts.Image = strings.TrimSpace(opts.Image)
	opts.PVC = strings.TrimSpace(opts.PVC)
	opts.MountPath = strings.TrimSpace(opts.MountPath)
	opts.Store = strings.TrimSpace(opts.Store)
	opts.History = strings.TrimSpace(opts.History)
	opts.Run = strings.TrimSpace(opts.Run)
	opts.Out = strings.TrimSpace(opts.Out)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.Experiment = strings.TrimSpace(opts.Experiment)
	opts.Group = strings.TrimSpace(opts.Group)
	opts.Source = strings.TrimSpace(opts.Source)
	opts.ServiceAccount = strings.TrimSpace(opts.ServiceAccount)
	opts.CPURequest = strings.TrimSpace(opts.CPURequest)
	opts.MemoryRequest = strings.TrimSpace(opts.MemoryRequest)
	opts.CPULimit = strings.TrimSpace(opts.CPULimit)
	opts.MemoryLimit = strings.TrimSpace(opts.MemoryLimit)
	if opts.Name == "" || opts.Namespace == "" || opts.Image == "" || opts.PVC == "" || opts.MountPath == "" || opts.Store == "" || opts.History == "" || opts.Run == "" || opts.Out == "" || opts.Project == "" || opts.Experiment == "" || opts.Group == "" {
		return "", fmt.Errorf("--name, --namespace, --image, --pvc, --mount-path, --store, --history, --run, --out, --project, --experiment, and --group are required")
	}
	if opts.Interval == "" {
		opts.Interval = "10s"
	}
	if _, err := time.ParseDuration(opts.Interval); err != nil {
		return "", fmt.Errorf("--interval: %w", err)
	}
	if opts.MaxIterations < 0 {
		return "", fmt.Errorf("--max-iterations must be non-negative")
	}
	if opts.ServiceAccount == "" {
		opts.ServiceAccount = "default"
	}
	if opts.Source == "" {
		opts.Source = "stellar-online"
	}
	if opts.RemoteWriteMaxAttempts < 0 {
		return "", fmt.Errorf("--remote-write-max-attempts must be non-negative")
	}
	if strings.TrimSpace(opts.RemoteWriteRetryBackoff) == "" {
		opts.RemoteWriteRetryBackoff = "1s"
	}
	if _, err := time.ParseDuration(opts.RemoteWriteRetryBackoff); err != nil {
		return "", fmt.Errorf("--remote-write-retry-backoff: %w", err)
	}
	if opts.CPURequest == "" || opts.MemoryRequest == "" || opts.CPULimit == "" || opts.MemoryLimit == "" {
		return "", fmt.Errorf("--cpu-request, --memory-request, --cpu-limit, and --memory-limit are required")
	}
	nodeSelector, err := parseAgentKeyValues(opts.NodeSelectors, "--node-selector")
	if err != nil {
		return "", err
	}
	args := []string{
		"experiment", "--store", opts.Store,
		"offload", "metrics",
		"--watch",
		"--interval", opts.Interval,
		"--history", opts.History,
		"--run", opts.Run,
		"--project", opts.Project,
		"--experiment", opts.Experiment,
		"--group", opts.Group,
		"--source", opts.Source,
		"--out", opts.Out,
	}
	for _, tag := range opts.Tags {
		args = append(args, "--tag", tag)
	}
	if opts.MaxIterations > 0 {
		args = append(args, "--max-iterations", fmt.Sprint(opts.MaxIterations))
	}
	if strings.TrimSpace(opts.CompletionFile) != "" {
		args = append(args, "--completion-file", strings.TrimSpace(opts.CompletionFile))
	}
	if strings.TrimSpace(opts.RemoteWriteEndpoint) != "" {
		args = append(args, "--remote-write-endpoint", strings.TrimSpace(opts.RemoteWriteEndpoint))
		args = append(args, "--remote-write-max-attempts", fmt.Sprint(opts.RemoteWriteMaxAttempts))
		args = append(args, "--remote-write-retry-backoff", strings.TrimSpace(opts.RemoteWriteRetryBackoff))
	}
	return renderOffloadAgentDeploymentManifest(offloadAgentDeploymentManifestOptions{
		Name:           opts.Name,
		Namespace:      opts.Namespace,
		AppName:        "tau-metrics-agent",
		ServiceAccount: opts.ServiceAccount,
		ContainerName:  "metrics-agent",
		Image:          opts.Image,
		Args:           args,
		CPURequest:     opts.CPURequest,
		MemoryRequest:  opts.MemoryRequest,
		CPULimit:       opts.CPULimit,
		MemoryLimit:    opts.MemoryLimit,
		VolumeName:     "tau-metrics",
		MountPath:      opts.MountPath,
		PVC:            opts.PVC,
		NodeSelector:   nodeSelector,
	})
}

func parseAgentKeyValues(values []string, flag string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("%s must be key=value, got %q", flag, raw)
		}
		out[key] = value
	}
	return out, nil
}

type remoteWriteConfig struct {
	Endpoint     string
	BatchSize    int
	MaxAttempts  int
	RetryBackoff time.Duration
}

func (c remoteWriteConfig) validate() error {
	if c.MaxAttempts < 0 {
		return fmt.Errorf("--remote-write-max-attempts must be non-negative")
	}
	if c.RetryBackoff < 0 {
		return fmt.Errorf("--remote-write-retry-backoff must be non-negative")
	}
	return nil
}
