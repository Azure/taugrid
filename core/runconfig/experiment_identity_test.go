package runconfig

import "testing"

// The experiment axis used to be defined as the run group, so a config had no
// way to say the two differ. These cases pin the separation: a named
// experiment spans its arms, and an unnamed one still falls back to the group
// so pre-existing configs keep their identity.
func TestExperimentRunMetadataSeparatesExperimentFromGroup(t *testing.T) {
	for _, tc := range []struct {
		name           string
		in             Experiment
		wantExperiment string
		wantGroup      string
	}{
		{
			name:           "named experiment spans its arms",
			in:             Experiment{Project: "project-alpha", Name: "candidate-training", Group: "reference-group"},
			wantExperiment: "candidate-training",
			wantGroup:      "reference-group",
		},
		{
			name:           "sibling arm resolves to the same experiment",
			in:             Experiment{Project: "project-alpha", Name: "candidate-training", Group: "candidate-group"},
			wantExperiment: "candidate-training",
			wantGroup:      "candidate-group",
		},
		{
			name:           "unnamed experiment falls back to the group",
			in:             Experiment{Project: "project-alpha", Group: "reference-group"},
			wantExperiment: "reference-group",
			wantGroup:      "reference-group",
		},
		{
			name:           "legacy title becomes the experiment identity",
			in:             Experiment{Project: "project-alpha", Title: "Candidate training API Surface"},
			wantExperiment: "candidate-training-api-surface",
		},
		{
			name:           "legacy title remains above the group arm",
			in:             Experiment{Project: "project-alpha", Title: "Candidate training API Surface", Group: "reference-group"},
			wantExperiment: "candidate-training-api-surface",
			wantGroup:      "reference-group",
		},
		{
			name:           "whitespace is not an experiment name",
			in:             Experiment{Project: "project-alpha", Name: "   ", Group: "reference-group"},
			wantExperiment: "reference-group",
			wantGroup:      "reference-group",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := ExperimentRunMetadata(tc.in)
			if meta.ExperimentID != tc.wantExperiment {
				t.Fatalf("ExperimentID = %q, want %q", meta.ExperimentID, tc.wantExperiment)
			}
			if meta.RunGroupID != tc.wantGroup {
				t.Fatalf("RunGroupID = %q, want %q", meta.RunGroupID, tc.wantGroup)
			}
			if got := meta.StellarMetadata().ExperimentID; got != tc.wantExperiment {
				t.Fatalf("StellarMetadata().ExperimentID = %q, want %q", got, tc.wantExperiment)
			}
		})
	}
}
