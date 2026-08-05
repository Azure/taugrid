package status

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type ExperimentRunDataOptions struct {
	Project       string
	RunGroupID    string
	Owner         string
	State         string
	Cluster       string
	CaptureSource string
}

// RunProfileRecord is a storage-neutral projection of a run profile.
//
// It deliberately mirrors the shape of the experiment store's run and run
// context records without depending on that store, so that the status package
// stays free of experiment-storage imports. Consumers that persist runs map
// this into their own record types.
type RunProfileRecord struct {
	RunID       string
	Project     string
	RunGroupID  string
	State       string
	Owner       string
	CreatedAt   string
	StartedAt   string
	CompletedAt string
	ConfigHash  string
	CodeSHA     string
	ImageDigest string
	TauCommand  string
	ResultURI   string

	Cluster          string
	Namespace        string
	Team             string
	Profile          string
	Lane             string
	LocalQueue       string
	KueueWorkload    string
	PodUID           string
	ResourceClaims   string
	GPUClass         string
	GPUCount         *int64
	NodeNames        string
	Mounts           string
	QueueWaitSeconds *float64
	GPUHours         *float64
	EstimatedCost    *float64

	CaptureSource string
}

// ExperimentRunProfile projects a status snapshot and cost profile into a
// storage-neutral run profile record.
func ExperimentRunProfile(s Snapshot, c CostProfile, opts ExperimentRunDataOptions) (RunProfileRecord, error) {
	if !s.JobFound && !s.RayJobFound {
		return RunProfileRecord{}, fmt.Errorf("cannot capture run profile: job %s/%s was not found", s.Namespace, s.Name)
	}
	runID := firstNonEmpty(label(s, experiment.LabelRunID), s.Name)
	if strings.TrimSpace(runID) == "" {
		return RunProfileRecord{}, fmt.Errorf("cannot capture run profile: run id is empty")
	}
	runGroupID := opts.RunGroupID
	if runGroupID == "" {
		runGroupID = "default"
	}
	state := opts.State
	if state == "" {
		state = experimentRunState(s)
	}
	owner := opts.Owner
	if owner == "" {
		owner = "tau-status"
	}
	captureSource := opts.CaptureSource
	if captureSource == "" {
		captureSource = "status-run-profile"
	}
	run := RunProfileRecord{
		RunID:       runID,
		Project:     opts.Project,
		RunGroupID:  runGroupID,
		State:       state,
		Owner:       owner,
		CreatedAt:   formatOptionalTime(snapshotCreatedAt(s)),
		StartedAt:   formatOptionalTime(snapshotStartedAt(s)),
		CompletedAt: formatOptionalTime(snapshotFinishedAt(s)),
		ConfigHash:  cleanProfileValue(annotationOrDefault(s, experiment.AnnotationConfigHash, "")),
		CodeSHA:     cleanProfileValue(annotationOrDefault(s, experiment.AnnotationCodeSHA, "")),
		ImageDigest: cleanProfileValue(annotationOrDefault(s, experiment.AnnotationImageDigest, "")),
		TauCommand:  cleanProfileValue(annotationOrDefault(s, experiment.AnnotationTauCommand, "")),
		ResultURI:   cleanProfileValue(annotationOrDefault(s, workloadmeta.AnnotationResultPath, "")),

		Cluster:          opts.Cluster,
		Namespace:        cleanProfileValue(annotationOrDefault(s, experiment.AnnotationNamespace, s.Namespace)),
		Team:             cleanProfileValue(label(s, workloadmeta.LabelTeam)),
		Profile:          cleanProfileValue(firstNonEmpty(label(s, workloadmeta.LabelProfile), c.Profile)),
		Lane:             cleanProfileValue(label(s, workloadmeta.LabelLane)),
		LocalQueue:       cleanProfileValue(s.EffectiveLocalQueue()),
		KueueWorkload:    cleanProfileValue(workloadNames(s)),
		PodUID:           cleanProfileValue(podUIDs(s)),
		ResourceClaims:   cleanProfileValue(resourceClaims(s)),
		GPUClass:         cleanProfileValue(firstNonEmpty(canonicalGPUClassLabel(label(s, workloadmeta.LabelGPUClass)), c.GPUType)),
		GPUCount:         runProfileGPUCount(s, c),
		NodeNames:        cleanProfileValue(nodesSummary(s)),
		Mounts:           cleanProfileValue(annotationOrDefault(s, experiment.AnnotationStorageMounts, "")),
		QueueWaitSeconds: runProfileQueueWait(s),
		GPUHours:         runProfileGPUHours(c),
		EstimatedCost:    runProfileEstimatedCost(c),

		CaptureSource: captureSource,
	}
	return run, nil
}

func experimentRunState(s Snapshot) string {
	switch deriveState(s) {
	case "Failed":
		return "failed"
	case "Complete":
		return "succeeded"
	case "Running":
		return "running"
	default:
		return "pending"
	}
}

func snapshotCreatedAt(s Snapshot) time.Time {
	if !s.JobCreatedAt.IsZero() {
		return s.JobCreatedAt
	}
	return s.RayJobCreatedAt
}

func snapshotStartedAt(s Snapshot) time.Time {
	if !s.JobStartedAt.IsZero() {
		return s.JobStartedAt
	}
	return s.RayJobStartedAt
}

func snapshotFinishedAt(s Snapshot) time.Time {
	if !s.JobFinishedAt.IsZero() {
		return s.JobFinishedAt
	}
	return s.RayJobFinishedAt
}

func runProfileGPUCount(s Snapshot, c CostProfile) *int64 {
	for _, value := range []string{
		annotationOrDefault(s, experiment.AnnotationGPUCount, ""),
		label(s, workloadmeta.AnnotationGPUCount),
	} {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && parsed > 0 {
			return &parsed
		}
	}
	if c.GPUsPerPod > 0 && c.Pods > 0 {
		value := int64(c.GPUsPerPod * c.Pods)
		return &value
	}
	return nil
}

func runProfileQueueWait(s Snapshot) *float64 {
	if seconds, ok := queueWaitSeconds(s); ok {
		value := float64(seconds)
		return &value
	}
	return nil
}

func runProfileGPUHours(c CostProfile) *float64 {
	if c.Error != "" || c.Hours <= 0 || c.GPUsPerPod <= 0 || c.Pods <= 0 {
		return nil
	}
	value := c.Hours * float64(c.GPUsPerPod*c.Pods)
	return &value
}

func runProfileEstimatedCost(c CostProfile) *float64 {
	if c.Error != "" || c.TotalUsd <= 0 {
		return nil
	}
	value := c.TotalUsd
	return &value
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func cleanProfileValue(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return ""
	case value == "-":
		return ""
	case strings.HasPrefix(value, "not collected"):
		return ""
	case strings.HasPrefix(value, "not declared"):
		return ""
	case strings.HasPrefix(value, "not assigned"):
		return ""
	case strings.HasPrefix(value, "not priced"):
		return ""
	default:
		return value
	}
}
