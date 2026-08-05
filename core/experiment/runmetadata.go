package experiment

import "context"

// RunMetadata is the experiment identity a run carries from its config into
// every command that records or labels it.
//
// It lives here rather than in either CLI because both products read it: the
// tau CLI attaches it when submitting a run, and taugrid-portal reads the same
// fields back out of a run config when defaulting experiment flags. Two copies
// of this struct would drift silently, and the drift would only surface as
// runs landing under the wrong project or experiment.
type RunMetadata struct {
	RunID string
	// Workspace, Project, ExperimentID and RunID are the four levels of the
	// identity hierarchy. RunGroupID is not a level: it labels an arm inside
	// an experiment (baseline vs ablation) so the arms stay comparable.
	Workspace    string
	Project      string
	ExperimentID string
	RunGroupID   string
	Tags         map[string]string
}

// Empty reports whether the metadata carries nothing worth recording.
func (m RunMetadata) Empty() bool {
	return m.RunID == "" && m.Workspace == "" && m.Project == "" &&
		m.ExperimentID == "" && m.RunGroupID == "" && len(m.Tags) == 0
}

// StellarMetadata projects the run identity onto the Stellar capture contract.
func (m RunMetadata) StellarMetadata() StellarMetadata {
	// Fall back to the group only when no experiment is named. This used to be
	// an unconditional assignment, which made the experiment axis unable to
	// hold anything but the group.
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

// WithRunMetadata attaches run identity to ctx, replacing any existing value.
func WithRunMetadata(ctx context.Context, meta RunMetadata) context.Context {
	return context.WithValue(ctx, runMetadataKey{}, meta)
}

// WithRunMetadataDefaults attaches defaults only when ctx carries no run
// identity yet, so an explicit caller always wins over config-derived values.
func WithRunMetadataDefaults(ctx context.Context, defaults RunMetadata) context.Context {
	if HasRunMetadata(ctx) {
		return ctx
	}
	return WithRunMetadata(ctx, defaults)
}

// RunMetadataFromContext returns the attached run identity, or the zero value.
func RunMetadataFromContext(ctx context.Context) RunMetadata {
	if meta, ok := ctx.Value(runMetadataKey{}).(RunMetadata); ok {
		return meta
	}
	return RunMetadata{}
}

// HasRunMetadata reports whether ctx carries run identity at all, which is
// distinct from carrying empty run identity.
func HasRunMetadata(ctx context.Context) bool {
	_, ok := ctx.Value(runMetadataKey{}).(RunMetadata)
	return ok
}

// IDFromTitle derives the stable experiment identifier Tau uses for a
// human-written title.
func IDFromTitle(title string) string {
	return KubernetesLabelValue(title)
}
