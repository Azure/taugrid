// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/dataset"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Fakes: an injectable kube runner and workspace fetcher so the whole
// render → apply → wait → logs → cleanup flow runs with NO live cluster.
// ---------------------------------------------------------------------------

type fakeRunnerCall struct {
	args  []string
	stdin string
}

type fakeKubeRunner struct {
	calls           []fakeRunnerCall
	succeeded       string // returned for {.status.succeeded}
	failedCondition string // returned for the terminal Failed Job condition
	logs            string // returned for `logs`
}

type retryingJobRunner struct {
	succeededPolls int
	queriedCount   bool
}

func (r *retryingJobRunner) Raw(_ context.Context, extraArgs []string, _ []byte) (string, error) {
	joined := strings.Join(extraArgs, " ")
	switch {
	case strings.Contains(joined, "{.status.succeeded}"):
		r.succeededPolls++
		if r.succeededPolls > 1 {
			return "1", nil
		}
		return "", nil
	case strings.Contains(joined, `@.type=="Failed"`):
		return "", nil
	case strings.Contains(joined, "{.status.failed}"):
		r.queriedCount = true
		return "1", nil
	default:
		return "", nil
	}
}

func newFakeKubeRunner() *fakeKubeRunner {
	// Default: the Job succeeds immediately so waitForJob returns without polling.
	return &fakeKubeRunner{succeeded: "1"}
}

func (f *fakeKubeRunner) Raw(_ context.Context, extraArgs []string, stdin []byte) (string, error) {
	f.calls = append(f.calls, fakeRunnerCall{args: append([]string(nil), extraArgs...), stdin: string(stdin)})
	joined := strings.Join(extraArgs, " ")
	switch {
	case strings.Contains(joined, "apply"):
		return "", nil
	case strings.Contains(joined, "{.status.succeeded}"):
		return f.succeeded, nil
	case strings.Contains(joined, `@.type=="Failed"`):
		return f.failedCondition, nil
	case strings.Contains(joined, "logs"):
		return f.logs, nil
	case strings.Contains(joined, "delete"):
		return "", nil
	default:
		return "", nil
	}
}

// appliedManifests returns the stdin of every `apply` call in order.
func (f *fakeKubeRunner) appliedManifests() []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.args, " "), "apply") {
			out = append(out, c.stdin)
		}
	}
	return out
}

func (f *fakeKubeRunner) sawArg(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.args, " "), sub) {
			return true
		}
	}
	return false
}

// installFakes swaps in the fake runner + workspace and returns a restore func.
func installFakes(t *testing.T, runner datasetKubeRunner, ws tauworkspace.Workspace, wsErr error) {
	t.Helper()
	origRunner := newDatasetKubeRunner
	origFetch := datasetFetchWorkspace
	newDatasetKubeRunner = func(string) datasetKubeRunner { return runner }
	datasetFetchWorkspace = func(context.Context, string, string, string) (tauworkspace.Workspace, error) {
		return ws, wsErr
	}
	t.Cleanup(func() {
		newDatasetKubeRunner = origRunner
		datasetFetchWorkspace = origFetch
	})
}

func readyDatasetWorkspace() tauworkspace.Workspace {
	return tauworkspace.Workspace{
		Metadata: tauworkspace.ObjectMeta{Name: "sample", Generation: 1},
		Spec: tauworkspace.WorkspaceSpec{
			Target: tauworkspace.WorkspaceTarget{Namespace: "sample"},
			WorkloadIdentity: &tauworkspace.WorkspaceWorkloadIdentity{
				ServiceAccountName: "sample-sa",
				ClientID:           "11111111-1111-1111-1111-111111111111",
			},
		},
		Status: tauworkspace.WorkspaceStatus{
			Phase:              "Ready",
			ObservedGeneration: 1,
			Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: "sample"},
		},
	}
}

const testDigestImage = "registry.example.com/taugrid/tau@sha256:" +
	"abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcab"

// ---------------------------------------------------------------------------
// validateWorkspaceForJob
// ---------------------------------------------------------------------------

