// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobdetail

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kustoquery"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func diagnosticReader() fakeReader {
	return fakeReader{
		job:       []byte(`{"metadata":{"name":"j","labels":{"tau.azure.com/run-id":"run-1"}},"status":{"active":1}}`),
		pods:      []byte(`{"items":[]}`),
		workloads: []byte(`{"items":[]}`),
		events:    []byte(`{"items":[]}`),
	}
}

func TestDetailDiagnosticSources(t *testing.T) {
	sensitive := errors.New("Bearer private-token: upstream-command --secret")
	for _, tt := range []struct {
		name string
		raw  []byte
		err  error
		want string
	}{
		{"empty", []byte(`{"items":[]}`), nil, "empty"},
		{"error", nil, sensitive, "unavailable"},
		{"malformed", []byte(`garbage`), nil, "unavailable"},
		{"missing items", []byte(`{}`), nil, "unavailable"},
		{"null response", []byte(`null`), nil, "unavailable"},
		{"null items", []byte(`{"items":null}`), nil, "unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := diagnosticReader()
			r.pods, r.events, r.workloads = tt.raw, tt.raw, tt.raw
			r.podErr, r.evtErr, r.wlErr = tt.err, tt.err, tt.err
			snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ns", Name: "j"})
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []SourceDiagnostic{snap.Diagnostics.Workloads, snap.Diagnostics.Pods, snap.Diagnostics.Events} {
				if source.State != tt.want {
					t.Fatalf("source=%+v want %s", source, tt.want)
				}
			}
			raw, err := json.Marshal(snap.Diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private-token") || strings.Contains(string(raw), "upstream-command") {
				t.Fatalf("source exposed upstream error: %s", raw)
			}
		})
	}
}

func TestDetailDiagnosticPartialAndNotConfigured(t *testing.T) {
	r := diagnosticReader()
	r.workloads = []byte(`{"items":[{"metadata":{"name":"wl","ownerReferences":[{"name":"j"}]}}]}`)
	r.events = []byte(`{"items":[{"reason":"Started","involvedObject":{"name":"j"}}]}`)
	r.podErr = errors.New("forbidden")
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ns", Name: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Diagnostics.Workloads.State != "ready" || snap.Diagnostics.Events.State != "ready" || snap.Diagnostics.Pods.State != "unavailable" || len(snap.Events) != 1 || len(snap.Workloads) != 1 {
		t.Fatalf("partial detail = %+v", snap)
	}
	r.wlErr = apierrors.NewNotFound(schema.GroupResource{Group: "kueue.x-k8s.io", Resource: "workloads"}, "")
	snap, err = Detail(context.Background(), r, nil, Options{Namespace: "ns", Name: "j"})
	if err != nil || snap.Diagnostics.Workloads.State != "not_configured" || snap.Diagnostics.Tracking.State != "not_configured" {
		t.Fatalf("unconfigured detail = %+v %v", snap, err)
	}
}

func metricRow(project, workspace string) kustoquery.Row {
	return kustoquery.Row{
		"metric_name": "train/loss", "value": 0.5, "step": 1.0,
		"project_id": project, "workspace_id": workspace, "wall_time": "2026-09-01T00:00:00Z",
	}
}

