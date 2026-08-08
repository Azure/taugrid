// Package sourcebundle builds, stages, and validates durable source archives.
package sourcebundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/projectzip"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	// MaxInputBytes is deliberately the same limit imposed on working_dir.
	MaxInputBytes = 8 << 20
	DurableRoot   = "/data/tau/source-bundles/sha256"
	helperImage   = "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	stageTimeout  = 3 * time.Minute
)

var digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// Bundle is the immutable local representation and durable location of source.
type Bundle struct {
	Digest  string
	Archive []byte
	Path    string
}

// Runtime is the archive reference a rendered workload needs. It deliberately
// excludes Archive so source bytes cannot be embedded in a workload object.
type Runtime struct {
	Digest     string
	Path       string
	Entrypoint string
}

// RuntimeFor returns the validated runtime reference for an entrypoint within
// this bundle.
func (b Bundle) RuntimeFor(entrypoint string) (Runtime, error) {
	if err := validBundle(b); err != nil {
		return Runtime{}, err
	}
	runtime := Runtime{
		Digest:     b.Digest,
		Path:       b.Path,
		Entrypoint: entrypoint,
	}
	if err := runtime.Validate(); err != nil {
		return Runtime{}, err
	}
	archive, err := zip.NewReader(bytes.NewReader(b.Archive), int64(len(b.Archive)))
	if err != nil {
		return Runtime{}, fmt.Errorf("read source bundle archive: %w", err)
	}
	for _, file := range archive.File {
		if file.Name == entrypoint && !file.FileInfo().IsDir() {
			return runtime, nil
		}
	}
	return Runtime{}, fmt.Errorf("source bundle does not contain entrypoint %q", entrypoint)
}

// Validate checks the stable rendered-workload reference without requiring the
// local archive bytes to be present.
func (r Runtime) Validate() error {
	match := digestRE.FindStringSubmatch(r.Digest)
	if match == nil {
		return fmt.Errorf("source bundle has invalid digest %q", r.Digest)
	}
	if r.Path != path.Join(DurableRoot, match[1]+".zip") {
		return fmt.Errorf("source bundle has non-canonical durable path %q", r.Path)
	}
	return ValidateEntrypointRelative(r.Entrypoint)
}

// BuildOptions selects the local project and optional extra exclusions.
type BuildOptions struct {
	Dir            string
	Excludes       []string
	ExpectedDigest string
}

// Build creates a deterministic project zip and computes its content address.
func Build(o BuildOptions) (Bundle, error) {
	if o.ExpectedDigest != "" && !digestRE.MatchString(o.ExpectedDigest) {
		return Bundle{}, fmt.Errorf("expected source bundle digest must be sha256:<64 lowercase hex characters>")
	}
	archive, _, err := projectzip.Build(projectzip.Options{
		Dir: o.Dir, Excludes: o.Excludes, MaxBytes: MaxInputBytes,
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("build source bundle: %w", err)
	}
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if o.ExpectedDigest != "" && o.ExpectedDigest != digest {
		return Bundle{}, fmt.Errorf("source bundle digest mismatch: expected %s, got %s", o.ExpectedDigest, digest)
	}
	return Bundle{
		Digest:  digest,
		Archive: archive,
		Path:    path.Join(DurableRoot, digest[len("sha256:"):]+".zip"),
	}, nil
}

// ValidateEntrypointRelative rejects an entrypoint that cannot be resolved
// beneath a source bundle extraction directory.
func ValidateEntrypointRelative(entrypoint string) error {
	if entrypoint == "" {
		return fmt.Errorf("source bundle entrypoint is empty")
	}
	if strings.Contains(entrypoint, "\x00") || strings.Contains(entrypoint, "\\") {
		return fmt.Errorf("source bundle entrypoint %q contains an unsafe path character", entrypoint)
	}
	if path.IsAbs(entrypoint) {
		return fmt.Errorf("source bundle entrypoint %q must be relative", entrypoint)
	}
	clean := path.Clean(entrypoint)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("source bundle entrypoint %q escapes the bundle", entrypoint)
	}
	return nil
}

