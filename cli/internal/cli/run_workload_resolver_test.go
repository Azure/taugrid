package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunIdentityQualifiesIndependentSubmissions(t *testing.T) {
	first := defaultRunDispatchOptions()
	second := defaultRunDispatchOptions()
	firstIdentity, err := ensureRunIdentity(&first, "kaggriculture-parity")
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := ensureRunIdentity(&second, "kaggriculture-parity")
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity.RunID == secondIdentity.RunID {
		t.Fatalf("independent submissions reused run ID %q", firstIdentity.RunID)
	}
	if firstIdentity.PhysicalName == secondIdentity.PhysicalName {
		t.Fatalf("independent submissions reused physical name %q", firstIdentity.PhysicalName)
	}
	for _, identity := range []runIdentity{firstIdentity, secondIdentity} {
		if identity.LogicalName != "kaggriculture-parity" {
			t.Fatalf("logical name = %q", identity.LogicalName)
		}
		if len(identity.PhysicalName) > maxRunPhysicalNameLen {
			t.Fatalf("physical name %q exceeds %d characters", identity.PhysicalName, maxRunPhysicalNameLen)
		}
		if !strings.HasPrefix(identity.PhysicalName, "kaggriculture-parity-") {
			t.Fatalf("physical name %q is not run-qualified", identity.PhysicalName)
		}
	}
}

func TestRunIdentityLeavesCompletedLogicalNameAvailable(t *testing.T) {
	options := defaultRunDispatchOptions()
	identity, err := ensureRunIdentity(&options, "fixed-name")
	if err != nil {
		t.Fatal(err)
	}
	if identity.PhysicalName == "fixed-name" {
		t.Fatal("real submission still uses the stale fixed name")
	}
	runner := submissionRunnerFunc(func(_ context.Context, args []string, manifest []byte) (string, error) {
		if args[0] != "create" {
			return "", fmt.Errorf("unexpected args: %v", args)
		}
		if !strings.Contains(string(manifest), identity.PhysicalName) {
			t.Fatalf("manifest does not contain immutable physical name %q", identity.PhysicalName)
		}
		return "rayjob.ray.io/" + identity.PhysicalName + " created\n", nil
	})
	if _, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "rayjob.ray.io",
		Name:         identity.PhysicalName,
		Namespace:    "research",
		SubmissionID: "new-submission",
		Manifest:     []byte("name: " + identity.PhysicalName),
	}); err != nil {
		t.Fatalf("new immutable run was blocked by completed logical predecessor: %v", err)
	}
}

func TestResolveRunWorkloadRejectsAmbiguousLogicalName(t *testing.T) {
	runner := runListFixtureRunner(t, []map[string]any{
		runListFixture("train-a1", "train", "run-a", "uid-a", false),
		runListFixture("train-b2", "train", "run-b", "uid-b", true),
	}, nil)
	_, err := resolveRunWorkload(context.Background(), runner, "research", "train")
	if err == nil || !stringsContainAll(err.Error(), "ambiguous", "train-a1", "run-a", "train-b2", "run-b") {
		t.Fatalf("error = %v", err)
	}
	ref, err := resolveRunWorkload(context.Background(), runner, "research", "run-b")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "train-b2" || ref.RunID != "run-b" || !ref.Terminal {
		t.Fatalf("resolved ref = %+v", ref)
	}
}

func TestResolveRunWorkloadIgnoresForeignObjects(t *testing.T) {
	foreign := runListFixture("train-foreign", "train", "run-foreign", "uid-foreign", false)
	foreign["metadata"].(map[string]any)["labels"].(map[string]string)[workloadmeta.LabelManagedBy] = "someone-else"
	runner := runListFixtureRunner(t, []map[string]any{foreign}, nil)
	_, err := resolveRunWorkload(context.Background(), runner, "research", "train-foreign")
	if err == nil || !strings.Contains(err.Error(), "no Tau-managed") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveRunWorkloadClassifiesOnlyConfirmedNoMatch(t *testing.T) {
	runner := runListFixtureRunner(t, nil, nil)
	_, err := resolveRunWorkload(context.Background(), runner, "research", "missing")
	if err == nil || !isRunWorkloadNotFound(err) {
		t.Fatalf("confirmed no-match error = %v", err)
	}

	transient := errors.New("temporary list failure")
	_, err = resolveRunWorkload(context.Background(), submissionRunnerFunc(
		func(context.Context, []string, []byte) (string, error) {
			return "", transient
		},
	), "research", "missing")
	if err == nil || isRunWorkloadNotFound(err) || !errors.Is(err, transient) {
		t.Fatalf("transient resolver error = %v", err)
	}
}

func TestParseResolvedRayJobTerminalStatusUsesAllSignals(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
	}{
		{
			name: "terminal job status overrides stale deployment status",
			status: map[string]any{
				"jobDeploymentStatus": "Running",
				"jobStatus":           "SUCCEEDED",
			},
		},
		{
			name: "end time marks terminal when states are stale",
			status: map[string]any{
				"jobDeploymentStatus": "Running",
				"jobStatus":           "RUNNING",
				"endTime":             "2026-08-07T12:00:00Z",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := runListFixture("train-a1", "train", "run-a", "uid-a", false)
			item["status"] = test.status
			data, err := json.Marshal(map[string]any{"items": []map[string]any{item}})
			if err != nil {
				t.Fatal(err)
			}
			runs, err := parseResolvedRunWorkloads(data, "rayjobs.ray.io", "RayJob")
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || !runs[0].Terminal {
				t.Fatalf("resolved runs = %+v", runs)
			}
		})
	}
}

