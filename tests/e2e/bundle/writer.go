// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bundle

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const defaultBundleRoot = "e2e-bundle"

type Writer struct {
	dir      string
	t        testing.TB
	fileOnly bool
}

func Root() string {
	if root := os.Getenv("E2E_BUNDLE_DIR"); root != "" {
		return root
	}
	return defaultBundleRoot
}

func New(t testing.TB) *Writer {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	return &Writer{
		dir:      filepath.Join(Root(), name),
		t:        t,
		fileOnly: os.Getenv("E2E_BUNDLE_DIR") != "",
	}
}

func (w *Writer) Dir() string {
	return w.dir
}

func (w *Writer) WriteFile(name string, data []byte) error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("bundle: create dir %s: %w", w.dir, err)
	}
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	return nil
}

func (w *Writer) WriterFor(name string) io.Writer {
	return &teeWriter{bundleWriter: w, name: name, t: w.t, fileOnly: w.fileOnly}
}

type teeWriter struct {
	bundleWriter *Writer
	name         string
	t            testing.TB
	fileOnly     bool
	file         *os.File
}

func (tw *teeWriter) Write(p []byte) (int, error) {
	tw.t.Helper()
	if tw.file == nil {
		if err := os.MkdirAll(tw.bundleWriter.dir, 0o755); err != nil {
			return 0, fmt.Errorf("bundle: create dir %s: %w", tw.bundleWriter.dir, err)
		}
		path := filepath.Join(tw.bundleWriter.dir, tw.name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, fmt.Errorf("bundle: open %s: %w", path, err)
		}
		tw.file = file
		tw.t.Cleanup(func() { _ = tw.file.Close() })
	}

	n, err := tw.file.Write(p)
	if !tw.fileOnly {
		tw.t.Logf("%s", strings.TrimRight(string(p[:n]), "\n"))
	}
	return n, err
}