func TestDetailTrackingIdentity(t *testing.T) {
	for _, tt := range []struct {
		name          string
		rows          []kustoquery.Row
		err           error
		workspace     string
		wantState     string
		wantProject   string
		wantWorkspace string
	}{
		{"active indexed", []kustoquery.Row{metricRow("Research Project", "")}, nil, "", "ready", "Research Project", ""},
		{"active selected workspace", []kustoquery.Row{metricRow("p", "team-b")}, nil, "team-b", "ready", "p", "team-b"},
		{"legacy unique workspace", []kustoquery.Row{metricRow("p", "team-b")}, nil, "", "ready", "p", "team-b"},
		{"pending indexing", nil, nil, "", "empty", "", ""},
		{"query unavailable", nil, errors.New("secret-token"), "", "unavailable", "", ""},
		{"query unconfigured", nil, kustoquery.ErrNoQueryCommand, "", "not_configured", "", ""},
		{"missing project", []kustoquery.Row{metricRow("", "")}, nil, "", "unavailable", "", ""},
		{"ambiguous projects", []kustoquery.Row{metricRow("p", ""), metricRow("q", "")}, nil, "", "unavailable", "", ""},
		{"ambiguous workspaces", []kustoquery.Row{metricRow("p", "a"), metricRow("p", "b")}, nil, "", "unavailable", "", ""},
		{"selected filters other workspace", []kustoquery.Row{metricRow("p", "a"), metricRow("q", "b")}, nil, "b", "ready", "q", "b"},
		{"selected missing workspace", []kustoquery.Row{metricRow("p", "")}, nil, "b", "empty", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := Detail(context.Background(), diagnosticReader(), fakeQuerier{rows: tt.rows, err: tt.err}, Options{Namespace: "ns", Name: "j", WorkspaceID: tt.workspace})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Diagnostics.Tracking.State != tt.wantState || strings.Contains(snap.Diagnostics.Tracking.Message, "secret-token") {
				t.Fatalf("diagnostic=%+v want %s", snap.Diagnostics.Tracking, tt.wantState)
			}
			if snap.Lifecycle != nil || snap.Status != "Running" {
				t.Fatalf("active metrics altered lifecycle: %+v", snap)
			}
			if tt.wantProject == "" {
				if snap.Links.StellarPath != "" {
					t.Fatalf("unexpected link %s", snap.Links.StellarPath)
				}
				return
			}
			u, err := url.Parse(snap.Links.StellarPath)
			if err != nil || u.Path != "/stellar" || u.Query().Get("target") != "run-1" || u.Query().Get("project") != tt.wantProject || u.Query().Get("workspace") != tt.wantWorkspace {
				t.Fatalf("link = %q, err=%v", snap.Links.StellarPath, err)
			}
		})
	}
}

func TestTrackingIgnoresUnrenderableMetrics(t *testing.T) {
	for _, row := range []kustoquery.Row{
		{"metric_name": "train/loss", "project_id": "p", "value": 1.0},
		{"metric_name": "train/loss", "project_id": "p", "step": 1.0},
		{"metric_name": "tau/run_status", "project_id": "p", "value": 0.0},
	} {
		link, lifecycle, diagnostic := tracking(context.Background(), fakeQuerier{rows: []kustoquery.Row{row}}, "run", Options{})
		if link != "" || lifecycle != nil || diagnostic.State != "empty" {
			t.Fatalf("unrenderable row=%+v yielded %s %+v %+v", row, link, lifecycle, diagnostic)
		}
	}
}

func TestTrackingLifecycleIndependentOfMetrics(t *testing.T) {
	marker := kustoquery.Row{"metric_name": "tau/run_status", "project_id": "p", "value": -1.0, "wall_time": "2026-09-01T00:00:00Z"}
	metric := metricRow("p", "")
	metric["wall_time"] = "2026-09-02T00:00:00Z"
	link, row, diagnostic := tracking(context.Background(), fakeQuerier{rows: []kustoquery.Row{metric, marker}}, "run", Options{})
	if link == "" || row == nil || row.State != "failed" || diagnostic.State != "ready" {
		t.Fatalf("tracking = %s %+v %+v", link, row, diagnostic)
	}
}

func TestTrackingQueryScopeAndEligibility(t *testing.T) {
	query := trackingQuery("r' | project", "w'", "c'")
	for _, want := range []string{
		"Labels['project']", "Labels.workspace_id", "run_id == @'r'' | project'",
		"workspace_id == @'w'''", "isnotnull(step) or metric_name == @'tau/run_status'",
		"cluster == @'c'''", "isnotnull(value) and isfinite(value)", "arg_max(wall_time, *) by project_id, workspace_id, cluster, row_kind", "| take 3",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(trackingQuery("run", "", ""), "where workspace_id ==") {
		t.Fatal("legacy lookup unexpectedly scoped")
	}
}

func TestTrackingClusterScope(t *testing.T) {
	a, b := metricRow("p", "w"), metricRow("p", "w")
	a["cluster"], b["cluster"] = "a", "b"
	q := fakeQuerier{rows: []kustoquery.Row{a, b}}
	for _, tt := range []struct{ cluster, state string }{
		{"", "unavailable"},
		{"a", "ready"},
		{"unobserved", "empty"},
	} {
		link, _, diagnostic := tracking(context.Background(), q, "run", Options{WorkspaceID: "w", Cluster: tt.cluster})
		if diagnostic.State != tt.state || (link != "") != (tt.state == "ready") {
			t.Fatalf("cluster %q: link=%s diagnostic=%+v", tt.cluster, link, diagnostic)
		}
	}
}