func TestValidateWorkspaceForJob_Ready(t *testing.T) {
	id, err := validateWorkspaceForJob(readyDatasetWorkspace())
	if err != nil {
		t.Fatalf("ready workspace: %v", err)
	}
	if id.Namespace != "sample" || id.ServiceAccountName != "sample-sa" || id.ClientID == "" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestValidateWorkspaceForJob_NotReady(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Status.Phase = "Pending"
	if _, err := validateWorkspaceForJob(ws); err == nil {
		t.Fatalf("not-Ready workspace must fail")
	}
}

func TestValidateWorkspaceForJob_StaleObservedGeneration(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Metadata.Generation = 2 // status only observed gen 1
	if _, err := validateWorkspaceForJob(ws); err == nil {
		t.Fatalf("stale observedGeneration must fail")
	}
}

func TestValidateWorkspaceForJob_MissingWorkloadIdentity(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Spec.WorkloadIdentity = nil
	if _, err := validateWorkspaceForJob(ws); err == nil {
		t.Fatalf("missing workloadIdentity must fail")
	}
}

func TestValidateWorkspaceForJob_MissingClientID(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Spec.WorkloadIdentity.ClientID = ""
	if _, err := validateWorkspaceForJob(ws); err == nil {
		t.Fatalf("missing clientID must fail")
	}
}

func TestValidateWorkspaceForJob_MissingNamespace(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Status.Target.ResolvedNamespace = ""
	ws.Spec.Target.Namespace = ""
	if _, err := validateWorkspaceForJob(ws); err == nil {
		t.Fatalf("missing namespace must fail")
	}
}

// ---------------------------------------------------------------------------
// renderDatasetWorkerJob: identity + security must always be present.
// ---------------------------------------------------------------------------

func TestRenderDatasetWorkerJob_IdentityAndSecurity(t *testing.T) {
	manifest, err := renderDatasetWorkerJob(datasetWorkerJobSpec{
		JobName:     "tau-ds-ingest-ds-v1",
		Namespace:   "sample",
		ServiceAcct: "sample-sa",
		Image:       testDigestImage,
		DatasetName: "ds",
		Version:     "v1",
		Command:     []string{"tau", "data", "dataset", "ingest-worker", "ds@v1"},
		FlagArgs:    []string{"--registry", "az://acct/ctr"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var job k8sJob
	if err := yaml.Unmarshal(manifest, &job); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Fatalf("wrong kind: %s/%s", job.APIVersion, job.Kind)
	}
	if job.Spec.BackoffLimit != datasetJobBackoffLimit ||
		job.Spec.TTLSecondsAfterFinished != datasetJobTTLSeconds ||
		job.Spec.ActiveDeadlineSeconds != datasetJobDeadlineSeconds {
		t.Fatalf("bounded limits not set: %+v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "sample-sa" {
		t.Fatalf("serviceAccountName not set: %q", pod.ServiceAccountName)
	}
	if pod.RestartPolicy != "Never" {
		t.Fatalf("restartPolicy must be Never, got %q", pod.RestartPolicy)
	}
	if got := job.Spec.Template.Metadata.Labels["azure.workload.identity/use"]; got != "true" {
		t.Fatalf("workload identity label missing: %q", got)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod runAsNonRoot must be true")
	}
	if pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("seccomp RuntimeDefault required")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if got := strings.Join(c.Command, " "); got != "tau data dataset ingest-worker ds@v1" {
		t.Fatalf("worker command = %q, want public dataset command", got)
	}
	if c.Image != testDigestImage {
		t.Fatalf("image not set: %q", c.Image)
	}
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("allowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("readOnlyRootFilesystem must be true")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("container runAsNonRoot must be true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("must drop ALL capabilities, got %+v", sc.Capabilities)
	}
}

func TestRenderDatasetWorkerJob_ConfigMapMount(t *testing.T) {
	manifest, err := renderDatasetWorkerJob(datasetWorkerJobSpec{
		JobName:     "tau-ds-register-ds-v1",
		Namespace:   "sample",
		ServiceAcct: "sample-sa",
		Image:       testDigestImage,
		DatasetName: "ds",
		Version:     "v1",
		Command:     []string{"tau", "data", "dataset", registerWorkerCmdName},
		ConfigMapMount: &datasetConfigMapMount{
			ConfigMapName: "tau-ds-register-ds-v1",
			MountPath:     datasetRecordMountPath,
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var job k8sJob
	if err := yaml.Unmarshal(manifest, &job); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pod := job.Spec.Template.Spec
	var foundVol, foundMountRO bool
	for _, v := range pod.Volumes {
		if v.Name == "record" && v.ConfigMap != nil && v.ConfigMap.Name == "tau-ds-register-ds-v1" {
			foundVol = true
		}
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "record" && m.MountPath == datasetRecordMountPath && m.ReadOnly {
			foundMountRO = true
		}
	}
	if !foundVol || !foundMountRO {
		t.Fatalf("read-only record ConfigMap mount not rendered: vol=%v mount=%v", foundVol, foundMountRO)
	}
}

// ---------------------------------------------------------------------------
// datasetJobName: deterministic + DNS-1123 bounded.
// ---------------------------------------------------------------------------

func TestDatasetJobName_DeterministicAndBounded(t *testing.T) {
	a := datasetJobName("tau-ds-ingest", "ds", "v1")
	b := datasetJobName("tau-ds-ingest", "ds", "v1")
	if a != b {
		t.Fatalf("names must be deterministic: %q vs %q", a, b)
	}

	long := datasetJobName("tau-ds-ingest", strings.Repeat("x", 200), "v1")
	if len(long) > 63 {
		t.Fatalf("name exceeds 63 chars: %d", len(long))
	}
	if strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Fatalf("name must not have leading/trailing dash: %q", long)
	}
}

func TestDatasetRunJobName_UniqueAndBounded(t *testing.T) {
	a := datasetRunJobName("tau-ds-ingest", "ds", "v1")
	b := datasetRunJobName("tau-ds-ingest", "ds", "v1")
	if a == b {
		t.Fatalf("worker attempts must use distinct Job names: %q", a)
	}

	if len(a) > 63 || len(b) > 63 {
		t.Fatalf("worker Job name exceeds 63 characters: %q %q", a, b)
	}
}

func TestWaitForJob_IgnoresRetryableFailedPodCount(t *testing.T) {
	runner := &retryingJobRunner{}
	phase, err := waitForJobPolling(
		context.Background(), runner, "ns", "job", time.Second, time.Millisecond,
	)
	if err != nil {
		t.Fatalf("waitForJobPolling: %v", err)
	}
	if phase != "Succeeded" {
		t.Fatalf("phase = %q, want Succeeded", phase)
	}
	if runner.queriedCount {
		t.Fatal("waitForJob must not use the cumulative failed-pod count as terminal state")
	}
}

// ---------------------------------------------------------------------------
// runIngestWorkspace: dry-run renders but does NOT apply.
// ---------------------------------------------------------------------------

func ingestTestRecord() dataset.Record {
	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          "ds",
		Version:       "v1",
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Prefix:        "ds/v1",
		Files:         []dataset.File{{Path: "a.bin", Bytes: 4, SHA256: strings.Repeat("a", 64)}},
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()
	return rec
}

func TestRunIngestWorkspace_DryRunDoesNotApply(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runIngestWorkspace(context.Background(), &out, &errOut,
		"ds", "v1",
		"az://acct/ctr/ds/v1", "az://acct/ctr/ds/v1", testDigestImage, "sample",
		"az://acct/ctr", "", "tau-system", false, true, "table")
	if err != nil {
		t.Fatalf("dry-run ingest: %v", err)
	}

	if len(runner.calls) != 0 {
		t.Fatalf("dry-run must not call the runner, got %d calls", len(runner.calls))
	}
	// Rendered manifest must carry the security/identity fields.
	if !strings.Contains(out.String(), "azure.workload.identity/use") ||
		!strings.Contains(out.String(), "serviceAccountName: sample-sa") ||
		!strings.Contains(out.String(), "restartPolicy: Never") {
		t.Fatalf("dry-run manifest missing identity/security:\n%s", out.String())
	}
	var job k8sJob
	if err := yaml.Unmarshal(out.Bytes(), &job); err != nil {
		t.Fatalf("parse dry-run Job: %v", err)
	}
	if job.Spec.ActiveDeadlineSeconds != datasetIngestDeadline {
		t.Fatalf("ingest deadline = %d, want %d", job.Spec.ActiveDeadlineSeconds, datasetIngestDeadline)
	}
	if got := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "); got != "tau data dataset ingest-worker ds@v1" {
		t.Fatalf("ingest worker command = %q, want public dataset command", got)
	}
}

func TestDatasetIngestWorkspace_DoesNotOpenCallerRegistry(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	originalRegistryClient := newDatasetIngestRegistryClient
	newDatasetIngestRegistryClient = func(registryFlags) (*dataset.Registry, error) {
		t.Fatal("workspace ingest must not instantiate or read the caller registry")
		return nil, nil
	}
	t.Cleanup(func() { newDatasetIngestRegistryClient = originalRegistryClient })

	cmd := newDatasetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", "az://registry-account/records",
		"--source-root", "https://huggingface.co/datasets/org/ds/resolve/main",
		"--workspace", "sample",
		"--worker-image", testDigestImage,
		"--dry-run",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workspace dry-run: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("workspace dry-run must not schedule a Job")
	}
	if strings.Contains(out.String(), "--destination") {
		t.Fatalf("omitted destination must be resolved by the worker, not caller: %s", out.String())
	}
}

func TestDatasetIngestWorkspace_SchedulingDoesNotOpenCallerRegistry(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	originalRegistryClient := newDatasetIngestRegistryClient
	newDatasetIngestRegistryClient = func(registryFlags) (*dataset.Registry, error) {
		t.Fatal("workspace ingest must not instantiate or read the caller registry")
		return nil, nil
	}
	t.Cleanup(func() { newDatasetIngestRegistryClient = originalRegistryClient })

	cmd := newDatasetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", "az://registry-account/records",
		"--source-root", "https://huggingface.co/datasets/org/ds/resolve/main",
		"--workspace", "sample",
		"--worker-image", testDigestImage,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workspace scheduling: %v", err)
	}
	if len(runner.appliedManifests()) != 1 {
		t.Fatalf("workspace ingest must schedule exactly one Job, got %+v", runner.calls)
	}
}

func TestRunIngestWorkspace_WaitSuccessBuildsResultWithReference(t *testing.T) {
	runner := newFakeKubeRunner()
	rec := ingestTestRecord()
	wout := datasetIngestResult{
		SchemaVersion: datasetIngestResultSchemaVersion,
		Status: dataset.IngestStatus{
			SchemaVersion: dataset.IngestStatusSchemaVersion,
			Name:          "ds",
			Version:       "v1",
			State:         dataset.IngestStateReady,
			RecordDigest:  rec.Digest,
			VerifiedFiles: 1,
			VerifiedBytes: 4,
			CompletedFiles: []dataset.FileProof{{
				Path: "a.bin", SHA256: strings.Repeat("a", 64), Bytes: 4,
				CommittedAt: time.Now().UTC().Format(time.RFC3339),
			}},
		},
		Reference: dataset.BuildReference(rec, dataset.ReferenceOptions{}),
	}
	logsJSON, _ := json.Marshal(wout)
	runner.logs = "some warning line\n" + string(logsJSON)
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runIngestWorkspace(context.Background(), &out, &errOut,
		"ds", "v1",
		"az://acct/ctr/ds/v1", "az://acct/ctr/ds/v1", testDigestImage, "sample",
		"az://acct/ctr", "", "tau-system", true, false, "json")
	if err != nil {
		t.Fatalf("wait ingest: %v", err)
	}
	// Command sequence: apply, get succeeded, logs.
	if !runner.sawArg("apply") || !runner.sawArg("{.status.succeeded}") || !runner.sawArg("logs") {
		t.Fatalf("expected apply/get/logs sequence, got %+v", runner.calls)
	}
	var result datasetIngestResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, out.String())
	}
	if result.Status.State != dataset.IngestStateReady {
		t.Fatalf("status state: %q", result.Status.State)
	}
	if result.Reference.Digest != rec.Digest || result.Reference.Digest == "" {
		t.Fatalf("result must carry resolved reference digest: got %q want %q", result.Reference.Digest, rec.Digest)
	}
}

func TestRunIngestWorkspace_FailedJobSurfacesLogs(t *testing.T) {
	runner := newFakeKubeRunner()
	runner.succeeded = ""
	runner.failedCondition = "True"
	runner.logs = "worker crashed: destination mismatch"
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runIngestWorkspace(context.Background(), &out, &errOut,
		"ds", "v1",
		"az://acct/ctr/ds/v1", "az://acct/ctr/ds/v1", testDigestImage, "sample",
		"az://acct/ctr", "", "tau-system", true, false, "table")
	if err == nil {
		t.Fatalf("failed job must error")
	}
	if !strings.Contains(errOut.String(), "worker crashed") {
		t.Fatalf("failed job must surface worker logs, got:\n%s", errOut.String())
	}
}

func TestRunIngestWorkspace_NotReadyWorkspaceFails(t *testing.T) {
	ws := readyDatasetWorkspace()
	ws.Status.Phase = "Pending"
	runner := newFakeKubeRunner()
	installFakes(t, runner, ws, nil)

	var out, errOut bytes.Buffer
	err := runIngestWorkspace(context.Background(), &out, &errOut,
		"ds", "v1",
		"az://acct/ctr/ds/v1", "az://acct/ctr/ds/v1", testDigestImage, "sample",
		"az://acct/ctr", "", "tau-system", true, false, "table")
	if err == nil {
		t.Fatalf("not-Ready workspace must fail")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no Job should be applied for a not-Ready workspace")
	}
}

// ---------------------------------------------------------------------------
// Direct local ingest validates an explicit destination against the immutable
// record before it runs.
// ---------------------------------------------------------------------------

func TestDatasetIngest_DestinationMismatchFailsBeforeApply(t *testing.T) {
	_, root := fileRegistryWithIngest(t)
	reg := dataset.NewRegistry(newFileBackend(root), datasetRegistryPaths(), nil)
	ingestRecord(t, reg, "ds", "v1", []dataset.File{{Path: "a.bin", Bytes: 4, SHA256: strings.Repeat("a", 64)}})

	// A fake runner that fails the test if it is ever called (no apply must happen).
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	cmd := newDatasetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", "file://" + root,
		"--source-root", "file:///nonexistent",
		"--destination", "az://wrong/ctr/ds/v1",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "does not match immutable record location") {
		t.Fatalf("mismatching destination must fail with record-mismatch error, got: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no Job should be applied when destination mismatches")
	}
}

