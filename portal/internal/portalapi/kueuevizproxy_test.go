package portalapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newKueueVizTestServer(rt http.RoundTripper, enabled bool) *Server {
	return &Server{
		kueueViz: KueueVizOptions{
			Enabled:         enabled,
			Namespace:       "kueue-system",
			BackendService:  "kueue-kueueviz-backend",
			FrontendService: "kueue-kueueviz-frontend",
			Transport:       rt,
		},
	}
}

// TestHandleKueueVizProxyDisabled asserts the board soft-degrades to 503 when
// the portal was started without --kueueviz, so the rest of the portal serves.
func TestHandleKueueVizProxyDisabled(t *testing.T) {
	s := newKueueVizTestServer(nil, false)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix, nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when kueueviz disabled", rec.Code)
	}
}

// TestHandleKueueVizAssetDisabled asserts the root-absolute asset handler also
// soft-degrades to 503 when the board is disabled.
func TestHandleKueueVizAssetDisabled(t *testing.T) {
	s := newKueueVizTestServer(nil, false)
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when kueueviz disabled", rec.Code)
	}
}

// TestHandleKueueVizEnvJSBaseOnly asserts the injected env.js points the SPA at
// the portal's same-origin proxy base (no /ws suffix — the frontend appends the
// subpath itself, so emitting /ws here would double it to /ws/ws/...).
func TestHandleKueueVizEnvJSBaseOnly(t *testing.T) {
	s := newKueueVizTestServer(nil, true)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"env.js", nil)
	req.Host = "portal.example.com"
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	want := `"ws://portal.example.com/api/portal/kueueviz"`
	if !strings.Contains(body, want) {
		t.Fatalf("env.js body = %q, want it to contain %s", body, want)
	}
	if strings.Contains(body, "/api/portal/kueueviz/ws") {
		t.Fatalf("env.js must be base-only (no /ws suffix), got %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want javascript", ct)
	}
}

// TestHandleKueueVizEnvJSHTTPS asserts the WS scheme follows X-Forwarded-Proto
// so the injected URL is wss behind a TLS-terminating LB.
func TestHandleKueueVizEnvJSHTTPS(t *testing.T) {
	s := newKueueVizTestServer(nil, true)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"env.js", nil)
	req.Host = "portal.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)

	if !strings.Contains(rec.Body.String(), `"wss://portal.example.com/api/portal/kueueviz"`) {
		t.Fatalf("env.js must use wss behind https LB, got %q", rec.Body.String())
	}
}

// TestHandleKueueVizProxyRoutesBackend asserts /ws/... and /auth/... route to
// the backend Service (with the Origin normalized to the fixed sentinel), while
// everything else routes to the frontend Service (Origin left untouched — the
// frontend serves only static assets and does no Origin check).
func TestHandleKueueVizProxyRoutesBackend(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantHostPfx string
		wantOrigin  string
	}{
		{"ws", kueueVizProxyPrefix + "ws/cluster-queues", "kueue-kueueviz-backend.", kueueVizBackendOrigin},
		{"auth", kueueVizProxyPrefix + "auth/status", "kueue-kueueviz-backend.", kueueVizBackendOrigin},
		{"frontend", kueueVizProxyPrefix + "index.html", "kueue-kueueviz-frontend.", "http://attacker.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dialedHost, dialedOrigin string
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				dialedHost = r.URL.Host
				dialedOrigin = r.Header.Get("Origin")
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       http.NoBody,
					Header:     make(http.Header),
				}, nil
			})
			s := newKueueVizTestServer(rt, true)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "http://attacker.example.com")
			rec := httptest.NewRecorder()
			s.handleKueueVizProxy(rec, req)

			if !strings.HasPrefix(dialedHost, tc.wantHostPfx) {
				t.Fatalf("dialed host = %q, want prefix %q", dialedHost, tc.wantHostPfx)
			}
			if dialedOrigin != tc.wantOrigin {
				t.Fatalf("forwarded Origin = %q, want %q", dialedOrigin, tc.wantOrigin)
			}
		})
	}
}

// TestHandleKueueVizAssetRequiresCookie asserts the prefix-less asset handler
// gates on the kueueviz_target cookie (a page must be loaded first).
func TestHandleKueueVizAssetRequiresCookie(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	s := newKueueVizTestServer(rt, true)

	// No cookie → rejected.
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without kueueviz_target cookie", rec.Code)
	}

	// With cookie → proxied.
	req = httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	req.AddCookie(&http.Cookie{Name: kueueVizTargetCookie, Value: "1"})
	rec = httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with kueueviz_target cookie", rec.Code)
	}
}

// TestHandleKueueVizProxySetsCookie asserts a loaded KueueViz page sets the
// kueueviz_target cookie so prefix-less asset fetches route to the frontend.
func TestHandleKueueVizProxySetsCookie(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	s := newKueueVizTestServer(rt, true)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"index.html", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)

	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == kueueVizTargetCookie && c.Value == "1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected kueueviz_target cookie to be set")
	}
}

// TestHandleKueueVizProxyCookieSecureFollowsScheme asserts the kueueviz_target
// cookie is not marked Secure over plain HTTP (browsers drop Secure cookies on
// HTTP, breaking kubectl port-forward) but is Secure when the request arrives
// over https (via X-Forwarded-Proto behind the LB).
func TestHandleKueueVizProxyCookieSecureFollowsScheme(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	s := newKueueVizTestServer(rt, true)

	cookieSecure := func(req *http.Request) bool {
		rec := httptest.NewRecorder()
		s.handleKueueVizProxy(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == kueueVizTargetCookie {
				return c.Secure
			}
		}
		t.Fatal("expected kueueviz_target cookie to be set")
		return false
	}

	httpReq := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"index.html", nil)
	if cookieSecure(httpReq) {
		t.Error("cookie must not be Secure over plain HTTP (browsers drop it, breaking port-forward)")
	}

	httpsReq := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"index.html", nil)
	httpsReq.Header.Set("X-Forwarded-Proto", "https")
	if !cookieSecure(httpsReq) {
		t.Error("cookie must be Secure when the request scheme is https")
	}
}

// TestHandleKueueVizProxyNoCookieOnWebSocket asserts the cookie is not written
// on /ws/ upgrades — Hijack() bypasses the ResponseWriter, so a header set
// before the ws branch would never reach the wire and the cookie only serves
// HTML/asset disambiguation anyway.
func TestHandleKueueVizProxyNoCookieOnWebSocket(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	s := newKueueVizTestServer(rt, true)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"ws/cluster-queues", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == kueueVizTargetCookie {
			t.Error("kueueviz_target cookie must not be set on WebSocket upgrade paths")
		}
	}
}

// TestHandleKueueVizProxyUnreachableReturns502 asserts that when the KueueViz
// Services are unreachable (the transport errors), the proxy soft-degrades to a
// 502 via the ReverseProxy ErrorHandler rather than a 500 or a hang. The
// frontend probes this status to show a "Services may not be deployed" message
// instead of embedding a bare error page in the iframe.
func TestHandleKueueVizProxyUnreachableReturns502(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	s := newKueueVizTestServer(rt, true)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix+"index.html", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the KueueViz Services are unreachable", rec.Code)
	}
}
