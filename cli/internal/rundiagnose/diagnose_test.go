package rundiagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/raylogoffload"
)

const (
	testNamespace = "research"
	testRun       = "train-001"
	testTime      = "2026-08-07T20:00:00Z"
)

type fakeRunner struct {
	run func([]string) (string, error)
}

func (f fakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	return f.run(args)
}

func TestGatherJobDiagnosticAndGeneratedFixture(t *testing.T) {
	snapshot, err := Gather(context.Background(), jobFixtureRunner(), testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "found" || len(snapshot.Jobs) != 1 || len(snapshot.RayJobs) != 0 {
		t.Fatalf("run/root objects = %+v jobs=%d rayJobs=%d", snapshot.Run, len(snapshot.Jobs), len(snapshot.RayJobs))
	}
	if len(snapshot.Workloads) != 1 || snapshot.Workloads[0].QueueName != "gpu-training" {
		t.Fatalf("workloads = %+v", snapshot.Workloads)
	}
	if len(snapshot.Pods) != 1 || len(snapshot.Pods[0].Containers) != 2 {
		t.Fatalf("pods = %+v", snapshot.Pods)
	}
	if len(snapshot.Events) != 1 || snapshot.Events[0].Reason != "BackOff" {
		t.Fatalf("events = %+v", snapshot.Events)
	}
	if len(snapshot.Logs) != 2 {
		t.Fatalf("logs = %+v", snapshot.Logs)
	}

	var output bytes.Buffer
	if err := WriteJSON(&output, snapshot); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("testdata", "job-diagnostic.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture, output.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("diagnostic fixture drifted; run UPDATE_GOLDEN=1 go test ./internal/rundiagnose")
	}
}

func TestGatherRayJobUsesOwnedRayClusterPods(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get rayjob.ray.io "+testRun+" -o json"):
			return rayJobJSON(), nil, true
		case strings.Contains(args, "get raycluster.ray.io train-001-cluster -o json"):
			return rayClusterJSON(), nil, true
		case strings.Contains(args, "get pods -l ray.io/cluster=train-001-cluster"):
			pods := podListJSONWithOwner("ray-head", "pod-ray", `{"ray.io/cluster":"train-001-cluster"}`, "ray-head", 0, "Complete", "RayCluster", "raycluster-1")
			return strings.ReplaceAll(pods, "log-offload", raylogoffload.SidecarContainerName), nil, true
		case strings.Contains(args, "logs ray-head -c ray-head"):
			return "ray driver ready\n", nil, true
		case strings.Contains(args, "logs ray-head -c "+raylogoffload.SidecarContainerName):
			return "ray driver output\n", nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "found" || len(snapshot.RayJobs) != 1 || len(snapshot.RayClusters) != 1 || len(snapshot.Pods) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Pods[0].Name != "ray-head" || len(snapshot.Logs) != 2 {
		t.Fatalf("ray pod/logs = %+v / %+v", snapshot.Pods, snapshot.Logs)
	}
	if !hasLogRole(snapshot.Logs, "ray-driver") {
		t.Fatalf("Ray driver log role missing: %+v", snapshot.Logs)
	}
}

func TestGatherExcludesPodsFromPreviousRayClusterUID(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get rayjob.ray.io "+testRun+" -o json"):
			return rayJobJSON(), nil, true
		case strings.Contains(args, "get raycluster.ray.io train-001-cluster -o json"):
			return rayClusterJSON(), nil, true
		case strings.Contains(args, "get pods -l ray.io/cluster=train-001-cluster"):
			return podListJSONWithOwner("stale-ray-head", "pod-ray-old", `{"ray.io/cluster":"train-001-cluster"}`, "ray-head", 0, "", "RayCluster", "raycluster-old"), nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pods) != 0 || !contains(snapshot.Warnings, "owner UID does not match") {
		t.Fatalf("stale Ray pod was attributed to the current RayCluster: %+v", snapshot)
	}
}

