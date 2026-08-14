// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package payload

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeIsDeterministic(t *testing.T) {
	files := map[string][]byte{
		"train.py":         []byte("print('train')\n"),
		"requirements.txt": []byte("torch==2.4.0\n"),
	}
	encodedA, digestA, err := Encode(New(files))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encodedB, digestB, err := Encode(New(files))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encodedA != encodedB {
		t.Fatalf("encoding is not deterministic:\n%s\n!=\n%s", encodedA, encodedB)
	}
	if digestA != digestB {
		t.Fatalf("digest is not deterministic: %s != %s", digestA, digestB)
	}
}

func TestEncodeOrdersFilesCanonicallyRegardlessOfMapOrder(t *testing.T) {
	// Go map iteration order is randomized; build the same logical payload
	// from maps that would iterate differently and confirm identical output.
	a := New(map[string][]byte{"b.py": []byte("b"), "a.py": []byte("a"), "c.py": []byte("c")})
	b := New(map[string][]byte{"c.py": []byte("c"), "a.py": []byte("a"), "b.py": []byte("b")})

	encodedA, digestA, err := Encode(a)
	if err != nil {
		t.Fatalf("encode a: %v", err)
	}
	encodedB, digestB, err := Encode(b)
	if err != nil {
		t.Fatalf("encode b: %v", err)
	}
	if encodedA != encodedB || digestA != digestB {
		t.Fatalf("payload built from differently-ordered maps did not converge to the same encoding")
	}
	if a.Files[0].Name != "a.py" || a.Files[1].Name != "b.py" || a.Files[2].Name != "c.py" {
		t.Fatalf("New() did not canonically sort files: %+v", a.Files)
	}
}

func TestEncodeRejectsPayloadOverSanityCeiling(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), MaxDecodedBytes+1)
	_, _, err := Encode(New(map[string][]byte{"train.py": oversized}))
	if err == nil {
		t.Fatal("expected error for payload over the decoded sanity ceiling, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"1048577",    // actual size
		"1048576",    // limit in bytes
		"1024 KiB",   // limit in human units
		"by 1 bytes", // overage
		"custom",     // image remedy
		"PVC",        // PVC remedy
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("over-ceiling error missing %q, actionable detail expected:\n%s", want, msg)
		}
	}
}

// TestEncodeRejectsIncompressiblePayloadOverEnvEntryLimit covers the binding
// constraint. Compression means the decoded ceiling is no longer what protects
// execve(2), so an incompressible payload well under MaxDecodedBytes must still
// be rejected -- otherwise it would render a pod whose initContainer cannot
// start.
func TestEncodeRejectsIncompressiblePayloadOverEnvEntryLimit(t *testing.T) {
	incompressible := make([]byte, 256*1024)
	if _, err := rand.Read(incompressible); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if len(incompressible) >= MaxDecodedBytes {
		t.Fatalf("test input must be under the decoded ceiling to isolate the env-entry guard")
	}
	_, _, err := Encode(New(map[string][]byte{"blob.bin": incompressible}))
	if err == nil {
		t.Fatal("expected error for incompressible payload over the env entry limit, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "environment entry") {
		t.Fatalf("error should identify the env entry limit as the breached one, got:\n%s", msg)
	}
	for _, want := range []string{"65536", "64 KiB", "131072", "custom", "PVC"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("env-entry error missing %q:\n%s", want, msg)
		}
	}
}

// TestEncodeAcceptsRealisticScriptWellOverOldCap is the regression test for the
// reported failure: a 66 KiB research pipeline used to be rejected outright by
// the old 64 KiB cap. Compression must leave it comfortably inside the budget.
func TestEncodeAcceptsRealisticScriptWellOverOldCap(t *testing.T) {
	const oldCap = 64 * 1024
	script := realisticPythonSource(66381) // the exact reported size
	if len(script) <= oldCap {
		t.Fatalf("test input must exceed the old cap to be a regression test, got %d", len(script))
	}
	encoded, _, err := Encode(New(map[string][]byte{"pipeline.py": script}))
	if err != nil {
		t.Fatalf("a %d-byte research script must render after compression: %v", len(script), err)
	}
	entry := len(EnvB64) + 1 + len(encoded)
	t.Logf("%d-byte script -> %d-byte env entry (limit=%d, headroom=%d bytes)",
		len(script), entry, MaxEnvEntryBytes, MaxEnvEntryBytes-entry)
}

