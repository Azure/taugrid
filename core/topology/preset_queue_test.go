package topology

import (
	"strings"
	"testing"
)

func queuePreset() ResolvedPreset {
	return ResolvedPreset{
		Preset: Preset{
			Name:      "azure.research.training.l",
			Namespace: "training-ns",
		},
		Options: Options{
			Team:      "research",
			Lane:      "training",
			QueueName: "research-training",
		},
	}
}

func TestReconcilePresetQueueOverrideNoopsWhenQueueMatchesEffectiveTeam(t *testing.T) {
	preset := queuePreset()
	got, err := ReconcilePresetQueueOverride(preset, "research-burst", "research", "training", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "research" || got.Warning != "" {
		t.Fatalf("result = %+v, want unchanged team with no warning", got)
	}
}

func TestReconcilePresetQueueOverrideRejectsExplicitTeamConflict(t *testing.T) {
	preset := queuePreset()
	_, err := ReconcilePresetQueueOverride(preset, "sample-training", "research", "training", true, true)
	if err == nil {
		t.Fatal("expected explicit team conflict")
	}
	want := `--queue="sample-training" conflicts with --team="research"; queue overrides must keep the Kueue LocalQueue and team intent consistent`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestReconcilePresetQueueOverrideInfersTeamFromQueue(t *testing.T) {
	preset := queuePreset()
	got, err := ReconcilePresetQueueOverride(preset, "sample-training", "research", "training", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "sample" {
		t.Fatalf("team = %q, want sample", got.Team)
	}
	want := `warning: --queue="sample-training" overrides preset azure.research.training.l queue "research-training"; inferred --team=sample so the Kueue LocalQueue and team intent stay consistent`
	if got.Warning != want {
		t.Fatalf("warning = %q, want %q", got.Warning, want)
	}
}

func TestReconcilePresetQueueOverrideRejectsUnownedQueue(t *testing.T) {
	preset := queuePreset()
	_, err := ReconcilePresetQueueOverride(preset, "sample", "research", "training", true, false)
	if err == nil {
		t.Fatal("expected unowned queue conflict")
	}
	want := `--queue="sample" overrides preset azure.research.training.l queue "research-training" but leaves team="research" from the preset; pass --team for the queue owner so Kueue LocalQueue and team intent stay consistent`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestPresetLocalQueueNamespaceFallsBackToPresetThenDefaultTeamNamespace(t *testing.T) {
	preset := queuePreset()
	if got := PresetLocalQueueNamespace("ray", preset); got != "ray" {
		t.Fatalf("namespace = %q, want ray", got)
	}
	if got := PresetLocalQueueNamespace("", preset); got != "training-ns" {
		t.Fatalf("namespace = %q, want training-ns", got)
	}
	preset.Preset.Namespace = ""
	if got := PresetLocalQueueNamespace("", preset); got != DefaultLocalQueueNamespace {
		t.Fatalf("namespace = %q, want %q", got, DefaultLocalQueueNamespace)
	}
}

func TestMissingPresetLocalQueueErrorNamesPresetOrOverride(t *testing.T) {
	preset := queuePreset()
	err := MissingPresetLocalQueueError(preset, "ray", "research-training", "not found")
	for _, want := range []string{"azure.research.training.l", `LocalQueue "research-training"`, `namespace "ray"`, "ask the platform owner to validate preset azure.research.training.l"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preset error missing %q: %v", want, err)
		}
	}
	overrideErr := MissingPresetLocalQueueError(preset, "ray", "sample-training", "not found")
	for _, want := range []string{"--queue override", `LocalQueue "sample-training"`, "azure.research.training.l"} {
		if !strings.Contains(overrideErr.Error(), want) {
			t.Fatalf("override error missing %q: %v", want, overrideErr)
		}
	}
	if strings.Contains(overrideErr.Error(), "targets LocalQueue") {
		t.Fatalf("override error should not blame the preset target: %v", overrideErr)
	}
}
