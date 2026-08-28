// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

const (
	workspaceAuthorizationRBAC        = "workspace-rbac"
	workspaceAuthorizationClusterWide = "cluster-wide"

	workspaceAvailabilityAvailable   = "available"
	workspaceAvailabilityRedirect    = "redirect"
	workspaceAvailabilityUnreachable = "unreachable"
	workspaceAvailabilityUnsupported = "unsupported"

	defaultViewerUserHeader   = "X-MS-CLIENT-PRINCIPAL-NAME"
	defaultViewerGroupsHeader = "X-MS-CLIENT-PRINCIPAL-GROUPS"

	maxViewerUserHeaderBytes   = 1024
	maxViewerGroupsHeaderBytes = 16 * 1024
	maxViewerGroups            = 256
)

var (
	errViewerUnauthenticated = errors.New("authenticated viewer identity is required")
	errViewerClaimsTooLarge  = errors.New("authenticated viewer claims exceed supported limits")
	errWorkspaceNotFound     = errors.New("workspace not found")
)

// Viewer is the authenticated identity projected by a trusted Entra-aware
// reverse proxy. The Portal never accepts workspace authorization from query
// parameters or browser-managed state.
type Viewer struct {
	ID     string
	Groups []string
}

// IdentityOptions names the trusted headers populated by the deployment's
// Entra-aware authentication proxy.
type IdentityOptions struct {
	UserHeader   string
	GroupsHeader string
}

// WorkspaceDirectory returns only scopes authorized for the supplied viewer.
// Implementations own the mapping from authenticated principals to workspaces.
type WorkspaceDirectory interface {
	List(ctx context.Context, viewer Viewer) []WorkspaceScope
	Resolve(ctx context.Context, viewer Viewer, workspaceID string) (WorkspaceScope, error)
}

// WorkspaceDirectoryConfig is the metadata-only on-disk Portal registry.
type WorkspaceDirectoryConfig struct {
	LocalCluster string                    `json:"localCluster"`
	Endpoints    []WorkspacePortalEndpoint `json:"endpoints,omitempty"`
	Workspaces   []WorkspaceRecord         `json:"workspaces"`
}

// WorkspacePortalEndpoint registers the Portal serving one cluster. Remote
// endpoints must be HTTPS and carry no query or fragment.
type WorkspacePortalEndpoint struct {
	Cluster      string `json:"cluster"`
	Endpoint     string `json:"endpoint,omitempty"`
	Availability string `json:"availability,omitempty"`
}

// WorkspaceRecord maps a globally unique Portal workspace ID to one immutable
// cluster/namespace/queue/result scope and an explicit visibility policy.
type WorkspaceRecord struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name,omitempty"`
	Cluster        string                 `json:"cluster"`
	Team           string                 `json:"team,omitempty"`
	Namespace      string                 `json:"namespace"`
	LocalQueue     string                 `json:"localQueue,omitempty"`
	ResultScope    string                 `json:"resultScope,omitempty"`
	Source         string                 `json:"source"`
	ExperimentsURL string                 `json:"experimentsUrl,omitempty"`
	Default        bool                   `json:"default,omitempty"`
	Authorization  WorkspaceAuthorization `json:"authorization"`
}

