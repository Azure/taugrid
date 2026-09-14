// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/taugrid/portal/internal/portal/ray"
)

// rayProxyPrefix is the portal-relative prefix under which the Ray dashboard is
// reverse-proxied: /api/portal/ray/proxy/{ns}/{cluster}/...
const rayProxyPrefix = "/api/portal/ray/proxy/"

// Workspace identity must survive relative asset/API requests, which do not
// inherit a document's query string. The first segment is raw base64url.
const rayWorkspaceProxyPrefix = "/api/portal/ray/workspaces/"

// rayDashboardPort is the Ray dashboard port every KubeRay head Service exposes.
const rayDashboardPort = "8265"

// Old origin-root requests contain no reliable target. Keep explicit failures
// rather than selecting a cluster from a cookie or a Referer.
var rayUnscopedPrefixes = []string{
	"/api/",
	"/static/",
	"/nodes",
	"/logical/",
	"/actors",
	"/events",
	"/log_proxy",
	"/worker/",
	"/favicon.ico",
	"/logo.png",
}

// serviceHost returns the in-cluster DNS name for a specific Service.
func serviceHost(ns, svcName string) string {
	return svcName + "." + ns + ".svc:" + rayDashboardPort
}

// rayTransport returns the RoundTripper the proxy dials head Services with.
func (s *Server) rayTransport() http.RoundTripper {
	if s.ray.Transport != nil {
		return s.ray.Transport
	}
	return http.DefaultTransport
}

// rayTargetCacheTTL is how long a validated head-svc discovery result is cached.
// The Ray dashboard SPA issues many parallel asset fetches on page load; without
// a cache each one would trigger a full ListServices call against the API server.
const rayTargetCacheTTL = 30 * time.Second

// rayTargetEntry is a cached validation result mapping {ns}/{cluster} to the
// discovered Service name used for dialing.
type rayTargetEntry struct {
	service string
	expiry  time.Time
}

// rayTargetCache caches discovered {ns}/{cluster} → service-name mappings.
// Entries expire after rayTargetCacheTTL.
type rayTargetCache struct {
	mu      sync.Mutex
	entries map[string]rayTargetEntry // key "ns/cluster"
}

func (c *rayTargetCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return "", false
	}
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiry) {
		delete(c.entries, key)
		return "", false
	}
	return e.service, true
}

func (c *rayTargetCache) set(key, service string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]rayTargetEntry)
	}
	c.entries[key] = rayTargetEntry{service: service, expiry: time.Now().Add(rayTargetCacheTTL)}
}

// validateRayTarget confirms {ns}/{cluster} names a currently-discovered Ray head
// Service before the proxy dials it. This is the SSRF guard: without it a client
// could steer the in-cluster proxy at an arbitrary host by crafting the path.
// It re-runs head-svc discovery (the same source of truth the board uses)
// rather than trusting the request.
//
// When s.ray.Namespace is configured, only that namespace is allowed — preventing
// URL-crafted namespace bypass.
//
// Returns the discovered Service name (to dial the exact Service, not a derived
// name) and whether validation passed.
func (s *Server) validateRayTarget(ctx context.Context, ns, cluster, allowedNamespace string) (string, bool) {
	if ns == "" || cluster == "" || s.ray.Reader == nil {
		return "", false
	}
	// Enforce configured namespace scope.
	if allowedNamespace != "" && ns != allowedNamespace {
		return "", false
	}
	key := ns + "/" + cluster
	if svc, ok := s.rayCache.get(key); ok {
		return svc, true
	}
	snap, err := ray.Board(ctx, s.ray.Reader, ray.Options{Namespace: ns})
	if err != nil {
		return "", false
	}
	for _, c := range snap.Clusters {
		if c.Namespace == ns && c.Name == cluster {
			s.rayCache.set(key, c.Service)
			return c.Service, true
		}
	}
	return "", false
}

