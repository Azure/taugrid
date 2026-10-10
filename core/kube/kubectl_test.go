// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunnerBaseArgsIncludeIsolatedKubeconfig(t *testing.T) {
	runner := NewWithKubeconfig("aks-flex", "/tmp/tau-kubeconfig")
	want := []string{"--kubeconfig", "/tmp/tau-kubeconfig", "--context", "aks-flex"}
	if got := runner.baseArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs = %#v, want %#v", got, want)
	}
}

func TestRunnerRawStopsWaitingForInheritedPipesAfterCancellation(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "child.pid")
	script := filepath.Join(tempDir, "kubectl")
	err := os.WriteFile(script, []byte(`#!/bin/sh
	python3 -c 'import os, time; os.setsid(); open(os.environ["KUBECTL_CHILD_PID_FILE"], "w").write(str(os.getpid())); time.sleep(30)' &
	echo started
	wait
	`), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("KUBECTL_CHILD_PID_FILE", pidFile)
	runner := &Runner{Path: script}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	output, err := runner.Raw(ctx, nil, nil)
	elapsed := time.Since(started)
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Raw error = %v, context error = %v; want canceled command", err, ctx.Err())
	}
	if !strings.Contains(output, "started") {
		t.Fatalf("Raw output = %q, want child startup marker", output)
	}
	if elapsed > kubectlWaitDelay+time.Second {
		t.Fatalf("Raw returned after %s, want inherited pipes closed near %s", elapsed, kubectlWaitDelay)
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	process, findErr := os.FindProcess(pid)
	if findErr == nil {
		_ = process.Kill()
	}
}