// TestEnvEntryLimitBoundaryIsExact pins both sides of the binding guard with a
// byte-exact boundary. Incompressible input is used so the crossing point is
// deterministic and the search is cheap: the largest accepted payload must
// produce an entry at or under MaxEnvEntryBytes, and one byte more must be
// rejected.
func TestEnvEntryLimitBoundaryIsExact(t *testing.T) {
	blob := make([]byte, 256*1024)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	fits := func(n int) bool {
		_, _, err := Encode(New(map[string][]byte{"blob.bin": blob[:n]}))
		return err == nil
	}
	lo, hi := 1, len(blob)
	if !fits(lo) {
		t.Fatal("a 1-byte payload must always fit")
	}
	if fits(hi) {
		t.Fatal("test blob is not large enough to cross the env entry limit")
	}
	for lo < hi-1 {
		mid := (lo + hi) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}

	encoded, _, err := Encode(New(map[string][]byte{"blob.bin": blob[:lo]}))
	if err != nil {
		t.Fatalf("largest fitting payload (%d bytes) must encode: %v", lo, err)
	}
	entry := len(EnvB64) + 1 + len(encoded)
	if entry > MaxEnvEntryBytes {
		t.Fatalf("accepted payload produced an over-limit entry: %d > %d", entry, MaxEnvEntryBytes)
	}
	if _, _, err := Encode(New(map[string][]byte{"blob.bin": blob[:lo+1]})); err == nil {
		t.Fatalf("one byte past the boundary (%d bytes) must be rejected", lo+1)
	}
	t.Logf("env entry boundary: %d incompressible bytes accepted (%d-byte entry), %d rejected",
		lo, entry, lo+1)
}

// realisticPythonSource generates deterministic, realistically-compressible
// Python-shaped source of exactly n bytes. Using repeated "x" bytes would make
// compression look far better than it is on real code; using random bytes would
// make it look far worse.
func realisticPythonSource(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "def stage_%d(dataset, config):\n"+
			"    # Normalize the %d-th shard before the export stage runs.\n"+
			"    rows = dataset.where(dataset[\"score_%d\"] > config.threshold)\n"+
			"    return rows.select(\"url\", \"text\", \"score_%d\")\n\n", i, i, i%7, i%11)
	}
	return b.Bytes()[:n]
}

