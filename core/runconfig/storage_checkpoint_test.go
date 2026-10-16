// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import "testing"

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
