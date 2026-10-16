// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

const kueueVizProxyPrefix = "/api/portal/kueueviz/"

const kueueVizTargetCookie = "kueueviz_target"

const kueueVizPort = "8080"

var kueueVizAssetPrefixes = []string{
	"/assets/",
}

type KueueVizOptions struct {
	Enabled         bool
	Namespace       string
	BackendService  string
	FrontendService string
	Transport       http.RoundTripper
}

func kueueVizServiceHost(ns, svcName string) string {
	return svcName + "." + ns + ".svc:" + kueueVizPort
}

func (s *Server) kueueVizTransport() http.RoundTripper {
	if s.kueueViz.Transport != nil {
		return s.kueueViz.Transport
	}
	return http.DefaultTransport
}

const kueueVizBackendOrigin = "http://portal.kueueviz.local"

func (s *Server) proxyToKueueViz(w http.ResponseWriter, r *http.Request, svcName, upstreamPath string) {
	s.proxyToKueueVizRewrite(w, r, svcName, upstreamPath, false)
}

func (s *Server) proxyToKueueVizRewrite(w http.ResponseWriter, r *http.Request, svcName, upstreamPath string, rewriteHTML bool) {
	target := &url.URL{Scheme: "http", Host: kueueVizServiceHost(s.kueueViz.Namespace, svcName)}
	proxy := &httputil.ReverseProxy{
		Transport: s.kueueVizTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = upstreamPath
			if svcName == s.kueueViz.BackendService {
				req.Header.Set("Origin", kueueVizBackendOrigin)
			}
			if rewriteHTML {
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

const kueueVizRouterBasename = "/api/portal/kueueviz"

var (
	kueueVizRouterCall    = []byte(`(pn,{children:`)
	kueueVizRouterCallNew = []byte(`(pn,{basename:"` + kueueVizRouterBasename + `",children:`)
)

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
		replaced = bytes.ReplaceAll(body, []byte(`"/assets/`), []byte(`"`+kueueVizProxyPrefix+`assets/`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/assets/`), []byte(`'`+kueueVizProxyPrefix+`assets/`))
		replaced = bytes.ReplaceAll(replaced, []byte(`"/env.js"`), []byte(`"`+kueueVizProxyPrefix+`env.js"`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/env.js'`), []byte(`'`+kueueVizProxyPrefix+`env.js'`))
		replaced = bytes.ReplaceAll(replaced, []byte(`"/favicon.ico"`), []byte(`"`+kueueVizProxyPrefix+`favicon.ico"`))
		replaced = bytes.ReplaceAll(replaced, []byte(`'/favicon.ico'`), []byte(`'`+kueueVizProxyPrefix+`favicon.ico'`))
	} else {
		if !bytes.Contains(body, kueueVizRouterCall) {
			log.Printf("portalapi/kueueviz: router basename marker %q not found in JS bundle; the Kueue (Live) board may render blank (upstream release-0.18 bundle drift). Pin the KueueViz image by digest.", kueueVizRouterCall)
		}
		replaced = bytes.Replace(body, kueueVizRouterCall, kueueVizRouterCallNew, 1)
		replaced = bytes.ReplaceAll(replaced, []byte(`"/kueueviz.png"`), []byte(`"`+kueueVizProxyPrefix+`kueueviz.png"`))
		replaced = bytes.ReplaceAll(replaced, []byte("`/kueueviz.png`"), []byte("`"+kueueVizProxyPrefix+"kueueviz.png`"))
	}

	resp.Body = io.NopCloser(bytes.NewReader(replaced))
	resp.ContentLength = int64(len(replaced))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(replaced)))
	return nil
}

func (s *Server) handleKueueVizProxy(w http.ResponseWriter, r *http.Request) {
	if !s.kueueViz.Enabled {
		http.Error(w, "kueue (live) board unavailable: portal started without --kueueviz", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, kueueVizProxyPrefix)

	if rest == "env.js" {
		s.handleKueueVizEnvJS(w, r)
		return
	}

	if strings.HasPrefix(rest, "ws/") || rest == "ws" {
		s.proxyToKueueViz(w, r, s.kueueViz.BackendService, "/"+rest)
		return
	}

	if strings.HasPrefix(rest, "auth/") || rest == "auth" {
		s.proxyToKueueViz(w, r, s.kueueViz.BackendService, "/"+rest)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     kueueVizTargetCookie,
		Value:    "1",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode,
	})

	upstreamPath := "/" + rest
	s.proxyToKueueVizRewrite(w, r, s.kueueViz.FrontendService, upstreamPath, true)
}

func (s *Server) handleKueueVizEnvJS(w http.ResponseWriter, r *http.Request) {
	scheme := "ws"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if strings.EqualFold(proto, "https") {
			scheme = "wss"
		}
	} else if r.TLS != nil {
		scheme = "wss"
	}
	wsURL := fmt.Sprintf("%s://%s/api/portal/kueueviz", scheme, r.Host)
	body := fmt.Sprintf("window.env = {\n"+
		"  VITE_WEBSOCKET_URL: %q,\n"+
		"  REACT_APP_WEBSOCKET_URL: %q\n"+
		"};\n", wsURL, wsURL)
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

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
