// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package raylogoffload

import "testing"

func TestHeadPodAnnotationsAddsContainerLogDestination(t *testing.T) {
	t.Parallel()

	base := map[string]string{"existing": "value"}
	got := HeadPodAnnotations(base)

	if got[AnnotationKey] != AnnotationValue {
		t.Fatalf("annotation = %q, want %q", got[AnnotationKey], AnnotationValue)
	}
	if got["existing"] != "value" {
		t.Fatalf("existing annotation = %q, want value", got["existing"])
	}
	if _, ok := base[AnnotationKey]; ok {
		t.Fatalf("expected base map to remain unchanged, got %#v", base)
	}
}

func TestHeadPodAnnotationsPreservesExistingLogDestination(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		AnnotationKey: "Logs:CustomDestination",
		"existing":    "value",
	}
	got := HeadPodAnnotations(base)

	if got[AnnotationKey] != "Logs:CustomDestination" {
		t.Fatalf("annotation = %q, want existing destination", got[AnnotationKey])
	}
	if base[AnnotationKey] != "Logs:CustomDestination" {
		t.Fatalf("expected base map to remain unchanged, got %#v", base)
	}
}

func TestPrepareInitContainerPreparesSharedRayTmpVolume(t *testing.T) {
	t.Parallel()

	got := PrepareInitContainer("example.com/ray:test")
	if got["name"] != PrepareInitName {
		t.Fatalf("name=%v want %s", got["name"], PrepareInitName)
	}
	if got["image"] != "example.com/ray:test" {
		t.Fatalf("image=%v", got["image"])
	}
	if got["command"] == nil {
		t.Fatal("expected prepare command")
	}
	securityContext, ok := got["securityContext"].(map[string]any)
	if !ok {
		t.Fatalf("security context = %#v", got["securityContext"])
	}
	if securityContext["runAsUser"] != int64(0) || securityContext["runAsGroup"] != int64(0) {
		t.Fatalf("prepare init should run as root: %v", securityContext)
	}
}
