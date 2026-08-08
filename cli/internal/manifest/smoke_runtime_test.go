// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"strings"
	"testing"
)

func TestSmokeRuntimePipUserSet(t *testing.T) {
	yamlBytes := []byte(`
schema_version: 1
name: smoke-render
compute: {gpus: 4, workers: 2}
runtime:
  pip:
    - torch==2.4.0
    - transformers==4.45.0
    - peft
`)
	m, err := Parse(yamlBytes)
	if err != nil {
		t.Fatal(err)
	}
	pip := m.RuntimePip()
	if len(pip) != 3 || pip[1] != "transformers==4.45.0" {
		t.Errorf("got pip=%v", pip)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      yamlBytes,
		ManifestFilename: "smoke.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "transformers==4.45.0") {
		t.Errorf("rendered output missing transformers from user-declared runtime.pip")
		t.Log(rendered)
	}
}

func TestSmokeRuntimePipRequired(t *testing.T) {
	// Tau ships no fallback pip list — a manifest without runtime.pip
	// must be rejected at Parse() time so the user knows to declare the
	// per-pod Python environment their trainer needs.
	yamlBytes := []byte(`
schema_version: 1
name: smoke-default
compute: {gpus: 1, workers: 1}
`)
	_, err := Parse(yamlBytes)
	if err == nil {
		t.Fatal("expected Parse to reject manifest without runtime.pip")
	}
	if !strings.Contains(err.Error(), "runtime.pip") {
		t.Errorf("error should mention runtime.pip; got: %v", err)
	}
}

func TestRuntimePipBlankRejected(t *testing.T) {
	yamlBytes := []byte(`
schema_version: 1
name: smoke-blank
compute: {gpus: 1, workers: 1}
runtime:
  pip:
    - torch==2.4.0
    - ""
`)
	_, err := Parse(yamlBytes)
	if err == nil {
		t.Fatal("expected validation error for blank pip entry")
	}
	if !strings.Contains(err.Error(), "runtime.pip") {
		t.Errorf("error %q should mention runtime.pip", err.Error())
	}
}
