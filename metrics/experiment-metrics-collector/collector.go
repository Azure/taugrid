// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/fileutil"
)

type Options struct {
	Run                     string
	Project                 string
	Experiment              string
	Group                   string
	Source                  string
	Out                     string
	History                 []string
	Tags                    map[string]string
	CompletionFile          string
	Interval                time.Duration
	BaselineExistingHistory bool
	ReadyFile               string
	DoneFile                string
	StatusArtifactURI       string
	StatusCheckpointURI     string
	Watch                   bool
	MaxIterations           int
	Sinks                   []Sink
	Now                     func() time.Time
	fault                   func(faultPoint) error
}

type Result struct {
	Chunks       int  `json:"chunks"`
	Events       int  `json:"events"`
	ReceiptsUsed int  `json:"receipts_used"`
	Completed    bool `json:"completed"`
}

type Runner struct {
	options Options
}

func New(options Options) (*Runner, error) {
	for name, value := range map[string]string{
		"run": options.Run, "project": options.Project, "experiment": options.Experiment,
		"group": options.Group, "source": options.Source, "out": options.Out,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("--%s is required", name)
		}
	}
	if len(options.History) == 0 {
		return nil, fmt.Errorf("--history is required")
	}
	if options.Interval <= 0 {
		return nil, fmt.Errorf("--interval must be positive")
	}
	if options.MaxIterations < 0 {
		return nil, fmt.Errorf("--max-iterations must be nonnegative")
	}
	if options.BaselineExistingHistory && strings.TrimSpace(options.ReadyFile) == "" {
		return nil, fmt.Errorf("--ready-file is required with --baseline-existing-history")
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	sinkNames := map[string]bool{}
	receiptNamespaces := map[string]bool{}
	for _, sink := range options.Sinks {
		if sink == nil || strings.TrimSpace(sink.Name()) == "" {
			return nil, fmt.Errorf("sink name is required")
		}
		if strings.TrimSpace(sink.ConfigIdentity()) == "" {
			return nil, fmt.Errorf("sink %s has empty ConfigIdentity", sink.Name())
		}
		if sinkNames[sink.Name()] {
			return nil, fmt.Errorf("sink name %s is duplicated", sink.Name())
		}
		sinkNames[sink.Name()] = true
		configHash := sha256.Sum256([]byte(sink.ConfigIdentity()))
		namespace := fileutil.SafePathComponent(sink.Name()) + "/" + hex.EncodeToString(configHash[:])
		if receiptNamespaces[namespace] {
			return nil, fmt.Errorf("sink %s collides with another receipt namespace", sink.Name())
		}
		receiptNamespaces[namespace] = true
	}
	options.Tags = cloneTags(options.Tags)
	applyEnvironmentMetadata(&options)
	return &Runner{options: options}, nil
}

