package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/types"
)

type submissionRunnerFunc func(context.Context, []string, []byte) (string, error)

func (f submissionRunnerFunc) Raw(ctx context.Context, args []string, stdin []byte) (string, error) {
	return f(ctx, args, stdin)
}

type cleanupSubmissionRunner struct {
	submissionRunnerFunc
	deleteWithUID func(context.Context, runSubmission, types.UID) error
}

type activationSubmissionRunner struct {
	submissionRunnerFunc
	activateQueueWithUID func(context.Context, runSubmission, types.UID, string) (string, error)
}

func (r activationSubmissionRunner) ActivateQueueWithUID(ctx context.Context, submission runSubmission, uid types.UID, queueName string) (string, error) {
	return r.activateQueueWithUID(ctx, submission, uid, queueName)
}

func (r cleanupSubmissionRunner) DeleteWithUID(ctx context.Context, submission runSubmission, uid types.UID) error {
	return r.deleteWithUID(ctx, submission, uid)
}

func TestSubmitRunWorkloadFreshCreate(t *testing.T) {
	runner := submissionRunnerFunc(func(_ context.Context, args []string, manifest []byte) (string, error) {
		if got, want := fmt.Sprint(args), "[create -n research -f -]"; got != want {
			t.Fatalf("args = %s, want %s", got, want)
		}
		if string(manifest) != "fresh-manifest" {
			t.Fatalf("manifest = %q", manifest)
		}
		return "rayjob.ray.io/fresh created\n", nil
	})

	result, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "rayjob.ray.io",
		Name:         "fresh",
		Namespace:    "research",
		SubmissionID: "submission-fresh",
		Manifest:     []byte("fresh-manifest"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "rayjob.ray.io/fresh created\n" || result.Recovered {
		t.Fatalf("result = %+v", result)
	}
}

func TestSubmitRunWorkloadRejectsOptionLikeNameBeforeKubectl(t *testing.T) {
	called := false
	runner := submissionRunnerFunc(func(context.Context, []string, []byte) (string, error) {
		called = true
		return "", nil
	})
	_, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "job",
		Name:         "--server=http://127.0.0.1:1234",
		Namespace:    "research",
		SubmissionID: "submission",
		Manifest:     []byte("manifest"),
	})
	if err == nil || !strings.Contains(err.Error(), "name") || called {
		t.Fatalf("error=%v kubectlCalled=%t", err, called)
	}
}

func TestSubmitRunWorkloadTerminatesLookupPositionalParsing(t *testing.T) {
	runner := submissionRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch args[0] {
		case "create":
			return "", errors.New("connection reset")
		case "get":
			want := "[get job -n research -o json -- uncertain]"
			if got := fmt.Sprint(args); got != want {
				t.Fatalf("lookup args=%s, want %s", got, want)
			}
			return existingSubmissionJSON("submission", ""), nil
		default:
			return "", fmt.Errorf("unexpected args: %v", args)
		}
	})
	if _, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "job",
		Name:         "uncertain",
		Namespace:    "research",
		SubmissionID: "submission",
		Manifest:     []byte("manifest"),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRunWorkloadRejectsExistingNamesRegardlessOfStateOrSpec(t *testing.T) {
	tests := map[string]string{
		"different spec": existingSubmissionJSON("old-submission", `"spec":{"workers":99}`),
		"completed":      existingSubmissionJSON("old-submission", `"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED"}`),
		"running":        existingSubmissionJSON("old-submission", `"status":{"jobDeploymentStatus":"Running","jobStatus":"RUNNING"}`),
		"legacy object":  `{"metadata":{},"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED"}}`,
	}
	for name, existing := range tests {
		t.Run(name, func(t *testing.T) {
			runner := submissionRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
				switch args[0] {
				case "create":
					return "", errors.New(`Error from server (AlreadyExists): rayjobs.ray.io "same-name" already exists`)
				case "get":
					return existing, nil
				default:
					t.Fatalf("unexpected args: %v", args)
					return "", nil
				}
			})

			_, err := submitRunWorkload(context.Background(), runner, runSubmission{
				Resource:     "rayjob.ray.io",
				Name:         "same-name",
				Namespace:    "research",
				SubmissionID: "new-submission",
				Manifest:     []byte("new-spec"),
			})
			var collision *runNameCollisionError
			if !errors.As(err, &collision) {
				t.Fatalf("error = %v, want runNameCollisionError", err)
			}
		})
	}
}

