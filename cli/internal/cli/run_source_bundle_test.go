package cli

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/sourcebundle"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestConfigToDispatchResolvesSourceBundleRelativeToConfig(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "tau.yaml")
	digest := "sha256:" + strings.Repeat("a", 64)
	options, err := configToDispatch(runconfig.Config{
		Run: runconfig.Run{
			Entrypoint: "project/pkg/train.py",
			SourceBundle: &runconfig.SourceBundle{
				Path:     "project",
				Excludes: []string{".git", "*.log"},
				Digest:   digest,
			},
		},
	}, configPath)
	if err != nil {
		t.Fatalf("configToDispatch: %v", err)
	}
	if got, want := options.script, filepath.Join(configDir, "project", "pkg", "train.py"); got != want {
		t.Fatalf("entrypoint = %q, want %q", got, want)
	}
	if got, want := options.sourceBundlePath, filepath.Join(configDir, "project"); got != want {
		t.Fatalf("source bundle path = %q, want %q", got, want)
	}
	if got, want := strings.Join(options.sourceBundleExcludes, ","), ".git,*.log"; got != want {
		t.Fatalf("source bundle excludes = %q, want %q", got, want)
	}
	if options.sourceBundleDigest != digest {
		t.Fatalf("source bundle digest = %q, want %q", options.sourceBundleDigest, digest)
	}
}

func TestBuildRunSourceBundleAllowsArchiveBeyondWorkingDirLimit(t *testing.T) {
	root := t.TempDir()
	entrypoint := writeProjectFile(t, root, "pkg/train.py", "print('ok')\n")
	large := make([]byte, (64<<10)+2048)
	if _, err := cryptorand.Read(large); err != nil {
		t.Fatalf("generate archive input: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := buildRunSourceBundle(runDispatchOptions{
		script:           entrypoint,
		sourceBundlePath: root,
	})
	if err != nil {
		t.Fatalf("build source bundle: %v", err)
	}
	if source == nil || len(source.bundle.Archive) <= 64<<10 {
		t.Fatalf("source archive = %d bytes, want generated archive over working_dir limit", len(source.bundle.Archive))
	}
	if got, want := source.runtime.Entrypoint, "pkg/train.py"; got != want {
		t.Fatalf("relative entrypoint = %q, want %q", got, want)
	}
	if !strings.HasPrefix(source.runtime.Digest, "sha256:") || source.runtime.Path == "" {
		t.Fatalf("source runtime = %#v", source.runtime)
	}
}

