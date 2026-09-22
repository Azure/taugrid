// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type runFixture struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	State     string `json:"state"`
	Workspace string `json:"workspace"`
}

func TestPortalHistoricalBoundaries(t *testing.T) {
	portal := buildPortal(t)
	portal.seed(t, []runFixture{
		{"before", "2026-09-16T09:59:59Z", "succeeded", "history-e2e"},
		{"start", "2026-09-16T10:00:00Z", "succeeded", "history-e2e"},
		{"middle", "2026-09-16T10:30:00Z", "failed", "history-e2e"},
		{"end", "2026-09-16T11:00:00Z", "succeeded", "history-e2e"},
		{"after", "2026-09-16T11:00:01Z", "succeeded", "history-e2e"},
		{"other-workspace", "2026-09-16T10:45:00Z", "succeeded", "another-workspace"},
		{"sub-before", "2026-09-17T10:00:00.099Z", "succeeded", "history-e2e"},
		{"sub-start", "2026-09-17T10:00:00.1Z", "succeeded", "history-e2e"},
		{"sub-middle", "2026-09-17T10:00:00.5Z", "succeeded", "history-e2e"},
		{"sub-end", "2026-09-17T10:00:00.9Z", "succeeded", "history-e2e"},
		{"sub-after", "2026-09-17T10:00:00.901Z", "succeeded", "history-e2e"},
	})
	portal.start(t)
	t.Run("inclusive bounds and filtering before limit", func(t *testing.T) {
		query := url.Values{"target": {"history-e2e"}, "start": {"2026-09-16T10:00:00Z"}, "end": {"2026-09-16T11:00:00Z"}, "limit": {"1"}}
		page := portal.runs(t, query)
		require.Equal(t, []string{"end"}, runIDs(page))
		require.True(t, page.Truncated)
		query.Set("limit", "2")
		page = portal.runs(t, query)
		require.Equal(t, []string{"end", "middle"}, runIDs(page))
		require.True(t, page.Truncated)
		query.Set("limit", "3")
		page = portal.runs(t, query)
		require.Equal(t, []string{"end", "middle", "start"}, runIDs(page))
		require.False(t, page.Truncated)
		require.Equal(t, "failed", page.Runs[1].LifecycleState)
		query.Set("start", "2026-09-16T12:00:00+02:00")
		query.Set("end", "2026-09-16T13:00:00+02:00")
		require.Equal(t, page, portal.runs(t, query))
		query.Set("lifecycle", "succeeded")
		require.Equal(t, []string{"end", "start"}, runIDs(portal.runs(t, query)))
	})
	t.Run("positive subsecond interval", func(t *testing.T) {
		query := url.Values{"start": {"2026-09-17T10:00:00.100Z"}, "end": {"2026-09-17T10:00:00.900Z"}}
		require.Equal(t, []string{"sub-end", "sub-middle", "sub-start"}, runIDs(portal.runs(t, query)))
	})
	t.Run("invalid then empty then valid range", func(t *testing.T) {
		for _, raw := range []string{
			"start=2026-09-16T10:00:00Z",
			"start=2026-09-16T11:00:00Z&end=2026-09-16T10:00:00Z",
			"window=24h&start=2026-09-16T10:00:00Z&end=2026-09-16T11:00:00Z",
			"window=24h&window=168h",
		} {
			portal.get(t, "/api/v2/stellar/runs?"+raw, http.StatusBadRequest)
		}
		query := url.Values{"start": {"2026-09-18T00:00:00Z"}, "end": {"2026-09-18T01:00:00Z"}}
		empty := portal.runs(t, query)
		require.Empty(t, empty.Runs)
		require.False(t, empty.Truncated)
		query.Set("start", "2026-09-16T10:00:00Z")
		query.Set("end", "2026-09-16T11:00:00Z")
		require.Equal(t, []string{"end", "middle", "start"}, runIDs(portal.runs(t, query)))
	})
}

func TestPortalHistoricalMetricOnlyActivity(t *testing.T) {
	portal := buildPortal(t)
	now := time.Now().UTC().Truncate(time.Second)
	oldTime := now.Add(-60 * 24 * time.Hour).Format(time.RFC3339)
	portal.seed(t, []runFixture{
		{"active", oldTime, "running", "history-e2e"},
		{"quiet", oldTime, "running", "history-e2e"},
		{"newer-outside", now.Add(48 * time.Hour).Format(time.RFC3339), "succeeded", "history-e2e"},
	})
	portal.start(t)
	query := url.Values{"start": {now.Add(-time.Hour).Format(time.RFC3339)}, "end": {now.Add(time.Hour).Format(time.RFC3339)}, "limit": {"1"}}
	require.Empty(t, portal.runs(t, query).Runs)
	history := filepath.Join(portal.dir, "fresh.jsonl")
	require.NoError(t, os.WriteFile(history, []byte(fmt.Sprintf("{\"_step\":2,\"_timestamp\":%d,\"train/loss\":0.21}\n", now.Unix())), 0o600))
	portal.command(t, "experiment", "import", "jsonl", "--store", portal.store, "--run", "active", "--history", history, "--json")
	for _, bounds := range []url.Values{query, {"window": {"24h"}, "limit": {"1"}}} {
		for _, lifecycle := range []string{"", "running"} {
			bounds.Set("lifecycle", lifecycle)
			page := portal.runs(t, bounds)
			require.Equal(t, []string{"active"}, runIDs(page))
			require.False(t, page.Truncated)
			require.Equal(t, oldTime, page.Runs[0].CreatedAt)
			require.Equal(t, "running", page.Runs[0].LifecycleState)
		}
	}
}

func (portal *portalProcess) seed(t *testing.T, fixtures []runFixture) {
	t.Helper()
	portal.command(t, "experiment", "init", "history-e2e", "--store", portal.store, "--project", "history-e2e")
	history := filepath.Join(portal.dir, "seed.jsonl")
	require.NoError(t, os.WriteFile(history, []byte("{\"_step\":1,\"train/loss\":0.42}\n"), 0o600))
	for _, fixture := range fixtures {
		portal.command(t, "experiment", "import", "jsonl", "--store", portal.store, "--run", fixture.ID,
			"--history", history, "--state", fixture.State, "--experiment", "history-e2e", "--tag", "tau_workspace="+fixture.Workspace)
	}
	data, err := json.Marshal(fixtures)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-c", `
import json
import sqlite3
import sys

with sqlite3.connect(sys.argv[1]) as database:
    assert database.execute("SELECT count(*) FROM events").fetchone()[0] == 0
    for fixture in json.loads(sys.argv[2]):
        timestamp = fixture["time"]
        run_id = fixture["id"]
        result = database.execute(
            "UPDATE runs SET created_at = ?, started_at = ?, completed_at = '' WHERE run_id = ?",
            (timestamp, timestamp, run_id),
        )
        assert result.rowcount == 1
        result = database.execute(
            "UPDATE metric_files SET created_at = ? WHERE run_id = ?", (timestamp, run_id)
        )
        assert result.rowcount == 1
`, filepath.Join(portal.store, "index.sqlite"), string(data))
	output, err := command.CombinedOutput()
	require.NoError(t, err, "set explicit fixture timestamps: %s", output)
}

func runIDs(page runPage) []string {
	ids := make([]string, len(page.Runs))
	for index, run := range page.Runs {
		ids[index] = run.RunID
	}
	return ids
}
