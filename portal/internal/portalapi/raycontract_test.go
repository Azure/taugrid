// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/portal/internal/expapi"
	"golang.org/x/net/html"
	"golang.org/x/net/websocket"
)

const twoRayHeads = `{"items":[
{"metadata":{"name":"alpha-head-svc","namespace":"ray","labels":{"ray.io/cluster":"alpha","ray.io/node-type":"head"}},"spec":{"type":"ClusterIP"}},
{"metadata":{"name":"beta-head-svc","namespace":"ray","labels":{"ray.io/cluster":"beta","ray.io/node-type":"head"}},"spec":{"type":"ClusterIP"}}
]}`

func rayContractServer(t *testing.T, transport http.RoundTripper) *Server {
	t.Helper()
	s, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Ray:     RayOptions{Reader: fakeRayReader{json: twoRayHeads}, Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rayResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRayTargetsInterleavedAssetsAndAPIs(t *testing.T) {
	// PUBLIC_URL="." and relative API paths are the production Ray 2.54/2.56
	// contract. Root-absolute HTML attributes are handled structurally too.
	const document = `<!doctype html><html><head>
<script src="./static/js/main.js"></script><link href="/static/css/main.css" rel="stylesheet">
</head><body><img src="/logo.png"><script>const untouched = "/api/jobs/";</script></body></html>`
	var calls []string
	s := rayContractServer(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Host+r.URL.RequestURI())
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get(defaultViewerUserHeader) != "" {
			t.Error("Portal credentials reached Ray")
		}
		if r.URL.Path == "/" {
			return rayResponse("text/html; charset=utf-8", document), nil
		}
		return rayResponse("text/plain", r.Host+r.URL.RequestURI()), nil
	}))
	for _, target := range []string{"alpha", "beta", "alpha", "beta"} {
		prefix := rayProxyPrefix + "ray/" + target + "/"
		for _, suffix := range []string{"", "static/js/main.js", "static/js/nested/chunk.js", "static/media/icon.svg", "api/jobs/?limit=2", "api/v0/logs/file?filename=raylet.out&lines=10"} {
			req := httptest.NewRequest(http.MethodGet, prefix+suffix, nil)
			req.AddCookie(&http.Cookie{Name: "ray_target", Value: "ray/the-other-target"})
			req.AddCookie(&http.Cookie{Name: "portal_session", Value: "private"})
			req.Header.Set("Referer", "https://untrusted.example/another-target/")
			req.Header.Set("Authorization", "Bearer portal-token")
			req.Header.Set(defaultViewerUserHeader, "viewer")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", suffix, rec.Code, rec.Body.String())
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Fatalf("target cookie returned: %+v", rec.Result().Cookies())
			}
			want := target + "-head-svc.ray.svc:8265/" + suffix
			if calls[len(calls)-1] != want {
				t.Fatalf("target crossed: got %s want %s", calls[len(calls)-1], want)
			}
			if suffix == "" {
				for _, expected := range []string{`<base href="` + prefix + `"/>`, `href="` + prefix + `static/css/main.css"`, `src="` + prefix + `logo.png"`, `const untouched = "/api/jobs/";`} {
					if !strings.Contains(rec.Body.String(), expected) {
						t.Errorf("rewritten document missing %q: %s", expected, rec.Body.String())
					}
				}
				doc, err := html.Parse(strings.NewReader(rec.Body.String()))
				if err != nil {
					t.Fatal(err)
				}
				var checkURLs func(*html.Node)
				checkURLs = func(node *html.Node) {
					for _, attr := range node.Attr {
						if attr.Key == "src" || attr.Key == "href" {
							base, _ := url.Parse("http://portal.example" + prefix)
							ref, _ := url.Parse(attr.Val)
							if !strings.HasPrefix(base.ResolveReference(ref).Path, prefix) {
								t.Errorf("asset escaped target: %s", attr.Val)
							}
						}
					}
					for child := node.FirstChild; child != nil; child = child.NextSibling {
						checkURLs(child)
					}
				}
				checkURLs(doc)
			}
		}
	}
	before := len(calls)
	for _, requestPath := range []string{"/api/jobs/", "/static/js/main.js", "/nodes"} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		req.AddCookie(&http.Cookie{Name: "ray_target", Value: "ray/alpha"})
		req.Header.Set("Referer", "http://portal.example"+rayProxyPrefix+"ray/alpha/")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ambiguous") {
			t.Fatalf("unscoped request accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(calls) != before {
		t.Fatal("ambiguous request dialed an upstream")
	}
}

