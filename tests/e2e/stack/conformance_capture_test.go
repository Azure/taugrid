// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

func conformanceRayCluster(ctx context.Context, client dynamic.Interface, namespace, job string) (string, error) {
	var cluster string
	err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		obj, err := client.Resource(e2e.RayJobGVR).Namespace(namespace).Get(ctx, job, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		cluster, _, err = unstructured.NestedString(obj.Object, "status", "rayClusterName")
		return cluster != "", err
	})
	return cluster, err
}

func fineWebCallerOwnsCleanup() (bool, error) {
	switch os.Getenv("FINEWEB_CLEANUP_OWNER") {
	case "", "test":
		return false, nil
	case "caller":
		return true, nil
	default:
		return false, fmt.Errorf("FINEWEB_CLEANUP_OWNER must be test or caller")
	}
}

func boundedDuration(raw string, fallback, maximum time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > maximum {
		return 0, fmt.Errorf("invalid timeout %q: require a positive duration <= %s", raw, maximum)
	}
	return d, nil
}

func conformanceExecutionBudget(fineweb bool) (time.Duration, error) {
	admission, err := boundedDuration(os.Getenv("CONFORMANCE_ADMISSION_TIMEOUT"), 30*time.Minute, 90*time.Minute)
	if err != nil {
		return 0, err
	}
	completion, capture := 120*time.Minute, 15*time.Minute
	if fineweb {
		if raw := os.Getenv("FINEWEB_RAYJOB_TIMEOUT_MINUTES"); raw != "" {
			minutes, err := strconv.Atoi(raw)
			if err != nil || minutes <= 0 || minutes > 180 {
				return 0, fmt.Errorf("FINEWEB_RAYJOB_TIMEOUT_MINUTES must be in (0,180]")
			}
			completion = time.Duration(minutes) * time.Minute
		}
		capture, err = boundedDuration(os.Getenv("FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT"), 15*time.Minute, 30*time.Minute)
		if err != nil {
			return 0, err
		}
	}
	minimum := int(completion/time.Minute) + 130
	for _, extra := range []time.Duration{admission - 30*time.Minute, capture - 15*time.Minute} {
		if extra > 0 {
			minimum += int((extra + time.Minute - 1) / time.Minute)
		}
	}
	goTimeout, err := boundedDuration(os.Getenv("CONFORMANCE_GO_TEST_TIMEOUT"), 245*time.Minute, 365*time.Minute)
	if err != nil {
		return 0, fmt.Errorf("CONFORMANCE_GO_TEST_TIMEOUT: %w", err)
	}
	budget := goTimeout + 5*time.Minute
	if budget < time.Duration(minimum)*time.Minute {
		return 0, fmt.Errorf("CONFORMANCE_GO_TEST_TIMEOUT must be >= %dm; caller execution needs >= %dm", minimum-5, minimum)
	}
	return budget, nil
}

