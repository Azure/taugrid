// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/status"
)

func TestRunLogsCommand_TerminalLocalRayJobDiscoversADX(t *testing.T) {
	t.Setenv("KUBECONFIG", "manager.conf")
	t.Setenv("TAU_CONTEXT", "manager")
	runner := fakeKubectlRunner(t, `#!/bin/sh
if [ "$*" != "--kubeconfig worker.conf --context worker-east -n custom-system get configmap tau-log-connection -o json" ]; then
  echo "unexpected route or discovery call: $*" >&2
  exit 1
fi
printf '%s' '`+loggingConfigMapJSON(t, `{"schema":"tau.logs.connection.v1","endpoint":"https://logs.example","database":"Logs","cluster":"worker-east"}`)+`'
`)
	runner.Context = "worker-east"
	runner.Kubeconfig = "worker.conf"
	var out bytes.Buffer
	err := runLogsCommandWithHooks(context.Background(), &out, runner, "train", runLogsOptions{
		Namespace:       "research",
		SystemNamespace: "custom-system",
		Tail:            10,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{RayJob: status.RayJob{
				Found: true, JobStatus: "SUCCEEDED", RayClusterName: "train-cluster",
			}}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("head pod not found for RayJob train")
		},
		queryADXLogs: func(_ context.Context, query kustoLogsQuery) ([]kustoquery.Row, error) {
			if query.Endpoint != "https://logs.example" || query.Database != "Logs" {
				t.Fatalf("connection = %#v", query)
			}
			for _, want := range []string{
				"| where Cluster == @'worker-east'",
				"| where Namespace == @'research'",
				"| where Pod startswith @'train-cluster-head'",
			} {
				if !strings.Contains(query.Query, want) {
					t.Fatalf("query missing %q: %s", want, query.Query)
				}
			}

			return []kustoquery.Row{{"Body": "training complete"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("historical log discovery failed: %v", err)
	}
	if out.String() != "training complete\n" {
		t.Fatalf("logs = %q", out.String())
	}
}

func loggingConfigMapJSON(t *testing.T, payload string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"name": logConnectionConfigMap, "namespace": "custom-system"},
		"data":     map[string]string{"connection.json": payload},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFetchLogConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantErr string
	}{
		{"complete", `{"schema":"tau.logs.connection.v1","endpoint":"https://logs.example","database":"Logs","cluster":"worker"}`, ""},
		{"partial", `{"schema":"tau.logs.connection.v1","cluster":"worker"}`, ""},
		{"missing data", "", "missing data[connection.json]"},
		{"invalid JSON", "{", "decode logging connection"},
		{"trailing JSON", `{"schema":"tau.logs.connection.v1"} {}`, "exactly one JSON object"},
		{"unknown field", `{"schema":"tau.logs.connection.v1","endpiont":"https://logs.example"}`, "unknown field"},
		{"wrong type", `{"schema":"tau.logs.connection.v1","cluster":["worker-a","worker-b"]}`, "cannot unmarshal array"},
		{"missing version", `{}`, "unsupported logging connection schema"},
		{"future version", `{"schema":"tau.logs.connection.v2"}`, "unsupported logging connection schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := rawRunnerFunc(func(_ context.Context, args []string, stdin []byte) (string, error) {
				want := []string{"-n", "custom-system", "get", "configmap", logConnectionConfigMap, "-o", "json"}
				if !reflect.DeepEqual(args, want) || stdin != nil {
					t.Fatalf("unexpected API request: %v %s", args, stdin)
				}
				return loggingConfigMapJSON(t, tc.payload), nil
			})
			got, err := fetchLogConnection(context.Background(), runner, "custom-system")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || got.Schema != logConnectionSchema || got.Cluster != "worker" {
				t.Fatalf("connection = %#v, error = %v", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  string
		err  error
		want string
	}{
		{"not found", "", errors.New("configmaps \"tau-log-connection\" not found"), "not found"},
		{"forbidden", "", errors.New("Forbidden: cannot get configmaps"), "Forbidden"},
		{"invalid response", "{", nil, "decode logging ConfigMap"},
		{"wrong namespace", `{"metadata":{"name":"tau-log-connection","namespace":"manager"}}`, nil, "identity mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := rawRunnerFunc(func(context.Context, []string, []byte) (string, error) { return tc.raw, tc.err })
			_, err := fetchLogConnection(context.Background(), runner, "custom-system")
			if err == nil || !strings.Contains(err.Error(), tc.want) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("error = %v, want %s with original cause", err, tc.want)
			}
		})
	}
}

func TestResolveTerminalLogConnectionOverrideCombinations(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			opts := runLogsOptions{}
			wantEndpoint, wantDatabase, wantCluster := "https://discovered.example", "Logs", "discovered-worker"
			if mask&1 != 0 {
				opts.KustoEndpoint, wantEndpoint = "https://explicit.example", "https://explicit.example"
			}
			if mask&2 != 0 {
				opts.KustoDatabase, wantDatabase = "Archive", "Archive"
			}
			if mask&4 != 0 {
				opts.KustoCluster, wantCluster = "explicit-worker", "explicit-worker"
			}
			calls := 0
			got, err := resolveTerminalLogConnection(context.Background(), opts, runLogsHooks{
				resolveLogConnection: func(context.Context) (logConnection, error) {
					calls++
					return logConnection{Endpoint: "https://discovered.example", Database: "Logs", Cluster: "discovered-worker"}, nil
				},
			})
			wantCalls := 1
			if mask == 7 {
				wantCalls = 0
			}
			if err != nil || calls != wantCalls || got.KustoEndpoint != wantEndpoint || got.KustoDatabase != wantDatabase || got.KustoCluster != wantCluster {
				t.Fatalf("connection = %#v, calls = %d, error = %v", got, calls, err)
			}
		})
	}
}

