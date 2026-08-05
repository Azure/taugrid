// Package expcapture maps status run profiles into experiment store records.
//
// It exists so that the status package can project a run without importing the
// experiment store: status produces a storage-neutral status.RunProfileRecord,
// and this package translates it into the store's record types.
package expcapture

import (
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

// RunData projects a status snapshot into experiment store records.
func RunData(s status.Snapshot, c status.CostProfile, opts status.ExperimentRunDataOptions) (expstore.RecordRunDataOptions, error) {
	profile, err := status.ExperimentRunProfile(s, c, opts)
	if err != nil {
		return expstore.RecordRunDataOptions{}, err
	}
	return FromRunProfile(profile), nil
}

// FromRunProfile converts a storage-neutral run profile into store records.
func FromRunProfile(p status.RunProfileRecord) expstore.RecordRunDataOptions {
	run := expstore.RunRecord{
		RunID:       p.RunID,
		Project:     p.Project,
		RunGroupID:  p.RunGroupID,
		State:       p.State,
		Owner:       p.Owner,
		CreatedAt:   p.CreatedAt,
		StartedAt:   p.StartedAt,
		CompletedAt: p.CompletedAt,
		ConfigHash:  p.ConfigHash,
		CodeSHA:     p.CodeSHA,
		ImageDigest: p.ImageDigest,
		TauCommand:  p.TauCommand,
		ResultURI:   p.ResultURI,
	}
	runContext := &expstore.RunContextRecord{
		RunID:            p.RunID,
		Cluster:          p.Cluster,
		Namespace:        p.Namespace,
		Team:             p.Team,
		Profile:          p.Profile,
		Lane:             p.Lane,
		LocalQueue:       p.LocalQueue,
		KueueWorkload:    p.KueueWorkload,
		PodUID:           p.PodUID,
		ResourceClaims:   p.ResourceClaims,
		GPUClass:         p.GPUClass,
		GPUCount:         p.GPUCount,
		NodeNames:        p.NodeNames,
		Mounts:           p.Mounts,
		QueueWaitSeconds: p.QueueWaitSeconds,
		GPUHours:         p.GPUHours,
		EstimatedCost:    p.EstimatedCost,
	}
	return expstore.RecordRunDataOptions{
		Run:        run,
		RunContext: runContext,
		Tags: []expstore.TagRecord{
			{ScopeType: "run", ScopeID: p.RunID, Key: "tau.capture.source", Value: p.CaptureSource},
		},
		Command: "exp capture run-profile",
	}
}