func TestSubmitRunWorkloadRecoversUncertainCreateOnlyForSameSubmission(t *testing.T) {
	const submissionID = "same-submission"
	runner := submissionRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch args[0] {
		case "create":
			return "", errors.New("connection reset after request body was sent")
		case "get":
			return existingSubmissionJSON(submissionID, ""), nil
		default:
			t.Fatalf("unexpected args: %v", args)
			return "", nil
		}
	})

	result, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "job",
		Name:         "uncertain",
		Namespace:    "research",
		SubmissionID: submissionID,
		Manifest:     []byte("manifest"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recovered {
		t.Fatalf("result = %+v, want recovered submission", result)
	}
}

func TestSubmitRunWorkloadDoesNotHideUnverifiableCreateFailure(t *testing.T) {
	runner := submissionRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		if args[0] == "create" {
			return "", errors.New("connection reset")
		}
		return "", errors.New("forbidden")
	})

	_, err := submitRunWorkload(context.Background(), runner, runSubmission{
		Resource:     "job",
		Name:         "uncertain",
		Namespace:    "research",
		SubmissionID: "submission",
		Manifest:     []byte("manifest"),
	})
	if err == nil || !stringsContainAll(err.Error(), "connection reset", "could not verify", "forbidden") {
		t.Fatalf("error = %v", err)
	}
}

