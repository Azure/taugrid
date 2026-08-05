package portalapi

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// kueueVizProxyPrefix is the portal-relative prefix under which the KueueViz
// dashboard is reverse-proxied: /api/portal/kueueviz/...
const kueueVizProxyPrefix = "/api/portal/kueueviz/"

// kueueVizTargetCookie marks that a KueueViz page has been loaded so the SPA's
// root-absolute asset fetches (which carry no proxy prefix) route back to the
// frontend Service. Unlike ray_target it carries no attacker-controllable host:
// the upstream Services are config-fixed (KueueVizOptions), so the cookie's only
// job is to disambiguate that a prefix-less asset request belongs to KueueViz.
const kueueVizTargetCookie = "kueueviz_target"

// kueueVizPort is the ClusterIP port both KueueViz Services expose (8080).
const kueueVizPort = "8080"

// kueueVizAssetPrefixes are the origin-root paths the KueueViz SPA fetches
// without the proxy prefix (its Vite build emits absolute /assets/... URLs).
// They route to handleKueueVizAsset, which proxies to the frontend Service when
// the kueueviz_target cookie is present. More specific portal routes still win
// via ServeMux longest-prefix matching.
var kueueVizAssetPrefixes = []string{
	"/assets/",
}

// KueueVizOptions configures the "Kueue (Live)" board's reverse proxy. When
// Enabled is false the board is disabled and /api/portal/kueueviz* returns 503,
// so the portal still serves every other board. Namespace, BackendService, and
// FrontendService name the fixed in-cluster Services the proxy dials; there is no
// request-derived upstream host, so no SSRF discovery guard is needed. Transport
// dials the Services over in-cluster DNS; when nil it defaults to
// http.DefaultTransport.
type KueueVizOptions struct {
	Enabled         bool
	Namespace       string
	BackendService  string
	FrontendService string
	Transport       http.RoundTripper
}

// kueueVizServiceHost returns the in-cluster DNS name for a KueueViz Service.
func kueueVizServiceHost(ns, svcName string) string {
	return svcName + "." + ns + ".svc:" + kueueVizPort
}

// kueueVizTransport returns the RoundTripper the proxy dials the Services with.
func (s *Server) kueueVizTransport() http.RoundTripper {
	if s.kueueViz.Transport != nil {
		return s.kueueViz.Transport
	}
	return http.DefaultTransport
}

// kueueVizBackendOrigin is the fixed Origin the proxy presents to the KueueViz
// backend on WebSocket/REST requests. The backend rejects browser Upgrades
// whose Origin is not in KUEUEVIZ_ALLOWED_ORIGINS. Because the Portal is a
// same-origin reverse proxy (env.js pins the WS URL to the page origin), that
// per-request browser Origin is arbitrary and externally variable (localhost
// under port-forward, the LB/ingress host in production). Rewriting it to this
// constant decouples the backend allowlist from the deployment's external
// origin: the chart sets KUEUEVIZ_ALLOWED_ORIGINS to exactly this value once
// and never has to track the Portal's reachable host. The browser-facing
// same-origin guarantee is unchanged (env.js still emits a same-origin WS URL).
const kueueVizBackendOrigin = "http://portal.kueueviz.local"

// proxyToKueueViz reverse-proxies the request to a KueueViz Service's :8080,
// rewriting the outgoing path to upstreamPath. It handles websocket Upgrade
// natively (httputil.ReverseProxy on Go 1.20+). Requests to the backend Service
// (WebSocket + REST) have their Origin normalized to kueueVizBackendOrigin so
// the backend's allowlist is independent of the browser's variable origin.
func (s *Server) proxyToKueueViz(w http.ResponseWriter, r *http.Request, svcName, upstreamPath string) {
	s.proxyToKueueVizRewrite(w, r, svcName, upstreamPath, false)
}

// proxyToKueueVizRewrite reverse-proxies to a KueueViz Service. When rewriteHTML
// is true and the upstream returns HTML, root-absolute references the Vite build
// emits (/assets/..., /env.js) are rewritten to carry the proxy prefix so the
// browser fetches them back through the portal (no root routes or cookies
// needed). This makes the board work identically over an http port-forward and
// an https LoadBalancer.
func (s *Server) proxyToKueueVizRewrite(w http.ResponseWriter, r *http.Request, svcName, upstreamPath string, rewriteHTML bool) {
	target := &url.URL{Scheme: "http", Host: kueueVizServiceHost(s.kueueViz.Namespace, svcName)}
	proxy := &httputil.ReverseProxy{
		Transport: s.kueueVizTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = upstreamPath
			// When dialing the backend Service, normalize the Origin to a fixed
			// sentinel so the backend's KUEUEVIZ_ALLOWED_ORIGINS allowlist is
			// independent of the browser's (externally variable) page origin.
			// The frontend Service serves only static assets and does no Origin
			// check, so leave its requests untouched.
			if svcName == s.kueueViz.BackendService {
				req.Header.Set("Origin", kueueVizBackendOrigin)
			}
			if rewriteHTML {
				// Force an uncompressed upstream response so the HTML/JS body
				// rewrites operate on plaintext. Go's transport transparently
				// re-adds gzip when we don't set it ourselves, but only when the
				// client didn't ask; stripping the client header lets the
				// transport handle decompression and hands us plaintext.
				req.Header.Del("Accept-Encoding")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "kueueviz unreachable: the KueueViz backend/frontend Services may not be deployed", http.StatusBadGateway)
		},
	}
	if rewriteHTML {
		proxy.ModifyResponse = rewriteKueueVizHTML
	}
	proxy.ServeHTTP(w, r)
}

