// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileURIFromPathRoundTripsLocalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dataset")
	uri, err := fileURIFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("Windows file URI = %q, want file:///X:/...", uri)
	}
	got, err := localPathFromFileURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("file URI round trip = %q, want %q", got, path)
	}
}