func TestSubmitRunWorkloadConcurrentCreatorsHaveOneWinner(t *testing.T) {
	type storedObject struct {
		submissionID string
	}
	var (
		mu     sync.Mutex
		stored *storedObject
	)
	runner := submissionRunnerFunc(func(_ context.Context, args []string, manifest []byte) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "create":
			id := string(manifest)
			if stored != nil {
				return "", errors.New("AlreadyExists")
			}
			stored = &storedObject{submissionID: id}
			return "created\n", nil
		case "get":
			return existingSubmissionJSON(stored.submissionID, ""), nil
		default:
			return "", fmt.Errorf("unexpected args: %v", args)
		}
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, id := range []string{"submission-a", "submission-b"} {
		id := id
		go func() {
			<-start
			_, err := submitRunWorkload(context.Background(), runner, runSubmission{
				Resource:     "rayjob.ray.io",
				Name:         "concurrent",
				Namespace:    "research",
				SubmissionID: id,
				Manifest:     []byte(id),
			})
			errs <- err
		}()
	}
	close(start)

	var successes, collisions int
	for range 2 {
		err := <-errs
		if err == nil {
			successes++
			continue
		}
		var collision *runNameCollisionError
		if errors.As(err, &collision) {
			collisions++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if successes != 1 || collisions != 1 {
		t.Fatalf("successes=%d collisions=%d, want one each", successes, collisions)
	}
}

func TestEnsureSubmissionIDIsFreshAndStableWithinOneCreateAttempt(t *testing.T) {
	first := defaultRunDispatchOptions()
	second := defaultRunDispatchOptions()
	if err := ensureSubmissionID(&first); err != nil {
		t.Fatal(err)
	}
	if err := ensureSubmissionID(&second); err != nil {
		t.Fatal(err)
	}
	if first.submissionID == "" || second.submissionID == "" || first.submissionID == second.submissionID {
		t.Fatalf("fresh IDs = %q and %q", first.submissionID, second.submissionID)
	}
	retryID := first.submissionID
	if err := ensureSubmissionID(&first); err != nil {
		t.Fatal(err)
	}
	if first.submissionID != retryID {
		t.Fatalf("same create attempt changed submission ID from %q to %q", retryID, first.submissionID)
	}
	replacement := first
	replacement.submissionID = ""
	if err := ensureSubmissionID(&replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.submissionID == retryID {
		t.Fatalf("replacement attempt reused submission ID %q", retryID)
	}
}

func TestEnsureSubmissionIDLeavesDryRunsDeterministic(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.dryRun = "client"
	if err := ensureSubmissionID(&options); err != nil {
		t.Fatal(err)
	}
	if options.submissionID != "" {
		t.Fatalf("client dry-run got submission ID %q", options.submissionID)
	}
}

func TestPrepareManagedSubmissionGatesPrimaryBeforeAncillaryReconcile(t *testing.T) {
	rendered := []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: run-payload
---
apiVersion: ray.io/v1
kind: RayJob
metadata:
  name: training
  labels:
    kueue.x-k8s.io/queue-name: jobqueue
  annotations:
    %s: submission-managed
spec:
  suspend: true
---
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: training-kv
`, workloadmeta.AnnotationSubmissionID))
	prepared, err := prepareManagedSubmission(rendered, "RayJob", "training", "submission-managed")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "jobqueue" {
		t.Fatalf("queue = %q, want jobqueue", prepared.QueueName)
	}
	var primary map[string]any
	if err := yaml.Unmarshal(prepared.Primary, &primary); err != nil {
		t.Fatal(err)
	}
	metadata := primary["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if _, exists := labels[runtopology.QueueLabel]; exists {
		t.Fatalf("gated primary still has queue label: %v", labels)
	}
	if primary["spec"].(map[string]any)["suspend"] != true {
		t.Fatalf("gated primary is not suspended: %v", primary["spec"])
	}
	ancillary := string(prepared.Ancillary)
	if !stringsContainAll(ancillary, "kind: ConfigMap", "kind: SecretProviderClass") ||
		strings.Contains(ancillary, "kind: RayJob") {
		t.Fatalf("ancillary documents =\n%s", ancillary)
	}
	if strings.Count(ancillary, workloadmeta.AnnotationSubmissionID+": submission-managed") != 2 {
		t.Fatalf("ancillary documents are not submission-stamped:\n%s", ancillary)
	}
}

func TestCleanupRunSubmissionUsesIndependentBoundedContextAndChecksIdentity(t *testing.T) {
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(ctx context.Context, args []string, _ []byte) (string, error) {
			if ctx.Err() != nil {
				t.Fatalf("cleanup inherited canceled context: %v", ctx.Err())
			}
			if args[0] == "get" {
				return existingSubmissionJSON("submission-cleanup", ""), nil
			}
			return "", fmt.Errorf("unexpected args: %v", args)
		},
		deleteWithUID: func(ctx context.Context, submission runSubmission, uid types.UID) error {
			if ctx.Err() != nil {
				t.Fatalf("delete inherited canceled context: %v", ctx.Err())
			}
			if submission.Name != "training" || uid != "uid-submission-cleanup" {
				t.Fatalf("delete submission=%+v uid=%q", submission, uid)
			}
			return nil
		},
	}
	if err := cleanupRunSubmission(runner, runSubmission{
		Resource:     "job",
		Name:         "training",
		Namespace:    "research",
		SubmissionID: "submission-cleanup",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithRunSubmissionCleanupReportsRollbackFailure(t *testing.T) {
	runner := cleanupSubmissionRunner{
		submissionRunnerFunc: func(_ context.Context, args []string, _ []byte) (string, error) {
			if args[0] == "get" {
				return existingSubmissionJSON("submission-cleanup", ""), nil
			}
			return "", fmt.Errorf("unexpected args: %v", args)
		},
		deleteWithUID: func(context.Context, runSubmission, types.UID) error {
			return errors.New("API unavailable")
		},
	}
	cause := errors.New("create headless service: AlreadyExists")
	err := withRunSubmissionCleanup(cause, runner, runSubmission{
		Resource:     "job",
		Name:         "training",
		Namespace:    "research",
		SubmissionID: "submission-cleanup",
	})
	if err == nil || !stringsContainAll(err.Error(), cause.Error(), "cleanup failed", "API unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunSubmissionGVRsSupportEveryCleanupResource(t *testing.T) {
	for _, resource := range []string{"job", "jobs.batch", "rayjob.ray.io", "raycluster.ray.io", "service", "secret"} {
		if _, err := runSubmissionGVR(resource); err != nil {
			t.Fatalf("runSubmissionGVR(%q): %v", resource, err)
		}
	}
	if _, err := runSubmissionGVR("configmap"); err == nil {
		t.Fatal("unsupported cleanup resource should fail")
	}
}

func TestActivateRunSubmissionQueueUsesOriginalUID(t *testing.T) {
	gets := 0
	runner := activationSubmissionRunner{
		submissionRunnerFunc: func(_ context.Context, args []string, _ []byte) (string, error) {
			if args[0] != "get" {
				return "", fmt.Errorf("unexpected args: %v", args)
			}
			gets++
			return existingSubmissionMetadataJSON("submission", "uid-original", nil), nil
		},
		activateQueueWithUID: func(_ context.Context, submission runSubmission, uid types.UID, queueName string) (string, error) {
			if submission.Name != "training" || uid != "uid-original" || queueName != "jobqueue" {
				t.Fatalf("activation submission=%+v uid=%q queue=%q", submission, uid, queueName)
			}
			return "activated\n", nil
		},
	}
	out, err := activateRunSubmissionQueue(context.Background(), runner, runSubmission{
		Resource:     "rayjob.ray.io",
		Name:         "training",
		Namespace:    "research",
		SubmissionID: "submission",
	}, "jobqueue")
	if err != nil {
		t.Fatal(err)
	}
	if out != "activated\n" || gets != 1 {
		t.Fatalf("out=%q gets=%d", out, gets)
	}
}

func TestActivateRunSubmissionQueueDoesNotRecoverAcrossReplacementUID(t *testing.T) {
	gets := 0
	runner := activationSubmissionRunner{
		submissionRunnerFunc: func(_ context.Context, args []string, _ []byte) (string, error) {
			if args[0] != "get" {
				return "", fmt.Errorf("unexpected args: %v", args)
			}
			gets++
			if gets == 1 {
				return existingSubmissionMetadataJSON("submission", "uid-original", nil), nil
			}
			return existingSubmissionMetadataJSON("submission", "uid-replacement", map[string]string{
				runtopology.QueueLabel: "jobqueue",
			}), nil
		},
		activateQueueWithUID: func(context.Context, runSubmission, types.UID, string) (string, error) {
			return "", errors.New("JSON patch test failed")
		},
	}
	_, err := activateRunSubmissionQueue(context.Background(), runner, runSubmission{
		Resource:     "rayjob.ray.io",
		Name:         "training",
		Namespace:    "research",
		SubmissionID: "submission",
	}, "jobqueue")
	if err == nil || !strings.Contains(err.Error(), "JSON patch test failed") {
		t.Fatalf("error=%v", err)
	}
}

func TestQueueActivationPatchTestsUIDAndSubmissionID(t *testing.T) {
	patch, err := queueActivationPatch("uid-original", "submission", "jobqueue")
	if err != nil {
		t.Fatal(err)
	}
	var operations []map[string]any
	if err := json.Unmarshal(patch, &operations); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 {
		t.Fatalf("operations=%v", operations)
	}
	if operations[0]["op"] != "test" || operations[0]["path"] != "/metadata/uid" || operations[0]["value"] != "uid-original" {
		t.Fatalf("UID test=%v", operations[0])
	}
	if operations[1]["op"] != "test" || operations[1]["path"] != "/metadata/annotations/tau.azure.com~1submission-id" {
		t.Fatalf("submission test=%v", operations[1])
	}
	if operations[2]["op"] != "add" || operations[2]["path"] != "/metadata/labels/kueue.x-k8s.io~1queue-name" {
		t.Fatalf("queue add=%v", operations[2])
	}
}

func stringsContainAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func existingSubmissionJSON(submissionID, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(
		`{"metadata":{"uid":%q,"annotations":{%q:%q}}%s}`,
		"uid-"+submissionID,
		workloadmeta.AnnotationSubmissionID,
		submissionID,
		extra,
	)
}

func existingSubmissionMetadataJSON(submissionID, uid string, labels map[string]string) string {
	encoded, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"uid": uid,
			"annotations": map[string]string{
				workloadmeta.AnnotationSubmissionID: submissionID,
			},
			"labels": labels,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
