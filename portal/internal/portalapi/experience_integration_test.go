// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/jobdetail"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type experienceReader struct {
	scopedPortalReader
	optionalReads int
}

func (r *experienceReader) ListNodes(context.Context) ([]byte, error) {
	r.optionalReads++
	return []byte(`{"items":[]}`), nil
}

func (r *experienceReader) ListServices(context.Context, string) ([]byte, error) {
	r.optionalReads++
	return []byte(`{"items":[]}`), nil
}

func (r *experienceReader) GetJob(_ context.Context, namespace, _ string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"metadata":{"name":"active","labels":{"tau.azure.com/run-id":"run-1"}},"status":{"active":1}}`), nil
}

func (r *experienceReader) GetRayJob(_ context.Context, namespace, name string) ([]byte, error) {
	r.record(namespace)
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "rayjobs"}, name)
}

func (r *experienceReader) ListEvents(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

type experienceQuerier struct {
	scopedPortalQuerier
	rows []kustoquery.Row
}

func (q *experienceQuerier) Query(_ context.Context, query string) ([]kustoquery.Row, error) {
	q.kqls = append(q.kqls, query)
	return q.rows, nil
}

func TestWorkloadOverviewSkipsOptionalSources(t *testing.T) {
	reader := &experienceReader{}
	querier := &experienceQuerier{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    testOperatorJobs(t, stubJobsReader{}),
		Nodes:   NodesOptions{Reader: reader},
		Ray:     RayOptions{Reader: reader},
		Cluster: ClusterOptions{Querier: querier},
		Cost:    CostOptions{Querier: querier},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview?view=workloads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cards.Queue == nil || got.Running == nil || got.Cards.QueueUnavailable != "" {
		t.Fatalf("workload projection lost core data: %+v", got)
	}
	if got.Cards.Fleet != nil || got.Cards.Health != nil || got.Cards.Cost != nil || got.Cards.Ray != nil ||
		reader.optionalReads != 0 || reader.daemonSetCalls != 0 || len(querier.kqls) != 0 {
		t.Fatalf("workload projection read optional sources: cards=%+v reader=%+v queries=%v", got.Cards, reader, querier.kqls)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview?view=unknown", nil))
	if rec.Code != http.StatusBadRequest || reader.optionalReads != 0 || len(querier.kqls) != 0 {
		t.Fatalf("invalid projection was not rejected before reads: %d %s", rec.Code, rec.Body.String())
	}
}

func TestManagedJobDetailPassesAuthoritativeTrackingScope(t *testing.T) {
	reader := &experienceReader{}
	querier := &experienceQuerier{rows: []kustoquery.Row{
		{"project_id": "selected-project", "workspace_id": "alpha", "cluster": "cluster-a", "metric_name": "train/loss", "step": 1.0, "value": 0.5},
		{"project_id": "other-workspace", "workspace_id": "beta", "cluster": "cluster-a", "metric_name": "train/loss", "step": 1.0, "value": 0.2},
		{"project_id": "other-cluster", "workspace_id": "alpha", "cluster": "cluster-b", "metric_name": "train/loss", "step": 1.0, "value": 0.1},
	}}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		WorkspaceDirectory: testWorkspaceDirectory(t),
		Runs:               RunsOptions{Reader: reader},
		Cluster:            ClusterOptions{Querier: querier},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/portal/runs/wrong-namespace/active?workspace=alpha&cluster=cluster-b", nil)
	req.Header.Set(defaultViewerUserHeader, "viewer@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || len(querier.kqls) != 0 || len(reader.namespaces) != 0 {
		t.Fatalf("conflicting cluster was not rejected before reads: %d %s", rec.Code, rec.Body.String())
	}
	req.URL.RawQuery = "workspace=alpha"
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var got jobdetail.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(got.Links.StellarPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Diagnostics.Tracking.State != "ready" || got.Lifecycle != nil ||
		link.Query().Get("project") != "selected-project" || link.Query().Get("workspace") != "alpha" {
		t.Fatalf("active tracking lost authoritative identity: %+v", got)
	}
	if len(querier.kqls) != 1 || !strings.Contains(querier.kqls[0], "workspace_id == @'alpha'") ||
		!strings.Contains(querier.kqls[0], "cluster == @'cluster-a'") {
		t.Fatalf("tracking query was not scoped: %v", querier.kqls)
	}
	for _, namespace := range reader.namespaces {
		if namespace != "team-alpha" {
			t.Fatalf("untrusted namespace reached reader: %q", namespace)
		}
	}
}
