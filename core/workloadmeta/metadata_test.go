// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadmeta

import (
	"reflect"
	"testing"
)

func TestStampWorkspaceWritesCanonicalLabel(t *testing.T) {
	labels := StampWorkspace(nil, "sample")
	if labels[LabelWorkspace] != "sample" {
		t.Fatalf("StampWorkspace() = %#v, want canonical workspace label", labels)
	}
}

func TestPodCorrelationAnnotations(t *testing.T) {
	got := PodCorrelationAnnotations(map[string]string{
		AnnotationWorkspaceID:                     "sample",
		AnnotationResultScope:                     "/data/runs",
		AnnotationExperimentSource:                "tau",
		AnnotationStellarExperimentID:             "experiment:exact",
		AnnotationStellarProject:                  "vision",
		AnnotationResultPath:                      "/data/runs/result.json",
		"kueue.x-k8s.io/podset-required-topology": "kubernetes.io/hostname",
		AnnotationStellarExperimentTitle:          "",
	})
	want := map[string]string{
		AnnotationWorkspaceID:         "sample",
		AnnotationResultScope:         "/data/runs",
		AnnotationExperimentSource:    "tau",
		AnnotationStellarExperimentID: "experiment:exact",
		AnnotationStellarProject:      "vision",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PodCorrelationAnnotations() = %#v, want %#v", got, want)
	}
}
