// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/Azure/taugrid/portal/internal/expcockpit"
)

// An experiment-expanded snapshot may truncate an exact run. Resolve that run
// through the workspace-scoped search API before reading its target-bound index.
func trustedExactRunArtifacts(client *http.Client, request *http.Request, scope WorkspaceScope) ([]expcockpit.ArtifactView, int) {
	target := request.URL.Query().Get("target")
	query := url.Values{
		"workspace": {scope.WorkspaceID}, "source": {request.URL.Query().Get("source")},
		"target": {target}, "limit": {"1"},
	}
	if project := request.URL.Query().Get("project"); project != "" {
		query.Set("project", project)
	}
	read := func(route string, limit int64, value any) int {
		probe, err := trustedStellarProbe(request, scope, route, query)
		if err != nil {
			return http.StatusBadGateway
		}
		response, err := client.Do(probe)
		if err != nil {
			return http.StatusBadGateway
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return http.StatusNotFound
		}
		if response.StatusCode != http.StatusOK {
			return http.StatusBadGateway
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, limit)).Decode(value); err != nil {
			return http.StatusBadGateway
		}
		return http.StatusOK
	}
	var runs struct {
		Runs []struct {
			RunID       string            `json:"run_id"`
			WorkspaceID string            `json:"workspace_id"`
			Tags        map[string]string `json:"tags"`
		} `json:"runs"`
	}
	if status := read("/runs", 1<<20, &runs); status != http.StatusOK {
		return nil, status
	}
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != target {
		return nil, http.StatusNotFound
	}
	run := runs.Runs[0]
	if (run.WorkspaceID != "" && run.WorkspaceID != scope.WorkspaceID) ||
		(run.Tags["tau_workspace"] != "" && run.Tags["tau_workspace"] != scope.WorkspaceID) {
		return nil, http.StatusNotFound
	}
	query.Del("limit")
	var index struct {
		Target    string                    `json:"target"`
		Artifacts []expcockpit.ArtifactView `json:"artifacts"`
	}
	if status := read("/artifacts", 8<<20, &index); status != http.StatusOK {
		return nil, status
	}
	if index.Target != target {
		return nil, http.StatusBadGateway
	}
	artifacts := make([]expcockpit.ArtifactView, 0, len(index.Artifacts))
	for _, artifact := range index.Artifacts {
		if artifact.RunID == target {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, http.StatusOK
}
