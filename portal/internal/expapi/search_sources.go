// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"fmt"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

type sourcedRun struct {
	expstore.RunSearchRun
	Source string `json:"source"`
}

type runSearchResponse struct {
	expstore.RunSearchResult
	Runs []sourcedRun `json:"runs"`
}

func withRunSource(result expstore.RunSearchResult, source string) runSearchResponse {
	out := runSearchResponse{RunSearchResult: result, Runs: make([]sourcedRun, 0, len(result.Runs))}
	for _, run := range result.Runs {
		out.Runs = append(out.Runs, sourcedRun{RunSearchRun: run, Source: source})
	}
	return out
}

func (s *Server) searchRuns(ctx context.Context, source string, opts expstore.RunSearchOptions) (runSearchResponse, error) {
	if source != "local" {
		return runSearchResponse{}, fmt.Errorf("local run search requires source=local")
	}
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		return runSearchResponse{}, err
	}
	defer store.Close()
	result, err := store.SearchRuns(ctx, opts)
	return withRunSource(result, "local"), err
}