func TestGatherIncompleteCreationKeepsOwnedWorkload(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		if strings.Contains(args, "get workloads.kueue.x-k8s.io -l "+workloadmetaLabel()+"="+testRun) {
			return workloadListJSON(), nil, true
		}
		return "", nil, false
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "incomplete" || len(snapshot.Workloads) != 1 {
		t.Fatalf("incomplete snapshot = %+v", snapshot)
	}
	if !contains(snapshot.Warnings, "creation or cleanup may be incomplete") {
		t.Fatalf("warnings = %v", snapshot.Warnings)
	}
}

func TestGatherIncompleteCreationDoesNotTrustPodLabelsWithoutRootUID(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get workloads.kueue.x-k8s.io -l "+workloadmetaLabel()+"="+testRun):
			return workloadListJSON(), nil, true
		case strings.Contains(args, "get pods -l "+workloadmetaLabel()+"="+testRun):
			return podListJSON("unproven-pod", "pod-1", `{"tau.azure.com/job":"train-001"}`, "trainer", 0, ""), nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 1 || len(snapshot.Pods) != 0 {
		t.Fatalf("rootless snapshot trusted pod labels: %+v", snapshot)
	}
	if !contains(snapshot.Warnings, "no current Job/RayJob UID") {
		t.Fatalf("missing ownership warning: %v", snapshot.Warnings)
	}
}

func TestGatherReportsRayClusterPending(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		if strings.Contains(args, "get rayjob.ray.io "+testRun+" -o json") {
			return strings.Replace(rayJobJSON(), `,"rayClusterName":"train-001-cluster"`, "", 1), nil, true
		}
		return "", nil, false
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !hasAccess(snapshot.Access, "raycluster.ray.io/<pending>", "absent") {
		t.Fatalf("pending RayCluster state missing: %+v", snapshot.Access)
	}
}

func TestGatherCapturesFailedSidecarTerminationAndLogs(t *testing.T) {
	runner := jobFixtureRunner()
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	var sidecar ContainerState
	for _, container := range snapshot.Pods[0].Containers {
		if container.Name == "log-offload" {
			sidecar = container
		}
	}
	if sidecar.Current.State != "terminated" || sidecar.Current.ExitCode == nil || *sidecar.Current.ExitCode != 17 {
		t.Fatalf("sidecar state = %+v", sidecar)
	}
	if !logContains(snapshot.Logs, "log-offload", "upload failed") {
		t.Fatalf("sidecar log missing: %+v", snapshot.Logs)
	}
}

func TestGatherMarksAmbiguousStaleRootObjects(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get job "+testRun+" -o json"):
			return jobJSON(), nil, true
		case strings.Contains(args, "get rayjob.ray.io "+testRun+" -o json"):
			return rayJobJSON(), nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "ambiguous" || !contains(snapshot.Warnings, "stale object") {
		t.Fatalf("stale object snapshot = %+v", snapshot)
	}
}

func TestGatherExcludesPodsOwnedByPreviousJobUID(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get job "+testRun+" -o json"):
			return jobJSON(), nil, true
		case strings.Contains(args, "get pods -l job-name="+testRun):
			return `{"items":[{
				"apiVersion":"v1","kind":"Pod",
				"metadata":{"name":"stale-pod","namespace":"research","uid":"pod-old",
					"labels":{"tau.azure.com/job":"train-001"},
					"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"train-001","uid":"job-old"}]},
				"spec":{"containers":[{"name":"trainer","image":"example/train:v0"}]},
				"status":{"phase":"Failed"}
			}]}`, nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pods) != 0 || !contains(snapshot.Warnings, "owner UID does not match") {
		t.Fatalf("stale pod was attributed to the current run: %+v", snapshot)
	}
}

func TestGatherExcludesSameNameObjectWithoutTauOwnership(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		if strings.Contains(args, "get job "+testRun+" -o json") {
			return strings.Replace(
				jobJSON(),
				`"tau.azure.com/managed-by":"tau","tau.azure.com/job":"train-001","kueue.x-k8s.io/queue-name":"gpu-training"`,
				`"app":"other"`,
				1,
			), nil, true
		}
		return "", nil, false
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 0 || !hasAccess(snapshot.Access, "job/"+testRun, "excluded") {
		t.Fatalf("non-Tau object was not excluded: %+v", snapshot)
	}
}

