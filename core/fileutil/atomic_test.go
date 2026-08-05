package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicCreatesParentAndReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WriteFileAtomic(path, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "second" {
		t.Fatalf("content = %q, want second", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("perm = %v, want 0600", got)
	}
}

func TestWriteFileAtomicSyncsBeforeRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	synced := false
	err := writeFileAtomic(path, []byte("state"), 0o600, func(f *os.File) error {
		synced = true
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("destination existed before temp file sync: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("temporary file was not synced")
	}

	syncErr := errors.New("sync failed")
	failedPath := filepath.Join(t.TempDir(), "failed", "state.json")
	err = writeFileAtomic(failedPath, []byte("state"), 0o600, func(*os.File) error {
		return syncErr
	})
	if !errors.Is(err, syncErr) {
		t.Fatalf("error = %v, want %v", err, syncErr)
	}
	if _, err := os.Stat(failedPath); !os.IsNotExist(err) {
		t.Fatalf("failed write created destination: %v", err)
	}
}

func TestChmodUnsupported(t *testing.T) {
	for _, err := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		if !ChmodUnsupported(err) {
			t.Fatalf("ChmodUnsupported(%v) = false, want true", err)
		}
		if !ChmodUnsupported(&os.PathError{Op: "chmod", Err: err}) {
			t.Fatalf("ChmodUnsupported(PathError{%v}) = false, want true", err)
		}
	}
	if ChmodUnsupported(errors.New("boom")) {
		t.Fatal("ChmodUnsupported(generic) = true, want false")
	}
	if ChmodUnsupported(syscall.ENOENT) {
		t.Fatal("ChmodUnsupported(ENOENT) = true, want false")
	}
}

func TestWriteJSONFileAtomicCreatesIndentedNewlineTerminatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WriteJSONFileAtomic(path, struct {
		Name string `json:"name"`
	}{Name: "tau"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"name\": \"tau\"\n}\n"
	if string(raw) != want {
		t.Fatalf("content = %q, want %q", raw, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("perm = %v, want 0644", got)
	}
}
