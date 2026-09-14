// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/status"
)

func TestRunLogsDatabaseControlsCannotReachQueryFailure(t *testing.T) {
	for _, database := range []string{
		"Logs\x1b[2Jspoofed", "Logs\nspoofed", "Logs\rspoofed",
		"Logs\tspoofed", "Logs\x00spoofed", "Logs\x7fspoofed", "Logs\u009b2Jspoofed",
	} {
		t.Run(fmt.Sprintf("%q", database), func(t *testing.T) {
			payload, err := json.Marshal(logConnection{
				Schema: logConnectionSchema, Endpoint: "https://logs.example", Database: database, Cluster: "worker",
			})
			if err != nil {
				t.Fatal(err)
			}
			runner := rawRunnerFunc(func(context.Context, []string, []byte) (string, error) {
				return loggingConfigMapJSON(t, string(payload)), nil
			})
			queryCalled := false
			var out bytes.Buffer
			err = runLogsCommandWithHooks(context.Background(), &out, nil, "train", runLogsOptions{
				Namespace: "research", Tail: 10,
			}, runLogsHooks{
				fetchSnapshot: func(context.Context) (status.Snapshot, error) {
					return status.Snapshot{RayJob: status.RayJob{
						Found: true, JobStatus: "SUCCEEDED", RayClusterName: "train-cluster",
					}}, nil
				},
				rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
					return "", errors.New("head pod not found")
				},
				resolveLogConnection: func(ctx context.Context) (logConnection, error) {
					return fetchLogConnection(ctx, runner, "custom-system")
				},
				queryADXLogs: func(_ context.Context, query kustoLogsQuery) ([]kustoquery.Row, error) {
					queryCalled = true
					return nil, fmt.Errorf("endpoint=%s database=%s: query failed", query.Endpoint, query.Database)
				},
			})
			if err == nil || !strings.Contains(err.Error(), "database must not contain control characters") {
				t.Fatalf("expected database validation error, got %q", err)
			}
			if queryCalled || out.Len() != 0 || strings.IndexFunc(err.Error(), unicode.IsControl) >= 0 {
				t.Fatalf("unsafe database reached query or diagnostics: called=%v output=%q error=%q", queryCalled, out.String(), err)
			}
		})
	}
}

func TestResolveTerminalLogConnectionDatabaseOverrideControls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		explicit   string
		discovered string
		wantError  bool
	}{
		{"safe override replaces unsafe metadata", "Archive", "Logs\x1b[2J", false},
		{"unsafe override rejected", "Archive\x1b[2J", "Logs", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTerminalLogConnection(context.Background(), runLogsOptions{
				KustoDatabase: tc.explicit,
			}, runLogsHooks{resolveLogConnection: func(context.Context) (logConnection, error) {
				return logConnection{Endpoint: "https://logs.example", Database: tc.discovered, Cluster: "worker"}, nil
			}})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "database must not contain control characters") {
					t.Fatalf("expected database validation error, got %q", err)
				}
			} else if err != nil || got.KustoDatabase != tc.explicit {
				t.Fatalf("safe override not preserved: database=%q error=%v", got.KustoDatabase, err)
			}
		})
	}
}
