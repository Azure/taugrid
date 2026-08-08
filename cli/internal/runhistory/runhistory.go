// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runhistory records metadata-only Kubernetes run lifecycle observations.
package runhistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/workloadmeta"
)

var (
	ErrRayJobCRDMissing   = errors.New("RayJob CRD is not installed")
	ErrWorkloadCRDMissing = errors.New("Kueue Workload CRD is not installed")
)

const (
	StateSubmitted = "submitted"
	StateQueued    = "queued"
	StateAdmitted  = "admitted"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Source is the narrow Kubernetes read surface required by the recorder.
type Source interface {
	ListJobs(context.Context, string) ([]Job, error)
	ListRayJobs(context.Context, string) ([]RayJob, error)
	ListWorkloads(context.Context, string) ([]Workload, error)
}

// Writer persists lifecycle observations. Implementations must acknowledge writes
// before returning nil.
type Writer interface {
	Write(context.Context, []Record) error
}

type WriterFactory func(WriterConfig) (Writer, error)

type WriterConfig struct {
	Endpoint string
	Database string
	Table    string
	Timeout  time.Duration
}

type Metadata struct {
	Name            string
	Namespace       string
	UID             string
	ResourceVersion string
	Generation      int64
	CreatedAt       time.Time
	Labels          map[string]string
	Annotations     map[string]string
	OwnerKind       string
	OwnerName       string
	Deleting        bool
}

type Condition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

type Job struct {
	Metadata
	Suspended      bool
	Active         int32
	Succeeded      int32
	Failed         int32
	StartTime      time.Time
	CompletionTime time.Time
	Conditions     []Condition
}

type RayJob struct {
	Metadata
	DeploymentStatus string
	StartTime        time.Time
	CompletionTime   time.Time
	Conditions       []Condition
}

type Workload struct {
	Metadata
	Queue        string
	ClusterQueue string
	Phase        string
	Admitted     bool
	AdmittedAt   time.Time
	FinishedAt   time.Time
	Conditions   []Condition
}

// Record intentionally contains only workload metadata. It never contains pod
// specs, environment values, logs, metrics, or artifact contents.
type Record struct {
	ObservedAt         time.Time         `json:"observed_at"`
	ObservationID      string            `json:"observation_id"`
	DurableID          string            `json:"durable_id"`
	RunID              string            `json:"run_id"`
	WorkspaceID        string            `json:"workspace_id,omitempty"`
	ResultScope        string            `json:"result_scope,omitempty"`
	Project            string            `json:"project,omitempty"`
	Group              string            `json:"run_group_id,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	OwnerKind          string            `json:"owning_resource_kind"`
	OwnerName          string            `json:"owning_resource_name"`
	Namespace          string            `json:"namespace"`
	Cluster            string            `json:"cluster"`
	ResourceUID        string            `json:"resource_uid,omitempty"`
	ResourceVersion    string            `json:"resource_version,omitempty"`
	Generation         int64             `json:"generation,omitempty"`
	SubmittedAt        time.Time         `json:"submit_time,omitempty"`
	CreatedAt          time.Time         `json:"created_time,omitempty"`
	AdmittedAt         time.Time         `json:"kueue_admitted_time,omitempty"`
	PodStartedAt       time.Time         `json:"pod_start_time,omitempty"`
	CompletionAt       time.Time         `json:"completion_time,omitempty"`
	State              string            `json:"state"`
	Reason             string            `json:"reason,omitempty"`
	Message            string            `json:"message,omitempty"`
	LocalQueue         string            `json:"local_queue,omitempty"`
	ClusterQueue       string            `json:"cluster_queue,omitempty"`
	WorkloadKind       string            `json:"workload_kind"`
	Image              string            `json:"image,omitempty"`
	ImageDigest        string            `json:"image_digest,omitempty"`
	ConfigHash         string            `json:"config_hash,omitempty"`
	CodeSHA            string            `json:"code_sha,omitempty"`
	TauCommand         string            `json:"tau_command,omitempty"`
	ResultPath         string            `json:"result_path,omitempty"`
	ResultPVC          string            `json:"result_pvc,omitempty"`
	ArtifactURI        string            `json:"artifact_uri,omitempty"`
	CheckpointURI      string            `json:"checkpoint_uri,omitempty"`
	ControllerVersion  string            `json:"controller_version,omitempty"`
	ExperimentTracking string            `json:"experiment_tracking"`
	ExperimentSource   string            `json:"experiment_source,omitempty"`
}

func (r Record) MarshalJSON() ([]byte, error) {
	type recordAlias Record
	data, err := json.Marshal(recordAlias(r))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for name, value := range map[string]time.Time{
		"submit_time":         r.SubmittedAt,
		"created_time":        r.CreatedAt,
		"kueue_admitted_time": r.AdmittedAt,
		"pod_start_time":      r.PodStartedAt,
		"completion_time":     r.CompletionAt,
	} {
		if value.IsZero() {
			delete(fields, name)
		}
	}
	return json.Marshal(fields)
}

type Result struct {
	Emitted         int    `json:"emitted"`
	RayJobsStatus   string `json:"rayjobs_status"`
	WorkloadsStatus string `json:"workloads_status"`
}

type Reconciler struct {
	Source Source
	Writer Writer

	Cluster       string
	WorkspaceID   string
	ResultScope   string
	WriterRetries int
	Now           func() time.Time

	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

func (r *Reconciler) Reconcile(ctx context.Context, namespace string) (Result, error) {
	if r.Source == nil {
		return Result{}, errors.New("run history source is required")
	}
	if r.Writer == nil {
		return Result{}, errors.New("run history writer is required")
	}
	if strings.TrimSpace(namespace) == "" {
		return Result{}, errors.New("run history namespace is required")
	}
	if strings.TrimSpace(r.Cluster) == "" {
		return Result{}, errors.New("run history cluster is required")
	}

	jobs, err := r.Source.ListJobs(ctx, namespace)
	if err != nil {
		return Result{}, fmt.Errorf("list Tau Jobs: %w", err)
	}
	rayJobs, rayErr := r.Source.ListRayJobs(ctx, namespace)
	result := Result{RayJobsStatus: "available", WorkloadsStatus: "available"}
	if rayErr != nil {
		if !errors.Is(rayErr, ErrRayJobCRDMissing) {
			return Result{}, fmt.Errorf("list Tau RayJobs: %w", rayErr)
		}
		result.RayJobsStatus = "missing-crd"
		rayJobs = nil
	}
	workloads, err := r.Source.ListWorkloads(ctx, namespace)
	if err != nil {
		if !errors.Is(err, ErrWorkloadCRDMissing) {
			return Result{}, fmt.Errorf("list Kueue Workloads: %w", err)
		}
		result.WorkloadsStatus = "missing-crd"
		workloads = nil
	}

	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	var pending []Record
	for _, job := range jobs {
		records := observationsForJob(job, correlatedWorkloads(job.Metadata, runID(job.Metadata), workloads), r.Cluster, r.WorkspaceID, r.ResultScope, now)
		assignObservationIDs(records)
		pending = append(pending, r.unsent(records)...)
	}
	for _, rayJob := range rayJobs {
		records := observationsForRayJob(rayJob, correlatedWorkloads(rayJob.Metadata, runID(rayJob.Metadata), workloads), r.Cluster, r.WorkspaceID, r.ResultScope, now)
		assignObservationIDs(records)
		pending = append(pending, r.unsent(records)...)
	}
	if len(pending) == 0 {
		return result, nil
	}
	if err := r.write(ctx, pending); err != nil {
		return Result{}, err
	}
	r.markSeen(pending)
	result.Emitted = len(pending)
	return result, nil
}

func (r *Reconciler) write(ctx context.Context, records []Record) error {
	attempts := r.WriterRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := r.Writer.Write(ctx, records); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("write %d lifecycle observations after %d attempt(s): %w", len(records), attempts, lastErr)
}

func (r *Reconciler) unsent(records []Record) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]map[string]struct{})
	}
	out := make([]Record, 0, len(records))
	for _, record := range records {
		fingerprint := recordFingerprint(record)
		if _, ok := r.seen[record.DurableID][fingerprint]; ok {
			continue
		}
		out = append(out, record)
	}
	return out
}

func (r *Reconciler) markSeen(records []Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]map[string]struct{})
	}
	for _, record := range records {
		if r.seen[record.DurableID] == nil {
			r.seen[record.DurableID] = make(map[string]struct{})
		}
		r.seen[record.DurableID][recordFingerprint(record)] = struct{}{}
	}
}

func recordFingerprint(record Record) string {
	record.ObservedAt = time.Time{}
	record.ObservationID = ""
	record.ResourceVersion = ""
	if record.State == StateSubmitted {
		record.AdmittedAt = time.Time{}
		record.PodStartedAt = time.Time{}
		record.CompletionAt = time.Time{}
		record.ClusterQueue = ""
	}
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assignObservationIDs(records []Record) {
	for i := range records {
		records[i].ObservationID = recordFingerprint(records[i])
	}
}

func observationsForJob(job Job, workloads []Workload, cluster, workspaceID, resultScope string, now time.Time) []Record {
	state, reason, message := jobState(job)
	state, reason, message = applyWorkloadState(state, reason, message, workloads)
	record := baseRecord(job.Metadata, cluster, workspaceID, resultScope, "Job", experiment.WorkloadKindJob, workloads, now)
	record.State, record.Reason, record.Message = state, reason, message
	record.PodStartedAt = job.StartTime
	record.CompletionAt = firstTime(job.CompletionTime, terminalConditionTime(job.Conditions))
	return initialAndCurrent(record)
}

func terminalConditionTime(conditions []Condition) time.Time {
	for i := len(conditions) - 1; i >= 0; i-- {
		condition := conditions[i]
		if (condition.Type == "Complete" || condition.Type == "Failed") &&
			strings.EqualFold(condition.Status, "true") {
			return condition.LastTransitionTime
		}
	}
	return time.Time{}
}

func observationsForRayJob(rayJob RayJob, workloads []Workload, cluster, workspaceID, resultScope string, now time.Time) []Record {
	state, reason, message := rayJobState(rayJob)
	state, reason, message = applyWorkloadState(state, reason, message, workloads)
	record := baseRecord(rayJob.Metadata, cluster, workspaceID, resultScope, "RayJob", experiment.WorkloadKindRayJob, workloads, now)
	record.State, record.Reason, record.Message = state, reason, message
	record.PodStartedAt, record.CompletionAt = rayJob.StartTime, rayJob.CompletionTime
	return initialAndCurrent(record)
}

func initialAndCurrent(record Record) []Record {
	receipt := record
	receipt.State = StateSubmitted
	receipt.Reason = "initial-observation"
	receipt.Message = ""
	receipt.ObservedAt = lifecycleEventTime(receipt)
	record.ObservedAt = lifecycleEventTime(record)
	if record.State == StateSubmitted {
		return []Record{receipt}
	}
	return []Record{receipt, record}
}

func lifecycleEventTime(record Record) time.Time {
	switch record.State {
	case StateSubmitted, StateQueued:
		return firstTime(record.CreatedAt, record.SubmittedAt, record.ObservedAt)
	case StateAdmitted:
		return firstTime(record.AdmittedAt, record.CreatedAt, record.ObservedAt)
	case StateRunning:
		return firstTime(record.PodStartedAt, record.AdmittedAt, record.CreatedAt, record.ObservedAt)
	case StateSucceeded, StateFailed, StateCancelled:
		return firstTime(record.CompletionAt, record.PodStartedAt, record.AdmittedAt, record.CreatedAt, record.ObservedAt)
	default:
		return record.ObservedAt
	}
}

func baseRecord(metadata Metadata, cluster, defaultWorkspaceID, defaultResultScope, ownerKind, workloadKind string, workloads []Workload, now time.Time) Record {
	annotations := metadata.Annotations
	labels := metadata.Labels
	if annotations == nil {
		annotations = map[string]string{}
	}
	if labels == nil {
		labels = map[string]string{}
	}
	project := text(annotations[experiment.AnnotationStellarProject])
	experimentID := text(annotations[experiment.AnnotationStellarExperimentID])
	tracking := "untracked"
	source := ""
	experimentSource := text(annotations[experiment.AnnotationExperimentSource])
	if experimentSource != "" && (project != "" || experimentID != "") {
		tracking, source = "tracked", experimentSource
	}
	localQueue, clusterQueue, admittedAt := queues(workloads)
	localQueue = first(localQueue, labels["kueue.x-k8s.io/queue-name"])
	ownerName := text(metadata.OwnerName)
	if ownerName == "" {
		ownerName = metadata.Name
	}
	ownerKindValue := text(metadata.OwnerKind)
	if ownerKindValue == "" {
		ownerKindValue = ownerKind
	}
	return Record{
		ObservedAt:         now,
		DurableID:          durableID(cluster, metadata, ownerKind),
		RunID:              runID(metadata),
		WorkspaceID:        first(annotations[experiment.AnnotationWorkspaceID], labels[workloadmeta.LabelWorkspace], defaultWorkspaceID),
		ResultScope:        first(annotations[experiment.AnnotationResultScope], defaultResultScope),
		Project:            project,
		Group:              text(annotations[experiment.AnnotationStellarGroup]),
		Tags:               stellarTags(annotations[experiment.AnnotationStellarTags]),
		OwnerKind:          ownerKindValue,
		OwnerName:          ownerName,
		Namespace:          metadata.Namespace,
		Cluster:            cluster,
		ResourceUID:        metadata.UID,
		ResourceVersion:    metadata.ResourceVersion,
		Generation:         metadata.Generation,
		SubmittedAt:        metadata.CreatedAt,
		CreatedAt:          metadata.CreatedAt,
		AdmittedAt:         admittedAt,
		LocalQueue:         localQueue,
		ClusterQueue:       clusterQueue,
		WorkloadKind:       first(labels[experiment.LabelWorkloadKind], workloadKind),
		Image:              text(annotations[experiment.AnnotationImage]),
		ImageDigest:        text(annotations[experiment.AnnotationImageDigest]),
		ConfigHash:         text(annotations[experiment.AnnotationConfigHash]),
		CodeSHA:            text(annotations[experiment.AnnotationCodeSHA]),
		TauCommand:         text(annotations[experiment.AnnotationTauCommand]),
		ResultPath:         text(annotations[experiment.AnnotationResultPath]),
		ResultPVC:          text(annotations[experiment.AnnotationResultPVC]),
		ArtifactURI:        uri(annotations[experiment.AnnotationArtifactURI]),
		CheckpointURI:      uri(annotations[experiment.AnnotationCheckpointURI]),
		ControllerVersion:  first(annotations[workloadmeta.AnnotationControllerVerion], annotations["app.kubernetes.io/version"]),
		ExperimentTracking: tracking,
		ExperimentSource:   source,
	}
}

func jobState(job Job) (string, string, string) {
	if condition, ok := trueCondition(job.Conditions, "Complete"); ok {
		return StateSucceeded, text(condition.Reason), text(condition.Message)
	}
	if condition, ok := trueCondition(job.Conditions, "Failed"); ok {
		return StateFailed, text(condition.Reason), text(condition.Message)
	}
	if job.Metadata.Deleting {
		return StateCancelled, "Deleting", "workload deletion observed before terminal completion"
	}
	if job.Active > 0 {
		return stateFromConditions(StateRunning, job.Conditions)
	}
	if job.Suspended {
		return StateQueued, "suspended", "waiting for Kueue admission"
	}
	return stateFromConditions(StateQueued, job.Conditions)
}

func trueCondition(conditions []Condition, typ string) (Condition, bool) {
	for i := len(conditions) - 1; i >= 0; i-- {
		if conditions[i].Type == typ && strings.EqualFold(conditions[i].Status, "true") {
			return conditions[i], true
		}
	}
	return Condition{}, false
}

func rayJobState(job RayJob) (string, string, string) {
	status := strings.ToLower(text(job.DeploymentStatus))
	switch status {
	case "complete", "completed", "succeeded":
		return stateFromConditions(StateSucceeded, job.Conditions)
	case "failed":
		return stateFromConditions(StateFailed, job.Conditions)
	}
	if job.Metadata.Deleting {
		return StateCancelled, "Deleting", "workload deletion observed before terminal completion"
	}
	switch status {
	case "running":
		return stateFromConditions(StateRunning, job.Conditions)
	case "suspended", "new", "initializing", "":
		return stateFromConditions(StateQueued, job.Conditions)
	default:
		return stateFromConditions(StateQueued, job.Conditions)
	}
}

func stateFromConditions(state string, conditions []Condition) (string, string, string) {
	for i := len(conditions) - 1; i >= 0; i-- {
		condition := conditions[i]
		if strings.EqualFold(condition.Status, "true") {
			return state, text(condition.Reason), text(condition.Message)
		}
	}
	return state, "", ""
}

func applyWorkloadState(state, reason, message string, workloads []Workload) (string, string, string) {
	if state != StateQueued {
		return state, reason, message
	}
	for _, workload := range workloads {
		if workload.Admitted {
			if reason == "" {
				reason = "KueueAdmitted"
			}
			if message == "" {
				message = "Kueue Workload admitted"
			}
			return StateAdmitted, reason, message
		}
	}
	return state, reason, message
}

func correlatedWorkloads(metadata Metadata, id string, workloads []Workload) []Workload {
	var out []Workload
	for _, workload := range workloads {
		if workload.Labels[experiment.LabelRunID] == id ||
			workload.Metadata.Labels[workloadmeta.LabelJob] == id ||
			workload.Metadata.Labels[workloadmeta.LabelJob] == metadata.Name ||
			workload.Metadata.Annotations["kueue.x-k8s.io/job-uid"] == metadata.UID ||
			(workload.OwnerName == metadata.Name && workload.OwnerKind != "") {
			out = append(out, workload)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func queues(workloads []Workload) (string, string, time.Time) {
	for _, workload := range workloads {
		if workload.Admitted {
			return text(workload.Queue), text(workload.ClusterQueue), workload.AdmittedAt
		}
	}
	if len(workloads) == 0 {
		return "", "", time.Time{}
	}
	return text(workloads[0].Queue), text(workloads[0].ClusterQueue), time.Time{}
}

func durableID(cluster string, metadata Metadata, kind string) string {
	identity := text(metadata.UID)
	if identity == "" {
		identity = strings.ToLower(kind) + "/" + metadata.Name
	}
	return text(cluster) + "/" + metadata.Namespace + "/" + identity
}

func runID(metadata Metadata) string {
	return first(metadata.Labels[experiment.LabelRunID], metadata.Labels[workloadmeta.LabelJob], metadata.Name)
}

func stellarTags(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var tags map[string]string
	if json.Unmarshal([]byte(raw), &tags) != nil {
		return nil
	}
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		if key = text(key); key != "" {
			out[key] = text(value)
		}
	}
	return out
}

func uri(value string) string {
	value = text(value)
	if strings.Contains(value, "://") {
		return value
	}
	return ""
}

func first(values ...string) string {
	for _, value := range values {
		if value = text(value); value != "" {
			return value
		}
	}
	return ""
}

func text(value string) string { return strings.TrimSpace(value) }
