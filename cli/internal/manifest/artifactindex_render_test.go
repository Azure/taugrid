// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
	"gopkg.in/yaml.v3"
)

const artifactManifestGPU = `
schema_version: 1
name: artifact-demo
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
artifacts:
  checkpoint: last.safetensors
`

const artifactManifestNoCheckpoint = `
schema_version: 1
name: artifact-demo
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`

func renderForArtifactTest(t *testing.T, raw string, kind string) string {
	t.Helper()
	m, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      []byte(raw),
		ManifestFilename: "artifact-demo.yaml",
		Namespace:        "research",
		MainScript:       []byte("# stub train script\n"),
		WorkloadKind:     kind,
	})
	if err != nil {
		t.Fatalf("Render(%s): %v", kind, err)
	}
	return string(out)
}

func TestRenderEmitsArtifactIndexStep(t *testing.T) {
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			s := renderForArtifactTest(t, artifactManifestGPU, kind)
			for _, want := range []string{
				"TAU_ARTIFACT_CHECKPOINT='last.safetensors'",
				"TAU_ARTIFACT_RUN='artifact-demo'",
				"TAU_ARTIFACT_NAMESPACE='research'",
				"TAU_ARTIFACT_INDEX_EOF",
				`"artifacts.json"`,
			} {
				if !strings.Contains(s, want) {
					t.Errorf("rendered %s missing %q", kind, want)
				}
			}
			idxTrain := strings.Index(s, "/script/train.py")
			idxFinal := strings.Index(s, "TAU_ARTIFACT_CHECKPOINT=")
			if idxTrain < 0 || idxFinal < 0 || idxFinal < idxTrain {
				t.Errorf("finalize step at %d must come after train.py at %d", idxFinal, idxTrain)
			}
			assertAllDocsParse(t, s)
		})
	}
}

func TestRenderOmitsArtifactIndexWhenNoCheckpointDeclared(t *testing.T) {
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			s := renderForArtifactTest(t, artifactManifestNoCheckpoint, kind)
			for _, unwanted := range []string{"TAU_ARTIFACT_CHECKPOINT", "TAU_ARTIFACT_INDEX_EOF"} {
				if strings.Contains(s, unwanted) {
					t.Errorf("rendered %s unexpectedly contains %q for a manifest with no artifacts.checkpoint", kind, unwanted)
				}
			}
			assertAllDocsParse(t, s)
		})
	}
}

func TestRenderAnnotatesDeclaredCheckpoint(t *testing.T) {
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			s := renderForArtifactTest(t, artifactManifestGPU, kind)
			got := firstDocAnnotation(t, s, workloadmeta.AnnotationCheckpointArtifact)
			if got != "last.safetensors" {
				t.Errorf("%s = %q, want %q", workloadmeta.AnnotationCheckpointArtifact, got, "last.safetensors")
			}
			assertAllDocsParse(t, s)
		})
	}
}

func TestRenderOmitsCheckpointAnnotationWhenNoneDeclared(t *testing.T) {
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			s := renderForArtifactTest(t, artifactManifestNoCheckpoint, kind)
			if strings.Contains(s, workloadmeta.AnnotationCheckpointArtifact) {
				t.Errorf("rendered %s carries the checkpoint annotation with no artifacts.checkpoint", kind)
			}
		})
	}
}

func firstDocAnnotation(t *testing.T, rendered, key string) string {
	t.Helper()
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		doc.Kind = ""
		doc.Metadata.Annotations = nil
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("no Job or RayJob document in render: %v", err)
		}
		if doc.Kind == "Job" || doc.Kind == "RayJob" {
			return doc.Metadata.Annotations[key]
		}
	}
}

func TestRenderedEntrypointArtifactStepRuns(t *testing.T) {
	for _, shell := range []string{"bash", "dash"} {
		if _, err := exec.LookPath(shell); err != nil {
			t.Skipf("%s not available", shell)
		}
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			entrypoint := renderedEntrypointCarryingArtifactStep(t, renderForArtifactTest(t, artifactManifestGPU, kind))

			for _, shell := range []string{"bash", "dash"} {
				cmd := exec.Command(shell, "-n")
				cmd.Stdin = strings.NewReader(entrypoint)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s entrypoint is not valid %s: %v\n%s", kind, shell, err, out)
				}
			}

			step := entrypoint[strings.Index(entrypoint, "TAU_ARTIFACT_CHECKPOINT="):]
			root := t.TempDir()
			hot := filepath.Join(root, "hot")
			if err := os.MkdirAll(hot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hot, "last.safetensors"), []byte("weights"), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, shell := range []string{"bash", "dash"} {
				t.Run(shell, func(t *testing.T) {
					durable := filepath.Join(root, shell, "durable")
					cmd := exec.Command(shell, "-eu")
					cmd.Stdin = strings.NewReader(step)
					cmd.Env = append(os.Environ(),
						"TAU_CHECKPOINTS_DIR="+hot,
						"TAU_DURABLE_CHECKPOINTS_DIR="+durable,
					)
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("artifact step failed under %s: %v\n%s", shell, err, out)
					}
					if _, err := os.Stat(filepath.Join(durable, "finetunes", "artifact-demo", "artifacts.json")); err != nil {
						t.Errorf("no artifact index written under %s: %v\n%s", shell, err, out)
					}
				})
			}
		})
	}
}

func TestRenderedHeredocTerminatorIsUnindented(t *testing.T) {
	const terminator = "TAU_ARTIFACT_INDEX_EOF"
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			entrypoint := renderedEntrypointCarryingArtifactStep(t, renderForArtifactTest(t, artifactManifestGPU, kind))
			found := false
			for _, line := range strings.Split(entrypoint, "\n") {
				if !strings.Contains(line, terminator) || strings.Contains(line, "<<") {
					continue
				}
				found = true
				if line != terminator {
					t.Errorf("decoded terminator is %q, want it flush at column 0", line)
				}
			}
			if !found {
				t.Fatalf("no %s terminator line found; the assertion is vacuous", terminator)
			}
		})
	}
}

func renderedEntrypointCarryingArtifactStep(t *testing.T, rendered string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc struct {
			Spec struct {
				Entrypoint string `yaml:"entrypoint"`
				Template   struct {
					Spec struct {
						Containers []struct {
							Args []string `yaml:"args"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("no rendered script carries the artifact step: %v", err)
		}
		candidates := []string{doc.Spec.Entrypoint}
		for _, c := range doc.Spec.Template.Spec.Containers {
			candidates = append(candidates, c.Args...)
		}
		for _, candidate := range candidates {
			if strings.Contains(candidate, "TAU_ARTIFACT_CHECKPOINT=") {
				return candidate
			}
		}
	}
}

func assertAllDocsParse(t *testing.T, s string) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(s))
	n := 0
	for {
		var doc any
		err := dec.Decode(&doc)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("rendered output is not valid YAML after %d docs: %v", n, err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no YAML documents decoded — the assertion is not testing what it claims")
	}
}
