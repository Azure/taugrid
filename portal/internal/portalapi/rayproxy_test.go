package portalapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeRayReader returns canned Services JSON so validateRayTarget can run
// head-svc discovery without a Kubernetes API.
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

// roundTripFunc is a stub RoundTripper so proxyToHead never dials a real head
// Service; it returns a fixed 200 response we can assert reached the upstream.
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
	// Server with a configured namespace scope.
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

func TestHandleRayProxySetsCookie(t *testing.T) {
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
	var target string
	for _, c := range rec.Result().Cookies() {
		if c.Name == rayTargetCookie {
			target = c.Value
		}
	}
	if target != "ray/alpha" {
		t.Fatalf("ray_target cookie = %q, want ray/alpha", target)
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

func TestHandleRayAssetRequiresCookie(t *testing.T) {
	s := newRayTestServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/static/js/main.js", nil)
	rec := httptest.NewRecorder()
	s.handleRayAsset(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without ray_target cookie", rec.Code)
	}
}

func TestHandleRayAssetRejectsUnknownCookie(t *testing.T) {
	s := newRayTestServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/static/js/main.js", nil)
	req.AddCookie(&http.Cookie{Name: rayTargetCookie, Value: "ray/ghost"})
	rec := httptest.NewRecorder()
	s.handleRayAsset(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown cluster in cookie", rec.Code)
	}
}

func TestHandleRayAssetProxiesWithCookie(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/static/js/main.js", nil)
	req.AddCookie(&http.Cookie{Name: rayTargetCookie, Value: "ray/alpha"})
	rec := httptest.NewRecorder()
	s.handleRayAsset(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if dialedPath != "/static/js/main.js" {
		t.Fatalf("upstream path = %q, want original /static/js/main.js", dialedPath)
	}
}

// serviceHost sanity: keeps the in-cluster DNS form the proxy dials.
func TestServiceHost(t *testing.T) {
	if got := serviceHost("ray", "alpha-head-svc"); got != "alpha-head-svc.ray.svc:8265" {
		t.Fatalf("serviceHost = %q", got)
	}
}