func TestDeleteOwnedRunWorkloadUsesUIDAndMatchingOwnership(t *testing.T) {
	run := resolvedRunWorkload{
		Resource:  "jobs.batch",
		Kind:      "Job",
		Name:      "train-a1",
		Namespace: "research",
		RunID:     "run-a",
		UID:       "uid-job",
		Labels: map[string]string{
			workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
			workloadmeta.LabelRunID:     "run-a",
		},
	}
	var deleted []string
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(_ context.Context, args []string, _ []byte) (string, error) {
			if args[0] != "get" {
				return "", fmt.Errorf("unexpected args: %v", args)
			}
			return existingOwnedObjectJSON("uid-service", "run-a"), nil
		},
		deleteWithUID: func(_ context.Context, submission runSubmission, uid types.UID) error {
			deleted = append(deleted, submission.Resource+"/"+submission.Name+"@"+string(uid))
			return nil
		},
	}
	if err := deleteOwnedRunWorkload(context.Background(), runner, run); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"service/train-a1-headless@uid-service",
		"jobs.batch/train-a1@uid-job",
	}
	if fmt.Sprint(deleted) != fmt.Sprint(want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
}

func TestDeleteOwnedRunWorkloadProtectsForeignOwnership(t *testing.T) {
	called := false
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(context.Context, []string, []byte) (string, error) {
			return "", errors.New("must not look up ancillary resources")
		},
		deleteWithUID: func(context.Context, runSubmission, types.UID) error {
			called = true
			return nil
		},
	}
	err := deleteOwnedRunWorkload(context.Background(), runner, resolvedRunWorkload{
		Resource:  "rayjobs.ray.io",
		Kind:      "RayJob",
		Name:      "foreign",
		Namespace: "research",
		RunID:     "run-a",
		UID:       "uid-a",
		Labels: map[string]string{
			workloadmeta.LabelManagedBy: "someone-else",
			workloadmeta.LabelRunID:     "run-a",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not managed by Tau") || called {
		t.Fatalf("error=%v deleteCalled=%t", err, called)
	}
}

func TestDeleteOwnedRunRayClustersTargetsOnlyMatchingImmutableRun(t *testing.T) {
	physicalName := physicalRunName("train", "run-a")
	var deleted []string
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(_ context.Context, args []string, _ []byte) (string, error) {
			if args[2] != "get" || args[3] != "rayclusters.ray.io" {
				return "", fmt.Errorf("unexpected args: %v", args)
			}
			data, err := json.Marshal(map[string]any{"items": []map[string]any{{
				"metadata": map[string]any{
					"name": "train-cluster",
					"uid":  "uid-cluster",
					"labels": map[string]string{
						rayOriginLabel:              physicalName,
						workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
						workloadmeta.LabelRunID:     "run-a",
					},
				},
			}}})
			return string(data), err
		},
		deleteWithUID: func(_ context.Context, submission runSubmission, uid types.UID) error {
			deleted = append(deleted, submission.Resource+"/"+submission.Name+"@"+string(uid))
			return nil
		},
	}
	names, err := deleteOwnedRunRayClusters(context.Background(), runner, "research", physicalName)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names) != "[train-cluster]" ||
		fmt.Sprint(deleted) != "[raycluster.ray.io/train-cluster@uid-cluster]" {
		t.Fatalf("names=%v deleted=%v", names, deleted)
	}
}