// kueueVizRouterBasename is the SPA router basename the built bundle must run
// under so React Router matches its root-based routes at the proxy subpath. The
// Vite build hardcodes the BrowserRouter with no basename (defaults to "/"), and
// it is not bound to BASE_URL or window.env, so it cannot be set at runtime — the
// only reliable fix is rewriting the built JS to inject the basename prop.
const kueueVizRouterBasename = "/api/portal/kueueviz"

// kueueVizRouterCall is the app's BrowserRouter render site in the minified
// bundle. The release-0.18 frontend (react-router 7.15.1) renders the app router
// as `(pn,{children:...)` where `pn` is the BrowserRouter component (its own
// definition `function pn({basename:e,children:t,...})` already accepts a
// basename prop but the app passes none, so it defaults to "/"). This substring
// appears exactly once. We inject a basename prop right after the "{" so the
// SPA's routes match under the proxy subpath.
//
// NOTE: `pn` is a minified alias tied to the mutable `release-0.18` tag; if the
// upstream bundle is rebuilt the alias can drift and this replacement becomes a
// no-op (the board renders blank with `No routes matched location`). Pinning the
// KueueViz image by digest removes that risk.
var (
	kueueVizRouterCall    = []byte(`(pn,{children:`)
	kueueVizRouterCallNew = []byte(`(pn,{basename:"` + kueueVizRouterBasename + `",children:`)
)

