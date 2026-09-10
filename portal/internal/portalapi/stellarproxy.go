// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Azure/taugrid/portal/internal/expcockpit"
)

// ExperimentsBackend grants the Portal authority to read a trusted, fixed-scope
// Stellar service. It is deliberately separate from browser navigation URLs.
type ExperimentsBackend struct {
	URL             string `json:"url"`
	BearerTokenFile string `json:"bearerTokenFile,omitempty"`
}

type NativeExperiments struct {
	State       string `json:"state"`
	APIBasePath string `json:"apiBasePath,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func validateExperimentsBackend(backend ExperimentsBackend) error {
	u, err := url.Parse(backend.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		strings.ContainsAny(backend.URL, "\\\r\n\t") || u.RawPath != "" ||
		(u.Path != "" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/") {
		return errors.New("experimentsBackend.url must be a trusted HTTP(S) origin or clean base path without credentials, query, or fragment")
	}
	if u.Scheme != "https" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		trustedHTTP := host == "localhost" || (ip != nil && ip.IsLoopback()) ||
			strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
		if u.Scheme != "http" || !trustedHTTP {
			return errors.New("experimentsBackend.url requires HTTPS except explicit cluster service or loopback HTTP destinations")
		}
	}
	if u.Port() != "" {
		if _, err := net.LookupPort("tcp", u.Port()); err != nil {
			return errors.New("experimentsBackend.url has an invalid port")
		}
	}
	if backend.BearerTokenFile != "" {
		info, err := os.Stat(backend.BearerTokenFile)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("experimentsBackend.bearerTokenFile must name an existing regular service-token file")
		}
	}
	return nil
}

func (s *Server) nativeExperiments(scope WorkspaceScope) NativeExperiments {
	if scope.experimentsBackend != nil {
		return NativeExperiments{State: "available", APIBasePath: "/api/v2/stellar"}
	}
	if scope.ExperimentsURL == "" {
		return NativeExperiments{State: "untracked", Reason: "Experiment tracking is not configured for this workspace."}
	}
	if scope.Availability != workspaceAvailabilityAvailable || !isSafeLocalAbsolutePath(scope.ExperimentsURL) {
		return NativeExperiments{State: "unavailable", Reason: "Configure a trusted experimentsBackend connection to use native experiments."}
	}
	if strings.Contains(strings.ToLower(scope.Source), "kusto") && !s.stellarKustoAvailable {
		return NativeExperiments{State: "unavailable", Reason: "The configured Kusto experiment source is unavailable."}
	}
	return NativeExperiments{State: "available", APIBasePath: "/api/v2/stellar"}
}

func (s *Server) proxyStellar(w http.ResponseWriter, r *http.Request, scope WorkspaceScope) {
	if strings.HasSuffix(r.URL.Path, "/artifacts") && r.URL.Query().Get("run") != "" {
		writeJSONError(w, http.StatusBadRequest, "use a run target for trusted-backend artifact listings; unscoped run indexes are not supported")
		return
	}
	backend := scope.experimentsBackend
	if err := validateExperimentsBackend(*backend); err != nil {
		writeJSONError(w, http.StatusBadGateway, "trusted experiment backend configuration is invalid")
		return
	}
	target, _ := url.Parse(backend.URL)
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawQuery = workspaceQuery(nil, r.URL.Query(), scope.WorkspaceID, scope.Source).Encode()

	timeout := 15 * time.Second
	if s.stellarBackendTimeout > 0 && s.stellarBackendTimeout < timeout {
		timeout = s.stellarBackendTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	// Start with an empty header map: no viewer identity, cookies, forwarding
	// headers, authorization, or other browser-supplied credentials cross this boundary.
	upstream, err := http.NewRequestWithContext(ctx, r.Method, target.String(), nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "cannot construct trusted experiment request")
		return
	}
	upstream.Header.Set("Accept", "application/json, image/*, video/*, application/octet-stream")
	if backend.BearerTokenFile != "" {
		file, err := os.Open(backend.BearerTokenFile)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "trusted experiment service authentication is unavailable")
			return
		}
		token, readErr := io.ReadAll(io.LimitReader(file, 16*1024+1))
		_ = file.Close()
		value := strings.TrimSpace(string(token))
		if readErr != nil || value == "" || len(token) > 16*1024 || strings.ContainsAny(value, "\r\n") {
			writeJSONError(w, http.StatusBadGateway, "trusted experiment service authentication is invalid")
			return
		}
		upstream.Header.Set("Authorization", "Bearer "+value)
	}
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if strings.HasSuffix(r.URL.Path, "/artifact") || strings.HasSuffix(r.URL.Path, "/artifacts") {
		artifacts, status := trustedStellarArtifacts(client, upstream, scope)
		if status != http.StatusOK {
			writeJSONError(w, status, "trusted experiment artifact target could not be authorized")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			records := make([]map[string]any, 0, len(artifacts))
			for _, artifact := range artifacts {
				if filter := strings.TrimSpace(r.URL.Query().Get("type")); filter != "" && artifact.Type != filter {
					continue
				}
				if filter := strings.TrimSpace(r.URL.Query().Get("rank")); filter != "" && artifact.Rank != filter {
					continue
				}
				matchesTags := true
				for _, tag := range r.URL.Query()["tag"] {
					if tag = strings.TrimSpace(tag); tag != "" && !strings.Contains(artifact.Tags, tag) {
						matchesTags = false
					}
				}
				if !matchesTags {
					continue
				}
				data, _ := json.Marshal(artifact)
				var record map[string]any
				_ = json.Unmarshal(data, &record)
				query := url.Values{"workspace": {scope.WorkspaceID}, "target": {r.URL.Query().Get("target")}, "artifact": {artifact.ArtifactID}}
				record["fetch_url"] = "/api/v2/stellar/artifact?" + query.Encode()
				records = append(records, record)
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]any{"source": "index", "target": r.URL.Query().Get("target"), "count": len(records), "artifacts": records})
			return
		}
		found := false
		for _, artifact := range artifacts {
			if artifact.ArtifactID == r.URL.Query().Get("artifact") {
				found = true
				break
			}
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "artifact was not found for the workspace target")
			return
		}
		// Range is the sole browser header forwarded, after media authorization,
		// so native video seeking works without forwarding identity or credentials.
		if value := r.Header.Get("Range"); value != "" {
			upstream.Header.Set("Range", value)
		}
	}
	response, err := client.Do(upstream)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "trusted experiment backend request failed")
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		writeJSONError(w, http.StatusBadGateway, "trusted experiment backend returned an unexpected redirect")
		return
	}
	if response.StatusCode >= 500 {
		writeJSONError(w, http.StatusBadGateway, "trusted experiment backend is unavailable")
		return
	}
	// Never replay Set-Cookie, Location, authentication challenges, CORS, or
	// upstream navigation policy. Media/report content is isolated with sandbox CSP.
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Accept-Ranges", "Content-Range"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func trustedStellarProbe(request *http.Request, scope WorkspaceScope, route string, query url.Values) (*http.Request, error) {
	if scope.experimentsBackend == nil {
		return nil, errors.New("trusted experiment backend is not configured")
	}
	if err := validateExperimentsBackend(*scope.experimentsBackend); err != nil {
		return nil, err
	}
	target, _ := url.Parse(scope.experimentsBackend.URL)
	base := strings.TrimRight(target.Path, "/")
	prefix := strings.TrimPrefix(path.Dir(request.URL.Path), base)
	switch prefix {
	case "/api/v2/stellar", "/api/v1/stellar", "/api/stellar":
	default:
		return nil, errors.New("trusted experiment probe requires a canonical API path")
	}
	switch route {
	case "/snapshot", "/runs", "/artifacts":
	default:
		return nil, errors.New("trusted experiment probe route is not supported")
	}
	// Rebuild from configured authority, never from a request's URL or Host.
	target.Path = base + prefix + route
	target.RawQuery = query.Encode()
	probe, err := http.NewRequestWithContext(request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	probe.Header.Set("Accept", "application/json")
	if token := request.Header.Get("Authorization"); token != "" {
		probe.Header.Set("Authorization", token)
	}
	return probe, nil
}

func trustedStellarArtifacts(client *http.Client, request *http.Request, scope WorkspaceScope) ([]expcockpit.ArtifactView, int) {
	query := request.URL.Query()
	query.Del("mode")
	query.Del("artifact")
	query.Del("run")
	query.Set("include_static", "true")
	probe, err := trustedStellarProbe(request, scope, "/snapshot", query)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	response, err := client.Do(probe)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway
	}
	var snapshot expcockpit.Snapshot
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&snapshot); err != nil ||
		snapshot.Target != query.Get("target") {
		return nil, http.StatusBadGateway
	}
	if snapshot.TargetType != "run" {
		return nil, http.StatusNotFound
	}
	allowed := make(map[string]bool)
	for _, run := range snapshot.Runs {
		if run.WorkspaceID != "" && run.WorkspaceID != scope.WorkspaceID {
			if snapshot.TargetType == "run" && run.RunID == snapshot.Target {
				return nil, http.StatusNotFound
			}
			continue
		}
		if run.RunID != snapshot.Target {
			continue
		}
		allowed[run.RunID] = true
	}
	if len(allowed) == 0 {
		if snapshot.TargetType == "run" {
			return trustedExactRunArtifacts(client, request, scope)
		}
		return nil, http.StatusNotFound
	}
	artifacts := make([]expcockpit.ArtifactView, 0, len(snapshot.Artifacts))
	for _, artifact := range snapshot.Artifacts {
		if allowed[artifact.RunID] {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, http.StatusOK
}
