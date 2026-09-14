// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/rdmavalidation"
)

type fakeRDMAValidationReader struct {
	summary      rdmavalidation.Summary
	page         rdmavalidation.Page
	detail       rdmavalidation.Detail
	summaryErr   error
	listErr      error
	detailErr    error
	summaryScope rdmavalidation.Scope
	listScope    rdmavalidation.Scope
	detailScope  rdmavalidation.Scope
	listOptions  rdmavalidation.ListOptions
	detailID     string
	calls        int
}

func (f *fakeRDMAValidationReader) Summary(_ context.Context, scope rdmavalidation.Scope) (rdmavalidation.Summary, error) {
	f.calls++
	f.summaryScope = scope
	return f.summary, f.summaryErr
}

func (f *fakeRDMAValidationReader) List(_ context.Context, scope rdmavalidation.Scope, opts rdmavalidation.ListOptions) (rdmavalidation.Page, error) {
	f.calls++
	f.listScope = scope
	f.listOptions = opts
	return f.page, f.listErr
}

func (f *fakeRDMAValidationReader) Get(_ context.Context, scope rdmavalidation.Scope, validationID string) (rdmavalidation.Detail, error) {
	f.calls++
	f.detailScope = scope
	f.detailID = validationID
	return f.detail, f.detailErr
}

