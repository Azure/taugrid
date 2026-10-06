// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"net/http"

	portalquota "github.com/Azure/taugrid/portal/internal/portal/quota"
)

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.quota.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "portal started without Kubernetes quota access")
		return
	}
	snapshot, err := portalquota.Read(r.Context(), s.quota.Reader, portalquota.Scope{
		Workspace: scope.WorkspaceID, Team: scope.Team,
		Namespace: scope.Namespace, LocalQueue: scope.LocalQueue,
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, "ready")
}