func TestGatherRecordsRBACDenialsInsteadOfFailing(t *testing.T) {
	runner := fakeRunner{run: func(_ []string) (string, error) {
		return "", errors.New("Error from server (Forbidden): user cannot get resource")
	}}
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "unknown" || !hasAccess(snapshot.Access, "job/"+testRun, "forbidden") {
		t.Fatalf("RBAC snapshot = %+v", snapshot)
	}
}

func TestGatherDoesNotMisclassifyLocalExecutableNotFound(t *testing.T) {
	runner := fakeRunner{run: func(args []string) (string, error) {
		return "", errors.New("run kubectl " + strings.Join(args, " ") + `: exec: "kubectl": executable file not found in $PATH`)
	}}
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.State != "unknown" || hasAccess(snapshot.Access, "job/"+testRun, "absent") {
		t.Fatalf("local executable failure was misclassified as absence: %+v", snapshot)
	}
}

func TestGatherIsReadOnlyAndOwnershipScoped(t *testing.T) {
	var calls []string
	base := jobFixtureRunner()
	runner := fakeRunner{run: func(args []string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return base.Raw(context.Background(), args, nil)
	}}
	if _, err := Gather(context.Background(), runner, testRun, testOptions()); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if !strings.Contains(call, " get ") && !strings.Contains(call, " logs ") {
			t.Fatalf("diagnostic issued a non-read-only call: %s", call)
		}
		if strings.Contains(call, " get pods ") && !strings.Contains(call, " -l ") {
			t.Fatalf("pod query is not selector-scoped: %s", call)
		}
		if strings.Contains(call, " get events ") && !strings.Contains(call, "--field-selector involvedObject.uid=") {
			t.Fatalf("event query is not UID-scoped: %s", call)
		}
		if strings.Contains(call, " logs ") &&
			(!strings.Contains(call, "--tail=20") || !strings.Contains(call, "--limit-bytes=4096")) {
			t.Fatalf("log query is not bounded: %s", call)
		}
	}
}

func TestGatherRejectsUnboundedLimits(t *testing.T) {
	tests := []Options{
		{Namespace: testNamespace, TailLines: MaxTailLines + 1, LogLimitBytes: 1, EventLimit: 1},
		{Namespace: testNamespace, TailLines: 1, LogLimitBytes: MaxLogLimitBytes + 1, EventLimit: 1},
		{Namespace: testNamespace, TailLines: 1, LogLimitBytes: 1, EventLimit: MaxEventLimit + 1},
	}
	for _, opts := range tests {
		if _, err := Gather(context.Background(), defaultRunner(func(string) (string, error, bool) {
			return "", nil, false
		}), testRun, opts); err == nil {
			t.Fatalf("Gather accepted unbounded options: %+v", opts)
		}
	}
}

func TestFetchEventsCapsAPISubjects(t *testing.T) {
	calls := 0
	runner := fakeRunner{run: func(_ []string) (string, error) {
		calls++
		return `{"items":[]}`, nil
	}}
	refs := make([]ObjectRef, MaxEventSubjects+10)
	for i := range refs {
		refs[i] = ObjectRef{Kind: "Pod", Name: "pod-" + strconv.Itoa(i), UID: "uid-" + strconv.Itoa(i)}
	}
	snapshot := Snapshot{}
	events := fetchEvents(context.Background(), runner, &snapshot, testNamespace, refs, MaxEventLimit)
	if len(events) != 0 || calls != MaxEventSubjects {
		t.Fatalf("events=%d calls=%d, want 0/%d", len(events), calls, MaxEventSubjects)
	}
	if !contains(snapshot.Warnings, "event collection subjects were truncated") {
		t.Fatalf("warnings = %v", snapshot.Warnings)
	}
}