func newRDMAValidationTestServer(t *testing.T, reader rdmavalidation.Reader) *Server {
	t.Helper()
	server, err := NewServer(Options{
		Stellar:         expapi.Options{Source: "kusto", Workspace: "research"},
		Cluster:         ClusterOptions{Cluster: "cluster-a"},
		Runs:            RunsOptions{Namespace: "team-alpha"},
		RDMAValidations: RDMAValidationOptions{Reader: reader},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func TestRDMAValidationRoutes(t *testing.T) {
	pass := "pass"
	reader := &fakeRDMAValidationReader{
		summary: rdmavalidation.Summary{Latest: &rdmavalidation.Validation{
			ValidationID: "nccl-rdma-summary", State: "passed", HistoricalStatus: &pass,
			WorkspaceID: "research", Cluster: "cluster-a",
		}},
		page: rdmavalidation.Page{Validations: []rdmavalidation.Validation{{
			ValidationID: "nccl-rdma-history", State: "stale", HistoricalStatus: &pass,
			WorkspaceID: "research", Cluster: "cluster-a",
		}}, NextCursor: "cursor-next", Truncated: true},
		detail: rdmavalidation.Detail{Validation: rdmavalidation.Validation{
			ValidationID: "nccl-rdma-detail", State: "passed", HistoricalStatus: &pass,
			WorkspaceID: "research", Cluster: "cluster-a",
		}, SchemaVersion: "rdma-validation.v1", Kind: "tau.rdma_validation"},
	}
	server := newRDMAValidationTestServer(t, reader)

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/api/portal/rdma-validations/summary", want: `"validationId":"nccl-rdma-summary"`},
		{path: "/api/portal/rdma-validations?limit=7&cursor=cursor-one", want: `"nextCursor":"cursor-next"`},
		{path: "/api/portal/rdma-validations/nccl-rdma-detail", want: `"schemaVersion":"rdma-validation.v1"`},
	} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
	}
	if reader.summaryScope.WorkspaceID != "research" || reader.summaryScope.Cluster != "cluster-a" {
		t.Fatalf("summary scope = %+v", reader.summaryScope)
	}
	if reader.detailScope.FetchArtifact == nil {
		t.Fatal("detail scope did not receive the trusted Stellar artifact fetcher")
	}
	if reader.listOptions.Limit != 7 || reader.listOptions.Cursor != "cursor-one" {
		t.Fatalf("list options = %+v", reader.listOptions)
	}
	if reader.detailID != "nccl-rdma-detail" {
		t.Fatalf("detail ID = %q", reader.detailID)
	}
}

func TestRDMAValidationEmptyAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader rdmavalidation.Reader
		path   string
		want   int
		code   string
	}{
		{name: "not configured", path: "/api/portal/rdma-validations/summary", want: http.StatusServiceUnavailable, code: "backend_unavailable"},
		{name: "summary unavailable", reader: &fakeRDMAValidationReader{summaryErr: rdmavalidation.ErrUnavailable}, path: "/api/portal/rdma-validations/summary", want: http.StatusServiceUnavailable, code: "backend_unavailable"},
		{name: "query failure", reader: &fakeRDMAValidationReader{listErr: errors.New("secret backend detail")}, path: "/api/portal/rdma-validations", want: http.StatusBadGateway, code: "query_failed"},
		{name: "invalid cursor", reader: &fakeRDMAValidationReader{listErr: rdmavalidation.ErrInvalidCursor}, path: "/api/portal/rdma-validations?cursor=bad", want: http.StatusBadRequest, code: "invalid_argument"},
		{name: "not found", reader: &fakeRDMAValidationReader{detailErr: rdmavalidation.ErrNotFound}, path: "/api/portal/rdma-validations/nccl-rdma-missing", want: http.StatusNotFound, code: "not_found"},
		{name: "scope mismatch", reader: &fakeRDMAValidationReader{detailErr: rdmavalidation.ErrScopeMismatch}, path: "/api/portal/rdma-validations/nccl-rdma-missing", want: http.StatusNotFound, code: "not_found"},
		{name: "unsupported schema", reader: &fakeRDMAValidationReader{detailErr: rdmavalidation.ErrUnsupportedSchema}, path: "/api/portal/rdma-validations/nccl-rdma-v2", want: http.StatusBadGateway, code: "unsupported_schema"},
		{name: "malformed artifact", reader: &fakeRDMAValidationReader{detailErr: rdmavalidation.ErrMalformedArtifact}, path: "/api/portal/rdma-validations/nccl-rdma-malformed", want: http.StatusBadGateway, code: "malformed_artifact"},
		{name: "integrity failure", reader: &fakeRDMAValidationReader{detailErr: rdmavalidation.ErrArtifactIntegrity}, path: "/api/portal/rdma-validations/nccl-rdma-corrupt", want: http.StatusBadGateway, code: "artifact_integrity_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newRDMAValidationTestServer(t, tc.reader).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "secret backend detail") {
				t.Fatalf("backend error leaked: %s", rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	newRDMAValidationTestServer(t, &fakeRDMAValidationReader{}).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"validations":[]`) ||
		!strings.Contains(rec.Body.String(), `"state":"no_data"`) {
		t.Fatalf("empty list status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRDMAValidationRejectsInvalidRequests(t *testing.T) {
	reader := &fakeRDMAValidationReader{}
	server := newRDMAValidationTestServer(t, reader)
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/api/portal/rdma-validations", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/portal/rdma-validations?limit=0", want: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/portal/rdma-validations?limit=101", want: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/portal/rdma-validations?cursor=" + strings.Repeat("x", maxRDMAValidationCursor+1), want: http.StatusBadRequest},
		{method: http.MethodGet, path: "/api/portal/rdma-validations/", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/portal/rdma-validations/nccl-rdma/extra", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/portal/rdma-validations/INVALID", want: http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRDMAValidationDetailFailsClosedOnScopeMismatch(t *testing.T) {
	reader := &fakeRDMAValidationReader{detail: rdmavalidation.Detail{Validation: rdmavalidation.Validation{
		ValidationID: "nccl-rdma-detail", State: "passed", WorkspaceID: "other", Cluster: "cluster-a",
	}}}
	rec := httptest.NewRecorder()
	newRDMAValidationTestServer(t, reader).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations/nccl-rdma-detail", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), `"state":"passed"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRDMAValidationAuthorizesWorkspaceBeforeRead(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", Source: "kusto",
			Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeRDMAValidationReader{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		RDMAValidations:    RDMAValidationOptions{Reader: reader},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations?workspace=alpha", nil))
	if rec.Code != http.StatusUnauthorized || reader.calls != 0 {
		t.Fatalf("unauthorized status=%d calls=%d body=%s", rec.Code, reader.calls, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations?workspace=alpha&namespace=team-secret", nil)
	req.Header.Set(defaultViewerUserHeader, "viewer@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "researchers")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || reader.calls != 0 {
		t.Fatalf("conflicting scope status=%d calls=%d body=%s", rec.Code, reader.calls, rec.Body.String())
	}
}

func TestRDMAValidationErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	newRDMAValidationTestServer(t, nil).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations/summary", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "backend_unavailable" || body["reason"] == "" || body["state"] != "unavailable" {
		t.Fatalf("body = %#v", body)
	}
}
