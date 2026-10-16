// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package experiment

import "context"

type RunMetadata struct {
	RunID        string
	Workspace    string
	Project      string
	ExperimentID string
	RunGroupID   string
	Tags         map[string]string
}

func (m RunMetadata) Empty() bool {
	return m.RunID == "" && m.Workspace == "" && m.Project == "" &&
		m.ExperimentID == "" && m.RunGroupID == "" && len(m.Tags) == 0
}

func (m RunMetadata) StellarMetadata() StellarMetadata {
	experimentID := m.ExperimentID
	if experimentID == "" {
		experimentID = m.RunGroupID
	}
	return StellarMetadata{
		Workspace:    m.Workspace,
		Project:      m.Project,
		ExperimentID: experimentID,
		RunGroupID:   m.RunGroupID,
		Tags:         m.Tags,
	}
}

type runMetadataKey struct{}

func WithRunMetadata(ctx context.Context, meta RunMetadata) context.Context {
	return context.WithValue(ctx, runMetadataKey{}, meta)
}

func WithRunMetadataDefaults(ctx context.Context, defaults RunMetadata) context.Context {
	if HasRunMetadata(ctx) {
		return ctx
	}
	return WithRunMetadata(ctx, defaults)
}

func RunMetadataFromContext(ctx context.Context) RunMetadata {
	if meta, ok := ctx.Value(runMetadataKey{}).(RunMetadata); ok {
		return meta
	}
	return RunMetadata{}
}

func HasRunMetadata(ctx context.Context) bool {
	_, ok := ctx.Value(runMetadataKey{}).(RunMetadata)
	return ok
}

func IDFromTitle(title string) string {
	return KubernetesLabelValue(title)
}