// ---------------------------------------------------------------------------
// runStatusWorkspace: reads IngestStatus from worker logs.
// ---------------------------------------------------------------------------

func validReadyStatus(name, version string) dataset.IngestStatus {
	return dataset.IngestStatus{
		SchemaVersion: dataset.IngestStatusSchemaVersion,
		Name:          name,
		Version:       version,
		State:         dataset.IngestStateReady,
		RecordDigest:  "sha256:" + strings.Repeat("d", 64),
		VerifiedFiles: 1,
		VerifiedBytes: 4,
		CompletedFiles: []dataset.FileProof{{
			Path: "a.bin", SHA256: strings.Repeat("a", 64), Bytes: 4,
			CommittedAt: time.Now().UTC().Format(time.RFC3339),
		}},
	}
}

func TestRunStatusWorkspace_ReadsStatusFromLogs(t *testing.T) {
	runner := newFakeKubeRunner()
	statusJSON, _ := json.Marshal(validReadyStatus("ds", "v1"))
	runner.logs = "log noise\n" + string(statusJSON)
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	rf := registryFlags{registry: "az://acct/ctr"}
	var errOut bytes.Buffer
	status, err := runStatusWorkspace(context.Background(), &errOut, "ds", "v1", rf, "sample", testDigestImage, true)
	if err != nil {
		t.Fatalf("status workspace: %v", err)
	}
	if status.State != dataset.IngestStateReady || status.Name != "ds" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if !runner.sawArg("apply") || !runner.sawArg("logs") {
		t.Fatalf("status must apply a Job and read logs")
	}
	applied := runner.appliedManifests()
	var job k8sJob
	if len(applied) != 1 {
		t.Fatalf("status must apply one Job, got %d manifests", len(applied))
	}
	if err := yaml.Unmarshal([]byte(applied[0]), &job); err != nil {
		t.Fatalf("parse status Job: %v", err)
	}
	if got := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "); got != "tau data dataset status-worker ds@v1" {
		t.Fatalf("status worker command = %q, want public dataset command", got)
	}
}

