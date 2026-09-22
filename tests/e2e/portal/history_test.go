// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portal_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type portalProcess struct {
	binary string
	store  string
	dir    string
	base   string
	client *http.Client
}

type runPage struct {
	Runs []struct {
		RunID          string `json:"run_id"`
		CreatedAt      string `json:"created_at"`
		LifecycleState string `json:"lifecycle_state"`
	} `json:"runs"`
	Truncated bool `json:"truncated"`
}

func TestPortalHistoricalImport(t *testing.T) {
	portal := buildPortal(t)
	portal.command(t, "experiment", "init", "history-e2e", "--store", portal.store, "--project", "history-e2e")
	history := filepath.Join(portal.dir, "history.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(fmt.Sprintf("{\"_step\":1,\"_timestamp\":%d,\"train/loss\":0.42}\n", time.Now().Unix())), 0o600))
	importArgs := []string{"experiment", "import", "jsonl", "--store", portal.store, "--run", "imported", "--history", history, "--experiment", "history-e2e", "--tag", "tau_workspace=history-e2e", "--json"}
	portal.command(t, importArgs...)
	portal.start(t)

	query := url.Values{"target": {"history-e2e"}, "window": {"24h"}, "limit": {"1"}}
	first := portal.runs(t, query)
	require.Len(t, first.Runs, 1)
	require.Equal(t, "imported", first.Runs[0].RunID)
	require.Equal(t, "succeeded", first.Runs[0].LifecycleState)
	require.False(t, first.Truncated)

	portal.command(t, importArgs...)
	require.Equal(t, first, portal.runs(t, query), "idempotent import must not duplicate runs")
	portal.get(t, "/api/v2/stellar/runs?window=invalid", http.StatusBadRequest)
	portal.get(t, "/api/v2/stellar/runs?workspace=another-workspace", http.StatusForbidden)
	require.Equal(t, first, portal.runs(t, query), "failed requests must not affect the next valid search")
	portal.get(t, "/portal/experiments", http.StatusOK)
}

func buildPortal(t *testing.T) *portalProcess {
	t.Helper()
	workingDir, err := os.Getwd()
	require.NoError(t, err)
	dir := t.TempDir()
	portal := &portalProcess{
		binary: filepath.Join(dir, "taugrid-portal"),
		store:  filepath.Join(dir, "store"),
		dir:    dir,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", portal.binary, "./cmd/taugrid-portal")
	command.Dir = filepath.Join(workingDir, "..", "..", "..", "portal")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "build current Portal: %s", output)
	return portal
}

func (portal *portalProcess) command(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, portal.binary, args...)
	command.Dir = portal.dir
	command.Env = portalEnvironment()
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%v: %s", args, output)
	return output
}

func (portal *portalProcess) start(t *testing.T) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "portal.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logFile.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, portal.binary, "portal", "serve", "--store", portal.store,
		"--source", "local", "--workspace", "history-e2e", "--addr", "127.0.0.1:0",
		"--kubeconfig", filepath.Join(portal.dir, "no-kubeconfig"))
	command.Dir = portal.dir
	command.Env = portalEnvironment()
	command.Stdout = logFile
	stderr, err := command.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	ready := make(chan string, 1)
	done := make(chan struct{})
	var processError error
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(logFile, line)
			if address, ok := strings.CutPrefix(line, "serving taugrid-portal portal at "); ok {
				ready <- strings.TrimSuffix(address, "/portal")
			}
		}
		processError = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if t.Failed() {
			output, readErr := os.ReadFile(logPath)
			require.NoError(t, readErr)
			t.Logf("Portal output:\n%s", output)
		}
	})
	select {
	case portal.base = <-ready:
	case <-done:
		t.Fatalf("Portal exited before publishing a listening address: %v", processError)
	case <-time.After(30 * time.Second):
		t.Fatal("Portal did not publish a listening address within 30 seconds")
	}
	portal.get(t, "/healthz", http.StatusOK)
}

func (portal *portalProcess) get(t *testing.T, path string, status int) []byte {
	t.Helper()
	response, err := portal.client.Get(portal.base + path)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, status, response.StatusCode, "%s: %s", path, body)
	return body
}

func (portal *portalProcess) runs(t *testing.T, query url.Values) runPage {
	t.Helper()
	body := portal.get(t, "/api/v2/stellar/runs?"+query.Encode(), http.StatusOK)
	var result runPage
	require.NoError(t, json.Unmarshal(body, &result), "%s", body)
	return result
}

func portalEnvironment() []string {
	var environment []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "TAU_") || strings.HasPrefix(name, "KUBERNETES_") ||
			strings.HasPrefix(name, "AZURE_") || name == "KUBECONFIG" {
			continue
		}
		environment = append(environment, value)
	}
	return environment
}