// Runner is the deliberately small kubectl contract used by staging.
type Runner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

// Stage makes bundle available on pvc at its content-addressed path. Archive
// bytes are sent only to the upload exec's standard input.
func Stage(ctx context.Context, runner Runner, namespace, pvc, runName string, bundle Bundle) error {
	if runner == nil {
		return fmt.Errorf("stage source bundle: runner is required")
	}
	if namespace == "" || pvc == "" || runName == "" {
		return fmt.Errorf("stage source bundle: namespace, pvc, and run name are required")
	}
	if err := validBundle(bundle); err != nil {
		return err
	}
	stageCtx, cancel := context.WithTimeout(ctx, stageTimeout)
	defer cancel()
	podName := helperPodName(
		runName,
		fmt.Sprintf("%s-%x", bundle.Digest[7:15], time.Now().UnixNano()),
	)
	manifest, err := helperManifest(podName, namespace, pvc, bundle)
	if err != nil {
		return fmt.Errorf("render source bundle helper pod: %w", err)
	}
	if _, err := runner.Raw(stageCtx, []string{"create", "-n", namespace, "-f", "-"}, manifest); err != nil {
		return fmt.Errorf("create source bundle helper pod: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = runner.Raw(cleanupCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()

	waitCtx, waitCancel := context.WithTimeout(stageCtx, 90*time.Second)
	defer waitCancel()
	if _, err := runner.Raw(waitCtx, []string{"wait", "-n", namespace, "--for=jsonpath={.status.phase}=Running", "pod/" + podName, "--timeout=90s"}, nil); err != nil {
		return fmt.Errorf("wait for source bundle helper pod %s to run: %w", podName, err)
	}

	state, err := probe(stageCtx, runner, namespace, podName, bundle)
	if err != nil {
		return err
	}
	if state == "present" {
		return nil
	}
	if state == "corrupt" {
		return fmt.Errorf("source bundle target %s exists but its sha256 does not match %s", bundle.Path, bundle.Digest)
	}
	if _, err := runner.Raw(stageCtx, execArgs(namespace, podName, installScript(bundle)), bundle.Archive); err != nil {
		return fmt.Errorf("upload source bundle %s to PVC %s: %w", bundle.Digest, pvc, err)
	}
	state, err = probe(stageCtx, runner, namespace, podName, bundle)
	if err != nil {
		return err
	}
	if state != "present" {
		return fmt.Errorf("source bundle %s was not verified after staging (state %s)", bundle.Digest, state)
	}
	return nil
}

func validBundle(b Bundle) error {
	match := digestRE.FindStringSubmatch(b.Digest)
	if match == nil {
		return fmt.Errorf("source bundle has invalid digest %q", b.Digest)
	}
	sum := sha256.Sum256(b.Archive)
	if hex.EncodeToString(sum[:]) != match[1] {
		return fmt.Errorf("source bundle archive does not match digest %s", b.Digest)
	}
	if b.Path != path.Join(DurableRoot, match[1]+".zip") {
		return fmt.Errorf("source bundle has non-canonical durable path %q", b.Path)
	}
	return nil
}

func probe(ctx context.Context, runner Runner, namespace, podName string, bundle Bundle) (string, error) {
	out, err := runner.Raw(ctx, execArgs(namespace, podName, probeScript(bundle)), nil)
	if err != nil {
		return "", fmt.Errorf("probe source bundle target %s: %w", bundle.Path, err)
	}
	switch strings.TrimSpace(out) {
	case "TAU_SOURCE_BUNDLE_ABSENT":
		return "absent", nil
	case bundle.Digest[7:]:
		return "present", nil
	default:
		return "corrupt", nil
	}
}

func execArgs(namespace, podName, script string) []string {
	return []string{"exec", "-i", "-n", namespace, podName, "--", "sh", "-c", script}
}

func probeScript(b Bundle) string {
	return fmt.Sprintf(`if [ ! -e %q ]; then echo TAU_SOURCE_BUNDLE_ABSENT; elif [ ! -f %q ]; then echo TAU_SOURCE_BUNDLE_CORRUPT; else sha256sum %q | awk '{print $1}'; fi`, b.Path, b.Path, b.Path)
}

func installScript(b Bundle) string {
	hexDigest := b.Digest[7:]
	return fmt.Sprintf(`set -eu
dir=%q
target=%q
tmp="$dir/.%s.$(hostname).$$"
mkdir -p "$dir"
cat > "$tmp"
actual="$(sha256sum "$tmp" | awk '{print $1}')"
[ "$actual" = %q ] || { rm -f "$tmp"; echo "source bundle stdin digest mismatch" >&2; exit 1; }
if [ -e "$target" ]; then
  existing="$(sha256sum "$target" 2>/dev/null | awk '{print $1}' || true)"
  rm -f "$tmp"
  [ "$existing" = %q ] || { echo "source bundle target exists but is corrupt" >&2; exit 1; }
else
  mv -n "$tmp" "$target"
  if [ -e "$tmp" ]; then rm -f "$tmp"; fi
fi
final="$(sha256sum "$target" 2>/dev/null | awk '{print $1}' || true)"
[ "$final" = %q ] || { echo "source bundle target failed verification" >&2; exit 1; }`,
		DurableRoot, b.Path, hexDigest, hexDigest, hexDigest, hexDigest)
}

type podManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		RestartPolicy         string         `yaml:"restartPolicy"`
		ActiveDeadlineSeconds int64          `yaml:"activeDeadlineSeconds"`
		Containers            []podContainer `yaml:"containers"`
		Volumes               []podVolume    `yaml:"volumes"`
	} `yaml:"spec"`
}
type podContainer struct {
	Name         string           `yaml:"name"`
	Image        string           `yaml:"image"`
	Command      []string         `yaml:"command"`
	VolumeMounts []podVolumeMount `yaml:"volumeMounts"`
}
type podVolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}
type podVolume struct {
	Name                  string `yaml:"name"`
	PersistentVolumeClaim struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
}

