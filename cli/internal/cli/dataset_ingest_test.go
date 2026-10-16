// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
)

func fileRegistryWithIngest(t *testing.T) (*dataset.Registry, string) {
	t.Helper()
	root := t.TempDir()
	return dataset.NewRegistry(newFileBackend(root), datasetRegistryPaths(), nil), root
}

func testFileURI(t *testing.T, path string) string {
	t.Helper()
	uri, err := fileURIFromPath(path)
	if err != nil {
		t.Fatalf("file URI for %s: %v", path, err)
	}
	return uri
}

func ingestRecord(t *testing.T, reg *dataset.Registry, name, version string, files []dataset.File) dataset.Record {
	t.Helper()
	total := int64(0)
	for _, f := range files {
		total += f.Bytes
	}
	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          name,
		Version:       version,
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Prefix:        name + "/" + version,
		Files:         files,
		TotalBytes:    total,
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
	registered, _, err := reg.EnsureRegister(t.Context(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister: %v", err)
	}
	_, _, err = reg.InitIngestStatus(t.Context(), registered)
	if err != nil {
		t.Fatalf("InitIngestStatus: %v", err)
	}
	return registered
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestDatasetStatusCmd_notFound(t *testing.T) {
	_, regRoot := fileRegistryWithIngest(t)
	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"status", "nope@v1", "--registry", testFileURI(t, regRoot)})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing status")
	}
}

func TestDatasetStatusCmd_json(t *testing.T) {
	reg, regRoot := fileRegistryWithIngest(t)

	content := []byte("data bytes")
	sha := sha256hex(content)
	_ = ingestRecord(t, reg, "ds", "v1", []dataset.File{
		{Path: "data.txt", Bytes: int64(len(content)), SHA256: sha},
	})

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.txt"), content, 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdirAll dst: %v", err)
	}

	ingestCmd := newDatasetCmd()
	var buf bytes.Buffer
	ingestCmd.SetOut(&buf)
	ingestCmd.SetErr(&buf)
	ingestCmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"--source-root", testFileURI(t, srcDir),
		"--destination", testFileURI(t, dstDir),
		"-o", "json",
	})
	if err := ingestCmd.Execute(); err != nil {
		t.Fatalf("ingest command: %v (output: %s)", err, buf.String())
	}

	buf.Reset()
	statusCmd := newDatasetCmd()
	statusCmd.SetOut(&buf)
	statusCmd.SetErr(&buf)
	statusCmd.SetArgs([]string{
		"status", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"-o", "json",
	})
	if err := statusCmd.Execute(); err != nil {
		t.Fatalf("status command: %v (output: %s)", err, buf.String())
	}
	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parse status JSON: %v (raw: %s)", err, buf.String())
	}
	state, ok := result["state"].(string)
	if !ok {
		t.Fatalf("state missing from JSON: %s", buf.String())
	}
	if state != "ready" {
		t.Errorf("state: want ready, got %q", state)
	}
}

func TestDatasetIngestCmd_dryRun(t *testing.T) {
	reg, regRoot := fileRegistryWithIngest(t)
	content := []byte("dryrun bytes")
	sha := sha256hex(content)
	_ = ingestRecord(t, reg, "ds", "v1", []dataset.File{
		{Path: "data.txt", Bytes: int64(len(content)), SHA256: sha},
	})

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"--source-root", testFileURI(t, srcDir),
		"--destination", testFileURI(t, dstDir),
		"--dry-run",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run command: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dry-run") {
		t.Errorf("expected dry-run output, got: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "data.txt")); err == nil {
		t.Error("dry-run: destination file was written (should not be)")
	}
}

func TestDatasetIngestCmd_mutableWorkerImageRejected(t *testing.T) {
	_, regRoot := fileRegistryWithIngest(t)
	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"--source-root", "az://myaccount/datasets",
		"--workspace", "my-workspace",
		"--worker-image", "mcr.microsoft.com/tau:latest",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for mutable worker image tag")
	}
	if !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "mutable") {
		t.Errorf("error should mention digest/sha256/mutable: %v", err)
	}
}

func TestDatasetIngestCmd_sasTokenRejected(t *testing.T) {
	_, regRoot := fileRegistryWithIngest(t)
	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"ingest", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"--source-root", "https://myaccount.blob.core.windows.net/ctr?sv=2020-08-04&sig=abc",
		"--workspace", "my-workspace",
		"--worker-image", "mcr.microsoft.com/tau@sha256:" + strings.Repeat("a", 64),
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for SAS token in source-root")
	}
}

func TestDatasetIngestCmd_requiresVersion(t *testing.T) {
	_, regRoot := fileRegistryWithIngest(t)
	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"ingest", "ds",
		"--registry", testFileURI(t, regRoot),
		"--source-root", "file:///some/src",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when version is omitted")
	}
}

func TestDatasetStatusCmd_tableFormat(t *testing.T) {
	reg, regRoot := fileRegistryWithIngest(t)
	content := []byte("table-format bytes")
	sha := sha256hex(content)
	registered := ingestRecord(t, reg, "ds", "v1", []dataset.File{
		{Path: "data.txt", Bytes: int64(len(content)), SHA256: sha},
	})
	_ = registered

	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"status", "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"-o", "table",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status table command: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ds") {
		t.Errorf("table output should contain dataset name: %s", out)
	}
	if !strings.Contains(out, "registered") {
		t.Errorf("table output should contain state 'registered': %s", out)
	}
}

func TestDatasetIngestWorkerCmd_fileMode(t *testing.T) {
	reg, regRoot := fileRegistryWithIngest(t)
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := []byte("worker bytes")
	sha := sha256hex(content)
	_ = ingestRecord(t, reg, "ds", "v1", []dataset.File{
		{Path: "w.txt", Bytes: int64(len(content)), SHA256: sha},
	})
	if err := os.WriteFile(filepath.Join(srcDir, "w.txt"), content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cmd := newDatasetCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		ingestWorkerCmdName, "ds@v1",
		"--registry", testFileURI(t, regRoot),
		"--source-root", testFileURI(t, srcDir),
		"--destination", testFileURI(t, dstDir),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ingest-worker command: %v (output: %s)", err, buf.String())
	}

	var result datasetIngestResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parse worker output JSON: %v (raw: %s)", err, buf.String())
	}
	if result.SchemaVersion != datasetIngestResultSchemaVersion ||
		result.Status.State != dataset.IngestStateReady ||
		result.Reference.Digest != result.Status.RecordDigest {
		t.Errorf("worker output must be a complete ready result: %+v", result)
	}

	if _, err := os.Stat(filepath.Join(dstDir, "w.txt")); err != nil {
		t.Errorf("destination file not found: %v", err)
	}
}
