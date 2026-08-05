package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/workloadmeta"
)

type fakeRunResultReader struct {
	responses map[string]string
	errors    map[string]error
	calls     []string
}

func (f *fakeRunResultReader) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	resource := args[1]
	f.calls = append(f.calls, resource)
	if err := f.errors[resource]; err != nil {
		return "", err
	}
	return f.responses[resource], nil
}

func TestLooksLikeDirectoryHeuristic(t *testing.T) {
	cases := map[string]bool{
		"/data/j1/results":         true,
		"/data/j1/results/":        true,
		"/data/j1/profile.ncu-rep": false,
		"/data/j1/metrics.json":    false,
	}
	for path, wantDirectory := range cases {
		if got := looksLikeDirectory(path); got != wantDirectory {
			t.Errorf("looksLikeDirectory(%q)=%v, want %v", path, got, wantDirectory)
		}
	}
}

func TestDecodePVCFileLogsPreservesBinaryArtifacts(t *testing.T) {
	want := []byte{0x00, 0x01, 0x02, 0x0a, 0xff, 0xfe, 'S', 'Q', 'L', 'i', 't', 'e'}
	encoded := base64.StdEncoding.EncodeToString(want)
	wrapped := encoded[:8] + "\n" + encoded[8:] + "\n"
	got, err := decodePVCFileLogs("/data/run/profile/rank-0.sqlite", "training-nfs", wrapped)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("decoded bytes=%v want %v", got, want)
	}
}

func TestRunGetReportsSettledEmptyListingAndPreservesKnownArtifactRead(t *testing.T) {
	const (
		resultPath = "/data/tau-workspaces/default/chaos-horizon/w128"
		pvcName    = "blob-training"
	)

	listCmd := &cobra.Command{}
	var listOut, listErr bytes.Buffer
	listCmd.SetOut(&listOut)
	listCmd.SetErr(&listErr)
	if err := writeRunGet(listCmd, "table", nil, nil, resultPath, pvcName, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listOut.String(), "(empty)") {
		t.Fatalf("listing output = %q", listOut.String())
	}
	if listErr.Len() != 0 {
		t.Fatalf("settled empty listing warning = %q", listErr.String())
	}

	artifact := []byte(`{"width":128,"onestep_rmse":0.00016553813475184143}`)
	artifactCmd := &cobra.Command{}
	var artifactOut, artifactErr bytes.Buffer
	artifactCmd.SetOut(&artifactOut)
	artifactCmd.SetErr(&artifactErr)
	if err := writeRunGet(
		artifactCmd,
		"raw",
		artifact,
		nil,
		resultPath+"/evidence_w128.json",
		pvcName,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifactOut.Bytes(), artifact) {
		t.Fatalf("artifact output = %q, want %q", artifactOut.Bytes(), artifact)
	}
	if artifactErr.Len() != 0 {
		t.Fatalf("artifact warning = %q", artifactErr.String())
	}
}

