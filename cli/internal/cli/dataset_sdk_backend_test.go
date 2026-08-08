// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/storage"
)

// fakeBlobOps is an in-memory sdkBlobOps that enforces the same conditional
// semantics as the real Azure block-blob backend, so registry immutability can
// be exercised without a network.
type fakeBlobOps struct {
	blobs map[string][]byte
	// forceWriteErr, when non-nil, is returned by write regardless of state.
	forceWriteErr error
}

func newFakeBlobOps() *fakeBlobOps { return &fakeBlobOps{blobs: map[string][]byte{}} }

func (f *fakeBlobOps) read(_ context.Context, blobName string) ([]byte, error) {
	b, ok := f.blobs[blobName]
	if !ok {
		return nil, dataset.ErrNotExist
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (f *fakeBlobOps) write(_ context.Context, blobName string, data []byte, overwrite bool) error {
	if f.forceWriteErr != nil {
		return f.forceWriteErr
	}
	if !overwrite {
		if _, exists := f.blobs[blobName]; exists {
			// Mirrors If-None-Match:* → 409/412 mapped to ErrExist.
			return dataset.ErrExist
		}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.blobs[blobName] = cp
	return nil
}

func (f *fakeBlobOps) listChildren(_ context.Context, prefix string) ([]string, error) {
	seen := map[string]bool{}
	var children []string
	for name := range f.blobs {
		if prefix != "" && len(name) < len(prefix) {
			continue
		}
		if prefix != "" && name[:len(prefix)] != prefix {
			continue
		}
		rest := name[len(prefix):]
		if rest == "" {
			continue
		}
		seg := rest
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' {
				seg = rest[:i]
				break
			}
		}
		if seg == "" || seen[seg] {
			continue
		}
		seen[seg] = true
		children = append(children, seg)
	}
	sort.Strings(children)
	return children, nil
}

func (f *fakeBlobOps) remove(_ context.Context, blobName string) error {
	delete(f.blobs, blobName)
	return nil
}

func sdkBackendWithFake() (*sdkAzBackend, *fakeBlobOps) {
	ops := newFakeBlobOps()
	return &sdkAzBackend{ops: ops, rootPrefix: storage.DatasetRegistryDir}, ops
}

// The SDK backend must satisfy the immutable-create contract: a second write of
// the same path with overwrite=false returns ErrExist and does NOT clobber the
// existing bytes.
func TestSDKBackend_ImmutableWriteRejectsOverwrite(t *testing.T) {
	b, _ := sdkBackendWithFake()
	ctx := context.Background()
	p := storage.DatasetRegistryDir + "/ds/v1/record.json"

	if err := b.WriteFile(ctx, p, []byte("first"), false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := b.WriteFile(ctx, p, []byte("second"), false)
	if err == nil || !dataset.IsExist(err) {
		t.Fatalf("second immutable write: want IsExist error, got %v", err)
	}
	got, err := b.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("immutable bytes were clobbered: got %q", got)
	}
}

// overwrite=true (mutable status companion) always wins.
func TestSDKBackend_OverwriteReplaces(t *testing.T) {
	b, _ := sdkBackendWithFake()
	ctx := context.Background()
	p := storage.DatasetRegistryDir + "/ds/v1/ingest-status.json"
	if err := b.WriteFile(ctx, p, []byte("a"), true); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := b.WriteFile(ctx, p, []byte("b"), true); err != nil {
		t.Fatalf("write b: %v", err)
	}
	got, _ := b.ReadFile(ctx, p)
	if string(got) != "b" {
		t.Fatalf("overwrite failed: got %q", got)
	}
}

func TestSDKBackend_ReadMissingIsNotExist(t *testing.T) {
	b, _ := sdkBackendWithFake()
	_, err := b.ReadFile(context.Background(), storage.DatasetRegistryDir+"/missing")
	if err == nil || !dataset.IsNotExist(err) {
		t.Fatalf("read missing: want IsNotExist, got %v", err)
	}
}

func TestSDKBackend_DeleteMissingIsNil(t *testing.T) {
	b, _ := sdkBackendWithFake()
	if err := b.Delete(context.Background(), storage.DatasetRegistryDir+"/missing"); err != nil {
		t.Fatalf("delete missing: want nil, got %v", err)
	}
}

func TestSDKBackend_ListChildren(t *testing.T) {
	b, ops := sdkBackendWithFake()
	ctx := context.Background()
	for _, p := range []string{
		storage.DatasetRegistryDir + "/ds-a/v1/record.json",
		storage.DatasetRegistryDir + "/ds-a/v2/record.json",
		storage.DatasetRegistryDir + "/ds-b/v1/record.json",
	} {
		if err := b.WriteFile(ctx, p, []byte("x"), false); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	_ = ops
	got, err := b.List(ctx, storage.DatasetRegistryDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(got)
	want := []string{"ds-a", "ds-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("list children: want %v, got %v", want, got)
	}
}

// EnsureRegister over the SDK backend must be idempotent for an identical
// digest and fail on drift — proving the conditional write path composes with
// the registry immutability contract.
func TestSDKBackend_EnsureRegisterIdempotenceAndDrift(t *testing.T) {
	b, _ := sdkBackendWithFake()
	reg := dataset.NewRegistry(b, datasetRegistryPaths(), nil)
	ctx := context.Background()

	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          "ds",
		Version:       "v1",
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Prefix:        "ds/v1",
		Files:         []dataset.File{{Path: "a.bin", Bytes: 4, SHA256: strings.Repeat("a", 64)}},
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
	_, created, err := reg.EnsureRegister(ctx, rec)
	if err != nil || !created {
		t.Fatalf("first EnsureRegister: created=%v err=%v", created, err)
	}
	// Identical again → idempotent no-op.
	_, created2, err := reg.EnsureRegister(ctx, rec)
	if err != nil {
		t.Fatalf("idempotent EnsureRegister: %v", err)
	}
	if created2 {
		t.Fatalf("second identical EnsureRegister should be a no-op (created=false)")
	}
	// Drift → must fail.
	drift := rec
	drift.Files = []dataset.File{{Path: "a.bin", Bytes: 8, SHA256: strings.Repeat("b", 64)}}
	if _, _, err := reg.EnsureRegister(ctx, drift); err == nil {
		t.Fatalf("drift EnsureRegister must fail")
	}
}

func TestSDKBackend_WriteSurfacesUnexpectedError(t *testing.T) {
	b, ops := sdkBackendWithFake()
	sentinel := errors.New("boom")
	ops.forceWriteErr = sentinel
	err := b.WriteFile(context.Background(), storage.DatasetRegistryDir+"/x", []byte("y"), false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("write error should surface: got %v", err)
	}
}