func TestFetchLogsPrioritizesDriverAndFailedStreams(t *testing.T) {
	pods := make([]Pod, 0, defaultMaxLogStreams+2)
	for i := 0; i < defaultMaxLogStreams; i++ {
		pods = append(pods, Pod{
			ObjectRef: ObjectRef{Name: "healthy-" + strconv.Itoa(i)},
			Containers: []ContainerState{{
				Name:    "trainer",
				Current: StateDetail{State: "running"},
			}},
		})
	}
	pods = append(pods,
		Pod{ObjectRef: ObjectRef{Name: "failed"}, Containers: []ContainerState{{
			Name:    "trainer",
			Current: StateDetail{State: "terminated", Reason: "Error"},
		}}},
		Pod{ObjectRef: ObjectRef{Name: "driver"}, Containers: []ContainerState{{
			Name:    raylogoffload.SidecarContainerName,
			Current: StateDetail{State: "running"},
		}}},
	)
	runner := fakeRunner{run: func(args []string) (string, error) {
		return strings.Join(args, " "), nil
	}}
	snapshot := Snapshot{}
	logs := fetchLogs(context.Background(), runner, &snapshot, testNamespace, pods, testOptions())
	if len(logs) != defaultMaxLogStreams {
		t.Fatalf("logs = %d, want %d", len(logs), defaultMaxLogStreams)
	}
	if !logContains(logs, raylogoffload.SidecarContainerName, "logs driver") ||
		!logContains(logs, "trainer", "logs failed") {
		t.Fatalf("priority logs missing: %+v", logs)
	}
	if !contains(snapshot.Warnings, "prioritized streams") {
		t.Fatalf("truncation warning missing: %v", snapshot.Warnings)
	}
}

func TestWriteTextIncludesRootAndAdmissionEvidence(t *testing.T) {
	snapshot, err := Gather(context.Background(), jobFixtureRunner(), testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteText(&output, snapshot); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, evidence := range []string{
		"Job train-001 uid=job-1",
		`active=1`,
		"clusterQueue=gpu-cq",
		"condition Admitted=True reason=AdmittedByTest",
		"admissionCheck name=multikueue state=Ready",
	} {
		if !strings.Contains(text, evidence) {
			t.Fatalf("text output missing %q:\n%s", evidence, text)
		}
	}
}

func TestRedactionCoversMetadataEventsErrorsAndLogs(t *testing.T) {
	runner := defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get job "+testRun+" -o json"):
			raw := strings.Replace(jobJSON(), `"tau.azure.com/managed-by":"tau"`, `"tau.azure.com/managed-by":"tau","tau.azure.com/password":"metadata-secret"`, 1)
			return raw, nil, true
		case strings.Contains(args, "get pods -l job-name="+testRun):
			return podListJSON("train-pod", "pod-1", `{"tau.azure.com/job":"train-001"}`, "trainer", 0, ""), nil, true
		case strings.Contains(args, "get events"):
			return `{"items":[{"involvedObject":{"kind":"Pod","name":"train-pod","uid":"pod-1"},"reason":"Failed","message":"{\"token\":\"event-secret\"}","lastTimestamp":"2026-08-07T19:59:00Z"}]}`, nil, true
		case strings.Contains(args, "logs train-pod -c trainer"):
			return "Authorization: ApiKey abc.def.ghi\nCookie: session=cookie-secret\n{\"authorization\":\"Basic json-auth-secret\",\"cookie\":\"json-cookie-secret\"}\ncredential=credential-secret\npat=pat-secret\nghp_12345678901234567890\napi_key=log-secret\nurl=https://example.test/?sig=sas-secret&x=1\nhttps://user:url-secret@example.test/\npostgres://user:db-secret@db.example/app\nDATABASE_URL=redis://user:redis-secret@cache.example/0\n", nil, true
		default:
			return "", nil, false
		}
	})
	snapshot, err := Gather(context.Background(), runner, testRun, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"metadata-secret", "event-secret", "abc.def.ghi", "cookie-secret",
		"json-auth-secret", "json-cookie-secret", "credential-secret", "pat-secret", "ghp_12345678901234567890",
		"log-secret", "sas-secret", "url-secret", "db-secret", "redis-secret",
	} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("secret %q leaked in %s", secret, raw)
		}
	}
	if !bytes.Contains(raw, []byte("[REDACTED]")) {
		t.Fatalf("redaction marker missing in %s", raw)
	}
}

