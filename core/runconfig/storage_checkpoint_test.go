// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import "testing"

// storage.checkpoint is joined onto <durable>/finetunes/<run>/artifacts/ by the
// finalizer, on a PVC shared by every run in the namespace. An unvalidated
// value is therefore a write primitive into another researcher's run, not a
// local mistake. The manifest entry point already rejected these; the run-config
// entry point reached the same finalizer without the check.
func TestStorageValidateRejectsCheckpointEscape(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint string
	}{
		{"parent traversal", "../../victim/model.bin"},
		{"single parent", "../model.bin"},
		{"absolute discards the run dir entirely", "/etc/passwd"},
		{"dot segment", "./model.bin"},
		{"interior traversal", "ckpt/../../../victim/model.bin"},
		{"empty segment", "ckpt//model.bin"},
		{"surrounding whitespace", " last.safetensors"},
		{"windows separator", `..\victim\model.bin`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Storage{Checkpoint: tc.checkpoint}.Validate()
			if err == nil {
				t.Fatalf("storage.checkpoint %q was accepted; it escapes the run's artifact directory", tc.checkpoint)
			}
		})
	}
}

// Positive control: the guard must not be so broad that it rejects the value
// the feature exists to carry. Without this, a validator that rejected
// everything would pass the test above.
func TestStorageValidateAcceptsOrdinaryCheckpoints(t *testing.T) {
	for _, checkpoint := range []string{
		"last.safetensors",
		"checkpoints/last.safetensors",
		"epoch-3/model.bin",
		"",
	} {
		if err := (Storage{Checkpoint: checkpoint}).Validate(); err != nil {
			t.Fatalf("storage.checkpoint %q should be accepted, got %v", checkpoint, err)
		}
	}
}
