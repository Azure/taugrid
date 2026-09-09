// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

// searchAutoSources keeps discovery fallback and partial failure reporting
// consistent. A failed source is never represented as a successful empty one.
func searchAutoSources[T any](ctx context.Context, hasKusto bool, kind string, localSearch, kustoSearch func() (T, error), merge func(T, T) T) (T, []string, error) {
	local, localErr := localSearch()
	if err := ctx.Err(); err != nil {
		return local, nil, err
	}
	if !hasKusto {
		return local, nil, localErr
	}
	kusto, kustoErr := kustoSearch()
	if err := ctx.Err(); err != nil {
		return local, nil, err
	}
	if localErr != nil {
		if kustoErr != nil {
			return local, nil, errors.Join(fmt.Errorf("local %s search: %w", kind, localErr), fmt.Errorf("Kusto %s search: %w", kind, kustoErr))
		}
		return kusto, []string{fmt.Sprintf("source=auto fell back to Kusto because local %s search failed: %v", kind, localErr)}, nil
	}
	if kustoErr != nil {
		return local, []string{fmt.Sprintf("source=auto skipped Kusto %s search because it failed: %v", kind, kustoErr)}, nil
	}
	return merge(local, kusto), nil, nil
}

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
	localSearch := func() (runSearchResponse, error) {
		store, err := expstore.Open(ctx, s.storeRoot)
		if err != nil {
			return runSearchResponse{}, err
		}
		defer store.Close()
		result, err := store.SearchRuns(ctx, opts)
		return withRunSource(result, "local"), err
	}
	kustoSearch := func() (runSearchResponse, error) {
		result, err := s.baseKustoSource().SearchRuns(ctx, opts)
		return withRunSource(result, "kusto"), err
	}
	switch source {
	case "local":
		return localSearch()
	case "kusto":
		return kustoSearch()
	case "auto":
		result, warnings, err := searchAutoSources(ctx, s.hasKustoSource(), "run", localSearch, kustoSearch,
			func(local, kusto runSearchResponse) runSearchResponse {
				return mergeRunSearchResults(local, kusto, opts.Limit)
			})
		result.Warnings = append(result.Warnings, warnings...)
		return result, err
	default:
		return runSearchResponse{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func mergeRunSearchResults(local, kusto runSearchResponse, limit int) runSearchResponse {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	runs := append([]sourcedRun{}, local.Runs...)
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		seen[run.RunID] = true
	}
	added, duplicates := 0, 0
	for _, run := range kusto.Runs {
		if seen[run.RunID] {
			duplicates++
			continue
		}
		seen[run.RunID] = true
		runs = append(runs, run)
		added++
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].CreatedAt != runs[j].CreatedAt {
			return runs[i].CreatedAt > runs[j].CreatedAt
		}
		return runs[i].RunID < runs[j].RunID
	})
	total := len(runs)
	truncated := local.Truncated || kusto.Truncated || total > limit
	if total > limit {
		runs = runs[:limit]
	}
	warnings := append([]string{}, local.Warnings...)
	warnings = append(warnings, kusto.Warnings...)
	if added > 0 {
		warnings = append(warnings, fmt.Sprintf("source=auto merged %d Kusto-backed runs with local expstore runs", added))
	}
	if duplicates > 0 {
		warnings = append(warnings, fmt.Sprintf("source=auto kept local records for %d duplicate Kusto run IDs", duplicates))
	}
	if local.Truncated || kusto.Truncated {
		warnings = append(warnings, "source=auto search is limited to the returned source windows; narrow the filters or increase limit (maximum 1000)")
	}
	return runSearchResponse{
		RunSearchResult: expstore.RunSearchResult{
			SchemaVersion: expstore.RunSearchSchemaVersion,
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     local.StorePath,
			Target:        local.Target,
			Total:         total,
			Truncated:     truncated,
			Warnings:      warnings,
		},
		Runs: runs,
	}
}