func TestRunStatusWorkspace_RejectsMutableImage(t *testing.T) {
	rf := registryFlags{registry: "az://acct/ctr"}
	var errOut bytes.Buffer
	_, err := runStatusWorkspace(context.Background(), &errOut, "ds", "v1", rf, "sample", "tau:latest", true)
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("mutable image must be rejected: %v", err)
	}
}

func TestRunStatusWorkspace_RequiresAzRegistry(t *testing.T) {
	rf := registryFlags{registry: "pvc"}
	var errOut bytes.Buffer
	_, err := runStatusWorkspace(context.Background(), &errOut, "ds", "v1", rf, "sample", testDigestImage, true)
	if err == nil || !strings.Contains(err.Error(), "az://") {
		t.Fatalf("workspace status must require az:// registry: %v", err)
	}
}

func TestRunStatusWorkspace_RequiresWait(t *testing.T) {
	rf := registryFlags{registry: "az://acct/ctr"}
	var errOut bytes.Buffer
	_, err := runStatusWorkspace(context.Background(), &errOut, "ds", "v1", rf, "sample", testDigestImage, false)
	if err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("workspace status must require --wait: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runRegisterWorkspace: idempotence/drift/cleanup/size-limit/non-wait.
// ---------------------------------------------------------------------------

func registerTestRecord() dataset.Record {
	return dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          "ds",
		Version:       "v1",
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Prefix:        "ds/v1",
		Files:         []dataset.File{{Path: "a.bin", Bytes: 4, SHA256: strings.Repeat("a", 64)}},
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
}

func TestRunRegisterWorkspace_SuccessAppliesAndCleansUp(t *testing.T) {
	runner := newFakeKubeRunner()
	res := datasetRegisterResult{SchemaVersion: 1, Name: "ds", Version: "v1", Digest: "sha256:" + strings.Repeat("d", 64), Created: true}
	resJSON, _ := json.Marshal(res)
	runner.logs = string(resJSON)
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, true, false, "table")
	if err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	// Both ConfigMap and Job must be applied.
	applied := runner.appliedManifests()
	if len(applied) != 2 {
		t.Fatalf("want 2 apply calls (ConfigMap + Job), got %d", len(applied))
	}
	if !strings.Contains(applied[0], "kind: ConfigMap") {
		t.Fatalf("first apply must be the ConfigMap:\n%s", applied[0])
	}
	if !strings.Contains(applied[1], "kind: Job") {
		t.Fatalf("second apply must be the Job:\n%s", applied[1])
	}
	var job k8sJob
	if err := yaml.Unmarshal([]byte(applied[1]), &job); err != nil {
		t.Fatalf("parse register Job: %v", err)
	}
	if got := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "); got != "tau data dataset register-worker" {
		t.Fatalf("register worker command = %q, want public dataset command", got)
	}
	// Transient ConfigMap must be cleaned up.
	if !runner.sawArg("delete configmap/") && !runner.sawArg("configmap/") {
		t.Fatalf("transient ConfigMap must be deleted; calls=%+v", runner.calls)
	}
	if !strings.Contains(out.String(), "registered ds@v1") {
		t.Fatalf("expected registered output, got:\n%s", out.String())
	}
}