// TestEncodedEnvValueStaysUnderMaxArgStrlen proves the budgeted
// MaxEnvEntryBytes really does keep the single TAU_PAYLOAD_B64=<encoded>
// environment entry under Linux's MAX_ARG_STRLEN (131072 bytes), the kernel's
// per-argument/environment value size limit enforced by execve(2). This is the
// constraint the payload cap is derived from -- not etcd object size, and not
// the client-side apply annotation limit.
func TestEncodedEnvValueStaysUnderMaxArgStrlen(t *testing.T) {
	if MaxEnvEntryBytes >= maxArgStrlen {
		t.Fatalf("MaxEnvEntryBytes=%d must stay under MAX_ARG_STRLEN=%d", MaxEnvEntryBytes, maxArgStrlen)
	}
	atCap := realisticPythonSource(200 * 1024)
	encoded, _, err := Encode(New(map[string][]byte{"train.py": atCap}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	envEntry := EnvB64 + "=" + encoded
	if len(envEntry) >= maxArgStrlen {
		t.Fatalf("TAU_PAYLOAD_B64 env entry is %d bytes, want < %d (Linux MAX_ARG_STRLEN)", len(envEntry), maxArgStrlen)
	}
	t.Logf("TAU_PAYLOAD_B64=<...> env entry for %d bytes of Python: %d bytes (budget=%d, MAX_ARG_STRLEN=%d)",
		len(atCap), len(envEntry), MaxEnvEntryBytes, maxArgStrlen)
}

// TestEncodeCompressesEnvelope pins the actual mechanism: the transported bytes
// must be gzip, and must be materially smaller than the raw JSON envelope.
func TestEncodeCompressesEnvelope(t *testing.T) {
	script := realisticPythonSource(64 * 1024)
	encoded, _, err := Encode(New(map[string][]byte{"train.py": script}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	transported, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if !bytes.HasPrefix(transported, []byte{0x1f, 0x8b}) {
		t.Fatalf("transported envelope is not gzip-compressed: % x", transported[:4])
	}
	if len(transported) >= len(script) {
		t.Fatalf("compressed envelope (%d bytes) is not smaller than the raw script (%d bytes)",
			len(transported), len(script))
	}
}

func TestDecodeRoundTrips(t *testing.T) {
	files := map[string][]byte{
		"train.py":         []byte("from ray.train.torch import TorchTrainer\n"),
		"requirements.txt": []byte("torch==2.4.0\ntransformers\n"),
	}
	encoded, digest, err := Encode(New(files))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(encoded, digest)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(files) {
		t.Fatalf("decoded file count=%d want %d", len(got), len(files))
	}
	for name, want := range files {
		if string(got[name]) != string(want) {
			t.Fatalf("decoded file %q=%q want %q", name, got[name], want)
		}
	}
}

func TestDecodeFailsIntegrityCheckOnTamperedDigest(t *testing.T) {
	encoded, digest, err := Encode(New(map[string][]byte{"train.py": []byte("print('x')\n")}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tampered := "0" + digest[1:]
	if tampered == digest {
		tampered = digest[:len(digest)-1] + "f"
	}
	_, err = Decode(encoded, tampered)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity check failure, got %v", err)
	}
}

func TestDecodeFailsIntegrityCheckOnTamperedEnvelope(t *testing.T) {
	encoded, digest, err := Encode(New(map[string][]byte{"train.py": []byte("print('x')\n")}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Flip the encoded envelope's first character to simulate truncation or
	// corruption in transit.
	mutated := []byte(encoded)
	if mutated[0] == 'A' {
		mutated[0] = 'B'
	} else {
		mutated[0] = 'A'
	}
	_, err = Decode(string(mutated), digest)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity check failure on corrupted envelope, got %v", err)
	}
}

// TestInitContainerScriptDecodesAndVerifies executes the exact Python script
// embedded via InitContainerScript against a real python3 interpreter,
// proving the runtime integrity/decoding logic (not just the Go mirror in
// Decode) works end-to-end. It skips if python3 is not on PATH.
func TestInitContainerScriptDecodesAndVerifies(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping runtime init-container script test")
	}

	files := map[string][]byte{
		"train.py":         []byte("from ray.train.torch import TorchTrainer\n"),
		"requirements.txt": []byte("torch==2.4.0\n"),
	}
	encoded, digest, err := Encode(New(files))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	targetDir := t.TempDir()
	cmd := exec.Command(python3, "-c", InitContainerScript)
	cmd.Env = append(os.Environ(),
		EnvB64+"="+encoded,
		EnvDigest+"="+digest,
		EnvTargetDir+"="+targetDir,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("init container script failed: %v\nstderr:\n%s", err, stderr.String())
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(targetDir, name))
		if err != nil {
			t.Fatalf("expected file %q to be written: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("written file %q=%q want %q", name, got, want)
		}
	}
}

// TestInitContainerScriptRejectsTamperedDigest proves the runtime script
// fails closed (non-zero exit, no files written) when the digest does not
// match, matching Decode's behavior.
func TestInitContainerScriptRejectsTamperedDigest(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping runtime init-container script test")
	}

	encoded, digest, err := Encode(New(map[string][]byte{"train.py": []byte("print('x')\n")}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tampered := "0" + digest[1:]

	targetDir := t.TempDir()
	cmd := exec.Command(python3, "-c", InitContainerScript)
	cmd.Env = append(os.Environ(),
		EnvB64+"="+encoded,
		EnvDigest+"="+tampered,
		EnvTargetDir+"="+targetDir,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit for tampered digest, stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "integrity check failed") {
		t.Fatalf("expected integrity check failure message, got stderr:\n%s", stderr.String())
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("read target dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files written on integrity failure, found: %v", entries)
	}
}