func TestBuildRunSourceBundleRejectsMissingAndOutsideEntrypoints(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	writeProjectFile(t, root, "pkg/train.py", "print('ok')\n")
	outside := writeProjectFile(t, base, "outside.py", "print('outside')\n")
	for _, test := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "outside", script: outside, want: "must live inside"},
		{name: "missing", script: filepath.Join(root, "missing.py"), want: "run.entrypoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildRunSourceBundle(runDispatchOptions{
				script:           test.script,
				sourceBundlePath: root,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("build source bundle error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildRunSourceBundleRejectsExcludedEntrypoint(t *testing.T) {
	root := t.TempDir()
	entrypoint := writeProjectFile(t, root, "pkg/train.py", "print('ok')\n")
	writeProjectFile(t, root, "kept.py", "print('kept')\n")
	_, err := buildRunSourceBundle(runDispatchOptions{
		script:               entrypoint,
		sourceBundlePath:     root,
		sourceBundleExcludes: []string{"pkg"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not contain entrypoint") {
		t.Fatalf("excluded entrypoint error = %v", err)
	}
}

func TestSourceBundleDefaultsToDurablePVCForBothEngines(t *testing.T) {
	job, err := newRunJobRequest(runDispatchOptions{
		script:           "train.py",
		sourceBundlePath: "source",
	}, "source-job")
	if err != nil {
		t.Fatalf("newRunJobRequest: %v", err)
	}
	if job.Options.dataPVC != defaultTauPVCName {
		t.Fatalf("job data PVC = %q, want %q", job.Options.dataPVC, defaultTauPVCName)
	}
	ray, err := newRunRayRequest(runDispatchOptions{
		script:           "train.py",
		sourceBundlePath: "source",
	}, "source-ray")
	if err != nil {
		t.Fatalf("newRunRayRequest: %v", err)
	}
	if ray.Options.dataPVC != defaultTauPVCName {
		t.Fatalf("ray data PVC = %q, want %q", ray.Options.dataPVC, defaultTauPVCName)
	}
}

type sourceBundleFakeRunner struct{}

func (sourceBundleFakeRunner) Raw(context.Context, []string, []byte) (string, error) {
	return "", nil
}

func TestStageRunSourceBundleSkipsDryRunsAndStagesRealSubmissionOnce(t *testing.T) {
	original := stageSourceBundle
	t.Cleanup(func() { stageSourceBundle = original })

	var calls int
	stageSourceBundle = func(_ context.Context, _ sourcebundle.Runner, namespace, pvc, name string, bundle sourcebundle.Bundle) error {
		calls++
		if namespace != "ns" || pvc != "source-pvc" || name != "source-run" {
			t.Fatalf("stage target = %s/%s/%s", namespace, pvc, name)
		}
		if bundle.Digest != "sha256:"+strings.Repeat("c", 64) {
			t.Fatalf("bundle digest = %q", bundle.Digest)
		}
		return nil
	}
	source := &preparedSourceBundle{bundle: sourcebundle.Bundle{
		Digest: "sha256:" + strings.Repeat("c", 64),
	}}
	for _, dryRun := range []string{"client", "server"} {
		if err := stageRunSourceBundle(context.Background(), dryRun, nil, "ns", "source-pvc", "source-run", source); err != nil {
			t.Fatalf("%s dry-run staging: %v", dryRun, err)
		}
	}
	if calls != 0 {
		t.Fatalf("dry-runs staged %d bundles, want 0", calls)
	}
	if err := stageRunSourceBundle(context.Background(), "", sourceBundleFakeRunner{}, "ns", "source-pvc", "source-run", source); err != nil {
		t.Fatalf("real submission staging: %v", err)
	}
	if calls != 1 {
		t.Fatalf("real submission staged %d bundles, want 1", calls)
	}
}

func TestSourceBundleClientDryRunsBuildAndRenderWithoutStaging(t *testing.T) {
	original := stageSourceBundle
	t.Cleanup(func() { stageSourceBundle = original })
	staged := 0
	stageSourceBundle = func(context.Context, sourcebundle.Runner, string, string, string, sourcebundle.Bundle) error {
		staged++
		return nil
	}

	root := t.TempDir()
	const sourceOnlyMarker = "tau-source-bundle-content-must-not-appear-in-manifest"
	entrypoint := writeProjectFile(t, root, "pkg/train.py", "print('"+sourceOnlyMarker+"')\n")
	for _, test := range []struct {
		name    string
		execute func(context.Context, *bytes.Buffer, *bytes.Buffer, string) error
	}{
		{
			name: "job",
			execute: func(ctx context.Context, stdout, stderr *bytes.Buffer, capture string) error {
				request, err := newRunJobRequest(runDispatchOptions{
					engine:           "job",
					script:           entrypoint,
					image:            "python:3.12",
					namespace:        "default",
					queue:            "jobqueue",
					dryRun:           "client",
					sourceBundlePath: root,
				}, "source-job")
				if err != nil {
					return err
				}
				return executeRunJob(ctx, stdout, stderr, &request, capture)
			},
		},
		{
			name: "ray",
			execute: func(ctx context.Context, stdout, stderr *bytes.Buffer, capture string) error {
				request, err := newRunRayRequest(runDispatchOptions{
					engine:           "ray",
					script:           entrypoint,
					image:            "ray:py3.12-ray2.54.0",
					namespace:        "default",
					queue:            "jobqueue",
					dryRun:           "client",
					sourceBundlePath: root,
					workers:          1,
					gpusPerWorker:    0,
				}, "source-ray")
				if err != nil {
					return err
				}
				return executeRunRay(ctx, stdout, stderr, &request, capture)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := test.execute(context.Background(), &stdout, &stderr, "tau run --dry-run=client"); err != nil {
				t.Fatalf("client dry-run: %v\nstderr:\n%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), workloadmeta.AnnotationSourceBundleDigest) {
				t.Fatalf("render did not retain source provenance:\n%s", stdout.String())
			}
			if strings.Contains(stdout.String(), sourceOnlyMarker) {
				t.Fatalf("render embedded source bundle contents:\n%s", stdout.String())
			}
		})
	}
	if staged != 0 {
		t.Fatalf("client dry-runs staged %d bundles, want 0", staged)
	}
}
