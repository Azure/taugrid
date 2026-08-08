// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package bundle writes diagnostic artifacts captured by e2e tests.
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

// Writer creates files in a directory dedicated to one test.
type Writer struct {
	dir      string
	t        testing.TB
	fileOnly bool
}

// Root returns the configured diagnostic bundle directory.
func Root() string {
	if root := os.Getenv("E2E_BUNDLE_DIR"); root != "" {
		return root
	}
	return defaultBundleRoot
}

// New returns a Writer scoped to the current test.
func New(t testing.TB) *Writer {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	return &Writer{
		dir:      filepath.Join(Root(), name),
		t:        t,
		fileOnly: os.Getenv("E2E_BUNDLE_DIR") != "",
	}
}

// Dir returns the test's diagnostic directory.
func (w *Writer) Dir() string {
	return w.dir
}

// WriteFile writes data to a file in the test's diagnostic directory.
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

// WriterFor returns a writer for one diagnostic file. Local runs also copy
// output to the test log; CI runs keep it only in the artifact file.
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
