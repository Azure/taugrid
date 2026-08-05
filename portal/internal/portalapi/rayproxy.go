package portalapi

import (
	"context"
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

// rayTargetCookie scopes root-absolute Ray dashboard asset requests (which carry
// no proxy prefix) to a {ns}/{cluster} focused target. handleRayProxy sets it;
// handleRayAsset reads it.
const rayTargetCookie = "ray_target"

// rayDashboardPort is the Ray dashboard port every KubeRay head Service exposes.
const rayDashboardPort = "8265"

// rayAssetPrefixes are the origin-root paths the Ray dashboard SPA fetches without
// a proxy prefix (its JS uses absolute URLs). They are routed to handleRayAsset,
// which resolves the upstream from the ray_target cookie. More specific portal
// routes (e.g. /api/portal/ray) still win via ServeMux longest-prefix matching.
//
// NOTE: "/api/" is a subtree pattern — any future portal route under /api/...
// that is NOT registered as a more-specific handler will fall through here
// (returning 400 when no ray_target cookie is set). Always register new
// /api/portal/... routes explicitly on the mux before these catch-alls.
var rayAssetPrefixes = []string{
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
// could steer the in-cluster proxy at an arbitrary host by crafting the path or
// cookie. It re-runs head-svc discovery (the same source of truth the board uses)
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
func (s *Server) proxyToHead(w http.ResponseWriter, r *http.Request, ns, svcName, upstreamPath string) {
	target := &url.URL{Scheme: "http", Host: serviceHost(ns, svcName)}
	proxy := &httputil.ReverseProxy{
		Transport: s.rayTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = upstreamPath
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "ray dashboard unreachable: the RayCluster may have been deleted", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// handleRayProxy serves the Ray dashboard under
// /api/portal/ray/proxy/{ns}/{cluster}/... It validates the target, sets the
// ray_target cookie so the SPA's root-absolute asset fetches route back to the
// same head Service, strips the prefix, and reverse-proxies to :8265.
func (s *Server) handleRayProxy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.ray.Reader == nil {
		http.Error(w, "ray board unavailable: portal started without Kubernetes access", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, rayProxyPrefix)
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
	cookieValue := ns + "/" + cluster
	if scope.Managed {
		cookieValue = scope.WorkspaceID + "|" + ns + "|" + cluster
	}
	http.SetCookie(w, &http.Cookie{
		Name:     rayTargetCookie,
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	s.proxyToHead(w, r, ns, svcName, upstreamPath)
}

// handleRayAsset serves the Ray dashboard's root-absolute assets (no proxy
// prefix). It resolves the focused target from the ray_target cookie, validates
// it, and proxies the request through with its original path unchanged.
func (s *Server) handleRayAsset(w http.ResponseWriter, r *http.Request) {
	if s.ray.Reader == nil {
		http.Error(w, "ray board unavailable: portal started without Kubernetes access", http.StatusServiceUnavailable)
		return
	}
	cookie, err := r.Cookie(rayTargetCookie)
	if err != nil {
		http.Error(w, "no Ray dashboard selected: open a cluster from the Ray board first", http.StatusBadRequest)
		return
	}
	var workspaceID, ns, cluster string
	if s.workspaceDirectory != nil {
		parts := strings.SplitN(cookie.Value, "|", 3)
		if len(parts) != 3 {
			http.Error(w, "invalid Ray target cookie", http.StatusBadRequest)
			return
		}
		workspaceID, ns, cluster = parts[0], parts[1], parts[2]
	} else {
		var ok bool
		ns, cluster, ok = strings.Cut(cookie.Value, "/")
		if !ok {
			http.Error(w, "invalid Ray target cookie", http.StatusBadRequest)
			return
		}
	}
	if ns == "" || cluster == "" {
		http.Error(w, "invalid Ray target cookie", http.StatusBadRequest)
		return
	}
	req := r
	if workspaceID != "" {
		clone := r.Clone(r.Context())
		clonedURL := *r.URL
		q := clonedURL.Query()
		q.Set("workspace", workspaceID)
		clonedURL.RawQuery = q.Encode()
		clone.URL = &clonedURL
		req = clone
	}
	scope, ok := s.localWorkspaceScope(w, req)
	if !ok {
		return
	}
	allowedNamespace := s.ray.Namespace
	if scope.Managed {
		allowedNamespace = scope.Namespace
	}
	svcName, valid := s.validateRayTarget(r.Context(), ns, cluster, allowedNamespace)
	if !valid {
		http.Error(w, "unknown Ray cluster: no matching head Service discovered", http.StatusNotFound)
		return
	}
	s.proxyToHead(w, r, ns, svcName, r.URL.Path)
}