// WorkspaceAuthorization is an explicit Portal visibility policy. It mirrors
// TauWorkspace's authorization mode vocabulary without changing its semantics.
type WorkspaceAuthorization struct {
	Mode   string   `json:"mode"`
	Users  []string `json:"users,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// WorkspaceScope is the server-resolved scope attached to every Portal board.
type WorkspaceScope struct {
	WorkspaceID       string `json:"workspace"`
	Name              string `json:"name"`
	Cluster           string `json:"cluster"`
	Team              string `json:"team,omitempty"`
	Namespace         string `json:"namespace"`
	LocalQueue        string `json:"localQueue,omitempty"`
	ResultScope       string `json:"resultScope,omitempty"`
	Source            string `json:"source"`
	AuthorizationMode string `json:"authorizationMode"`
	ExperimentsURL    string `json:"experimentsUrl,omitempty"`
	PortalEndpoint    string `json:"portalEndpoint,omitempty"`
	Availability      string `json:"availability"`
	Managed           bool   `json:"managed"`
}

type staticWorkspaceDirectory struct {
	localCluster string
	endpoints    map[string]WorkspacePortalEndpoint
	workspaces   []WorkspaceRecord
}

type workspaceDirectoryResponse struct {
	Workspaces []WorkspaceScope `json:"workspaces"`
	Selected   *WorkspaceScope  `json:"selected,omitempty"`
	Managed    bool             `json:"managed"`
}

// LoadWorkspaceDirectory loads and validates a metadata-only JSON directory.
func LoadWorkspaceDirectory(path string) (WorkspaceDirectory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workspace directory: %w", err)
	}
	var cfg WorkspaceDirectoryConfig
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode workspace directory: %w", err)
	}
	return NewWorkspaceDirectory(cfg)
}

// NewWorkspaceDirectory validates cfg and returns an authorization-filtering
// in-memory directory.
func NewWorkspaceDirectory(cfg WorkspaceDirectoryConfig) (WorkspaceDirectory, error) {
	cfg.LocalCluster = strings.TrimSpace(cfg.LocalCluster)
	if cfg.LocalCluster == "" {
		return nil, errors.New("workspace directory localCluster is required")
	}

	endpoints := make(map[string]WorkspacePortalEndpoint, len(cfg.Endpoints))
	for i := range cfg.Endpoints {
		ep := cfg.Endpoints[i]
		ep.Cluster = strings.TrimSpace(ep.Cluster)
		ep.Endpoint = strings.TrimRight(strings.TrimSpace(ep.Endpoint), "/")
		ep.Availability = strings.ToLower(strings.TrimSpace(ep.Availability))
		if ep.Cluster == "" {
			return nil, fmt.Errorf("workspace directory endpoint %d: cluster is required", i)
		}
		if _, exists := endpoints[ep.Cluster]; exists {
			return nil, fmt.Errorf("workspace directory endpoint %d: duplicate cluster %q", i, ep.Cluster)
		}
		if ep.Availability == "" {
			ep.Availability = workspaceAvailabilityAvailable
		}
		switch ep.Availability {
		case workspaceAvailabilityAvailable:
			if ep.Cluster != cfg.LocalCluster {
				if err := validatePortalEndpoint(ep.Endpoint); err != nil {
					return nil, fmt.Errorf("workspace directory endpoint %d: %w", i, err)
				}
			}
		case workspaceAvailabilityUnreachable, workspaceAvailabilityUnsupported:
		default:
			return nil, fmt.Errorf("workspace directory endpoint %d: invalid availability %q", i, ep.Availability)
		}
		endpoints[ep.Cluster] = ep
	}

	seen := make(map[string]struct{}, len(cfg.Workspaces))
	defaults := 0
	workspaces := make([]WorkspaceRecord, len(cfg.Workspaces))
	for i := range cfg.Workspaces {
		ws := cfg.Workspaces[i]
		ws.ID = strings.TrimSpace(ws.ID)
		ws.Name = strings.TrimSpace(ws.Name)
		ws.Cluster = strings.TrimSpace(ws.Cluster)
		ws.Team = normalizeScopeValue(ws.Team)
		ws.Namespace = strings.TrimSpace(ws.Namespace)
		ws.LocalQueue = strings.TrimSpace(ws.LocalQueue)
		ws.ResultScope = strings.TrimSpace(ws.ResultScope)
		ws.Source = strings.TrimSpace(ws.Source)
		ws.ExperimentsURL = strings.TrimSpace(ws.ExperimentsURL)
		ws.Authorization.Mode = strings.ToLower(strings.TrimSpace(ws.Authorization.Mode))
		if ws.ID == "" || ws.Cluster == "" || ws.Namespace == "" || ws.Source == "" {
			return nil, fmt.Errorf("workspace directory workspace %d: id, cluster, namespace, and source are required", i)
		}
		if _, exists := seen[ws.ID]; exists {
			return nil, fmt.Errorf("workspace directory workspace %d: duplicate id %q", i, ws.ID)
		}
		seen[ws.ID] = struct{}{}
		switch ws.Authorization.Mode {
		case workspaceAuthorizationRBAC, workspaceAuthorizationClusterWide:
		default:
			return nil, fmt.Errorf("workspace directory workspace %q: invalid authorization mode %q", ws.ID, ws.Authorization.Mode)
		}
		ws.Authorization.Users = normalizePrincipals(ws.Authorization.Users)
		ws.Authorization.Groups = normalizePrincipals(ws.Authorization.Groups)
		if len(ws.Authorization.Users) == 0 && len(ws.Authorization.Groups) == 0 {
			return nil, fmt.Errorf("workspace directory workspace %q: authorization must name at least one user or group", ws.ID)
		}
		if ws.Default {
			defaults++
		}
		if ws.ExperimentsURL != "" {
			if !strings.Contains(strings.ToLower(ws.Source), "kusto") {
				return nil, fmt.Errorf("workspace directory workspace %q: experimentsUrl requires a Kusto-backed source", ws.ID)
			}
			if err := validateExperimentsURL(ws.ExperimentsURL, ws.Cluster == cfg.LocalCluster); err != nil {
				return nil, fmt.Errorf("workspace directory workspace %q: %w", ws.ID, err)
			}
		}
		workspaces[i] = ws
	}
	if len(workspaces) == 0 {
		return nil, errors.New("workspace directory must contain at least one workspace")
	}
	if defaults > 1 {
		return nil, errors.New("workspace directory may contain at most one default workspace")
	}
	return &staticWorkspaceDirectory{
		localCluster: cfg.LocalCluster,
		endpoints:    endpoints,
		workspaces:   workspaces,
	}, nil
}

func (d *staticWorkspaceDirectory) List(_ context.Context, viewer Viewer) []WorkspaceScope {
	if strings.TrimSpace(viewer.ID) == "" {
		return nil
	}
	scopes := make([]WorkspaceScope, 0, len(d.workspaces))
	for _, ws := range d.workspaces {
		if authorized(viewer, ws.Authorization) {
			scopes = append(scopes, d.scope(ws))
		}
	}
	sort.SliceStable(scopes, func(i, j int) bool {
		return scopes[i].Name < scopes[j].Name
	})
	return scopes
}

func (d *staticWorkspaceDirectory) Resolve(ctx context.Context, viewer Viewer, workspaceID string) (WorkspaceScope, error) {
	if strings.TrimSpace(viewer.ID) == "" {
		return WorkspaceScope{}, errViewerUnauthenticated
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID != "" {
		for _, ws := range d.workspaces {
			if ws.ID == workspaceID && authorized(viewer, ws.Authorization) {
				return d.scope(ws), nil
			}
		}
		return WorkspaceScope{}, errWorkspaceNotFound
	}
	for _, ws := range d.workspaces {
		if ws.Default && authorized(viewer, ws.Authorization) {
			return d.scope(ws), nil
		}
	}
	scopes := d.List(ctx, viewer)
	if len(scopes) == 0 {
		return WorkspaceScope{}, errWorkspaceNotFound
	}
	return scopes[0], nil
}

func (d *staticWorkspaceDirectory) scope(ws WorkspaceRecord) WorkspaceScope {
	name := ws.Name
	if name == "" {
		name = ws.ID
	}
	scope := WorkspaceScope{
		WorkspaceID:       ws.ID,
		Name:              name,
		Cluster:           ws.Cluster,
		Team:              ws.Team,
		Namespace:         ws.Namespace,
		LocalQueue:        ws.LocalQueue,
		ResultScope:       ws.ResultScope,
		Source:            ws.Source,
		AuthorizationMode: ws.Authorization.Mode,
		ExperimentsURL:    ws.ExperimentsURL,
		Availability:      workspaceAvailabilityAvailable,
		Managed:           true,
	}
	if ws.Cluster == d.localCluster {
		return scope
	}
	ep, ok := d.endpoints[ws.Cluster]
	if !ok {
		scope.Availability = workspaceAvailabilityUnsupported
		return scope
	}
	scope.PortalEndpoint = ep.Endpoint
	switch ep.Availability {
	case workspaceAvailabilityAvailable:
		scope.Availability = workspaceAvailabilityRedirect
	case workspaceAvailabilityUnreachable:
		scope.Availability = workspaceAvailabilityUnreachable
	case workspaceAvailabilityUnsupported:
		scope.Availability = workspaceAvailabilityUnsupported
	}
	return scope
}

func authorized(viewer Viewer, auth WorkspaceAuthorization) bool {
	id := strings.ToLower(strings.TrimSpace(viewer.ID))
	for _, user := range auth.Users {
		if id == user {
			return true
		}
	}
	groups := make(map[string]struct{}, len(viewer.Groups))
	for _, group := range viewer.Groups {
		group = strings.ToLower(strings.TrimSpace(group))
		if group != "" {
			groups[group] = struct{}{}
		}
	}
	for _, group := range auth.Groups {
		if _, ok := groups[group]; ok {
			return true
		}
	}
	return false
}

func normalizePrincipals(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}

		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeScopeValue(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "_", "-")
	return strings.ReplaceAll(value, " ", "-")
}

func validatePortalEndpoint(raw string) error {
	if raw == "" {
		return errors.New("available remote endpoint requires an HTTPS endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("remote endpoint %q must be an HTTPS origin or base path without credentials, query, or fragment", raw)
	}
	return nil
}

func validateExperimentsURL(raw string, local bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid experimentsUrl %q", raw)
	}
	if local && isSafeLocalAbsolutePath(raw) && u.Host == "" {
		return nil
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("experimentsUrl %q must be a local absolute path or HTTPS URL", raw)
	}
	return nil
}

func isSafeLocalAbsolutePath(raw string) bool {
	return strings.HasPrefix(raw, "/") &&
		!strings.HasPrefix(raw, "//") &&
		!strings.Contains(raw, `\`)
}

func normalizeIdentityOptions(opts IdentityOptions) IdentityOptions {
	opts.UserHeader = strings.TrimSpace(opts.UserHeader)
	opts.GroupsHeader = strings.TrimSpace(opts.GroupsHeader)
	if opts.UserHeader == "" {
		opts.UserHeader = defaultViewerUserHeader
	}
	if opts.GroupsHeader == "" {
		opts.GroupsHeader = defaultViewerGroupsHeader
	}
	return opts
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) viewer(r *http.Request) (Viewer, error) {
	user := strings.TrimSpace(r.Header.Get(s.identity.UserHeader))
	rawGroups := r.Header.Get(s.identity.GroupsHeader)
	if len(user) > maxViewerUserHeaderBytes || len(rawGroups) > maxViewerGroupsHeaderBytes {
		return Viewer{}, errViewerClaimsTooLarge
	}
	groups := strings.FieldsFunc(rawGroups, func(r rune) bool {
		return r == ',' || r == ';'
	})
	if len(groups) > maxViewerGroups {
		return Viewer{}, errViewerClaimsTooLarge
	}
	return Viewer{ID: user, Groups: groups}, nil
}

func (s *Server) resolveWorkspaceScope(r *http.Request) (WorkspaceScope, error) {
	if s.workspaceDirectory == nil {
		scope := s.singleWorkspaceScope
		if scope.WorkspaceID == "" {
			scope.WorkspaceID = "default"
		}
		if scope.Name == "" {
			scope.Name = "Default"
		}
		if scope.Namespace == "" {
			scope.Namespace = firstNonEmpty(s.runs.Namespace, s.ray.Namespace)
		}
		if scope.Cluster == "" {
			scope.Cluster = firstNonEmpty(s.cluster.Cluster, s.cost.Cluster, s.nodeUtil.Cluster)
		}
		if scope.Availability == "" {
			scope.Availability = workspaceAvailabilityAvailable
		}
		if values, ok := r.URL.Query()["namespace"]; ok && len(values) > 0 {
			scope.Namespace = values[0]
		}
		if values, ok := r.URL.Query()["cluster"]; ok && len(values) > 0 {
			scope.Cluster = values[0]
		}
		return scope, nil
	}

	viewer, err := s.viewer(r)
	if err != nil {
		return WorkspaceScope{}, err
	}
	scope, err := s.workspaceDirectory.Resolve(r.Context(), viewer, r.URL.Query().Get("workspace"))
	if err != nil {
		return WorkspaceScope{}, err
	}
	for key, expected := range map[string]string{
		"namespace": scope.Namespace,
		"cluster":   scope.Cluster,
		"team":      scope.Team,
		"queue":     scope.LocalQueue,
	} {
		if values, ok := r.URL.Query()[key]; ok && len(values) > 0 && values[0] != expected {
			return WorkspaceScope{}, fmt.Errorf("%s query conflicts with resolved workspace scope", key)
		}
	}
	return scope, nil
}

func (s *Server) localWorkspaceScope(w http.ResponseWriter, r *http.Request) (WorkspaceScope, bool) {
	scope, err := s.resolveWorkspaceScope(r)
	if err != nil {
		switch {
		case errors.Is(err, errViewerClaimsTooLarge):
			writeJSONError(w, http.StatusRequestHeaderFieldsTooLarge, err.Error())
		case errors.Is(err, errViewerUnauthenticated):
			writeJSONError(w, http.StatusUnauthorized, err.Error())
		case errors.Is(err, errWorkspaceNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return WorkspaceScope{}, false
	}
	if scope.Availability == workspaceAvailabilityAvailable {
		return scope, true
	}
	payload := map[string]any{
		"reason": "workspace data is not served by this Portal",
	}
	if scope.Availability == workspaceAvailabilityRedirect {
		payload["redirectUrl"] = workspaceRedirectURL(scope, r)
	}
	writeScopedJSON(w, http.StatusOK, payload, scope, scope.Availability)
	return WorkspaceScope{}, false
}

func workspaceRedirectURL(scope WorkspaceScope, r *http.Request) string {
	if scope.PortalEndpoint == "" {
		return ""
	}
	target, err := url.Parse(scope.PortalEndpoint)
	if err != nil {
		return ""
	}
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	q := r.URL.Query()
	q.Set("workspace", scope.WorkspaceID)
	for _, key := range []string{"namespace", "cluster", "endpoint", "portalEndpoint", "experimentsUrl"} {
		q.Del(key)
	}
	target.RawQuery = q.Encode()
	return target.String()
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.workspaceDirectory == nil {
		scope, _ := s.resolveWorkspaceScope(r)
		writeJSON(w, http.StatusOK, workspaceDirectoryResponse{
			Workspaces: []WorkspaceScope{scope},
			Selected:   &scope,
		})
		return
	}

	viewer, err := s.viewer(r)
	if err != nil {
		writeJSONError(w, http.StatusRequestHeaderFieldsTooLarge, err.Error())
		return
	}
	if strings.TrimSpace(viewer.ID) == "" {
		writeJSONError(w, http.StatusUnauthorized, errViewerUnauthenticated.Error())
		return
	}
	scopes := s.workspaceDirectory.List(r.Context(), viewer)
	resp := workspaceDirectoryResponse{Workspaces: scopes, Managed: true}
	if requested := strings.TrimSpace(r.URL.Query().Get("workspace")); requested != "" {
		scope, err := s.workspaceDirectory.Resolve(r.Context(), viewer, requested)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, errWorkspaceNotFound.Error())
			return
		}
		resp.Selected = &scope
	} else if scope, err := s.workspaceDirectory.Resolve(r.Context(), viewer, ""); err == nil {
		resp.Selected = &scope
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) workspaceAwareStellar(next http.Handler) http.Handler {
	if s.workspaceDirectory == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, err := s.resolveWorkspaceScope(r)
		if err != nil {
			switch {
			case errors.Is(err, errViewerClaimsTooLarge):
				writeJSONError(w, http.StatusRequestHeaderFieldsTooLarge, err.Error())
			case errors.Is(err, errViewerUnauthenticated):
				writeJSONError(w, http.StatusUnauthorized, err.Error())
			case errors.Is(err, errWorkspaceNotFound):
				writeJSONError(w, http.StatusNotFound, err.Error())
			default:
				writeJSONError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		if scope.Availability == workspaceAvailabilityRedirect {
			http.Redirect(w, r, workspaceRedirectURL(scope, r), http.StatusTemporaryRedirect)
			return
		}
		if scope.Availability != workspaceAvailabilityAvailable {
			writeScopedJSON(w, http.StatusConflict, map[string]string{
				"reason": "experiment source is unavailable for this workspace",
			}, scope, scope.Availability)
			return
		}
		if scope.ExperimentsURL == "" {
			writeScopedJSON(w, http.StatusConflict, map[string]string{
				"reason": "experiment tracking is untracked for this workspace",
			}, scope, "untracked")
			return
		}
		if !managedStellarRouteAllowed(r) {
			writeScopedJSON(w, http.StatusForbidden, map[string]string{
				"reason": "this Stellar route is not workspace-scoped in managed Portal mode",
			}, scope, "forbidden")
			return
		}
		target, err := url.Parse(scope.ExperimentsURL)
		if err != nil {
			writeScopedJSON(w, http.StatusConflict, map[string]string{
				"reason": "experiment endpoint is invalid",
			}, scope, workspaceAvailabilityUnsupported)
			return
		}
		if target.Host == "" {
			next.ServeHTTP(w, workspaceScopedRequest(r, target.Query(), scope.WorkspaceID, scope.Source))
			return
		}
		http.Redirect(w, r, workspaceExperimentRedirectURL(target, r, scope.WorkspaceID, scope.Source), http.StatusTemporaryRedirect)
	})
}

func managedStellarRouteAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path == "/stellar" || r.URL.Path == "/stellar/" || strings.HasPrefix(r.URL.Path, "/stellar/assets/") {
		return true
	}
	switch r.URL.Path {
	case "/api/stellar/capabilities",
		"/api/v1/stellar/capabilities",
		"/api/v2/stellar/capabilities",
		"/api/stellar/experiments",
		"/api/v1/stellar/experiments",
		"/api/v2/stellar/experiments",
		"/api/stellar/runs",
		"/api/v1/stellar/runs",
		"/api/v2/stellar/runs",
		"/api/stellar/snapshot",
		"/api/v1/stellar/snapshot",
		"/api/v2/stellar/snapshot",
		"/api/stellar/series",
		"/api/v1/stellar/series",
		"/api/v2/stellar/series":
		return true
	default:
		return false
	}
}

func workspaceScopedRequest(r *http.Request, defaults url.Values, workspaceID, source string) *http.Request {
	clone := r.Clone(r.Context())
	clonedURL := *r.URL
	clonedURL.RawQuery = workspaceQuery(defaults, r.URL.Query(), workspaceID, source).Encode()
	clone.URL = &clonedURL
	return clone
}

func workspaceExperimentRedirectURL(target *url.URL, r *http.Request, workspaceID, source string) string {
	redirect := *target
	if r.URL.Path != "/stellar" && r.URL.Path != "/stellar/" {
		redirect.Path = r.URL.Path
		redirect.RawPath = ""
	}
	redirect.RawQuery = workspaceQuery(target.Query(), r.URL.Query(), workspaceID, source).Encode()
	return redirect.String()
}

func workspaceQuery(defaults, request url.Values, workspaceID, source string) url.Values {
	out := url.Values{}
	for key, values := range defaults {
		out[key] = append([]string(nil), values...)
	}
	for key, values := range request {
		out[key] = append([]string(nil), values...)
	}
	out.Set("workspace", workspaceID)
	if strings.Contains(strings.ToLower(source), "kusto") {
		out.Set("source", "kusto")
	}
	return out
}

func writeScopedJSON(w http.ResponseWriter, status int, payload any, scope WorkspaceScope, state string) {
	obj := map[string]any{}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err == nil {
			if err := json.Unmarshal(data, &obj); err != nil {
				obj = map[string]any{"data": payload}
			}
		} else {
			obj["data"] = payload
		}
	}
	obj["scope"] = scope
	obj["state"] = state
	writeJSON(w, status, obj)
}

func writeScopedError(w http.ResponseWriter, status int, scope WorkspaceScope, reason string) {
	writeScopedJSON(w, status, map[string]string{"reason": reason}, scope, "unavailable")
}

func writeJSONError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}

func dataState(empty bool) string {
	if empty {
		return "no_data"
	}
	return "ready"
}
