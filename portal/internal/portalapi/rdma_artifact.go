// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
	portalvalidation "github.com/Azure/taugrid/portal/internal/portal/rdmavalidation"
)

const maxRDMAArtifactBytes = 8 << 20

type limitedInternalResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int
	tooLarge bool
}

func newLimitedInternalResponse(limit int) *limitedInternalResponse {
	return &limitedInternalResponse{header: make(http.Header), limit: limit}
}

func (w *limitedInternalResponse) Header() http.Header {
	return w.header
}

func (w *limitedInternalResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *limitedInternalResponse) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(raw) > w.limit-w.body.Len() {
		w.tooLarge = true
		return 0, errors.New("internal response exceeds limit")
	}
	return w.body.Write(raw)
}

func (s *Server) rdmaArtifactFetcher(request *http.Request, scope WorkspaceScope) portalvalidation.ArtifactFetcher {
	return func(ctx context.Context, expected portalvalidation.ArtifactMetadata) ([]byte, portalvalidation.ArtifactMetadata, error) {
		return s.fetchRDMAArtifact(ctx, request, scope, expected)
	}
}

func (s *Server) fetchRDMAArtifact(
	ctx context.Context,
	request *http.Request,
	scope WorkspaceScope,
	expected portalvalidation.ArtifactMetadata,
) ([]byte, portalvalidation.ArtifactMetadata, error) {
	if expected.RunID == "" || expected.ValidationID == "" ||
		expected.URI == "" || expected.SHA256 == "" {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrArtifactIntegrity
	}
	query := workspaceQuery(nil, url.Values{
		"target": {expected.RunID},
		"type":   {corevalidation.ArtifactType},
	}, scope.WorkspaceID, scope.Source)
	listResponse := s.invokeTrustedStellar(ctx, request, "/api/v2/stellar/artifacts?"+query.Encode(), 1<<20)
	if listResponse.tooLarge {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrUnavailable
	}
	if listResponse.status == http.StatusNotFound {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrNotFound
	}
	if listResponse.status != http.StatusOK {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrUnavailable
	}
	var index struct {
		Target    string `json:"target"`
		Artifacts []struct {
			ArtifactID  string `json:"artifact_id"`
			RunID       string `json:"run_id"`
			Type        string `json:"type"`
			URI         string `json:"uri"`
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
			Digest      string `json:"digest"`
			SizeBytes   string `json:"size_bytes"`
			FetchURL    string `json:"fetch_url"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(listResponse.body.Bytes(), &index); err != nil || index.Target != expected.RunID {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrUnavailable
	}
	var selected *struct {
		ArtifactID  string `json:"artifact_id"`
		RunID       string `json:"run_id"`
		Type        string `json:"type"`
		URI         string `json:"uri"`
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		Digest      string `json:"digest"`
		SizeBytes   string `json:"size_bytes"`
		FetchURL    string `json:"fetch_url"`
	}
	for i := range index.Artifacts {
		artifact := &index.Artifacts[i]
		if artifact.ArtifactID == expected.ValidationID+"-result" &&
			artifact.RunID == expected.RunID &&
			artifact.Type == corevalidation.ArtifactType &&
			artifact.Name == expected.ValidationID+".json" {
			if selected != nil {
				return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrArtifactIntegrity
			}
			selected = artifact
		}
	}
	if selected == nil {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrNotFound
	}
	size, err := strconv.ParseInt(selected.SizeBytes, 10, 64)
	if err != nil || size <= 0 || size > maxRDMAArtifactBytes ||
		selected.ContentType != corevalidation.ArtifactContentType ||
		selected.URI != expected.URI || selected.Digest != expected.SHA256 {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrArtifactIntegrity
	}
	fetchURL, err := validatedRDMAFetchURL(selected.FetchURL, scope, expected.RunID, selected.ArtifactID)
	if err != nil {
		return nil, portalvalidation.ArtifactMetadata{}, err
	}
	content := s.invokeTrustedStellar(ctx, request, fetchURL, maxRDMAArtifactBytes)
	if content.tooLarge {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrArtifactIntegrity
	}
	if content.status == http.StatusNotFound {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrNotFound
	}
	if content.status != http.StatusOK {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrUnavailable
	}
	if int64(content.body.Len()) != size {
		return nil, portalvalidation.ArtifactMetadata{}, portalvalidation.ErrArtifactIntegrity
	}
	metadata := expected
	metadata.ContentType = selected.ContentType
	metadata.SizeBytes = size
	return append([]byte(nil), content.body.Bytes()...), metadata, nil
}

func (s *Server) invokeTrustedStellar(
	ctx context.Context,
	request *http.Request,
	path string,
	limit int,
) *limitedInternalResponse {
	internal, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		response := newLimitedInternalResponse(limit)
		response.status = http.StatusBadGateway
		return response
	}
	for _, header := range []string{s.identity.UserHeader, s.identity.GroupsHeader} {
		if value := request.Header.Get(header); value != "" {
			internal.Header.Set(header, value)
		}
	}
	response := newLimitedInternalResponse(limit)
	s.workspaceAwareStellar(s.stellar.Handler()).ServeHTTP(response, internal)
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response
}

func validatedRDMAFetchURL(raw string, scope WorkspaceScope, runID, artifactID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawPath != "" {
		return "", portalvalidation.ErrArtifactIntegrity
	}
	switch parsed.Path {
	case "/api/stellar/artifact", "/api/v1/stellar/artifact", "/api/v2/stellar/artifact":
	default:
		return "", portalvalidation.ErrArtifactIntegrity
	}
	if parsed.Query().Get("target") != runID || parsed.Query().Get("artifact") != artifactID {
		return "", portalvalidation.ErrArtifactIntegrity
	}
	query := workspaceQuery(nil, parsed.Query(), scope.WorkspaceID, scope.Source)
	for _, key := range []string{"target", "artifact", "workspace"} {
		if len(query[key]) != 1 {
			return "", fmt.Errorf("%w: duplicate %s query", portalvalidation.ErrArtifactIntegrity, key)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
