// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// The `tau run --config` engine:rayjob path must emit the artifact index that
// `tau serve deploy --from-finetune` reads, or CLI-only train->serve is broken.
func TestRenderEmitsArtifactIndexWhenCheckpointDeclared(t *testing.T) {
	out, err := Render(Options{
		Name:               "idx-ray",
		Namespace:          "ray",
		ScriptName:         "train.py",
		Script:             []byte("print('train')\n"),
		Workers:            1,
		DataPVC:            "training-data",
		CheckpointArtifact: "last.safetensors",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "artifacts.json") {
		t.Fatalf("rendered RayJob does not write artifacts.json:\n%s", s)
	}
	if !strings.Contains(s, "last.safetensors") {
		t.Errorf("declared checkpoint name absent from render")
	}
	if strings.Index(s, "artifacts.json") < strings.Index(s, "python3") {
		t.Errorf("artifact index written before the training command")
	}
}

func TestRenderOmitsArtifactIndexWhenNoCheckpointDeclared(t *testing.T) {
	out, err := Render(Options{
		Name:       "idx-ray",
		Namespace:  "ray",
		ScriptName: "train.py",
		Script:     []byte("print('train')\n"),
		Workers:    1,
		DataPVC:    "training-data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "artifacts.json") {
		t.Fatalf("artifact index emitted without a declared checkpoint")
	}
}

// Same contract as the engine:job path: the declaration has to be legible from
// the workload, not only from the entrypoint text.
func TestRenderAnnotatesDeclaredCheckpoint(t *testing.T) {
	out, err := Render(Options{
		Name:               "idx-ray",
		Namespace:          "ray",
		ScriptName:         "train.py",
		Script:             []byte("print('train')\n"),
		Workers:            1,
		DataPVC:            "training-data",
		CheckpointArtifact: "last.safetensors",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := workloadmeta.AnnotationCheckpointArtifact + ": last.safetensors"
	if !strings.Contains(string(out), want) {
		t.Errorf("rendered RayJob lacks %q:\n%s", want, out)
	}
}

func TestRenderOmitsCheckpointAnnotationWhenNoneDeclared(t *testing.T) {
	out, err := Render(Options{
		Name:       "idx-ray",
		Namespace:  "ray",
		ScriptName: "train.py",
		Script:     []byte("print('train')\n"),
		Workers:    1,
		DataPVC:    "training-data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), workloadmeta.AnnotationCheckpointArtifact) {
		t.Errorf("checkpoint annotation present without a declared checkpoint:\n%s", out)
	}
}
