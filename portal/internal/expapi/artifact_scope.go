// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func (s *Server) buildArtifactSnapshot(ctx context.Context, r *http.Request, target string) (expcockpit.Snapshot, error) {
	snapshot, handled, err := s.localArtifactRunSnapshot(ctx, r, target)
	if !handled {
		snapshot, err = s.buildSnapshot(ctx, r, target, "")
	}
	if err != nil {
		return snapshot, err
	}
	if restricted, _ := r.Context().Value(workspaceRoutePolicyKey{}).(bool); restricted {
		exactRun := false
		for _, run := range snapshot.Runs {
			if snapshot.TargetType == "run" && run.RunID == target {
				exactRun = true
				break
			}
		}
		if !exactRun {
			return expcockpit.Snapshot{}, expstore.ErrNotFound
		}
	}
	if handled {
		return snapshot, nil
	}
	// Cockpit snapshots deliberately include experiment-wide comparison runs.
	// Media authorization must additionally bind those scoped runs to the target.
	allowed := make(map[string]bool)
	runs := snapshot.Runs[:0]
	for _, run := range snapshot.Runs {
		switch snapshot.TargetType {
		case "run":
			if run.RunID != target {
				continue
			}
		case "run_group":
			if snapshot.Status.RunGroup == nil || run.RunGroupID != snapshot.Status.RunGroup.RunGroupID {
				continue
			}
		}
		allowed[run.RunID] = true
		runs = append(runs, run)
	}
	if len(allowed) == 0 {
		return expcockpit.Snapshot{}, expstore.ErrNotFound
	}
	snapshot.Runs = runs
	artifacts := snapshot.Artifacts[:0]
	for _, artifact := range snapshot.Artifacts {
		if allowed[artifact.RunID] {
			artifacts = append(artifacts, artifact)
		}
	}
	snapshot.Artifacts = artifacts
	return snapshot, nil
}

func (s *Server) localArtifactRunSnapshot(ctx context.Context, r *http.Request, target string) (expcockpit.Snapshot, bool, error) {
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		return expcockpit.Snapshot{}, true, err
	}
	if source == "" {
		source = s.source
	}
	if source == "kusto" {
		return expcockpit.Snapshot{}, false, nil
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		return expcockpit.Snapshot{}, true, err
	}
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		return expcockpit.Snapshot{}, source != "auto" || !errors.Is(err, expstore.ErrNotFound), err
	}
	defer store.Close()
	candidate := strings.TrimSpace(r.URL.Query().Get("run"))
	if candidate == "" {
		candidate = target
	}
	// SearchRuns applies exact target and fixed-workspace predicates before its
	// limit. Comparison snapshots instead expand targets and truncate first.
	result, err := store.SearchRuns(ctx, expstore.RunSearchOptions{
		Target: candidate, Workspace: workspace, Project: strings.TrimSpace(r.URL.Query().Get("project")), Limit: 1,
	})
	if err != nil {
		return expcockpit.Snapshot{}, source != "auto" || !errors.Is(err, expstore.ErrNotFound), err
	}
	if len(result.Runs) == 0 {
		return expcockpit.Snapshot{}, true, expstore.ErrNotFound
	}
	run := result.Runs[0]
	if run.RunID != candidate {
		return expcockpit.Snapshot{}, false, nil
	}
	if target != run.RunID {
		status, err := store.Status(ctx, target)
		if err != nil {
			return expcockpit.Snapshot{}, true, err
		}
		matches := (status.TargetType == "experiment" && status.Experiment != nil && status.Experiment.ExperimentID == run.ExperimentID) ||
			(status.TargetType == "run_group" && status.RunGroup != nil && status.RunGroup.RunGroupID == run.RunGroupID)
		if !matches {
			return expcockpit.Snapshot{}, true, expstore.ErrNotFound
		}
	}
	// A direct artifact lookup is safe only after exact run ownership and any
	// enclosing experiment/group membership have both been established.
	records, err := store.ArtifactsForRun(ctx, run.RunID)
	if err != nil {
		return expcockpit.Snapshot{}, true, err
	}
	artifacts := make([]expcockpit.ArtifactView, 0, len(records))
	for _, record := range records {
		view := expcockpit.ArtifactView{
			ArtifactID: record.ArtifactID, RunID: record.RunID, Type: record.Type, URI: record.URI, Name: record.Name,
			DurableRef: record.DurableRef, ContentType: record.ContentType, Digest: record.Digest,
			Tags: record.Tags, CreatedAt: record.CreatedAt, Preview: record.Preview, ExternalRef: record.ExternalRef,
			Caption: record.Caption, Direction: record.Direction, Alias: record.Alias,
			SourceArtifactID: record.SourceArtifactID, SourceRunID: record.SourceRunID,
			SourceDatasetName: record.SourceDatasetName, SourceDatasetVersion: record.SourceDatasetVersion, SourceDatasetDigest: record.SourceDatasetDigest,
		}
		if record.SizeBytes != nil {
			view.SizeBytes = strconv.FormatInt(*record.SizeBytes, 10)
		}
		if record.Step != nil {
			view.Step = strconv.FormatInt(*record.Step, 10)
		}
		if record.Rank != nil {
			view.Rank = strconv.FormatInt(*record.Rank, 10)
		}
		artifacts = append(artifacts, view)
	}
	return expcockpit.Snapshot{
		Target: target, TargetType: "run",
		Runs:      []expcockpit.RunView{{RunID: run.RunID, RunGroupID: run.RunGroupID}},
		Artifacts: artifacts,
	}, true, nil
}
