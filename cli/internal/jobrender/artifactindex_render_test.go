// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobrender

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// The `tau run --config` engine:job path must emit the artifact index that
// `tau serve deploy --from-finetune` reads, or CLI-only train->serve is broken.
func TestRenderEmitsArtifactIndexWhenCheckpointDeclared(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:               "idx-job",
		Namespace:          "ray",
		Command:            []string{"python", "train.py"},
		PVCMount:           "blob-training",
		CheckpointArtifact: "last.safetensors",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "artifacts.json") {
		t.Fatalf("rendered Job does not write artifacts.json:\n%s", s)
	}
	if !strings.Contains(s, "last.safetensors") {
		t.Errorf("declared checkpoint name absent from render")
	}
	// The wrap is `bash -lc 'set -e; "$@"; <index>' -- tau-entrypoint python train.py,
	// so the training command reaches argv BEFORE the index text in the -c
	// string. The property that matters is execution order inside that string:
	// `"$@"` must precede the index, and `set -e` must precede both so a failed
	// training run never reaches the bookkeeping step.
	setE := strings.Index(s, "set -e")
	dollarAt := strings.Index(s, `"$@"`)
	idx := strings.Index(s, "artifacts.json")
	if setE < 0 || dollarAt < 0 {
		t.Fatalf("expected a `set -e` + `\"$@\"` wrap, got:\n%s", s)
	}
	if !(setE < dollarAt && dollarAt < idx) {
		t.Errorf("wrap order wrong: set -e=%d, \"$@\"=%d, index=%d", setE, dollarAt, idx)
	}
	parseYAML(t, out)
}

func TestRenderOmitsArtifactIndexWhenNoCheckpointDeclared(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "idx-job",
		Namespace: "ray",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "artifacts.json") {
		t.Fatalf("artifact index emitted without a declared checkpoint")
	}
}

// The declared checkpoint has to be legible from the workload itself, not only
// from the entrypoint text. Without it no later command can distinguish "this
// run produced no artifacts" from "this run promised an artifact and did not
// deliver one", which is what makes BUG-26 read as an ordinary empty result.
func TestRenderAnnotatesDeclaredCheckpoint(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:               "idx-job",
		Namespace:          "ray",
		Command:            []string{"python", "train.py"},
		PVCMount:           "blob-training",
		CheckpointArtifact: "last.safetensors",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := jobAnnotation(t, out, workloadmeta.AnnotationCheckpointArtifact); got != "last.safetensors" {
		t.Errorf("%s = %q, want %q", workloadmeta.AnnotationCheckpointArtifact, got, "last.safetensors")
	}
}

// The annotation must be absent, not empty: `tau run get` treats presence as
// "a checkpoint was declared", so an empty-string annotation would make every
// run without a checkpoint look like a run whose index went missing.
func TestRenderOmitsCheckpointAnnotationWhenNoneDeclared(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "idx-job",
		Namespace: "ray",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), workloadmeta.AnnotationCheckpointArtifact) {
		t.Errorf("checkpoint annotation present without a declared checkpoint:\n%s", out)
	}
}

// Options is passed by value but carries a map, whose header is shared with
// the caller. Stamping the annotation through o.Annotations would leak it onto
// the caller's next render — and because run get treats presence as the
// declaration, that second run would be reported as having promised a
// checkpoint it never declared.
func TestRenderDoesNotLeakCheckpointAnnotationIntoCallerOptions(t *testing.T) {
	shared := map[string]string{workloadmeta.AnnotationTauCommand: "tau run"}
	base := Options{
		Namespace:   "ray",
		Command:     []string{"python", "train.py"},
		PVCMount:    "blob-training",
		Annotations: shared,
	}

	withCheckpoint := base
	withCheckpoint.Name = "first"
	withCheckpoint.CheckpointArtifact = "last.safetensors"
	if _, err := Render(trainProfile(), withCheckpoint); err != nil {
		t.Fatalf("render with checkpoint: %v", err)
	}
	if got, ok := shared[workloadmeta.AnnotationCheckpointArtifact]; ok {
		t.Errorf("Render wrote %s=%q into the caller's map", workloadmeta.AnnotationCheckpointArtifact, got)
	}

	withoutCheckpoint := base
	withoutCheckpoint.Name = "second"
	out, err := Render(trainProfile(), withoutCheckpoint)
	if err != nil {
		t.Fatalf("render without checkpoint: %v", err)
	}
	if got := jobAnnotation(t, out, workloadmeta.AnnotationCheckpointArtifact); got != "" {
		t.Errorf("second render carries %s=%q from the first render", workloadmeta.AnnotationCheckpointArtifact, got)
	}
}

func jobAnnotation(t *testing.T, manifest []byte, key string) string {
	t.Helper()
	metadata, _ := parseYAML(t, manifest)["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	value, _ := annotations[key].(string)
	return value
}