// rewriteKueueVizHTML rewrites the KueueViz SPA responses so they work under the
// proxy subpath. For text/html it rewrites root-absolute asset and env.js
// references to carry the proxy prefix. For the SPA's JavaScript bundle it
// injects the React Router basename so routes match at /api/portal/kueueviz/*.
func rewriteKueueVizHTML(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	isHTML := strings.Contains(ct, "text/html")
	isJS := strings.Contains(ct, "javascript")
	if !isHTML && !isJS {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var replaced []byte
	if isHTML {
		// Rewrite src="/assets/..., href="/assets/..., and /env.js to the proxy
		// prefix. The Vite build mixes single and double quotes, so handle both.
		replaced = bytes.ReplaceAll(body, []byte(`"/assets/`), []byte(`"`+kueueVizProxyPrefix+`assets/`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/assets/`), []byte(`'`+kueueVizProxyPrefix+`assets/`))
		replaced = bytes.ReplaceAll(replaced, []byte(`"/env.js"`), []byte(`"`+kueueVizProxyPrefix+`env.js"`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/env.js'`), []byte(`'`+kueueVizProxyPrefix+`env.js'`))
		// The index HTML links a root-absolute favicon; carry the proxy prefix
		// so it loads back through the portal instead of 400ing at the root.
		replaced = bytes.ReplaceAll(replaced, []byte(`"/favicon.ico"`), []byte(`"`+kueueVizProxyPrefix+`favicon.ico"`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/favicon.ico'`), []byte(`'`+kueueVizProxyPrefix+`favicon.ico'`))
	} else {
		// Inject the router basename so the SPA's root-based routes match under
		// the proxy subpath. Single, deterministic replacement.
		if !bytes.Contains(body, kueueVizRouterCall) {
			// The minified `pn` alias is tied to the mutable release-0.18 tag; a
			// rebuilt upstream bundle can drift it, turning this into a no-op and
			// rendering the board blank ("No routes matched location"). Log it so
			// the failure is observable instead of silent. Pin the image by digest
			// to remove the risk.
			log.Printf("portalapi/kueueviz: router basename marker %q not found in JS bundle; the Kueue (Live) board may render blank (upstream release-0.18 bundle drift). Pin the KueueViz image by digest.", kueueVizRouterCall)
		}
		replaced = bytes.Replace(body, kueueVizRouterCall, kueueVizRouterCallNew, 1)
		// The bundle references a root-absolute logo (`src:"/kueueviz.png"`);
		// rewrite it to the proxy prefix so it loads instead of 404ing.
		replaced = bytes.ReplaceAll(replaced, []byte(`"/kueueviz.png"`), []byte(`"`+kueueVizProxyPrefix+`kueueviz.png"`))
		replaced = bytes.ReplaceAll(replaced, []byte("`/kueueviz.png`"), []byte("`"+kueueVizProxyPrefix+"kueueviz.png`"))
	}

	resp.Body = io.NopCloser(bytes.NewReader(replaced))
	resp.ContentLength = int64(len(replaced))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(replaced)))
	return nil
}

// handleKueueVizProxy serves the KueueViz dashboard under /api/portal/kueueviz/.
// It splits by path prefix: /env.js is server-side injected (same-origin WS URL),
// /ws/... routes to the backend Service, and everything else routes to the
// frontend Service (SPA + assets). It sets the kueueviz_target cookie so the
// SPA's root-absolute asset fetches route back to the frontend Service.
func (s *Server) handleKueueVizProxy(w http.ResponseWriter, r *http.Request) {
	if !s.kueueViz.Enabled {
		http.Error(w, "kueue (live) board unavailable: portal started without --kueueviz", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, kueueVizProxyPrefix)

	// /env.js — inject a same-origin WebSocket URL so the browser dials the
	// Portal's proxy path, not the raw backend ingress host baked by the chart.
	if rest == "env.js" {
		s.handleKueueVizEnvJS(w, r)
		return
	}

	// /ws/... — WebSocket endpoints served by the backend Service. These upgrade
	// via Hijack(), which bypasses the ResponseWriter — any header (including a
	// Set-Cookie) written before this branch never reaches the wire, so the
	// cookie is set only on the HTML/asset paths below where it is actually used.
	if strings.HasPrefix(rest, "ws/") || rest == "ws" {
		s.proxyToKueueViz(w, r, s.kueueViz.BackendService, "/"+rest)
		return
	}

	// /auth/... — REST endpoints (e.g. /auth/status) the SPA fetches from the
	// backend base (Qw()+"/auth/status"). These are served by the backend
	// Service, not the frontend SPA, so route them there too.
	if strings.HasPrefix(rest, "auth/") || rest == "auth" {
		s.proxyToKueueViz(w, r, s.kueueViz.BackendService, "/"+rest)
		return
	}

	// Mark the session so prefix-less /assets/... fetches route to the frontend.
	// Secure must track the request scheme: a hard-coded Secure=true makes
	// browsers silently drop the cookie over plain HTTP (kubectl port-forward,
	// the primary dev/test workflow), after which every /assets/ fetch misses the
	// cookie, 400s in handleKueueVizAsset, and the board renders blank. Match the
	// scheme detection used in handleKueueVizEnvJS.
	http.SetCookie(w, &http.Cookie{
		Name:     kueueVizTargetCookie,
		Value:    "1",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode,
	})

	// Everything else — SPA HTML/JS/CSS served by the frontend Service. Rewrite
	// the index HTML's root-absolute asset/env.js paths to the proxy prefix so
	// they load back through the portal (no cookie/root-route dependency).
	upstreamPath := "/" + rest
	s.proxyToKueueVizRewrite(w, r, s.kueueViz.FrontendService, upstreamPath, true)
}

// handleKueueVizEnvJS serves a server-injected /env.js pointing the KueueViz
// frontend at the Portal's same-origin WebSocket proxy path. This makes the page
// origin and socket origin identical by construction, satisfying the backend's
// Origin check without exposing the raw backend host to the browser. The scheme
// (ws vs wss) follows X-Forwarded-Proto (behind the LB) or the request TLS state;
// the host is the request Host header, so the value is always same-origin.
func (s *Server) handleKueueVizEnvJS(w http.ResponseWriter, r *http.Request) {
	scheme := "ws"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if strings.EqualFold(proto, "https") {
			scheme = "wss"
		}
	} else if r.TLS != nil {
		scheme = "wss"
	}
	// Base URL only — the KueueViz frontend appends the subpath itself
	// (o$(e)=base+e for WS "/ws/cluster-queues", Qw()+"/auth/status" for REST).
	// Emitting the "/ws" suffix here would double it to "/ws/ws/..." and break
	// every socket, leaving the board empty.
	wsURL := fmt.Sprintf("%s://%s/api/portal/kueueviz", scheme, r.Host)
	body := fmt.Sprintf("window.env = {\n"+
		"  VITE_WEBSOCKET_URL: %q,\n"+
		"  REACT_APP_WEBSOCKET_URL: %q\n"+
		"};\n", wsURL, wsURL)
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// handleKueueVizAsset serves the KueueViz SPA's root-absolute assets (no proxy
// prefix, e.g. /assets/index-*.js). It routes to the frontend Service when the
// kueueviz_target cookie is present. The upstream is config-fixed, so the cookie
// only gates access to the board (no host is derived from the request).
func (s *Server) handleKueueVizAsset(w http.ResponseWriter, r *http.Request) {
	if !s.kueueViz.Enabled {
		http.Error(w, "kueue (live) board unavailable: portal started without --kueueviz", http.StatusServiceUnavailable)
		return
	}
	if _, err := r.Cookie(kueueVizTargetCookie); err != nil {
		http.Error(w, "no KueueViz page loaded: open the Kueue (Live) board first", http.StatusBadRequest)
		return
	}
	s.proxyToKueueViz(w, r, s.kueueViz.FrontendService, r.URL.Path)
}
