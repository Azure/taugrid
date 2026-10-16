// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

func TestHandleKueueVizProxyDisabled(t *testing.T) {
	s := newKueueVizTestServer(nil, false)
	req := httptest.NewRequest(http.MethodGet, kueueVizProxyPrefix, nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizProxy(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when kueueviz disabled", rec.Code)
	}
}

func TestHandleKueueVizAssetDisabled(t *testing.T) {
	s := newKueueVizTestServer(nil, false)
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when kueueviz disabled", rec.Code)
	}
}

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

func TestHandleKueueVizAssetRequiresCookie(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	s := newKueueVizTestServer(rt, true)

	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	rec := httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without kueueviz_target cookie", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	req.AddCookie(&http.Cookie{Name: kueueVizTargetCookie, Value: "1"})
	rec = httptest.NewRecorder()
	s.handleKueueVizAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with kueueviz_target cookie", rec.Code)
	}
}

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