func TestRunRegisterWorkspace_IdempotentNoOp(t *testing.T) {
	runner := newFakeKubeRunner()
	res := datasetRegisterResult{SchemaVersion: 1, Name: "ds", Version: "v1", Digest: "sha256:" + strings.Repeat("d", 64), Created: false}
	resJSON, _ := json.Marshal(res)
	runner.logs = string(resJSON)
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, true, false, "table")
	if err != nil {
		t.Fatalf("idempotent register: %v", err)
	}
	if !strings.Contains(out.String(), "already registered") {
		t.Fatalf("idempotent no-op must be reported, got:\n%s", out.String())
	}
}

func TestRunRegisterWorkspace_DriftFails(t *testing.T) {
	runner := newFakeKubeRunner()
	runner.succeeded = ""
	runner.failedCondition = "True"
	runner.logs = "dataset ds@v1 already registered with digest sha256:aaa; drift detected (versions are immutable)"
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, true, false, "table")
	if err == nil {
		t.Fatalf("drift (failed Job) must error")
	}
	if !strings.Contains(errOut.String(), "drift detected") {
		t.Fatalf("drift error must surface worker logs, got:\n%s", errOut.String())
	}
	// Even on failure, the transient ConfigMap must be cleaned up.
	if !runner.sawArg("configmap/") {
		t.Fatalf("ConfigMap must be cleaned up even on failure")
	}
}

