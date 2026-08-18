// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"strings"
	"testing"
)

// TauWorkspace.spec.defaults.ttlSecondsAfterFinished reaches the rendered batch
// Job through RenderOptions.TTLSecondsAfterFinished. Zero must reproduce the
// retention the template has always used, so adopting the override is a no-op
// for every workspace that does not set one.

func ttlFixtureManifest(t *testing.T) ([]byte, *Manifest) {
	t.Helper()
	raw := []byte(`
schema_version: 1
name: ttl-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return raw, m
}

func renderWithTTL(t *testing.T, kind string, ttl int64) string {
	t.Helper()
	raw, m := ttlFixtureManifest(t)
	out, err := Render(RenderOptions{
		Manifest:                m,
		ManifestRaw:             raw,
		ManifestFilename:        "ttl-job.yaml",
		Namespace:               "tau-default",
		MainScript:              []byte("# stub SDK wrapper\n"),
		WorkloadKind:            kind,
		TTLSecondsAfterFinished: ttl,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func ttlLines(doc string) []string {
	var out []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "ttlSecondsAfterFinished") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func TestJobTTLSecondsAfterFinishedFallsBackToTemplateDefault(t *testing.T) {
	if got := jobTTLSecondsAfterFinished(0); got != defaultManagedJobTTLSecondsAfterFinished {
		t.Errorf("jobTTLSecondsAfterFinished(0) = %d, want %d", got, defaultManagedJobTTLSecondsAfterFinished)
	}
	// A negative value is not a request to delete immediately; it is nonsense,
	// and must fall back rather than render an invalid Job.
	if got := jobTTLSecondsAfterFinished(-1); got != defaultManagedJobTTLSecondsAfterFinished {
		t.Errorf("jobTTLSecondsAfterFinished(-1) = %d, want the default", got)
	}
	if got := jobTTLSecondsAfterFinished(604800); got != 604800 {
		t.Errorf("jobTTLSecondsAfterFinished(604800) = %d, want 604800", got)
	}
}

func TestRenderedJobCarriesTTLOverride(t *testing.T) {
	got := ttlLines(renderWithTTL(t, WorkloadKindJob, 604800))
	if len(got) != 1 || got[0] != "ttlSecondsAfterFinished: 604800" {
		t.Errorf("rendered Job retention = %v, want the 604800 override", got)
	}
}

func TestRenderedJobKeepsBuiltInTTLWhenUnset(t *testing.T) {
	got := ttlLines(renderWithTTL(t, WorkloadKindJob, 0))
	if len(got) != 1 || got[0] != "ttlSecondsAfterFinished: 86400" {
		t.Errorf("rendered Job retention = %v, want the unchanged built-in 86400", got)
	}
}

// RayJob ttlSecondsAfterFinished controls Ray cluster teardown, not lifecycle
// record retention. A workspace retention default must not stretch a Ray
// cluster's lifetime by a week.
func TestRayJobRetentionIsUnaffectedByTTLOverride(t *testing.T) {
	got := ttlLines(renderWithTTL(t, WorkloadKindRayJob, 604800))
	for _, line := range got {
		if strings.Contains(line, "604800") {
			t.Errorf("workspace TTL leaked into RayJob teardown: %v", got)
		}
	}
}