func (r *Runner) configIdentity() string {
	config := struct {
		Version             string            `json:"version"`
		Run                 string            `json:"run"`
		Project             string            `json:"project"`
		Experiment          string            `json:"experiment"`
		Group               string            `json:"group"`
		Source              string            `json:"source"`
		History             []string          `json:"history"`
		CompletionFile      string            `json:"completion_file"`
		Tags                map[string]string `json:"tags"`
		StatusArtifactURI   string            `json:"status_artifact_uri"`
		StatusCheckpointURI string            `json:"status_checkpoint_uri"`
	}{
		Version: ChunkManifestSchemaV1, Run: r.options.Run, Project: r.options.Project,
		Experiment: r.options.Experiment, Group: r.options.Group, Source: r.options.Source,
		History: append([]string(nil), r.options.History...), CompletionFile: r.options.CompletionFile,
		Tags: r.options.Tags, StatusArtifactURI: r.options.StatusArtifactURI,
		StatusCheckpointURI: r.options.StatusCheckpointURI,
	}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r *Runner) sourceStoreID() string {
	identity := struct {
		Schema      string `json:"schema"`
		WorkspaceID string `json:"workspace_id"`
		Cluster     string `json:"cluster"`
		Namespace   string `json:"namespace"`
		Out         string `json:"out"`
	}{
		Schema:      exptelemetry.MetricEventSchemaV1,
		WorkspaceID: r.options.Tags[exptelemetry.TauWorkspaceTag],
		Cluster:     r.options.Tags[exptelemetry.TauClusterTag],
		Namespace:   r.options.Tags[exptelemetry.TauNamespaceTag],
		Out:         filepath.Clean(r.options.Out),
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return "tau-metrics-" + hex.EncodeToString(sum[:])[:16]
}

func (r *Runner) replaySpool(
	ctx context.Context,
	manifests []chunkManifest,
	checkpoints *checkpointSet,
	checkpointPath string,
	result *Result,
) error {
	bySequence := make(map[uint64]chunkManifest, len(manifests))
	for _, manifest := range manifests {
		if _, exists := bySequence[manifest.Sequence]; exists {
			return fmt.Errorf("multiple spool manifests use sequence %d", manifest.Sequence)
		}
		bySequence[manifest.Sequence] = manifest
		events, err := decodeCanonicalEvents(manifest.NDJSON)
		if err != nil {
			return err
		}
		chunk := MetricEventChunk{
			SchemaVersion: ChunkSchemaV1,
			Digest:        manifest.ChunkDigest,
			Path:          filepath.Join(r.options.Out, "chunks", manifest.ChunkFile),
			SourcePath:    manifest.SourcePath,
			SourceFileID:  manifest.SourceFileID,
			StartOffset:   manifest.StartOffset,
			EndOffset:     manifest.EndOffset,
			Sequence:      manifest.Sequence,
			Events:        events,
			NDJSON:        manifest.NDJSON,
		}
		changed, err := advanceCheckpointForManifest(checkpoints, manifest)
		if err != nil {
			return err
		}
		if changed {
			if err := writeCheckpoints(checkpointPath, *checkpoints); err != nil {
				return err
			}
		}
		if err := r.deliverChunk(ctx, chunk, result); err != nil {
			return fmt.Errorf("replay durable chunk: %w", err)
		}
	}
	for source, checkpoint := range checkpoints.Sources {
		if checkpoint.Sequence == 0 {
			continue
		}
		manifest, ok := bySequence[checkpoint.Sequence]
		if !ok || manifest.Kind != "history" || manifest.SourcePath != source ||
			manifest.ChunkDigest != checkpoint.ChunkDigest ||
			manifest.EndOffset > checkpoint.Offset {
			return fmt.Errorf("source checkpoint %s does not match a durable manifest", source)
		}
	}
	if checkpoints.Terminal != nil {
		manifest, ok := bySequence[checkpoints.Terminal.Sequence]
		if !ok || manifest.Kind != "terminal" || manifest.ChunkDigest != checkpoints.Terminal.ChunkDigest {
			return fmt.Errorf("terminal checkpoint does not match a durable manifest")
		}
	}
	if checkpoints.NextSequence != uint64(len(manifests))+1 {
		return fmt.Errorf("checkpoint next sequence %d does not match %d durable manifests", checkpoints.NextSequence, len(manifests))
	}
	return nil
}

func advanceCheckpointForManifest(checkpoints *checkpointSet, manifest chunkManifest) (bool, error) {
	if checkpoints.NextSequence > manifest.Sequence {
		return false, nil
	}
	if checkpoints.NextSequence != manifest.Sequence {
		return false, fmt.Errorf("spool sequence gap: checkpoint expects %d, manifest is %d", checkpoints.NextSequence, manifest.Sequence)
	}
	switch manifest.Kind {
	case "history":
		checkpoint := checkpoints.Sources[manifest.SourcePath]
		if checkpoint.Path != "" &&
			(checkpoint.FileID != manifest.SourceFileID ||
				checkpoint.Offset != manifest.StartOffset ||
				checkpoint.Lines != manifest.StartLines ||
				checkpoint.PrefixSHA256 != manifest.StartPrefixSHA256) {
			return false, fmt.Errorf("durable manifest start does not match source checkpoint for %s", manifest.SourcePath)
		}
		if checkpoint.Path == "" && (manifest.StartOffset != 0 || manifest.StartLines != 0 || manifest.StartPrefixSHA256 != "") {
			return false, fmt.Errorf("durable manifest starts after missing source checkpoint for %s", manifest.SourcePath)
		}
		checkpoints.Sources[manifest.SourcePath] = SourceCheckpoint{
			Path: manifest.SourcePath, FileID: manifest.SourceFileID,
			Offset: manifest.EndOffset, PrefixSHA256: manifest.PrefixSHA256,
			Sequence: manifest.Sequence, ChunkDigest: manifest.ChunkDigest, Lines: manifest.EndLines,
		}
	case "terminal":
		if checkpoints.Terminal != nil {
			return false, fmt.Errorf("multiple terminal manifests")
		}
		checkpoints.Terminal = &terminalCheckpoint{Sequence: manifest.Sequence, ChunkDigest: manifest.ChunkDigest}
	default:
		return false, fmt.Errorf("unknown manifest kind %q", manifest.Kind)
	}
	checkpoints.NextSequence++
	return true, nil
}

func (r *Runner) Run(ctx context.Context) (Result, error) {
	var result Result
	if err := os.MkdirAll(r.options.Out, 0o755); err != nil {
		return result, err
	}
	checkpointPath := filepath.Join(r.options.Out, "checkpoint.json")
	checkpoints, existed, err := loadCheckpoints(checkpointPath)
	if err != nil {
		return result, err
	}
	manifests, err := loadSpool(r.options.Out, r.configIdentity(), r.options.Sinks)
	if err != nil {
		return result, err
	}
	if err := r.replaySpool(ctx, manifests, &checkpoints, checkpointPath, &result); err != nil {
		return result, err
	}
	files, err := expandHistory(r.options.History)
	if err != nil {
		return Result{}, err
	}
	if r.options.BaselineExistingHistory && !existed && len(manifests) == 0 {
		for _, path := range files {
			checkpoint, err := baselineSource(path, checkpoints.NextSequence-1)
			if err != nil {
				return Result{}, err
			}
			checkpoints.Sources[path] = checkpoint
		}
		if err := writeCheckpoints(checkpointPath, checkpoints); err != nil {
			return Result{}, err
		}
	}
	if r.options.ReadyFile != "" {
		if err := fileutil.WriteFileAtomic(r.options.ReadyFile, []byte("ready\n"), 0o644); err != nil {
			return Result{}, fmt.Errorf("publish ready file: %w", err)
		}
	}

	for iteration := 1; ; iteration++ {
		if err := r.drain(ctx, &checkpoints, checkpointPath, &result, false); err != nil {
			return result, err
		}
		completed, err := fileExists(r.options.CompletionFile)
		if err != nil {
			return result, err
		}
		if completed {
			if err := r.drain(ctx, &checkpoints, checkpointPath, &result, true); err != nil {
				return result, err
			}
			if err := r.publishStatus(ctx, &checkpoints, &result); err != nil {
				return result, err
			}
			result.Completed = true
			if r.options.DoneFile != "" {
				if err := fileutil.WriteFileAtomic(r.options.DoneFile, []byte("done\n"), 0o644); err != nil {
					return result, fmt.Errorf("publish done file: %w", err)
				}
			}
			return result, nil
		}
		if !r.options.Watch || (r.options.MaxIterations > 0 && iteration >= r.options.MaxIterations) {
			return result, nil
		}
		timer := time.NewTimer(r.options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runner) drain(ctx context.Context, checkpoints *checkpointSet, checkpointPath string, result *Result, includeTrailingRecords bool) error {
	files, err := expandHistory(r.options.History)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(files))
	for _, path := range files {
		present[path] = true
	}
	for path := range checkpoints.Sources {
		if !present[path] {
			return fmt.Errorf("checkpointed history source disappeared: %s", path)
		}
	}
	for _, path := range files {
		checkpoint := checkpoints.Sources[path]
		read, err := readSource(path, checkpoint, includeTrailingRecords)
		if err != nil {
			return err
		}
		if len(read.data) == 0 {
			continue
		}
		events, ndjson, err := r.project(read)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			lines := sourceLineCount(read.data)
			checkpoints.Sources[path] = SourceCheckpoint{
				Path: path, FileID: read.fileID, Offset: read.end, PrefixSHA256: read.prefix,
				Sequence: checkpoint.Sequence, ChunkDigest: checkpoint.ChunkDigest, Lines: checkpoint.Lines + lines,
			}
			if err := writeCheckpoints(checkpointPath, *checkpoints); err != nil {
				return err
			}
			continue
		}
		chunk, err := writeChunk(r.options.Out, MetricEventChunk{
			SourcePath: read.path, SourceFileID: read.fileID, StartOffset: read.start,
			EndOffset: read.end, Sequence: checkpoints.NextSequence, Events: events, NDJSON: ndjson,
		}, newHistoryManifest(r.configIdentity(), checkpoint, read, len(events)))
		if err != nil {
			return err
		}
		if err := r.inject(faultAfterChunkWrite); err != nil {
			return err
		}
		lines := sourceLineCount(read.data)
		checkpoints.Sources[path] = SourceCheckpoint{
			Path: path, FileID: read.fileID, Offset: read.end, PrefixSHA256: read.prefix,
			Sequence: checkpoints.NextSequence, ChunkDigest: chunk.Digest, Lines: checkpoint.Lines + lines,
		}
		checkpoints.NextSequence++
		if err := writeCheckpoints(checkpointPath, *checkpoints); err != nil {
			return err
		}
		if err := r.inject(faultAfterCheckpointWrite); err != nil {
			return err
		}
		if err := r.deliverChunk(ctx, chunk, result); err != nil {
			return err
		}
		result.Chunks++
		result.Events += len(events)
	}
	return nil
}

func (r *Runner) project(read sourceRead) ([]exptelemetry.MetricEvent, []byte, error) {
	var events []exptelemetry.MetricEvent
	var ndjson bytes.Buffer
	for index, line := range bytes.Split(read.data, []byte{'\n'}) {
		if len(line) == 0 && index == bytes.Count(read.data, []byte{'\n'}) {
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, nil, fmt.Errorf("%s line %d is empty", read.path, read.startLine+index)
		}
		var row map[string]any
		if err := decodeOneJSON(line, &row); err != nil {
			return nil, nil, fmt.Errorf("%s line %d: %w", read.path, read.startLine+index, err)
		}
		if row == nil {
			return nil, nil, fmt.Errorf("%s line %d must be a JSON object", read.path, read.startLine+index)
		}
		projected, err := exptelemetry.ProjectJSONLHistoryRow(row, exptelemetry.JSONLHistoryOptions{
			WorkspaceID:   r.options.Tags[exptelemetry.TauWorkspaceTag],
			Cluster:       r.options.Tags[exptelemetry.TauClusterTag],
			Namespace:     r.options.Tags[exptelemetry.TauNamespaceTag],
			SourceStoreID: r.sourceStoreID(),
			Project:       r.options.Project, ExperimentID: r.options.Experiment, RunGroupID: r.options.Group,
			RunID: r.options.Run, Source: r.options.Source, Tags: r.options.Tags,
			HistoryPath: read.path, HistoryLine: read.startLine + index,
			ExportedAt: r.options.Now(), MetricFileID: logicalMetricFileID(read.path), MetricFilePath: read.path,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("%s line %d: %w", read.path, read.startLine+index, err)
		}
		for _, event := range projected {
			raw, err := event.MarshalNDJSON()
			if err != nil {
				return nil, nil, err
			}
			events = append(events, event)
			ndjson.Write(raw)
		}
	}
	return events, ndjson.Bytes(), nil
}

func (r *Runner) publishStatus(ctx context.Context, checkpoints *checkpointSet, result *Result) error {
	if checkpoints.Terminal != nil {
		return nil
	}
	status, err := readCompletion(r.options.CompletionFile)
	if err != nil {
		return err
	}
	tags := cloneTags(r.options.Tags)
	tags[exptelemetry.RunStatusStateTag] = status.State
	if status.Reason != "" {
		tags[exptelemetry.RunStatusReasonTag] = status.Reason
	}
	if status.Message != "" {
		tags[exptelemetry.RunStatusMessageTag] = status.Message
	}
	if r.options.StatusArtifactURI != "" {
		tags[exptelemetry.RunStatusArtifactURITag] = r.options.StatusArtifactURI
	}
	if r.options.StatusCheckpointURI != "" {
		tags[exptelemetry.RunStatusCheckpointURITag] = r.options.StatusCheckpointURI
	}
	value := map[string]float64{"succeeded": 1, "failed": -1, "cancelled": -2}[status.State]
	event, err := exptelemetry.NewMetricEvent(exptelemetry.MetricEvent{
		WorkspaceID: tags[exptelemetry.TauWorkspaceTag], Cluster: tags[exptelemetry.TauClusterTag],
		Namespace: tags[exptelemetry.TauNamespaceTag], SourceStoreID: r.sourceStoreID(), Project: r.options.Project,
		ExperimentID: r.options.Experiment, RunGroupID: r.options.Group, RunID: r.options.Run,
		MetricName: exptelemetry.RunStatusMetricName, Step: 0, WallTime: status.CompletedAt,
		Value: value, Source: r.options.Source + "-status", Tags: tags, ExportedAt: status.CompletedAt,
	})
	if err != nil {
		return err
	}
	raw, err := event.MarshalNDJSON()
	if err != nil {
		return err
	}
	chunk, err := writeChunk(r.options.Out, MetricEventChunk{
		Sequence: checkpoints.NextSequence, Events: []exptelemetry.MetricEvent{event}, NDJSON: raw,
		SourcePath: r.options.CompletionFile, SourceFileID: "completion-status",
	}, newTerminalManifest(r.configIdentity(), checkpoints.NextSequence, r.options.CompletionFile, raw))
	if err != nil {
		return err
	}
	if err := r.inject(faultAfterTerminalChunkWrite); err != nil {
		return err
	}
	checkpoints.Terminal = &terminalCheckpoint{Sequence: chunk.Sequence, ChunkDigest: chunk.Digest}
	checkpoints.NextSequence++
	if err := writeCheckpoints(filepath.Join(r.options.Out, "checkpoint.json"), *checkpoints); err != nil {
		return err
	}
	if err := r.inject(faultAfterTerminalCheckpointWrite); err != nil {
		return err
	}
	if err := r.deliverChunk(ctx, chunk, result); err != nil {
		return fmt.Errorf("deliver terminal status: %w", err)
	}
	result.Chunks++
	result.Events++
	return nil
}

func (r *Runner) deliverChunk(ctx context.Context, chunk MetricEventChunk, result *Result) error {
	for _, sink := range r.options.Sinks {
		_, reused, err := deliverWithReceipt(ctx, r.options.Out, sink, chunk, r.inject)
		if err != nil {
			return fmt.Errorf("deliver chunk %s to %s: %w", chunk.Digest, sink.Name(), err)
		}
		if reused {
			result.ReceiptsUsed++
		}
	}
	return nil
}

func boundedText(text string) string {
	const limit = 1024
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit-3] + "..."
}

func (r *Runner) inject(point faultPoint) error {
	if r.options.fault == nil {
		return nil
	}
	return r.options.fault(point)
}

type completion struct {
	State       string    `json:"state"`
	Reason      string    `json:"reason"`
	Message     string    `json:"message"`
	CompletedAt time.Time `json:"completed_at"`
}

func readCompletion(path string) (completion, error) {
	info, err := os.Stat(path)
	if err != nil {
		return completion{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return completion{}, err
	}
	value := completion{State: "succeeded", CompletedAt: info.ModTime().UTC()}
	if len(bytes.TrimSpace(raw)) > 0 {
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			var wire struct {
				State       string `json:"state"`
				Reason      string `json:"reason"`
				Message     string `json:"message"`
				CompletedAt string `json:"completed_at"`
			}
			if err := decodeOneJSON(raw, &wire); err != nil {
				return completion{}, fmt.Errorf("read completion status %s: %w", path, err)
			}
			value.State, value.Reason, value.Message = wire.State, wire.Reason, wire.Message
			if strings.TrimSpace(wire.CompletedAt) != "" {
				value.CompletedAt, err = time.Parse(time.RFC3339Nano, wire.CompletedAt)
				if err != nil {
					return completion{}, fmt.Errorf("completion completed_at must be RFC3339: %w", err)
				}
				value.CompletedAt = value.CompletedAt.UTC()
			}
		} else {
			value.State = strings.TrimSpace(string(raw))
		}
	}
	switch strings.ToLower(strings.TrimSpace(value.State)) {
	case "", "success", "successful", "succeeded", "complete", "completed", "done":
		value.State = "succeeded"
	case "fail", "failed", "failure", "error", "errored":
		value.State = "failed"
	case "cancel", "cancelled", "canceled":
		value.State = "cancelled"
	default:
		return completion{}, fmt.Errorf("completion state must be succeeded, failed, or cancelled")
	}
	return value, nil
}

func fileExists(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
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

func cloneTags(tags map[string]string) map[string]string {
	result := make(map[string]string, len(tags)+3)
	for key, value := range tags {
		result[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return result
}

func applyEnvironmentMetadata(options *Options) {
	for key, value := range map[string]string{
		exptelemetry.TauWorkspaceTag: os.Getenv("TAU_WORKSPACE"),
		exptelemetry.TauNamespaceTag: os.Getenv("POD_NAMESPACE"),
		exptelemetry.TauClusterTag:   os.Getenv("TAU_CLUSTER"),
	} {
		if strings.TrimSpace(options.Tags[key]) == "" && strings.TrimSpace(value) != "" {
			options.Tags[key] = strings.TrimSpace(value)
		}
	}
}
