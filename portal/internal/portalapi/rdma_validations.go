// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/portal/internal/portal/rdmavalidation"
)

const (
	defaultRDMAValidationLimit = 20
	maxRDMAValidationLimit     = 100
	maxRDMAValidationCursor    = 2048
)

type rdmaValidationError struct {
	Code      string `json:"code"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (s *Server) handleRDMAValidationSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.rdmaValidations.Reader == nil {
		writeRDMAValidationError(w, http.StatusServiceUnavailable, scope, "backend_unavailable", "InfiniBand validation data is not configured", true)
		return
	}
	summary, err := s.rdmaValidations.Reader.Summary(r.Context(), s.rdmaValidationScope(r, scope))
	if err != nil {
		s.writeRDMAValidationReadError(w, scope, err)
		return
	}
	writeScopedJSON(w, http.StatusOK, summary, scope, dataState(summary.Latest == nil))
}

func (s *Server) handleRDMAValidations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.rdmaValidations.Reader == nil {
		writeRDMAValidationError(w, http.StatusServiceUnavailable, scope, "backend_unavailable", "InfiniBand validation data is not configured", true)
		return
	}
	opts, err := rdmaValidationListOptions(r)
	if err != nil {
		writeRDMAValidationError(w, http.StatusBadRequest, scope, "invalid_argument", err.Error(), false)
		return
	}
	page, err := s.rdmaValidations.Reader.List(r.Context(), s.rdmaValidationScope(r, scope), opts)
	if err != nil {
		s.writeRDMAValidationReadError(w, scope, err)
		return
	}
	if page.Validations == nil {
		page.Validations = []rdmavalidation.Validation{}
	}
	writeScopedJSON(w, http.StatusOK, page, scope, dataState(len(page.Validations) == 0))
}

func (s *Server) handleRDMAValidationDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	validationID := strings.TrimPrefix(r.URL.Path, "/api/portal/rdma-validations/")
	if validationID == "" || strings.Contains(validationID, "/") || exptelemetry.ValidateID("validation ID", validationID) != nil {
		writeRDMAValidationError(w, http.StatusNotFound, scope, "not_found", "InfiniBand validation not found", false)
		return
	}
	if s.rdmaValidations.Reader == nil {
		writeRDMAValidationError(w, http.StatusServiceUnavailable, scope, "backend_unavailable", "InfiniBand validation data is not configured", true)
		return
	}
	detail, err := s.rdmaValidations.Reader.Get(r.Context(), s.rdmaValidationScope(r, scope), validationID)
	if err != nil {
		s.writeRDMAValidationReadError(w, scope, err)
		return
	}
	if detail.ValidationID != validationID ||
		(detail.WorkspaceID != "" && detail.WorkspaceID != scope.WorkspaceID) ||
		(detail.Cluster != "" && scope.Cluster != "" && detail.Cluster != scope.Cluster) {
		writeRDMAValidationError(w, http.StatusNotFound, scope, "not_found", "InfiniBand validation not found", false)
		return
	}
	writeScopedJSON(w, http.StatusOK, detail, scope, detail.State)
}

func (s *Server) rdmaValidationScope(request *http.Request, scope WorkspaceScope) rdmavalidation.Scope {
	return rdmavalidation.Scope{
		WorkspaceID:   scope.WorkspaceID,
		Cluster:       scope.Cluster,
		FetchArtifact: s.rdmaArtifactFetcher(request, scope),
	}
}

func rdmaValidationListOptions(r *http.Request) (rdmavalidation.ListOptions, error) {
	opts := rdmavalidation.ListOptions{Limit: defaultRDMAValidationLimit, Cursor: strings.TrimSpace(r.URL.Query().Get("cursor"))}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxRDMAValidationLimit {
			return rdmavalidation.ListOptions{}, errors.New("limit must be an integer from 1 to 100")
		}
		opts.Limit = limit
	}
	if len(opts.Cursor) > maxRDMAValidationCursor {
		return rdmavalidation.ListOptions{}, errors.New("cursor is too long")
	}
	return opts, nil
}

func (s *Server) writeRDMAValidationReadError(w http.ResponseWriter, scope WorkspaceScope, err error) {
	switch {
	case errors.Is(err, rdmavalidation.ErrNotFound):
		writeRDMAValidationError(w, http.StatusNotFound, scope, "not_found", "InfiniBand validation not found", false)
	case errors.Is(err, rdmavalidation.ErrInvalidCursor):
		writeRDMAValidationError(w, http.StatusBadRequest, scope, "invalid_argument", "InfiniBand validation cursor is invalid", false)
	case errors.Is(err, rdmavalidation.ErrUnavailable):
		writeRDMAValidationError(w, http.StatusServiceUnavailable, scope, "backend_unavailable", "InfiniBand validation data is unavailable", true)
	case errors.Is(err, rdmavalidation.ErrScopeMismatch):
		writeRDMAValidationError(w, http.StatusNotFound, scope, "not_found", "InfiniBand validation not found", false)
	case errors.Is(err, rdmavalidation.ErrUnsupportedSchema):
		writeRDMAValidationError(w, http.StatusBadGateway, scope, "unsupported_schema", "InfiniBand validation schema is unsupported", false)
	case errors.Is(err, rdmavalidation.ErrMalformedArtifact):
		writeRDMAValidationError(w, http.StatusBadGateway, scope, "malformed_artifact", "InfiniBand validation artifact is malformed", false)
	case errors.Is(err, rdmavalidation.ErrArtifactIntegrity):
		writeRDMAValidationError(w, http.StatusBadGateway, scope, "artifact_integrity_failed", "InfiniBand validation artifact verification failed", false)
	default:
		writeRDMAValidationError(w, http.StatusBadGateway, scope, "query_failed", "InfiniBand validation query failed", true)
	}
}

func writeRDMAValidationError(w http.ResponseWriter, status int, scope WorkspaceScope, code, reason string, retryable bool) {
	writeScopedJSON(w, status, rdmaValidationError{Code: code, Reason: reason, Retryable: retryable}, scope, "unavailable")
}