func TestResolveTerminalLogConnectionErrorsAndPartialMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       runLogsOptions
		discovered logConnection
		err        error
		want       string
	}{
		{"partial record completed by flag", runLogsOptions{KustoEndpoint: "https://explicit.example"}, logConnection{Database: "Logs", Cluster: "worker"}, nil, ""},
		{"explicit replaces invalid discovered endpoint", runLogsOptions{KustoEndpoint: "https://explicit.example"}, logConnection{Endpoint: "not a URL", Database: "Logs", Cluster: "worker"}, nil, ""},
		{"missing source", runLogsOptions{}, logConnection{Endpoint: "https://logs.example", Database: "Logs"}, nil, "--kusto-cluster"},
		{"missing database", runLogsOptions{}, logConnection{Endpoint: "https://logs.example", Cluster: "worker"}, nil, "--kusto-database"},
		{"missing endpoint", runLogsOptions{}, logConnection{Database: "Logs", Cluster: "worker"}, nil, "--kusto-endpoint"},
		{"read denied", runLogsOptions{}, logConnection{}, errors.New("Forbidden"), "Forbidden"},
		{"bad endpoint", runLogsOptions{}, logConnection{Endpoint: "http://logs.example", Database: "Logs", Cluster: "worker"}, nil, "absolute HTTPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveTerminalLogConnection(context.Background(), tc.opts, runLogsHooks{
				resolveLogConnection: func(context.Context) (logConnection, error) { return tc.discovered, tc.err },
			})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			if tc.err != nil && (!errors.Is(err, tc.err) || !strings.Contains(err.Error(), logConnectionConfigMap)) {
				t.Fatalf("missing original error or remediation: %v", err)
			}
		})
	}
	for _, endpoint := range []string{"https://", "logs.example", "http://logs.example", "https://user:password@logs.example", "https://logs.example/path", "https://logs.example?db=Logs", "https://logs.example#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := resolveTerminalLogConnection(context.Background(), runLogsOptions{
				KustoEndpoint: endpoint, KustoDatabase: "Logs", KustoCluster: "worker",
			}, runLogsHooks{resolveLogConnection: func(context.Context) (logConnection, error) {
				t.Fatal("fully explicit connection must not trigger discovery")
				return logConnection{}, nil
			}})
			if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunLogsCommandDiscoveryIsLazyAndLocalOnly(t *testing.T) {
	for _, manager := range []bool{false, true} {
		t.Run(fmt.Sprintf("manager=%v", manager), func(t *testing.T) {
			var out bytes.Buffer
			err := runLogsCommandWithHooks(context.Background(), &out, nil, "train", runLogsOptions{Namespace: "research", Tail: 10}, runLogsHooks{
				fetchSnapshot: func(context.Context) (status.Snapshot, error) {
					if manager {
						return multiKueueManagerLogSnapshot("remote-worker"), nil
					}
					return status.Snapshot{RayJob: status.RayJob{Found: true, JobStatus: "SUCCEEDED"}}, nil
				},
				rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) { return "live logs", nil },
				resolveLogConnection: func(context.Context) (logConnection, error) {
					t.Fatal("live logs and manager-side remote logs must not discover local ADX metadata")
					return logConnection{}, nil
				},
			})
			if manager {
				if err == nil || !strings.Contains(err.Error(), "--kusto-endpoint") {
					t.Fatalf("manager missing explicit destination: %v", err)
				}
			} else if err != nil || out.String() != "live logs" {
				t.Fatalf("live logs = %q, error = %v", out.String(), err)
			}
		})
	}
}

func TestRunLifecyclePreservesVerifiedSystemNamespace(t *testing.T) {
	root := t.TempDir()
	runRunRoutingGit(t, root, "init", "--quiet")
	t.Chdir(root)
	t.Setenv("TAU_CONTEXT", "")
	t.Setenv(systemNamespaceEnvironment, "wrong-default-system")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var flags runLifecycleConnectionFlags
	flags.add(cmd)
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "worker", Namespace: "research", SystemNamespace: "verified-system",
	}}
	kubeContext, namespace, restore, err := flags.resolveWithEnsurer(cmd, ensurer)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if kubeContext != "worker" || namespace != "research" || flags.systemNamespace != "verified-system" || ensurer.calls != 1 {
		t.Fatalf("route = %s/%s, system namespace = %s, calls = %d", kubeContext, namespace, flags.systemNamespace, ensurer.calls)
	}
}

func TestObservedRunConnectionEnsurerPreservesExactDiscovery(t *testing.T) {
	discovery := workspaceconnection.Discovery{}
	want := workspaceconnection.ActiveConnection{SystemNamespace: "catalog-system"}
	underlying := &fakeRunConnectionEnsurer{connection: want}
	var observed workspaceconnection.ActiveConnection
	ensurer := &observedRunConnectionEnsurer{
		underlying: underlying,
		connected:  func(connection workspaceconnection.ActiveConnection) { observed = connection },
	}
	got, err := ensurer.EnsureDiscovery(context.Background(), discovery)
	if err != nil || got != want || observed != want || len(underlying.discoveries) != 1 {
		t.Fatalf("connection=%#v observed=%#v exact calls=%d err=%v", got, observed, len(underlying.discoveries), err)
	}
	underlying.err = errors.New("verification failed")
	observed = workspaceconnection.ActiveConnection{}
	_, err = ensurer.EnsureDiscovery(context.Background(), discovery)
	if !errors.Is(err, underlying.err) || observed != (workspaceconnection.ActiveConnection{}) {
		t.Fatalf("failed connection must not be observed: %#v, error = %v", observed, err)
	}
}
