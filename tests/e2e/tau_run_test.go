// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTauSubmissionUsesRunConfigAndFailsClosed(t *testing.T) {
	config := map[string]any{"name": "nightly", "engine": "job", "entrypoint": "gpu.py"}
	files := map[string][]byte{"gpu.py": []byte("print('GPU')\n")}
	t.Setenv("TAU_BINARY", "")
	require.ErrorContains(t, SubmitTauRun(t, context.Background(), config, files), "requires TAU_BINARY")
	directory := t.TempDir()
	binary := filepath.Join(directory, "tau")
	args := filepath.Join(directory, "args")
	t.Setenv("TAU_TEST_ARGS", args)
	t.Setenv("TAU_BINARY", binary)
	t.Setenv("AI_RUNTIME_E2E_KUBE_CONTEXT", "test-context")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TAU_TEST_ARGS\"\nexit \"${TAU_TEST_EXIT:-0}\"\n"), 0700))
	require.NoError(t, SubmitTauRun(t, context.Background(), config, files))
	data, err := os.ReadFile(args)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 5)
	require.Equal(t, "run", lines[0])
	require.Equal(t, "--config", lines[1])
	require.FileExists(t, lines[2])
	require.Equal(t, []string{"--context", "test-context"}, lines[3:])
	require.FileExists(t, filepath.Join(filepath.Dir(lines[2]), "gpu.py"))
	t.Setenv("TAU_TEST_EXIT", "23")
	require.ErrorContains(t, SubmitTauRun(t, context.Background(), config, files), "exit status 23")
	require.NoError(t, os.Remove(args))
	require.ErrorContains(t, SubmitTauRun(t, context.Background(), config, map[string][]byte{"../unsafe.py": nil}), "invalid Tau source file")
	require.NoFileExists(t, args)
}

func TestTauCleanupUsesCancelAndFailsClosed(t *testing.T) {
	t.Setenv("TAU_BINARY", "")
	require.ErrorContains(t, CancelTauRun(context.Background(), "nightly", "matrix-43"), "requires TAU_BINARY")
	directory := t.TempDir()
	binary := filepath.Join(directory, "tau")
	args := filepath.Join(directory, "args")
	t.Setenv("TAU_TEST_ARGS", args)
	t.Setenv("TAU_BINARY", binary)
	t.Setenv("AI_RUNTIME_E2E_KUBE_CONTEXT", "test-context")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TAU_TEST_ARGS\"\nexit \"${TAU_TEST_EXIT:-0}\"\n"), 0700))
	require.NoError(t, CancelTauRun(context.Background(), "nightly", "matrix-43"))
	data, err := os.ReadFile(args)
	require.NoError(t, err)
	require.Equal(t, []string{"run", "cancel", "matrix-43", "--namespace", "nightly", "--teardown-timeout", "15m", "--context", "test-context"}, strings.Split(strings.TrimSpace(string(data)), "\n"))
	t.Setenv("TAU_TEST_EXIT", "23")
	require.ErrorContains(t, CancelTauRun(context.Background(), "nightly", "matrix-43"), "exit status 23")
}
