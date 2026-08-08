package sourcebundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildUsesProjectZipAndContentAddress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.pyc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := Build(BuildOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasPrefix(bundle.Digest, "sha256:") || !strings.HasSuffix(bundle.Path, bundle.Digest[7:]+".zip") {
		t.Fatalf("unexpected bundle: %#v", bundle)
	}
	if _, err := Build(BuildOptions{Dir: dir, ExpectedDigest: "sha256:" + strings.Repeat("0", 64)}); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestValidateEntrypointRelative(t *testing.T) {
	for _, entry := range []string{"train.py", "pkg/train.py"} {
		if err := ValidateEntrypointRelative(entry); err != nil {
			t.Errorf("ValidateEntrypointRelative(%q): %v", entry, err)
		}

	}
	for _, entry := range []string{"", "/train.py", "../train.py", `pkg\train.py`, "."} {
		if err := ValidateEntrypointRelative(entry); err == nil {
			t.Errorf("ValidateEntrypointRelative(%q) accepted unsafe path", entry)
		}
	}

}

func TestRuntimeForExcludesArchiveBytesAndValidatesReference(t *testing.T) {
	bundle := testBundle(t)
	runtime, err := bundle.RuntimeFor("main.py")
	if err != nil {
		t.Fatalf("RuntimeFor: %v", err)
	}
	if runtime.Digest != bundle.Digest || runtime.Path != bundle.Path || runtime.Entrypoint != "main.py" {
		t.Fatalf("runtime = %#v, bundle = %#v", runtime, bundle)
	}
	if err := runtime.Validate(); err != nil {
		t.Fatalf("validate runtime: %v", err)
	}
	runtime.Path = "/data/not-the-content-address.zip"
	if err := runtime.Validate(); err == nil {
		t.Fatal("non-canonical runtime path must fail validation")
	}
}

type runnerCall struct {
	args  []string
	stdin []byte
}

type stagingRunner struct {
	calls   []runnerCall
	target  string // absent, present, corrupt
	digest  string
	uploads int
}

func (r *stagingRunner) Raw(_ context.Context, args []string, stdin []byte) (string, error) {
	r.calls = append(r.calls, runnerCall{append([]string(nil), args...), append([]byte(nil), stdin...)})
	if len(args) > 0 && args[0] == "wait" {
		return "", nil
	}
	if len(args) > 0 && args[0] == "exec" {
		script := args[len(args)-1]
		if strings.Contains(script, "TAU_SOURCE_BUNDLE_ABSENT") {
			switch r.target {
			case "present":
				return r.digest, nil
			case "corrupt":
				return "not-the-digest", nil
			default:
				return "TAU_SOURCE_BUNDLE_ABSENT", nil
			}
		}
		if len(stdin) > 0 {
			r.uploads++
			r.target = "present"
		}
	}
	return "", nil
}

func testBundle(t *testing.T) Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := Build(BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestStageStreamsOnlyViaExecAndCleansUp(t *testing.T) {
	bundle := testBundle(t)
	runner := &stagingRunner{target: "absent", digest: bundle.Digest[7:]}
	if err := Stage(context.Background(), runner, "ns", "data", "Run One", bundle); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if runner.uploads != 1 {
		t.Fatalf("uploads = %d, want 1", runner.uploads)
	}
	var create []byte
	cleaned := false
	for _, call := range runner.calls {
		if call.args[0] == "create" {
			create = call.stdin
		}
		if call.args[0] != "exec" && bytes.Equal(call.stdin, bundle.Archive) {
			t.Fatalf("non-exec command received archive bytes: %#v", call.args)
		}
		if call.args[0] == "delete" {
			cleaned = true
		}
	}
	if !cleaned {
		t.Fatal("helper pod was not cleaned up")
	}
	if !bytes.Contains(create, []byte(bundle.Digest)) || !bytes.Contains(create, []byte(bundle.Path)) {
		t.Fatalf("manifest must identify bundle digest and path: %s", create)
	}
	if !bytes.Contains(create, []byte("activeDeadlineSeconds: 180")) {
		t.Fatalf("manifest must bound helper lifetime: %s", create)
	}
	if bytes.Contains(create, bundle.Archive) {
		t.Fatal("manifest contains archive payload")
	}
}

func TestStageReusesOnlyMatchingDigest(t *testing.T) {
	bundle := testBundle(t)
	runner := &stagingRunner{target: "absent", digest: bundle.Digest[7:]}
	if err := Stage(context.Background(), runner, "ns", "data", "run", bundle); err != nil {
		t.Fatal(err)
	}
	if runner.uploads != 1 {
		t.Fatalf("initial upload = %d", runner.uploads)
	}
	reused := &stagingRunner{target: "present", digest: bundle.Digest[7:]}
	if err := Stage(context.Background(), reused, "ns", "data", "run", bundle); err != nil {
		t.Fatal(err)
	}
	if reused.uploads != 0 {
		t.Fatalf("deduplicated stage uploaded %d times", reused.uploads)
	}
}

func TestStageRejectsCorruptExistingTargetAndCleansUp(t *testing.T) {
	bundle := testBundle(t)
	runner := &stagingRunner{target: "corrupt", digest: bundle.Digest[7:]}
	err := Stage(context.Background(), runner, "ns", "data", "run", bundle)
	if err == nil || !strings.Contains(err.Error(), "exists but its sha256") {
		t.Fatalf("Stage error = %v, want corrupt target error", err)
	}
	if runner.uploads != 0 {
		t.Fatal("corrupt target must not be overwritten")
	}
	foundDelete := false
	for _, call := range runner.calls {
		foundDelete = foundDelete || call.args[0] == "delete"
	}
	if !foundDelete {
		t.Fatal("helper pod was not cleaned after corrupt target")
	}
}

func TestInstallScriptUsesPodUniqueTemporaryPath(t *testing.T) {
	script := installScript(testBundle(t))
	if !strings.Contains(script, `$(hostname).$$`) {
		t.Fatalf("install script temp path is not unique per helper pod:\n%s", script)
	}
}

func TestInitScriptContracts(t *testing.T) {
	valid := zipBytes(t, func(zw *zip.Writer) {
		w, err := zw.Create("pkg/train.py")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("print('ok')"))
	})
	runInit(t, valid, "validate", true)
	runInit(t, valid, "extract", true)
	runInitMissing(t)
	for _, archive := range [][]byte{
		[]byte("not a zip"),
		zipBytes(t, func(zw *zip.Writer) { mustCreate(t, zw, "../escape", nil) }),
		zipBytes(t, func(zw *zip.Writer) {
			header := &zip.FileHeader{Name: "link"}
			header.SetMode(fs.ModeSymlink | 0o777)
			w, err := zw.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte("target"))
		}),
		zipBytes(t, func(zw *zip.Writer) {
			mustCreate(t, zw, "same", nil)
			mustCreate(t, zw, "same", nil)
		}),
		zipBytes(t, func(zw *zip.Writer) {
			for i := 0; i <= 4096; i++ {
				mustCreate(t, zw, fmt.Sprintf("f-%d", i), nil)
			}
		}),
	} {
		runInit(t, archive, "validate", false)
	}
}

