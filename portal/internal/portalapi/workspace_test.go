// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expapi"
)

func testWorkspaceDirectory(t *testing.T) WorkspaceDirectory {
	t.Helper()
	dir, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Endpoints: []WorkspacePortalEndpoint{
			{Cluster: "cluster-b", Endpoint: "https://cluster-b.portal.example"},
			{Cluster: "cluster-c", Availability: workspaceAvailabilityUnreachable},
		},
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Name: "Alpha", Cluster: "cluster-a", Team: "Team Alpha", Namespace: "team-alpha",
				LocalQueue: "jobqueue", ResultScope: "az://results/alpha", Source: "kubernetes+kusto",
				ExperimentsURL: "/stellar", Default: true,
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"group-alpha"}},
			},
			{
				ID: "beta", Name: "Beta", Cluster: "cluster-a", Team: "beta", Namespace: "team-beta",
				LocalQueue: "beta-queue", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"beta@example.com"}},
			},
			{
				ID: "remote", Name: "Remote", Cluster: "cluster-b", Team: "remote", Namespace: "team-remote",
				Source:        "kubernetes+kusto",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationClusterWide, Groups: []string{"group-alpha"}},
			},
			{
				ID: "offline", Name: "Offline", Cluster: "cluster-c", Team: "offline", Namespace: "team-offline",
				Source:        "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationClusterWide, Groups: []string{"group-alpha"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceDirectory: %v", err)
	}
	return dir
}

func TestWorkspaceDirectoryFiltersAuthorizedScopes(t *testing.T) {
	dir := testWorkspaceDirectory(t)
	scopes := dir.List(context.Background(), Viewer{ID: "user@example.com", Groups: []string{"GROUP-ALPHA"}})
	if len(scopes) != 3 {
		t.Fatalf("authorized scopes = %+v, want alpha, offline, remote", scopes)
	}
	for _, scope := range scopes {
		if scope.WorkspaceID == "beta" {
			t.Fatal("unauthorized beta workspace leaked through directory listing")
		}
	}

	if _, err := dir.Resolve(context.Background(), Viewer{ID: "user@example.com", Groups: []string{"group-alpha"}}, "beta"); !errors.Is(err, errWorkspaceNotFound) {
		t.Fatalf("unauthorized direct resolve error = %v, want workspace not found", err)
	}
}

func TestWorkspaceDirectoryResolvesLocalRemoteAndUnavailable(t *testing.T) {
	dir := testWorkspaceDirectory(t)
	viewer := Viewer{ID: "user@example.com", Groups: []string{"group-alpha"}}

	local, err := dir.Resolve(context.Background(), viewer, "")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if local.WorkspaceID != "alpha" || local.Availability != workspaceAvailabilityAvailable || local.Team != "team-alpha" || local.Namespace != "team-alpha" {
		t.Fatalf("local scope = %+v", local)
	}
	if local.AuthorizationMode != workspaceAuthorizationRBAC {
		t.Fatalf("local authorization mode = %q, want %q", local.AuthorizationMode, workspaceAuthorizationRBAC)
	}
	if local.ExperimentsURL != "/stellar" {
		t.Fatalf("local experiments URL = %q, want scoped same-origin Stellar", local.ExperimentsURL)
	}

	remote, err := dir.Resolve(context.Background(), viewer, "remote")
	if err != nil {
		t.Fatalf("resolve remote: %v", err)
	}
	if remote.Availability != workspaceAvailabilityRedirect || remote.PortalEndpoint != "https://cluster-b.portal.example" {
		t.Fatalf("remote scope = %+v", remote)
	}
	if remote.AuthorizationMode != workspaceAuthorizationClusterWide {
		t.Fatalf("remote authorization mode = %q, want %q", remote.AuthorizationMode, workspaceAuthorizationClusterWide)
	}

	offline, err := dir.Resolve(context.Background(), viewer, "offline")
	if err != nil {
		t.Fatalf("resolve offline: %v", err)
	}
	if offline.Availability != workspaceAvailabilityUnreachable {
		t.Fatalf("offline availability = %q, want unreachable", offline.Availability)
	}
}