func TestRunGetJSONRepresentsSettledEmptyDirectory(t *testing.T) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	if err := writeRunGet(cmd, "json", nil, nil, "/data/results", "blob-training", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"entries": null`) {
		t.Fatalf("JSON output does not carry an empty entry set:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("settled empty listing warning = %q", errOut.String())
	}
}

func TestRunResultRefFallsBackFromJobToRayJob(t *testing.T) {
	reader := &fakeRunResultReader{
		responses: map[string]string{
			"job":           "",
			"rayjob.ray.io": `{"metadata":{"annotations":{"` + workloadmeta.AnnotationResultPath + `":"/data/research-workspace/runs/modernbert-ray","` + workloadmeta.AnnotationResultPVC + `":"research-workspace","` + workloadmeta.AnnotationArtifactPublication + `":"staged","` + workloadmeta.AnnotationArtifactPublicationID + `":"publication-1"}}}`,
		},
	}
	ref, err := runResultRefWithReader(context.Background(), reader, "research-workspace", "modernbert-ray")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Path != "/data/research-workspace/runs/modernbert-ray" || ref.PVC != "research-workspace" {
		t.Fatalf("RayJob result ref = %+v", ref)
	}
	if ref.Publication != "staged" {
		t.Fatalf("RayJob publication = %q", ref.Publication)
	}
	if ref.PublicationID != "publication-1" {
		t.Fatalf("RayJob publication ID = %q", ref.PublicationID)
	}
	if strings.Join(reader.calls, ",") != "job,rayjob.ray.io" {
		t.Fatalf("lookup order = %v", reader.calls)
	}
}

func TestRunResultRefExplainsPostDeletionOverrideContract(t *testing.T) {
	reader := &fakeRunResultReader{
		responses: map[string]string{
			"job":           "",
			"rayjob.ray.io": "",
		},
	}

	_, err := runResultRefWithReader(context.Background(), reader, "research-workspace", "deleted-ray")
	if err == nil || !strings.Contains(err.Error(), "pass both --path and --pvc") {
		t.Fatalf("post-deletion error = %v", err)
	}
}

func TestRunResultRefDoesNotMaskJobAuthorizationFailure(t *testing.T) {
	reader := &fakeRunResultReader{
		errors: map[string]error{
			"job": errors.New("jobs.batch is forbidden: user cannot get resource"),
		},
		responses: map[string]string{
			"rayjob.ray.io": `{"metadata":{"annotations":{"` + workloadmeta.AnnotationResultPath + `":"/data/wrong","` + workloadmeta.AnnotationResultPVC + `":"wrong"}}}`,
		},
	}
	_, err := runResultRefWithReader(context.Background(), reader, "research-workspace", "same-name")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("authorization error = %v", err)
	}
	if strings.Join(reader.calls, ",") != "job" {
		t.Fatalf("authorization failure should not fall back to RayJob: calls=%v", reader.calls)
	}
}

func TestRunResultRefRejectsCrossKindNameAmbiguity(t *testing.T) {
	reader := &fakeRunResultReader{responses: map[string]string{
		"job":           `{"metadata":{"annotations":{"` + workloadmeta.AnnotationResultPath + `":"/data/job","` + workloadmeta.AnnotationResultPVC + `":"job-pvc"}}}`,
		"rayjob.ray.io": `{"metadata":{"annotations":{"` + workloadmeta.AnnotationResultPath + `":"/data/ray","` + workloadmeta.AnnotationResultPVC + `":"ray-pvc"}}}`,
	}}
	_, err := runResultRefWithReader(context.Background(), reader, "research-workspace", "same-name")
	if err == nil || !strings.Contains(err.Error(), "both Job and RayJob exist") {
		t.Fatalf("ambiguity error = %v", err)
	}
}

// The declared checkpoint has to survive the annotation read, or run get has
// nothing to key its diagnostic off.
func TestRunResultRefCarriesDeclaredCheckpoint(t *testing.T) {
	reader := &fakeRunResultReader{
		responses: map[string]string{
			"job": `{"metadata":{"annotations":{"` + workloadmeta.AnnotationResultPath + `":"/data/runs/demo","` +
				workloadmeta.AnnotationResultPVC + `":"blob-training","` +
				workloadmeta.AnnotationCheckpointArtifact + `":"last.safetensors"}}}`,
			"rayjob.ray.io": "",
		},
	}
	ref, err := runResultRefWithReader(context.Background(), reader, "demo", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if ref.CheckpointArtifact != "last.safetensors" {
		t.Fatalf("CheckpointArtifact = %q, want %q", ref.CheckpointArtifact, "last.safetensors")
	}
}

// BUG-26's tail: the run reported success, wrote no artifact index, and
// `tau run get` said nothing about the checkpoint the config had declared.
// Naming the declaration is what connects an empty result back to a model that
// was supposed to be servable.
//
// The wording deliberately points at the index rather than asserting it is
// missing because the index lives outside the result path.
func TestRunGetExplainsDeclaredButMissingCheckpoint(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := writeRunGet(cmd, "table", nil, nil, "/data/runs/demo", "blob-training", "last.safetensors"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"last.safetensors", "storage.checkpoint", "artifact index", "--from-finetune"} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q; got:\n%s", want, got)
		}
	}
	// It must not claim the out-of-tree index was never written.
	for _, banned := range []string{"no artifact index was written"} {
		if strings.Contains(got, banned) {
			t.Errorf("output asserts %q, which an empty listing does not establish:\n%s", banned, got)
		}
	}
}

// A run that declared nothing must keep main's plain listing: adding a
// checkpoint note to every empty result would train researchers to ignore it.
func TestRunGetKeepsPlainEmptyWhenNoCheckpointDeclared(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := writeRunGet(cmd, "table", nil, nil, "/data/runs/demo", "blob-training", ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "(empty)") {
		t.Errorf("expected the settled empty listing, got:\n%s", got)
	}
	if strings.Contains(got, "storage.checkpoint") {
		t.Errorf("checkpoint diagnostic shown for a run that declared none:\n%s", got)
	}
}

// A declared checkpoint whose artifacts DID land must not be reported as
// missing — the diagnostic keys off an empty listing, not off the declaration.
func TestRunGetStaysQuietWhenArtifactsPresent(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := writeRunGet(cmd, "table", nil, []string{"last.safetensors"}, "/data/runs/demo", "blob-training", "last.safetensors"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "storage.checkpoint") {
		t.Errorf("missing-checkpoint diagnostic shown for a run with artifacts:\n%s", got)
	}
}