func TestInitScriptPreservesSafeExecutableBit(t *testing.T) {
	archive := zipBytes(t, func(zw *zip.Writer) {
		header := &zip.FileHeader{Name: "run.sh", Method: zip.Deflate}
		header.SetMode(0o775)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("#!/bin/sh\nexit 0\n"))
	})
	dir := t.TempDir()
	source := filepath.Join(dir, "bundle.zip")
	target := filepath.Join(dir, "extract")
	if err := os.WriteFile(source, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(archive))
	cmd := exec.Command("python3", "-c", InitScript())
	cmd.Env = append(os.Environ(),
		InitEnvSourcePath+"="+source,
		InitEnvDigest+"=sha256:"+sum,
		InitEnvTargetDir+"="+target,
		InitEnvMode+"=extract",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extract executable: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(target, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("extracted executable mode = %o, want safe mode 755", got)
	}
}

func zipBytes(t *testing.T, fill func(*zip.Writer)) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	fill(zw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustCreate(t *testing.T, zw *zip.Writer, name string, data []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
}

func runInit(t *testing.T, archive []byte, mode string, wantOK bool) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "bundle.zip")
	if err := os.WriteFile(source, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(archive))
	cmd := exec.Command("python3", "-c", InitScript())
	cmd.Env = append(os.Environ(),
		InitEnvSourcePath+"="+source,
		InitEnvDigest+"=sha256:"+sum,
		InitEnvTargetDir+"="+filepath.Join(dir, "extract"),
		InitEnvMode+"="+mode,
	)
	out, err := cmd.CombinedOutput()
	if (err == nil) != wantOK {
		t.Fatalf("python result error=%v wantOK=%t output=%s", err, wantOK, out)
	}
}

func runInitMissing(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("python3", "-c", InitScript())
	cmd.Env = append(os.Environ(),
		InitEnvSourcePath+"="+filepath.Join(dir, "missing.zip"),
		InitEnvDigest+"=sha256:"+strings.Repeat("0", 64),
		InitEnvTargetDir+"="+filepath.Join(dir, "extract"),
		InitEnvMode+"=validate",
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("missing archive succeeded: %s", out)
	}
}