func TestRedactCoversTruncatedPrivateKey(t *testing.T) {
	input := "prefix\n-----BEGIN PRIVATE KEY-----\npartial-private-material"
	redacted := Redact(input)
	if strings.Contains(redacted, "partial-private-material") || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("truncated private key leaked: %q", redacted)
	}
}

func jobFixtureRunner() Runner {
	return defaultRunner(func(args string) (string, error, bool) {
		switch {
		case strings.Contains(args, "get job "+testRun+" -o json"):
			return jobJSON(), nil, true
		case strings.Contains(args, "get workloads.kueue.x-k8s.io"):
			return workloadListJSON(), nil, true
		case strings.Contains(args, "get pods -l job-name="+testRun), strings.Contains(args, "get pods -l "+workloadmetaLabel()+"="+testRun):
			return podListJSON("train-pod", "pod-1", `{"tau.azure.com/job":"train-001"}`, "trainer", 17, "Error"), nil, true
		case strings.Contains(args, "get events") && strings.Contains(args, "pod-1"):
			return `{"items":[{"involvedObject":{"kind":"Pod","name":"train-pod","uid":"pod-1"},"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container","count":3,"firstTimestamp":"2026-08-07T19:58:00Z","lastTimestamp":"2026-08-07T19:59:00Z"}]}`, nil, true
		case strings.Contains(args, "logs train-pod -c log-offload"):
			return "upload failed\n", nil, true
		case strings.Contains(args, "logs train-pod -c trainer"):
			return "step=42 loss=0.12\n", nil, true
		default:
			return "", nil, false
		}
	})
}

func defaultRunner(override func(string) (string, error, bool)) Runner {
	return fakeRunner{run: func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		if output, err, ok := override(joined); ok {
			return output, err
		}
		switch {
		case strings.Contains(joined, " get job "+testRun+" -o json"):
			return "", errors.New(`Error from server (NotFound): jobs.batch "` + testRun + `" not found`)
		case strings.Contains(joined, " get rayjob.ray.io "+testRun+" -o json"):
			return "", errors.New(`Error from server (NotFound): rayjobs.ray.io "` + testRun + `" not found`)
		case strings.Contains(joined, " get ") && strings.Contains(joined, " -o json"):
			return `{"items":[]}`, nil
		case strings.Contains(joined, " logs "):
			return "", nil
		default:
			return "", errors.New("unexpected call: " + joined)
		}
	}}
}

func testOptions() Options {
	now, _ := time.Parse(time.RFC3339, testTime)
	return Options{
		Namespace:     testNamespace,
		Context:       "research-cluster",
		TailLines:     20,
		LogLimitBytes: 4096,
		EventLimit:    10,
		Now:           func() time.Time { return now },
	}
}

func jobJSON() string {
	return `{
		"apiVersion":"batch/v1","kind":"Job",
		"metadata":{"name":"train-001","namespace":"research","uid":"job-1","generation":2,"creationTimestamp":"2026-08-07T19:50:00Z",
			"labels":{"tau.azure.com/managed-by":"tau","tau.azure.com/job":"train-001","kueue.x-k8s.io/queue-name":"gpu-training"},
			"annotations":{"tau.azure.com/image":"example.azurecr.io/train:v1","tau.azure.com/workloadspec-execution":"must-not-appear"}},
		"spec":{"suspend":false,"parallelism":1,"template":{"spec":{"containers":[{"name":"trainer","env":[{"name":"TOKEN","value":"must-not-appear"}]}]}}},
		"status":{"active":1,"startTime":"2026-08-07T19:51:00Z"}
	}`
}

