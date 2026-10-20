// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expapi"
)

type fakeRayReader struct {
	json string
}

func (f fakeRayReader) ListServices(_ context.Context, _ string) ([]byte, error) {
	return []byte(f.json), nil
}

func (f fakeRayReader) ListPods(_ context.Context, _ string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

const rayHeadSvcJSON = `{"items":[
  {"metadata":{"name":"alpha-head-svc","namespace":"ray",
    "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"head"}},
   "spec":{"type":"ClusterIP"}}
]}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newRayTestServer(rt http.RoundTripper) *Server {
	return &Server{
		ray: RayOptions{
			Reader:    fakeRayReader{json: rayHeadSvcJSON},
			Transport: rt,
		},
	}
}

func newRoutedRayTestServer(t *testing.T, rt http.RoundTripper) *Server {
	t.Helper()
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Ray: RayOptions{
			Reader:    fakeRayReader{json: rayHeadSvcJSON},
			Namespace: "ray",
			Transport: rt,
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func TestRayAPIRoutesRemainUnscopedAndScoped(t *testing.T) {
	routes := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/authenticate"},
		{method: http.MethodGet, path: "/api/authentication_mode"},
		{method: http.MethodGet, path: "/api/cluster_status"},
		{method: http.MethodGet, path: "/api/grafana_health"},
		{method: http.MethodGet, path: "/api/jobs/"},
		{method: http.MethodGet, path: "/api/jobs/job-id"},
		{method: http.MethodGet, path: "/api/prometheus_health"},
		{method: http.MethodGet, path: "/api/serve/applications/"},
		{method: http.MethodGet, path: "/api/v0/cluster_metadata"},
		{method: http.MethodGet, path: "/api/v0/logs"},
		{method: http.MethodGet, path: "/api/v0/logs/file"},
		{method: http.MethodGet, path: "/api/v0/placement_groups"},
		{method: http.MethodGet, path: "/api/v0/platform_events"},
		{method: http.MethodGet, path: "/api/v0/tasks"},
		{method: http.MethodGet, path: "/api/v0/tasks/summarize"},
		{method: http.MethodGet, path: "/api/v0/tasks/timeline"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			var upstreamPath string
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				upstreamPath = r.URL.Path
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			})
			server := newRoutedRayTestServer(t, rt)

			unscoped := httptest.NewRecorder()
			server.Handler().ServeHTTP(unscoped, httptest.NewRequest(route.method, route.path, nil))
			if unscoped.Code != http.StatusBadRequest {
				t.Fatalf("unscoped status = %d, want 400; body = %s", unscoped.Code, unscoped.Body.String())
			}
			if !strings.Contains(unscoped.Body.String(), "ambiguous Ray request") {
				t.Fatalf("unscoped body = %q, want ambiguous Ray request", unscoped.Body.String())
			}

			scopedPath := rayProxyPrefix + "ray/alpha" + route.path
			scoped := httptest.NewRecorder()
			server.Handler().ServeHTTP(scoped, httptest.NewRequest(route.method, scopedPath, nil))
			if scoped.Code != http.StatusOK {
				t.Fatalf("scoped status = %d, want 200; body = %s", scoped.Code, scoped.Body.String())
			}
			if upstreamPath != route.path {
				t.Fatalf("upstream path = %q, want %q", upstreamPath, route.path)
			}
		})
	}
}

func TestRayProfilingRouteRemainsAvailableUnscopedAndScoped(t *testing.T) {
	server := newRoutedRayTestServer(t, nil)
	for _, path := range []string{
		"/api/profiling_enabled",
		rayProxyPrefix + "ray/alpha/api/profiling_enabled",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
			}
		})
	}
}

func TestValidateRayTarget(t *testing.T) {
	s := newRayTestServer(nil)
	if svc, ok := s.validateRayTarget(context.Background(), "ray", "alpha", "ray"); !ok {
		t.Fatal("ray/alpha is a discovered head Service, want valid")
	} else if svc != "alpha-head-svc" {
		t.Fatalf("service = %q, want alpha-head-svc", svc)
	}
	if _, ok := s.validateRayTarget(context.Background(), "ray", "ghost", "ray"); ok {
		t.Fatal("ray/ghost is not discovered, want invalid (SSRF guard)")
	}
	if _, ok := s.validateRayTarget(context.Background(), "", "alpha", "ray"); ok {
		t.Fatal("empty namespace must be rejected")
	}
}

func TestValidateRayTargetEnforcesNamespace(t *testing.T) {
	s := &Server{
		ray: RayOptions{
			Reader:    fakeRayReader{json: rayHeadSvcJSON},
			Namespace: "ray",
		},
	}
	if _, ok := s.validateRayTarget(context.Background(), "other-ns", "alpha", "ray"); ok {
		t.Fatal("namespace outside configured scope must be rejected")
	}
	if _, ok := s.validateRayTarget(context.Background(), "ray", "alpha", "ray"); !ok {
		t.Fatal("namespace matching configured scope must be allowed")
	}
}

func TestHandleRayProxyDoesNotSetTargetCookie(t *testing.T) {
	var dialedPath string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dialedPath = r.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
		}, nil
	})
	s := newRayTestServer(rt)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/ray/proxy/ray/alpha/nodes", nil)
	rec := httptest.NewRecorder()
	s.handleRayProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("proxy set shared cookies: %+v", cookies)
	}
	if dialedPath != "/nodes" {
		t.Fatalf("upstream path = %q, want /nodes", dialedPath)
	}
}

func TestHandleRayProxyRejectsUnknownCluster(t *testing.T) {
	s := newRayTestServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/ray/proxy/ray/ghost/", nil)
	rec := httptest.NewRecorder()
	s.handleRayProxy(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown cluster", rec.Code)
	}
}

func TestHandleRayProxyRejectsBadPath(t *testing.T) {
	s := newRayTestServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/ray/proxy/ray", nil)
	rec := httptest.NewRecorder()
	s.handleRayProxy(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for incomplete path", rec.Code)
	}
}

func TestServiceHost(t *testing.T) {
	if got := serviceHost("ray", "alpha-head-svc"); got != "alpha-head-svc.ray.svc:8265" {
		t.Fatalf("serviceHost = %q", got)
	}
}