func conformanceContext(t *testing.T, fineweb bool) context.Context {
	t.Helper()
	budget, err := conformanceExecutionBudget(fineweb)
	if err != nil {
		t.Fatal(err)
	}
	duration := budget - 20*time.Minute
	if deadline, ok := t.Deadline(); ok {
		if remaining := time.Until(deadline) - 8*time.Minute; remaining < duration {
			duration = remaining
		}
	}
	if duration <= 0 {
		t.Fatal("Go test timeout leaves no execution/cleanup budget; use the conformance harness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	t.Cleanup(cancel)
	return ctx
}

func registerRayJobCleanup(t *testing.T, fixture string, cleanup func()) {
	t.Helper()
	if fixture == "fineweb-rayjob-16xh200-ib.yaml" {
		caller, err := fineWebCallerOwnsCleanup()
		if err != nil {
			t.Fatal(err)
		}
		if caller {
			t.Log("FINEWEB_CALLER_CLEANUP: caller must attempt artifact publication then always clean up, including on failure/cancel")
			return
		}
	}
	t.Cleanup(cleanup)
}

type fineWebCheckpoint struct {
	Step      int    `json:"step"`
	Path      string `json:"source_path"`
	Bytes     int64  `json:"bytes"`
	Params    int64  `json:"params"`
	SHA256    string `json:"sha256"`
	Pod       string `json:"source_pod"`
	Namespace string `json:"namespace"`
	RayJob    string `json:"ray_job"`
	File      string `json:"file"`
}

var verifiedCheckpointRE = regexp.MustCompile(`FINEWEB_FIRST_CHECKPOINT step=(\d+) path=(/mnt/fineweb-ckpt/fineweb-step-\d+\.pt) bytes=(\d+) params=(\d+) sha256=([a-f0-9]{64}) pod=([a-z0-9][a-z0-9.-]*) readback=verified`)

func parseVerifiedCheckpoint(logs string) (fineWebCheckpoint, error) {
	m := verifiedCheckpointRE.FindStringSubmatch(logs)
	if m == nil {
		return fineWebCheckpoint{}, fmt.Errorf("missing full-state readback-verified checkpoint metadata")
	}
	step, err := strconv.Atoi(m[1])
	if err != nil {
		return fineWebCheckpoint{}, err
	}
	size, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil || size <= 0 || size > 32<<30 {
		return fineWebCheckpoint{}, fmt.Errorf("checkpoint size must be in (0, 32GiB]")
	}
	params, err := strconv.ParseInt(m[4], 10, 64)
	if err != nil || step <= 0 || m[2] != fmt.Sprintf("/mnt/fineweb-ckpt/fineweb-step-%d.pt", step) {
		return fineWebCheckpoint{}, fmt.Errorf("invalid checkpoint step/path/params")
	}
	return fineWebCheckpoint{
		Step: step, Path: m[2], Bytes: size, Params: params, SHA256: m[5], Pod: m[6],
		Namespace: stackNamespace, RayJob: rayJobNameFineWeb, File: filepath.Base(m[2]),
	}, nil
}

func checkpointArtifactDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("FINEWEB_CHECKPOINT_ARTIFACT_DIR must be an absolute existing runner artifact directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checkpoint artifact directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("checkpoint artifact destination is not a directory")
	}
	return nil
}

type checkpointWriter struct {
	ctx  context.Context
	dst  io.Writer
	left int64
}

func (w *checkpointWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.left {
		return 0, fmt.Errorf("checkpoint exceeds advertised size")
	}
	n, err := w.dst.Write(p)
	w.left -= int64(n)
	return n, err
}

func streamCheckpoint(ctx context.Context, dir string, meta fineWebCheckpoint, copySource func(context.Context, io.Writer) error) (err error) {
	if err := checkpointArtifactDir(dir); err != nil {
		return err
	}
	if meta.File != filepath.Base(meta.File) || meta.File == "." || meta.File == "" {
		return fmt.Errorf("unsafe checkpoint artifact name")
	}
	final := filepath.Join(dir, meta.File)
	for _, path := range []string{final, filepath.Join(dir, "fineweb-checkpoint.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("checkpoint artifact already exists or cannot be checked: %s", path)
		}
	}
	partial := final + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(partial)
	}()
	hash := sha256.New()
	writer := &checkpointWriter{ctx: ctx, dst: io.MultiWriter(f, hash), left: meta.Bytes}
	if err := copySource(ctx, writer); err != nil {
		return fmt.Errorf("copy checkpoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer.left != 0 || hex.EncodeToString(hash.Sum(nil)) != meta.SHA256 {
		return fmt.Errorf("checkpoint size/SHA256 mismatch after streaming")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(partial, final); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(final)
		}
	}()
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	m, err := os.OpenFile(filepath.Join(dir, "fineweb-checkpoint.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := m.Write(append(payload, '\n'))
	syncErr := m.Sync()
	closeErr := m.Close()
	for _, e := range []error{writeErr, syncErr, closeErr} {
		if e != nil {
			os.Remove(m.Name())
			return e
		}
	}
	return nil
}

func copyCheckpointFromPod(ctx context.Context, meta fineWebCheckpoint, out io.Writer) error {
	args := []string{}
	if name := os.Getenv("AI_RUNTIME_E2E_KUBE_CONTEXT"); name != "" {
		args = append(args, "--context", name)
	}
	args = append(args, "-n", meta.Namespace, "exec", meta.Pod, "-c", "ray-worker", "--", "cat", meta.Path)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