// proxyToHead reverse-proxies the request to a Ray head Service's :8265,
// rewriting the outgoing path to upstreamPath. svcName is the exact discovered
// Service name (not derived). It handles websocket Upgrade natively
// (httputil.ReverseProxy on Go 1.20+).
func (s *Server) proxyToHead(w http.ResponseWriter, r *http.Request, ns, svcName, upstreamPath, prefix string) {
	target := &url.URL{Scheme: "http", Host: serviceHost(ns, svcName), Path: upstreamPath}
	query := r.URL.Query()
	query.Del("workspace")
	target.RawQuery = query.Encode()
	proxy := &httputil.ReverseProxy{
		Transport: s.rayTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = upstreamPath
			req.URL.RawPath = ""
			req.URL.RawQuery = target.RawQuery
			// Portal credentials must not be sent to a workload-controlled head.
			token, _ := req.Cookie(rayAuthCookieName(prefix))
			req.Header.Del("Cookie")
			// Ray's login dialog explicitly exchanges its user-entered bearer
			// token here. Never forward a Portal bearer on passive requests.
			if req.Method != http.MethodPost || upstreamPath != "/api/authenticate" {
				req.Header.Del("Authorization")
			}
			for _, header := range []string{
				defaultViewerUserHeader, defaultViewerGroupsHeader,
				s.identity.UserHeader, s.identity.GroupsHeader,
			} {
				if header != "" {
					req.Header.Del(header)
				}
			}
			if token != nil {
				req.AddCookie(&http.Cookie{Name: rayUpstreamAuthCookie, Value: token.Value})
			}
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Del("If-None-Match")
			req.Header.Del("If-Modified-Since")
			if upstreamPath == "/" {
				req.Header.Del("Range")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return rewriteRayResponse(resp, target, prefix, r.Method)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "ray dashboard proxy failed: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// handleRayProxy serves the Ray dashboard under
// /api/portal/ray/proxy/{ns}/{cluster}/... and the workspace-qualified canonical
// form. Ray 2.54/2.56 use PUBLIC_URL=".", HashRouter and relative API URLs.
// https://docs.ray.io/en/latest/cluster/configure-manage-dashboard.html#running-behind-a-reverse-proxy
func (s *Server) handleRayProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, rayProxyPrefix)
	scopedPath := strings.HasPrefix(r.URL.Path, rayWorkspaceProxyPrefix)
	if scopedPath {
		encoded, suffix, found := strings.Cut(strings.TrimPrefix(r.URL.Path, rayWorkspaceProxyPrefix), "/")
		workspace, err := base64.RawURLEncoding.DecodeString(encoded)
		if !found || err != nil || len(workspace) == 0 || s.workspaceDirectory == nil {
			http.Error(w, "invalid workspace-qualified Ray target", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if requested := q.Get("workspace"); requested != "" && requested != string(workspace) {
			http.Error(w, "Ray path and query workspace must match", http.StatusBadRequest)
			return
		}
		r = r.Clone(r.Context())
		q.Set("workspace", string(workspace))
		r.URL.RawQuery = q.Encode()
		rest = suffix
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.ray.Reader == nil {
		http.Error(w, "ray board unavailable: portal started without Kubernetes access", http.StatusServiceUnavailable)
		return
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "ray proxy path must be /api/portal/ray/proxy/{namespace}/{cluster}/", http.StatusBadRequest)
		return
	}
	ns, cluster := parts[0], parts[1]
	allowedNamespace := s.ray.Namespace
	if scope.Managed {
		allowedNamespace = scope.Namespace
	}
	svcName, ok := s.validateRayTarget(r.Context(), ns, cluster, allowedNamespace)
	if !ok {
		http.Error(w, "unknown Ray cluster: no matching head Service discovered", http.StatusNotFound)
		return
	}
	upstreamPath := "/"
	if len(parts) == 3 {
		upstreamPath = "/" + parts[2]
	}
	prefix := rayProxyPrefix + ns + "/" + cluster + "/"
	if scope.Managed {
		prefix = rayWorkspaceProxyPrefix + base64.RawURLEncoding.EncodeToString([]byte(scope.WorkspaceID)) + "/" + ns + "/" + cluster + "/"
	}
	if upgrade := r.Header.Get("Upgrade"); upgrade != "" && !isRayWebsocket(r) {
		http.Error(w, "unsupported Ray protocol upgrade", http.StatusNotImplemented)
		return
	}
	if !rayReadRouteAllowed(r.Method, upstreamPath, isRayWebsocket(r)) {
		// Ray treats 401/403 as a token-login challenge. A policy limitation is
		// not an authentication failure and must not trap the UI in a login loop.
		http.Error(w, "unsupported Ray route: Portal permits only passive dashboard reads and Ray authentication", http.StatusNotImplemented)
		return
	}
	if (r.Method == http.MethodPost || isRayWebsocket(r)) && !raySameOrigin(r) {
		http.Error(w, "cross-origin Ray authentication and WebSockets are forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if (scope.Managed && !scopedPath) || len(parts) == 2 {
		target := *r.URL
		target.Path = prefix + strings.TrimPrefix(upstreamPath, "/")
		target.RawPath = ""
		q := target.Query()
		q.Del("workspace")
		target.RawQuery = q.Encode()
		http.Redirect(w, r, target.RequestURI(), http.StatusTemporaryRedirect)
		return
	}
	if upstreamPath == "/api/profiling_enabled" {
		handleRayProfilingDisabled(w, r)
		return
	}
	s.proxyToHead(w, r, ns, svcName, upstreamPath, prefix)
}

func handleRayUnscoped(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "ambiguous Ray request: use a target-prefixed dashboard URL; cookie routing is unsupported", http.StatusBadRequest)
}