func helperManifest(name, namespace, pvc string, bundle Bundle) ([]byte, error) {
	var pod podManifest
	pod.APIVersion, pod.Kind = "v1", "Pod"
	pod.Metadata.Name, pod.Metadata.Namespace = name, namespace
	pod.Metadata.Labels = map[string]string{"app.kubernetes.io/name": "tau-source-bundle-stage"}
	pod.Metadata.Annotations = map[string]string{
		workloadmeta.AnnotationSourceBundleDigest: bundle.Digest,
		workloadmeta.AnnotationSourceBundlePath:   bundle.Path,
	}
	pod.Spec.RestartPolicy = "Never"
	pod.Spec.ActiveDeadlineSeconds = int64(stageTimeout.Seconds())
	pod.Spec.Containers = []podContainer{{
		Name: "stage", Image: helperImage, Command: []string{"sh", "-c", "trap : TERM INT; sleep 300 & wait"},
		VolumeMounts: []podVolumeMount{{Name: "data", MountPath: "/data"}},
	}}
	volume := podVolume{Name: "data"}
	volume.PersistentVolumeClaim.ClaimName = pvc
	pod.Spec.Volumes = []podVolume{volume}
	return yaml.Marshal(pod)
}

func helperPodName(runName, suffix string) string {
	name := strings.ToLower(runName)
	name = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "run"
	}
	const prefix = "tau-source-stage-"
	max := 63 - len(prefix) - 1 - len(suffix)
	if len(name) > max {
		name = strings.Trim(name[:max], "-")
	}
	return prefix + name + "-" + suffix
}