func TestDeleteOwnedRunRayClustersRejectsForeignOwnership(t *testing.T) {
	physicalName := physicalRunName("train", "run-a")
	called := false
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(context.Context, []string, []byte) (string, error) {
			return `{"items":[{"metadata":{"name":"foreign","uid":"uid","labels":{"` +
				rayOriginLabel + `":"` + physicalName + `","` +
				workloadmeta.LabelManagedBy + `":"someone-else","` +
				workloadmeta.LabelRunID + `":"run-a"}}}]}`, nil
		},
		deleteWithUID: func(context.Context, runSubmission, types.UID) error {
			called = true
			return nil
		},
	}
	_, err := deleteOwnedRunRayClusters(context.Background(), runner, "research", physicalName)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") || called {
		t.Fatalf("error=%v deleteCalled=%t", err, called)
	}
}

func TestManagerCleanupUsesOwnershipSafeDeleteHook(t *testing.T) {
	called := false
	err := deleteWorkloadAndWaitForManagerCleanup(
		context.Background(),
		rawRunnerFunc(func(context.Context, []string, []byte) (string, error) {
			return "", fmt.Errorf("unsafe runner delete must not be used")
		}),
		"train-a1",
		"research",
		io.Discard,
		managerCleanupOptions{Timeout: time.Second, Interval: time.Millisecond},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				return status.Snapshot{Name: "train-a1", Namespace: "research"}, nil
			},
			deleteExact: func(context.Context, kubeRawRunner, string, string, io.Writer) error {
				called = true
				return nil
			},
			wait: func(context.Context, time.Duration) error { return nil },
			now:  time.Now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("ownership-safe delete hook was not used")
	}
}

func TestArchiveRunPatchProtectsUIDAndRunID(t *testing.T) {
	ttl := int32(28800)
	patch, err := archiveRunPatch(resolvedRunWorkload{
		Kind:                    "Job",
		UID:                     "uid-a",
		RunID:                   "run-a",
		Annotations:             map[string]string{},
		TTLSecondsAfterFinished: &ttl,
	}, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var operations []map[string]any
	if err := json.Unmarshal(patch, &operations); err != nil {
		t.Fatal(err)
	}
	encoded := string(patch)
	if !stringsContainAll(encoded, "uid-a", "run-a", "managed-by", "ttlSecondsAfterFinished", "archived-at", "2026-08-07T12:00:00Z") {
		t.Fatalf("patch = %v", operations)
	}
}

func TestArchiveRunPatchRemovesTTLWithoutReplacingExistingArchiveTimestamp(t *testing.T) {
	ttl := int32(28800)
	patch, err := archiveRunPatch(resolvedRunWorkload{
		Kind:  "Job",
		UID:   "uid-a",
		RunID: "run-a",
		Annotations: map[string]string{
			workloadmeta.AnnotationArchivedAt: "2026-08-06T12:00:00Z",
		},
		TTLSecondsAfterFinished: &ttl,
	}, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(patch)
	if !strings.Contains(encoded, "ttlSecondsAfterFinished") ||
		strings.Contains(encoded, workloadmeta.AnnotationArchivedAt) {
		t.Fatalf("patch = %s", encoded)
	}
}

func runListFixture(name, logicalName, runID, uid string, terminal bool) map[string]any {
	statusBlock := map[string]any{"active": 1}
	if terminal {
		statusBlock = map[string]any{
			"active":     0,
			"conditions": []map[string]string{{"type": "Complete", "status": "True"}},
		}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":      name,
			"namespace": "research",
			"uid":       uid,
			"labels": map[string]string{
				workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
				workloadmeta.LabelRun:       logicalName,
				workloadmeta.LabelRunID:     runID,
			},
		},
		"status": statusBlock,
	}
}

func runListFixtureRunner(t *testing.T, jobs, rayJobs []map[string]any) submissionRunnerFunc {
	t.Helper()
	return func(_ context.Context, args []string, _ []byte) (string, error) {
		var items []map[string]any
		switch args[3] {
		case "jobs.batch":
			items = jobs
		case "rayjobs.ray.io":
			items = rayJobs
		default:
			return "", fmt.Errorf("unexpected args: %v", args)
		}
		data, err := json.Marshal(map[string]any{"items": items})
		return string(data), err
	}
}

func existingOwnedObjectJSON(uid, runID string) string {
	data, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"uid": uid,
			"labels": map[string]string{
				workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
				workloadmeta.LabelRunID:     runID,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}
