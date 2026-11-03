// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"strings"
	"testing"
)

func TestStorageValidateRejectsInvalidHotSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hotSize string
		wantErr string
	}{
		{"zero", "0", "must be > 0"},
		{"zero with unit", "0Gi", "must be > 0"},
		{"negative", "-1Gi", "must be > 0"},
		{"malformed", "lots", "not a valid Kubernetes resource quantity"},
		{"bad unit", "2TB", "not a valid Kubernetes resource quantity"},
		{"inner space", "2 Ti", "not a valid Kubernetes resource quantity"},
		{"surrounding whitespace", " 2Ti", "not a valid Kubernetes resource quantity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Storage{HotSize: tc.hotSize}.Validate()
			if err == nil || !strings.Contains(err.Error(), "storage.hot_size") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("storage.hot_size %q error = %v, want %q", tc.hotSize, err, tc.wantErr)
			}
		})
	}
}

func TestStorageValidateAcceptsHotSize(t *testing.T) {
	for _, hotSize := range []string{"", "500Gi", "2Ti", "1G", "1073741824"} {
		if err := (Storage{HotSize: hotSize}).Validate(); err != nil {
			t.Fatalf("storage.hot_size %q should be accepted, got %v", hotSize, err)
		}
	}
}

func TestParseDirectConfigCarriesHotSize(t *testing.T) {
	cfg, _, err := parseWithDiagnostics([]byte("name: hot\nengine: job\nentrypoint: train.py\nstorage:\n  hot_size: 500Gi\n"), "tau.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.HotSize != "500Gi" {
		t.Fatalf("storage.hot_size = %q, want 500Gi", cfg.Storage.HotSize)
	}
}

func TestParseDirectConfigRejectsInvalidHotSize(t *testing.T) {
	_, _, err := parseWithDiagnostics([]byte("name: hot\nengine: job\nentrypoint: train.py\nstorage:\n  hot_size: \"0\"\n"), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "storage.hot_size") {
		t.Fatalf("parse error = %v, want storage.hot_size validation error", err)
	}
}
