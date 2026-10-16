// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scriptpayload

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenVectorSingleFileMatchesPR1Format(t *testing.T) {
	const (
		wantEncoded = "eyJ2ZXJzaW9uIjoxLCJmaWxlcyI6W3sibmFtZSI6InRyYWluLnB5IiwiZGF0YSI6ImNISnBiblFvSjJkdmJHUmxiaTEyWldOMGIzSW5LUW89In1dfQ=="
		wantDigest  = "bf8d956f29bdeb979a458003c4f05184a19b49875b80565626d3e2d89185e632"
	)

	encoded, digest, err := Encode(map[string][]byte{
		"train.py": []byte("print('golden-vector')\n"),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encoded != wantEncoded {
		t.Fatalf("encoded envelope diverges from PR1's payload package golden vector:\ngot:  %s\nwant: %s", encoded, wantEncoded)
	}
	if digest != wantDigest {
		t.Fatalf("digest diverges from PR1's payload package golden vector:\ngot:  %s\nwant: %s", digest, wantDigest)
	}
}

func TestGoldenVectorMultiFileMatchesPR1FormatOrdering(t *testing.T) {
	const (
		wantEncoded = "eyJ2ZXJzaW9uIjoxLCJmaWxlcyI6W3sibmFtZSI6ImEucHkiLCJkYXRhIjoiWVMxamIyNTBaVzUwQ2c9PSJ9LHsibmFtZSI6ImIucHkiLCJkYXRhIjoiWWkxamIyNTBaVzUwQ2c9PSJ9XX0="
		wantDigest  = "e99e7829fa69c083a8be66b6f80abc0fd5bb51bb701a20f661d004e98961457b"
	)

	encoded, digest, err := Encode(map[string][]byte{
		"b.py": []byte("b-content\n"),
		"a.py": []byte("a-content\n"),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encoded != wantEncoded {
		t.Fatalf("encoded envelope diverges from PR1's payload package golden vector:\ngot:  %s\nwant: %s", encoded, wantEncoded)
	}
	if digest != wantDigest {
		t.Fatalf("digest diverges from PR1's payload package golden vector:\ngot:  %s\nwant: %s", digest, wantDigest)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	files := map[string][]byte{
		"a.py": []byte("print('a')\n"),
		"b.py": []byte("print('b')\n"),
	}
	encodedA, digestA, err := Encode(files)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encodedB, digestB, err := Encode(files)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encodedA != encodedB || digestA != digestB {
		t.Fatalf("encoding is not deterministic")
	}
}

func TestEncodeRejectsPayloadOverCap(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), MaxDecodedBytes+1)
	_, _, err := Encode(map[string][]byte{"train.py": oversized})
	if err == nil {
		t.Fatal("expected error for over-cap payload, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"65537", "65536", "64 KiB", "custom", "PVC"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("over-cap error missing %q, actionable detail expected:\n%s", want, msg)
		}
	}
}

func TestEncodeAcceptsPayloadExactlyAtCap(t *testing.T) {
	atCap := bytes.Repeat([]byte("x"), MaxDecodedBytes)
	_, _, err := Encode(map[string][]byte{"train.py": atCap})
	if err != nil {
		t.Fatalf("payload exactly at cap should be accepted: %v", err)
	}
}

func TestEncodedEnvValueStaysUnderMaxArgStrlen(t *testing.T) {
	const maxArgStrlen = 131072
	atCap := bytes.Repeat([]byte("x"), MaxDecodedBytes)
	encoded, _, err := Encode(map[string][]byte{"train.py": atCap})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	envEntry := EnvB64 + "=" + encoded
	if len(envEntry) >= maxArgStrlen {
		t.Fatalf("TAU_PAYLOAD_B64 env entry at payload cap is %d bytes, want < %d (Linux MAX_ARG_STRLEN)", len(envEntry), maxArgStrlen)
	}
	t.Logf("TAU_PAYLOAD_B64=<...> env entry at %d-byte payload cap: %d bytes (MAX_ARG_STRLEN=%d, headroom=%d bytes)",
		MaxDecodedBytes, len(envEntry), maxArgStrlen, maxArgStrlen-len(envEntry))
}

func TestDecodeRoundTrips(t *testing.T) {
	files := map[string][]byte{
		"inference_job.py": []byte("import ray\nprint('inference')\n"),
	}
	encoded, digest, err := Encode(files)
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
	encoded, digest, err := Encode(map[string][]byte{"train.py": []byte("print('x')\n")})
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
	encoded, digest, err := Encode(map[string][]byte{"train.py": []byte("print('x')\n")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
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

func TestInitContainerScriptDecodesAndVerifies(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping runtime init-container script test")
	}

	files := map[string][]byte{
		"inference_job.py": []byte("import ray\nprint('inference')\n"),
	}
	encoded, digest, err := Encode(files)
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

func TestInitContainerScriptRejectsTamperedDigest(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping runtime init-container script test")
	}

	encoded, digest, err := Encode(map[string][]byte{"train.py": []byte("print('x')\n")})
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

func TestInitContainerScriptRejectsUnsafeFileNames(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping runtime init-container script test")
	}

	encoded, digest, err := Encode(map[string][]byte{"../escape.py": []byte("print('x')\n")})
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
	err = cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit for unsafe file name, stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "rejecting unsafe file name") {
		t.Fatalf("expected unsafe file name rejection message, got stderr:\n%s", stderr.String())
	}
}
