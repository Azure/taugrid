// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

const rayUpstreamAuthCookie = "ray-authentication-token"
const maxRayDocumentBytes = 2 << 20

func rayAuthCookieName(prefix string) string {
	return fmt.Sprintf("tau_ray_auth_%x", sha256.Sum256([]byte(prefix)))
}

func isRayWebsocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func raySameOrigin(r *http.Request) bool {
	if r.Header.Get("Origin") == "" {
		return true // Non-browser clients need not send Origin.
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.User == nil && (origin.Scheme == "http" || origin.Scheme == "https") &&
		strings.EqualFold(origin.Host, r.Host) && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

// These passive routes match the Ray 2.54/2.56 dashboard service clients.
// A GET is not necessarily a read: profiling, traceback, JVM diagnostics and
// dataset stats-actor creation are deliberately excluded.
func rayReadRouteAllowed(method, upstreamPath string, websocket bool) bool {
	if path.Clean(upstreamPath) != strings.TrimSuffix(upstreamPath, "/") && upstreamPath != "/" {
		return false
	}
	parts := strings.Split(strings.Trim(upstreamPath, "/"), "/")
	logTail := len(parts) == 5 && parts[0] == "api" && parts[1] == "jobs" &&
		parts[2] != "" && parts[3] == "logs" && parts[4] == "tail"
	if websocket {
		return method == http.MethodGet && logTail
	}
	if method == http.MethodPost {
		return upstreamPath == "/api/authenticate"
	}
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	switch upstreamPath {
	case "/", "/favicon.ico", "/logo.png", "/manifest.json",
		"/api/jobs/", "/logical/actors", "/nodes", "/events",
		"/api/v0/tasks", "/api/v0/tasks/summarize", "/api/v0/tasks/timeline",
		"/api/v0/placement_groups", "/api/v0/logs", "/api/v0/logs/file",
		"/api/serve/applications/", "/api/cluster_status", "/api/v0/cluster_metadata",
		"/usage_stats_enabled", "/api/grafana_health", "/api/prometheus_health",
		"/timezone", "/api/authentication_mode", "/api/profiling_enabled", "/api/v0/platform_events":
		return true
	}
	if strings.HasPrefix(upstreamPath, "/static/") {
		return true
	}
	return len(parts) == 2 && parts[0] == "nodes" && parts[1] != "" ||
		len(parts) == 3 && parts[0] == "logical" && parts[1] == "actors" && parts[2] != "" ||
		len(parts) == 3 && parts[0] == "api" && parts[1] == "jobs" && parts[2] != ""
}

// Ray 2.56's only native root-absolute fetch is this capability probe. Returning
// Portal's invariant policy (not a cluster's state) is safe without a target.
func handleRayProfilingDisabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": true,
		"msg":    "Profiling is unavailable through the read-only Portal proxy.",
		"data":   map[string]bool{"profilingEnabled": false},
		"reason": "Portal permits passive dashboard reads, not active profiling.",
	})
}

func rewriteRayResponse(resp *http.Response, target *url.URL, prefix, method string) error {
	resp.Header.Set("Cache-Control", "no-store")
	cookies := resp.Cookies()
	resp.Header.Del("Set-Cookie")
	for _, cookie := range cookies {
		if cookie.Name != rayUpstreamAuthCookie {
			continue
		}
		cookie.Name = rayAuthCookieName(prefix)
		cookie.Domain = ""
		cookie.Path = prefix
		cookie.HttpOnly = true
		cookie.SameSite = http.SameSiteStrictMode
		resp.Header.Add("Set-Cookie", cookie.String())
	}
	if location := resp.Header.Get("Location"); location != "" {
		reference, err := url.Parse(location)
		if err != nil {
			return fmt.Errorf("invalid Ray redirect: %w", err)
		}
		resolved := target.ResolveReference(reference)
		if resolved.Host != target.Host || resolved.Scheme != target.Scheme {
			return fmt.Errorf("unsupported cross-origin Ray redirect")
		}
		resolved.Scheme, resolved.Host = "", ""
		resolved.Path = prefix + strings.TrimPrefix(resolved.Path, "/")
		resolved.RawPath = ""
		resp.Header.Set("Location", resolved.String())
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/html" || method == http.MethodHead {
		return nil
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("unsupported Ray document encoding %q", encoding)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRayDocumentBytes+1))
	if err != nil {
		return fmt.Errorf("read Ray document: %w", err)
	}
	if len(body) > maxRayDocumentBytes {
		return fmt.Errorf("Ray document exceeds %d bytes", maxRayDocumentBytes)
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parse Ray document: %w", err)
	}
	rewriteRayDocument(doc, prefix)
	var rewritten bytes.Buffer
	if err := html.Render(&rewritten, doc); err != nil {
		return fmt.Errorf("render Ray document: %w", err)
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close Ray document: %w", err)
	}
	resp.Body = io.NopCloser(&rewritten)
	resp.ContentLength = int64(rewritten.Len())
	resp.Header.Set("Content-Length", strconv.Itoa(rewritten.Len()))
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	return nil
}

// Rewrite only structural HTML URL attributes, never JavaScript source. Ray's
// webpack chunks/CSS/API calls already use relative URLs in supported builds.
func rewriteRayDocument(node *html.Node, prefix string) {
	if node.Type == html.ElementNode {
		if node.Data == "head" {
			base := &html.Node{Type: html.ElementNode, Data: "base", Attr: []html.Attribute{{Key: "href", Val: prefix}}}
			node.InsertBefore(base, node.FirstChild)
		}
		for i := range node.Attr {
			attr := &node.Attr[i]
			if node.Data == "base" && attr.Key == "href" {
				attr.Val = prefix
				continue
			}
			switch attr.Key {
			case "href", "src", "action", "poster":
				if strings.HasPrefix(attr.Val, "/") && !strings.HasPrefix(attr.Val, "//") {
					attr.Val = prefix + strings.TrimPrefix(attr.Val, "/")
				}
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		rewriteRayDocument(child, prefix)
	}
}