func TestWorkspaceAwareStellarDelegatesLocalRouteWithScope(t *testing.T) {
	server := &Server{
		workspaceDirectory: testWorkspaceDirectory(t),
		identity:           normalizeIdentityOptions(IdentityOptions{}),
	}
	var gotPath, gotWorkspace, gotProject, gotSource string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotWorkspace = r.URL.Query().Get("workspace")
		gotProject = r.URL.Query().Get("project")
		gotSource = r.URL.Query().Get("source")
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?workspace=alpha&project=vision&source=local", nil)
	req.Header.Set(defaultViewerUserHeader, "user@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
	rec := httptest.NewRecorder()

	server.workspaceAwareStellar(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/stellar/experiments" || gotWorkspace != "alpha" || gotProject != "vision" || gotSource != "kusto" {
		t.Fatalf("delegated route = path %q workspace %q project %q source %q", gotPath, gotWorkspace, gotProject, gotSource)
	}
}

func TestSingleWorkspaceCanonicalStellarResolvesAutoSource(t *testing.T) {
	native := func(context.Context, string) (string, error) { return "[]", nil }
	tests := []struct {
		name string
		opts expapi.Options
		want string
	}{
		{name: "kusto available", opts: expapi.Options{Source: "auto", StorePath: t.TempDir(), KustoNativeQuery: native}, want: "kusto"},
		{name: "local only", opts: expapi.Options{Source: "auto", StorePath: t.TempDir()}, want: "local"},
		{name: "explicit source", opts: expapi.Options{Source: "kusto", KustoNativeQuery: native}, want: "kusto"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(Options{Stellar: test.opts})
			if err != nil {
				t.Fatal(err)
			}
			if server.singleWorkspaceScope.Source != test.want {
				t.Fatalf("single-workspace source=%q, want %q", server.singleWorkspaceScope.Source, test.want)
			}
			var gotSource string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSource = r.URL.Query().Get("source")
				w.WriteHeader(http.StatusNoContent)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search", nil)
			server.workspaceAwareStellar(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if gotSource != test.want || gotSource == "auto" {
				t.Fatalf("canonical source=%q, want %q", gotSource, test.want)
			}
		})
	}
}

func TestWorkspaceAwareStellarBlocksUnscopedManagedRoutes(t *testing.T) {
	server := &Server{
		workspaceDirectory: testWorkspaceDirectory(t),
		identity:           normalizeIdentityOptions(IdentityOptions{}),
	}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/stellar/experiments?workspace=alpha"},
		{method: http.MethodGet, path: "/api/stellar/artifact?workspace=alpha&artifact=other-workspace"},
		{method: http.MethodGet, path: "/stellar/unknown-route?workspace=alpha"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set(defaultViewerUserHeader, "user@example.com")
		req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
		rec := httptest.NewRecorder()
		server.workspaceAwareStellar(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if called {
		t.Fatal("blocked managed Stellar route reached the unscoped handler")
	}
}

func TestWorkspaceExperimentRedirectPreservesAPIRouteAndQuery(t *testing.T) {
	target, err := url.Parse("https://stellar.example/stellar?source=kusto")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?project=vision", nil)
	redirect, err := url.Parse(workspaceExperimentRedirectURL(target, req, "sample", "kusto"))
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Path != "/api/stellar/experiments" ||
		redirect.Query().Get("workspace") != "sample" ||
		redirect.Query().Get("project") != "vision" ||
		redirect.Query().Get("source") != "kusto" {
		t.Fatalf("redirect = %s", redirect)
	}
}

func TestWorkspaceDirectoryRequiresAuthenticatedViewer(t *testing.T) {
	dir := testWorkspaceDirectory(t)
	if got := dir.List(context.Background(), Viewer{}); len(got) != 0 {
		t.Fatalf("anonymous directory listing = %+v, want empty", got)
	}
	if _, err := dir.Resolve(context.Background(), Viewer{}, "alpha"); !errors.Is(err, errViewerUnauthenticated) {
		t.Fatalf("anonymous resolve error = %v, want unauthenticated", err)
	}
}

func TestWorkspaceDirectoryValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  WorkspaceDirectoryConfig
	}{
		{
			name: "missing local cluster",
			cfg:  WorkspaceDirectoryConfig{},
		},
		{
			name: "duplicate workspace",
			cfg: WorkspaceDirectoryConfig{LocalCluster: "c", Workspaces: []WorkspaceRecord{
				{ID: "w", Cluster: "c", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
				{ID: "w", Cluster: "c", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
			}},
		},
		{
			name: "remote endpoint must use https",
			cfg: WorkspaceDirectoryConfig{
				LocalCluster: "c",
				Endpoints:    []WorkspacePortalEndpoint{{Cluster: "remote", Endpoint: "http://portal.example"}},
				Workspaces: []WorkspaceRecord{
					{ID: "w", Cluster: "remote", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
				},
			},
		},
		{
			name: "remote endpoint rejects credentials",
			cfg: WorkspaceDirectoryConfig{
				LocalCluster: "c",
				Endpoints:    []WorkspacePortalEndpoint{{Cluster: "remote", Endpoint: "https://user:pass@portal.example"}},
				Workspaces: []WorkspaceRecord{
					{ID: "w", Cluster: "remote", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
				},
			},
		},
		{
			name: "remote endpoint rejects query",
			cfg: WorkspaceDirectoryConfig{
				LocalCluster: "c",
				Endpoints:    []WorkspacePortalEndpoint{{Cluster: "remote", Endpoint: "https://portal.example/base?next=https://evil.example"}},
				Workspaces: []WorkspaceRecord{
					{ID: "w", Cluster: "remote", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
				},
			},
		},
		{
			name: "remote endpoint rejects fragment",
			cfg: WorkspaceDirectoryConfig{
				LocalCluster: "c",
				Endpoints:    []WorkspacePortalEndpoint{{Cluster: "remote", Endpoint: "https://portal.example/base#fragment"}},
				Workspaces: []WorkspaceRecord{
					{ID: "w", Cluster: "remote", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"u"}}},
				},
			},
		},
		{
			name: "explicit authorization required",
			cfg: WorkspaceDirectoryConfig{LocalCluster: "c", Workspaces: []WorkspaceRecord{
				{ID: "w", Cluster: "c", Namespace: "n", Source: "kubernetes", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationClusterWide}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewWorkspaceDirectory(tt.cfg); err == nil {
				t.Fatal("NewWorkspaceDirectory succeeded, want validation error")
			}
		})
	}
}

func TestIsSafeLocalAbsolutePath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "root", raw: "/", want: true},
		{name: "local route", raw: "/stellar", want: true},
		{name: "base path", raw: "/base/stellar", want: true},
		{name: "query and fragment", raw: "/stellar?project=vision#runs", want: true},
		{name: "URL in query", raw: "/stellar?next=https://example.com", want: true},
		{name: "encoded slashes", raw: "/%2F%2Fexample.com/stellar", want: true},
		{name: "encoded backslash", raw: "/%5Cexample.com/stellar", want: true},
		{name: "Unicode path", raw: "/実験", want: true},
		{name: "empty"},
		{name: "relative path", raw: "stellar"},
		{name: "absolute URL", raw: "https://stellar.example/stellar"},
		{name: "double slash", raw: "//attacker.example/stellar"},
		{name: "triple slash", raw: "///attacker.example/stellar"},
		{name: "slash backslash", raw: `/\attacker.example/stellar`},
		{name: "backslash slash", raw: `\/attacker.example/stellar`},
		{name: "double backslash", raw: `\\attacker.example/stellar`},
		{name: "backslash in path", raw: `/stellar\other`},
		{name: "backslash in query", raw: `/stellar?project=\other`},
		{name: "backslash in fragment", raw: `/stellar#\other`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSafeLocalAbsolutePath(tt.raw); got != tt.want {
				t.Fatalf("isSafeLocalAbsolutePath(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidateExperimentsURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		local   bool
		wantErr bool
	}{
		{name: "local root", raw: "/", local: true},
		{name: "local route", raw: "/stellar?project=vision#runs", local: true},
		{name: "local encoded path", raw: "/%2F%5Cstellar", local: true},
		{name: "local Unicode path", raw: "/実験", local: true},
		{name: "local workspace HTTPS navigation", raw: "https://stellar.example/base/stellar?project=vision#runs", local: true},
		{name: "remote workspace HTTPS navigation", raw: "https://stellar.example/base/stellar?project=vision#runs"},
		{name: "remote workspace local path", raw: "/stellar", wantErr: true},
		{name: "HTTP navigation", raw: "http://stellar.example/stellar", local: true, wantErr: true},
		{name: "credentials", raw: "https://user:pass@stellar.example/stellar", local: true, wantErr: true},
		{name: "malformed escape", raw: "/stellar%zz", local: true, wantErr: true},
		{name: "control character", raw: "/\t/attacker.example", local: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateExperimentsURL(tt.raw, tt.local); (err != nil) != tt.wantErr {
				t.Fatalf("validateExperimentsURL(%q, %v) = %v, wantErr %v", tt.raw, tt.local, err, tt.wantErr)
			}
		})
	}
}

func TestWorkspaceDirectoryRejectsUnsafeLocalExperimentPaths(t *testing.T) {
	for _, experimentsURL := range []string{"//attacker.example/stellar", `/\attacker.example/stellar`} {
		t.Run(experimentsURL, func(t *testing.T) {
			_, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
				LocalCluster: "cluster-a",
				Workspaces: []WorkspaceRecord{{
					ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", Source: "kusto",
					ExperimentsURL: experimentsURL,
					Authorization:  WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"viewer@example.com"}},
				}},
			})
			if err == nil {
				t.Fatalf("NewWorkspaceDirectory accepted unsafe experimentsUrl %q", experimentsURL)
			}
		})
	}
}

func TestLoadWorkspaceDirectoryRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.json")
	if err := os.WriteFile(path, []byte(`{"localCluster":"c","unexpected":true,"workspaces":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspaceDirectory(path); err == nil {
		t.Fatal("LoadWorkspaceDirectory accepted unknown field")
	}
}

func TestWorkspaceRedirectURLPreservesEndpointBasePath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		"/portal/runs?workspace=remote&namespace=ignored&endpoint=https%3A%2F%2Fevil.example&portalEndpoint=https%3A%2F%2Fevil.example&target=run-1",
		nil)
	got := workspaceRedirectURL(WorkspaceScope{
		WorkspaceID:    "remote",
		PortalEndpoint: "https://portal.example/base",
	}, req)
	want := "https://portal.example/base/portal/runs?target=run-1&workspace=remote"
	if got != want {
		t.Fatalf("workspaceRedirectURL = %q, want %q", got, want)
	}
}