func TestRunRegisterWorkspace_NonWaitRefused(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, false, false, "table")
	if err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("non-wait workspace register must be refused: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("nothing should be applied when refused")
	}
}

func TestRunRegisterWorkspace_DryRunRendersNoApply(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, false, true, "table")
	if err != nil {
		t.Fatalf("dry-run register: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("dry-run must not apply anything, got %d calls", len(runner.calls))
	}
	if !strings.Contains(out.String(), "kind: ConfigMap") || !strings.Contains(out.String(), "kind: Job") {
		t.Fatalf("dry-run must render both ConfigMap and Job:\n%s", out.String())
	}
}

func TestRunRegisterWorkspace_SizeLimitEnforced(t *testing.T) {
	rec := registerTestRecord()
	// Build a record whose JSON exceeds the ConfigMap transport limit.
	rec.Files = nil
	for i := 0; i < 6000; i++ {
		rec.Files = append(rec.Files, dataset.File{
			Path:   "shard-" + strings.Repeat("x", 20) + "-" + itoa(i) + ".bin",
			Bytes:  2,
			SHA256: strings.Repeat("a", 64),
		})
	}
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		rec, registryFlags{registry: "az://acct/ctr"},
		"sample", testDigestImage, true, false, "table")
	if err == nil || !strings.Contains(err.Error(), "ConfigMap transport limit") {
		t.Fatalf("oversized record must be rejected by size limit: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("nothing should be applied when the record is too large")
	}
}

