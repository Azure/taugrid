// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

func exactJobFixture(name, namespace, submissionID string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    kueue.x-k8s.io/queue-name: research-training
    kueue.x-k8s.io/max-exec-time-seconds: "4500"
  annotations:
    %s: %s
spec:
  suspend: true
  activeDeadlineSeconds: 4500
  backoffLimit: 0
  template:
    spec:
      terminationGracePeriodSeconds: 600
      containers:
        - name: train
          image: example.invalid/h200-rl@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          resources:
            limits:
              nvidia.com/gpu: 2
      restartPolicy: Never
`, name, namespace, workloadmeta.LabelManagedBy, workloadmeta.ManagedByValue, workloadmeta.AnnotationSubmissionID, submissionID))
}

func TestServerDryRunYAMLOutputRequiresManifestCapture(t *testing.T) {
	if got := serverDryRunOutputFormat("server", ""); got != "" {
		t.Fatalf("non-opt-in server dry-run output format = %q, want legacy default", got)
	}
	if got := serverDryRunOutputFormat("server", "checked.yaml"); got != "yaml" {
		t.Fatalf("exact-manifest server dry-run output format = %q, want yaml", got)
	}
}

func TestSubmitExactJobManifestUsesByteIdenticalInput(t *testing.T) {
	manifest := exactJobFixture("h200-rl", "research", "submission-1")
	calls := 0
	runner := submissionRunnerFunc(func(_ context.Context, args []string, stdin []byte) (string, error) {
		calls++
		if got, want := fmt.Sprint(args), "[create -n research -f -]"; got != want {
			t.Fatalf("args = %s, want %s", got, want)
		}
		if !bytes.Equal(stdin, manifest) {
			t.Fatalf("submitted bytes differ from checked manifest")
		}
		return "job.batch/h200-rl created\n", nil
	})
	result, err := submitExactJobManifest(
		context.Background(),
		runner,
		manifest,
		exactManifestDigest(manifest),
		"h200-rl",
		"research",
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Output != "job.batch/h200-rl created\n" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

func TestValidateExactJobManifestRejectsDriftAndIdentityMismatch(t *testing.T) {
	manifest := exactJobFixture("h200-rl", "research", "submission-1")
	digest := exactManifestDigest(manifest)
	tests := []struct {
		name      string
		manifest  []byte
		digest    string
		jobName   string
		namespace string
		want      string
	}{
		{
			name:      "changed bytes",
			manifest:  append(append([]byte{}, manifest...), '\n'),
			digest:    digest,
			jobName:   "h200-rl",
			namespace: "research",
			want:      "digest mismatch",
		},
		{
			name:      "wrong name",
			manifest:  manifest,
			digest:    digest,
			jobName:   "different",
			namespace: "research",
			want:      "identity mismatch",
		},
		{
			name:      "wrong namespace",
			manifest:  manifest,
			digest:    digest,
			jobName:   "h200-rl",
			namespace: "other",
			want:      "identity mismatch",
		},
		{
			name:      "arbitrary object",
			manifest:  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: h200-rl, namespace: research}\n"),
			jobName:   "h200-rl",
			namespace: "research",
			want:      "batch/v1 Job",
		},
		{
			name:      "multiple objects",
			manifest:  append(append([]byte{}, manifest...), []byte("---\napiVersion: v1\nkind: ConfigMap\n")...),
			jobName:   "h200-rl",
			namespace: "research",
			want:      "one YAML document",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.digest == "" {
				tt.digest = exactManifestDigest(tt.manifest)
			}
			_, err := validateExactJobManifest(tt.manifest, tt.digest, tt.jobName, tt.namespace)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateExactJobManifestRejectsMissingTauIdentity(t *testing.T) {
	manifest := []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: h200-rl
  namespace: research
`)
	_, err := validateExactJobManifest(manifest, exactManifestDigest(manifest), "h200-rl", "research")
	if err == nil || !strings.Contains(err.Error(), "Tau-managed") {
		t.Fatalf("error = %v, want Tau-managed rejection", err)
	}
}

func TestRunSubmitManifestCommandNeedsNoConfigOrRender(t *testing.T) {
	manifest := exactJobFixture("h200-rl", "research", "submission-command")
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "checked.yaml")
	submittedPath := filepath.Join(dir, "submitted.yaml")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeKubectl(t, `
	case "$*" in
	  *"create -n research -f -"*)
	    cat > "`+submittedPath+`"
	    printf '%s\n' 'job.batch/h200-rl created'
	    ;;
	  *)
	    printf '%s\n' "unexpected kubectl call: $*" >&2
	    exit 1
	    ;;
	esac
	`)

	cmd := NewRoot()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"run", "submit-manifest",
		"--manifest", manifestPath,
		"--digest", exactManifestDigest(manifest),
		"--name", "h200-rl",
		"--namespace", "research",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit-manifest: %v\nstderr:\n%s", err, stderr.String())
	}
	submitted, err := os.ReadFile(submittedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(submitted, manifest) {
		t.Fatal("submit-manifest changed the checked bytes")
	}
	if stdout.String() != "job.batch/h200-rl created\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
