package bundle_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/taugrid/tests/e2e/bundle"
)

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("E2E_BUNDLE_DIR", root)

	w := bundle.New(t)
	data := []byte("hello bundle")
	if err := w.WriteFile("output.txt", data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	wantDir := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_"))
	if w.Dir() != wantDir {
		t.Errorf("Dir() = %q, want %q", w.Dir(), wantDir)
	}
	got, err := os.ReadFile(filepath.Join(wantDir, "output.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("file contents = %q, want %q", got, data)
	}
}

func TestWriterFor(t *testing.T) {
	t.Run("local tees to log", func(t *testing.T) {
		t.Setenv("E2E_BUNDLE_DIR", "")
		t.Cleanup(func() { _ = os.RemoveAll("e2e-bundle") })

		var logs []string
		capture := &logCapture{t: t, lines: &logs}
		writer := bundle.New(capture).WriterFor("diag.txt")
		if _, err := fmt.Fprint(writer, "diagnostic line\n"); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "diagnostic line") {
			t.Fatalf("logs = %v, want diagnostic line", logs)
		}
	})

	t.Run("CI writes only to file", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("E2E_BUNDLE_DIR", root)

		var logs []string
		capture := &logCapture{t: t, lines: &logs}
		writer := bundle.New(capture)
		if _, err := fmt.Fprint(writer.WriterFor("diag.txt"), "diagnostic line\n"); err != nil {
			t.Fatalf("Write: %v", err)
		}

		got, err := os.ReadFile(filepath.Join(writer.Dir(), "diag.txt"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "diagnostic line\n" {
			t.Errorf("file = %q, want diagnostic line", got)
		}
		if len(logs) != 0 {
			t.Errorf("logs = %v, want no CI log copy", logs)
		}
	})
}

type logCapture struct {
	testing.TB
	t     *testing.T
	mu    sync.Mutex
	lines *[]string
}

func (l *logCapture) Helper() {
	l.t.Helper()
}

func (l *logCapture) Name() string {
	return l.t.Name()
}

func (l *logCapture) Logf(format string, args ...any) {
	l.mu.Lock()
	*l.lines = append(*l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logCapture) Cleanup(f func()) {
	l.t.Cleanup(f)
}