func TestRunRegisterWorkspace_RejectsMutableImage(t *testing.T) {
	runner := newFakeKubeRunner()
	installFakes(t, runner, readyDatasetWorkspace(), nil)
	var out, errOut bytes.Buffer
	err := runRegisterWorkspace(context.Background(), &out, &errOut,
		registerTestRecord(), registryFlags{registry: "az://acct/ctr"},
		"sample", "tau:latest", true, false, "table")
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("mutable image must be rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// register command wiring: --workspace --manifest end-to-end (no cluster).
// ---------------------------------------------------------------------------

func TestDatasetRegisterCommand_WorkspaceManifest(t *testing.T) {
	// Write a minimal manifest.
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	man := datasetManifest{
		Files: []manifestFile{{Path: "a.bin", Bytes: 4, SHA256: strings.Repeat("a", 64)}},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	runner := newFakeKubeRunner()
	res := datasetRegisterResult{SchemaVersion: 1, Name: "ds", Version: "v1", Digest: "sha256:" + strings.Repeat("d", 64), Created: true}
	resJSON, _ := json.Marshal(res)
	runner.logs = string(resJSON)
	installFakes(t, runner, readyDatasetWorkspace(), nil)

	cmd := newDatasetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"register", "ds@v1",
		"--purpose", "pretrain",
		"--manifest", manifestPath,
		"--account", "acct", "--container", "ctr", "--prefix", "ds/v1",
		"--tokenizer", "gpt2",
		"--workspace", "sample",
		"--worker-image", testDigestImage,
		"--registry", "az://acct/ctr",
		"--wait",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("register --workspace: %v\n%s", err, out.String())
	}
	if !runner.sawArg("apply") {
		t.Fatalf("register --workspace must apply manifests")
	}
	if !runner.sawArg("configmap/") {
		t.Fatalf("register --workspace must clean up the transient ConfigMap")
	}
}

// itoa avoids importing strconv just for a tiny helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