func TestRayCanonicalWorkspaceAuthorization(t *testing.T) {
	var calls []string
	s, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"}, WorkspaceDirectory: testWorkspaceDirectory(t),
		Ray: RayOptions{Reader: &markerPortalReader{}, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.Host+r.URL.RequestURI())
			return rayResponse("text/plain", r.Host), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{"alpha", "beta", "alpha", "beta"} {
		namespace := "team-" + workspace
		cluster := workspace + "-marker-ray"
		entry := rayProxyPrefix + namespace + "/" + cluster + "?workspace=" + workspace
		req := httptest.NewRequest(http.MethodGet, entry, nil)
		req.Header.Set(defaultViewerUserHeader, "beta@example.com")
		req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		prefix := rayWorkspaceProxyPrefix + base64.RawURLEncoding.EncodeToString([]byte(workspace)) + "/" + namespace + "/" + cluster + "/"
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != prefix {
			t.Fatalf("canonical entry: %d %s", rec.Code, rec.Header().Get("Location"))
		}
		for _, suffix := range []string{"", "static/js/nested.js", "api/jobs/"} {
			req := httptest.NewRequest(http.MethodGet, prefix+suffix, nil)
			req.Header.Set(defaultViewerUserHeader, "beta@example.com")
			req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), workspace+"-marker") {
				t.Fatalf("workspace not carried by path: %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(calls[len(calls)-1], "workspace=") {
				t.Fatal("Portal workspace selector leaked to Ray")
			}
		}
	}
	before := len(calls)
	betaPrefix := rayWorkspaceProxyPrefix + base64.RawURLEncoding.EncodeToString([]byte("beta")) + "/team-beta/beta-marker-ray/"
	for _, tc := range []struct {
		path   string
		status int
	}{
		{betaPrefix + "api/jobs/", http.StatusNotFound},
		{betaPrefix + "static/js/app.js", http.StatusNotFound},
		{betaPrefix + "?workspace=alpha", http.StatusBadRequest},
		{rayWorkspaceProxyPrefix + "YWxwaGE/team-beta/beta-marker-ray/", http.StatusNotFound},
	} {
		rec := alphaRequest(t, s, tc.path)
		if rec.Code != tc.status {
			t.Fatalf("scope escaped: %s => %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	if len(calls) != before {
		t.Fatal("unauthorized request used warm target cache")
	}
}

func TestRayReadOnlyContract(t *testing.T) {
	s := rayContractServer(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unsupported operation dialed Ray: %s %s", r.Method, r.URL)
		return nil, nil
	}))
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodPost, "api/jobs/"}, {http.MethodDelete, "api/jobs/job"},
		{http.MethodGet, "worker/cpu_profile"}, {http.MethodGet, "task/traceback"},
		{http.MethodGet, "logical/kill_actor"}, {http.MethodGet, "memory_profile"},
		{http.MethodGet, "utils/jstack"}, {http.MethodGet, "api/data/datasets/job"},
		{http.MethodGet, "api/unknown"}, {http.MethodPost, "static/js/main.js"},
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, rayProxyPrefix+"ray/alpha/"+tc.suffix, nil))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s status %d", tc.method, tc.suffix, rec.Code)
		}
	}
	for _, requestPath := range []string{"/api/profiling_enabled", rayProxyPrefix + "ray/alpha/api/profiling_enabled", rayProxyPrefix + "ray/beta/api/profiling_enabled"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, requestPath, nil))
		var payload struct {
			Data   struct{ ProfilingEnabled bool } `json:"data"`
			Reason string                          `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || payload.Data.ProfilingEnabled || payload.Reason == "" {
			t.Fatalf("profiling policy is not explicit: %d %s", rec.Code, rec.Body.String())
		}
	}
	if !rayReadRouteAllowed(http.MethodPost, "/api/authenticate", false) {
		t.Fatal("Ray authentication must remain possible")
	}
}

func TestRayAuthenticationCookiesAreTargetLocal(t *testing.T) {
	s := rayContractServer(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		target := strings.TrimSuffix(r.Host, "-head-svc.ray.svc:8265")
		if r.URL.Path == "/api/authenticate" {
			if r.Header.Get("Authorization") != "Bearer "+target+"-token" {
				response := rayResponse("application/json", `{"error":"missing or invalid Ray token"}`)
				response.StatusCode = http.StatusUnauthorized
				return response, nil
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "{}" {
				t.Errorf("native Ray authentication body = %q, err=%v", body, err)
			}
			response := rayResponse("application/json", `{}`)
			response.Header.Set("Set-Cookie", rayUpstreamAuthCookie+"="+target+"-token; Path=/; HttpOnly")
			return response, nil
		}
		if len(r.Cookies()) != 1 || r.Cookies()[0].Name != rayUpstreamAuthCookie || r.Cookies()[0].Value != target+"-token" {
			t.Errorf("cookies crossed target %s: %s", target, r.Header.Get("Cookie"))
		}
		return rayResponse("application/json", `{}`), nil
	}))
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	portalURL, _ := url.Parse(portal.URL)
	jar.SetCookies(portalURL, []*http.Cookie{
		{Name: "portal_session", Value: "private", Path: "/"},
		{Name: rayUpstreamAuthCookie, Value: "obsolete-root-token", Path: "/"},
	})
	for _, target := range []string{"alpha", "beta"} {
		req, err := http.NewRequest(http.MethodPost, portal.URL+rayProxyPrefix+"ray/"+target+"/api/authenticate", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", portal.URL)
		req.Header.Set("Authorization", "Bearer "+target+"-token")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authentication status %d", response.StatusCode)
		}
		cookies := response.Cookies()
		prefix := rayProxyPrefix + "ray/" + target + "/"
		if len(cookies) != 1 || cookies[0].Name != rayAuthCookieName(prefix) || cookies[0].Path != prefix || cookies[0].Domain != "" {
			t.Fatalf("cookie isolation missing: %+v", cookies)
		}
	}
	for _, target := range []string{"alpha", "beta", "alpha", "beta"} {
		response, err := client.Get(portal.URL + rayProxyPrefix + "ray/" + target + "/api/jobs/")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("read status %d", response.StatusCode)
		}
	}
}

func TestRayRedirectsAndCookieDeletionKeepPrefix(t *testing.T) {
	prefix := rayProxyPrefix + "ray/alpha/"
	target, _ := url.Parse("http://alpha-head-svc.ray.svc:8265/nodes/id?old=1")
	for _, tc := range []struct{ location, want string }{
		{"/api/v0/logs/file?filename=profile.html", prefix + "api/v0/logs/file?filename=profile.html"},
		{"../api/jobs/", prefix + "api/jobs/"},
		{"?page=2", prefix + "nodes/id?page=2"},
		{"http://alpha-head-svc.ray.svc:8265/nodes", prefix + "nodes"},
	} {
		response := rayResponse("text/plain", "")
		response.Header.Set("Location", tc.location)
		response.Header.Add("Set-Cookie", rayUpstreamAuthCookie+"=; Domain=alpha-head-svc.ray.svc; Path=/; Max-Age=0")
		response.Header.Add("Set-Cookie", "unrelated=value; Path=/")
		if err := rewriteRayResponse(response, target, prefix, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		if got := response.Header.Get("Location"); got != tc.want {
			t.Fatalf("redirect %q => %q want %q", tc.location, got, tc.want)
		}
		cookies := response.Cookies()
		if len(cookies) != 1 || cookies[0].Path != prefix || cookies[0].Name != rayAuthCookieName(prefix) || cookies[0].MaxAge != -1 || cookies[0].Domain != "" {
			t.Fatalf("deletion escaped target: %+v", cookies)
		}
	}
	for _, location := range []string{"https://other.example/api/jobs/", "//beta-head-svc.ray.svc:8265/"} {
		response := rayResponse("text/plain", "")
		response.Header.Set("Location", location)
		if err := rewriteRayResponse(response, target, prefix, http.MethodGet); err == nil {
			t.Fatalf("cross-origin redirect accepted: %s", location)
		}
	}
}

func TestRayTrailingSlashAndOriginGuards(t *testing.T) {
	s := rayContractServer(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("guarded request reached upstream: %s", r.URL)
		return nil, nil
	}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rayProxyPrefix+"ray/alpha?keep=yes", nil))
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != rayProxyPrefix+"ray/alpha/?keep=yes" {
		t.Fatalf("trailing slash contract: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	for _, tc := range []struct{ method, suffix, upgrade string }{
		{http.MethodPost, "api/authenticate", ""},
		{http.MethodGet, "api/jobs/job/logs/tail", "websocket"},
		{http.MethodGet, "api/jobs/", "h2c"},
	} {
		req := httptest.NewRequest(tc.method, rayProxyPrefix+"ray/alpha/"+tc.suffix, nil)
		req.Header.Set("Origin", "https://attacker.example")
		req.Header.Set("Upgrade", tc.upgrade)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		wantStatus := http.StatusForbidden
		if tc.upgrade == "h2c" {
			wantStatus = http.StatusNotImplemented
		}
		if rec.Code != wantStatus {
			t.Fatalf("cross-origin/protocol guard status %d", rec.Code)
		}
	}
}

func TestRayUnsupportedDocumentFailsExplicitly(t *testing.T) {
	for _, response := range []*http.Response{
		rayResponse("text/html", strings.Repeat("x", maxRayDocumentBytes+1)),
		{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}, "Content-Encoding": {"gzip"}}, Body: http.NoBody},
	} {
		s := rayContractServer(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rayProxyPrefix+"ray/alpha/", nil))
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ray dashboard proxy failed") {
			t.Fatalf("unsupported document looked successful: %d %s", rec.Code, rec.Body.String())
		}
	}
}
func TestRayWebsocketsStayWithTheirTargets(t *testing.T) {
	upstream := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		for {
			var message string
			if err := websocket.Message.Receive(conn, &message); err != nil {
				return
			}
			if err := websocket.Message.Send(conn, conn.Request().Host+conn.Request().URL.RequestURI()+":"+message); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	s := rayContractServer(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Host = upstreamURL.Host
		return http.DefaultTransport.RoundTrip(r)
	}))
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	connections := make([]*websocket.Conn, 0, 2)
	for _, target := range []string{"alpha", "beta"} {
		address := "ws" + strings.TrimPrefix(portal.URL, "http") + rayProxyPrefix + "ray/" + target + "/api/jobs/job/logs/tail?follow=true"
		conn, err := websocket.Dial(address, "", portal.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	for _, index := range []int{0, 1, 0, 1} {
		conn := connections[index]
		if err := websocket.Message.Send(conn, "tick"); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := websocket.Message.Receive(conn, &got); err != nil {
			t.Fatal(err)
		}
		target := []string{"alpha", "beta"}[index]
		want := fmt.Sprintf("%s-head-svc.ray.svc:8265/api/jobs/job/logs/tail?follow=true:tick", target)
		if got != want {
			t.Fatalf("websocket crossed targets: %s want %s", got, want)
		}
	}
	for _, suffix := range []string{"api/jobs/job", "api/authenticate", "worker/cpu_profile"} {
		req := httptest.NewRequest(http.MethodGet, rayProxyPrefix+"ray/alpha/"+suffix, nil)
		req.Header.Set("Upgrade", "websocket")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("unsupported websocket status %d", rec.Code)
		}
	}
}