func rayJobJSON() string {
	return `{
		"apiVersion":"ray.io/v1","kind":"RayJob",
		"metadata":{"name":"train-001","namespace":"research","uid":"ray-1","creationTimestamp":"2026-08-07T19:50:00Z",
			"labels":{"tau.azure.com/managed-by":"tau","tau.azure.com/job":"train-001"}},
		"spec":{"managedBy":"ray.io/kuberay-operator","rayClusterSpec":{"headGroupSpec":{"template":{"spec":{"containers":[{"env":[{"name":"TOKEN","value":"must-not-appear"}]}]}}}}},
		"status":{"jobDeploymentStatus":"Running","rayClusterName":"train-001-cluster","jobId":"raysubmit_abc"}
	}`
}

func rayClusterJSON() string {
	return `{
		"apiVersion":"ray.io/v1","kind":"RayCluster",
		"metadata":{"name":"train-001-cluster","namespace":"research","uid":"raycluster-1","creationTimestamp":"2026-08-07T19:50:01Z",
			"ownerReferences":[{"apiVersion":"ray.io/v1","kind":"RayJob","name":"train-001","uid":"ray-1"}]},
		"status":{"state":"ready","readyWorkerReplicas":1}
	}`
}

func workloadListJSON() string {
	return `{"items":[{
		"apiVersion":"kueue.x-k8s.io/v1beta1","kind":"Workload",
		"metadata":{"name":"job-train-001-a1b2","namespace":"research","uid":"wl-1","creationTimestamp":"2026-08-07T19:50:01Z",
			"labels":{"tau.azure.com/job":"train-001"},"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"train-001","uid":"job-1"}]},
		"spec":{"queueName":"gpu-training","priorityClassName":"normal"},
		"status":{"admission":{"clusterQueue":"gpu-cq"},"admissionChecks":[{"name":"multikueue","state":"Ready","message":"reservation active"}],"conditions":[{"type":"Admitted","status":"True","reason":"AdmittedByTest","lastTransitionTime":"2026-08-07T19:50:03Z"}]}
	}]}`
}

func podListJSON(name, uid, labels, mainContainer string, sidecarExit int, sidecarReason string) string {
	return podListJSONWithOwner(name, uid, labels, mainContainer, sidecarExit, sidecarReason, "Job", "job-1")
}

func podListJSONWithOwner(name, uid, labels, mainContainer string, sidecarExit int, sidecarReason, ownerKind, ownerUID string) string {
	sidecar := ""
	sidecarSpec := ""
	if sidecarReason != "" {
		sidecarSpec = `,{"name":"log-offload","image":"example/log-offload:v1"}`
		sidecar = `,{"name":"log-offload","image":"example/log-offload:v1","ready":false,"restartCount":0,"state":{"terminated":{"reason":"` + sidecarReason + `","message":"upload failed","exitCode":` + strconv.Itoa(sidecarExit) + `,"startedAt":"2026-08-07T19:51:00Z","finishedAt":"2026-08-07T19:52:00Z"}}}`
	}
	return `{"items":[{
		"apiVersion":"v1","kind":"Pod",
		"metadata":{"name":"` + name + `","namespace":"research","uid":"` + uid + `","creationTimestamp":"2026-08-07T19:50:02Z","labels":` + labels + `,"ownerReferences":[{"apiVersion":"v1","kind":"` + ownerKind + `","name":"train-001","uid":"` + ownerUID + `"}]},
		"spec":{"nodeName":"gpu-node-1","containers":[{"name":"` + mainContainer + `","image":"example/train:v1"}` + sidecarSpec + `]},
		"status":{"phase":"Running","containerStatuses":[{"name":"` + mainContainer + `","image":"example/train:v1","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-08-07T19:51:00Z"}}}` + sidecar + `]}
	}]}`
}

func workloadmetaLabel() string { return "tau.azure.com/job" }

func contains(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func hasAccess(values []Access, resource, status string) bool {
	for _, value := range values {
		if value.Resource == resource && value.Status == status {
			return true
		}
	}
	return false
}

func logContains(values []ContainerLog, container, fragment string) bool {
	for _, value := range values {
		if value.Container == container && strings.Contains(value.Text, fragment) {
			return true
		}
	}
	return false
}

func hasLogRole(values []ContainerLog, role string) bool {
	for _, value := range values {
		if value.Role == role {
			return true
		}
	}
	return false
}
