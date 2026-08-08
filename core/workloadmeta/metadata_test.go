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
		AnnotationWorkspaceID:         "sample",
		AnnotationResultScope:         "/data/runs",
		AnnotationExperimentSource:    "tau",
		AnnotationStellarExperimentID: "experiment:exact",
		AnnotationStellarProject:      "vision",
		// Carried on the workload but deliberately not correlated onto pods.
		AnnotationResultPath: "/data/runs/result.json",
		// Not a Tau key at all.
		"kueue.x-k8s.io/podset-required-topology": "kubernetes.io/hostname",
		// Empty values are dropped even when the key does match.
		AnnotationStellarExperimentTitle: "",
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

func TestSourceBundleProvenanceAnnotationKeys(t *testing.T) {
	got := map[string]string{
		"digest": AnnotationSourceBundleDigest,
		"pvc":    AnnotationSourceBundlePVC,
		"path":   AnnotationSourceBundlePath,
	}
	want := map[string]string{
		"digest": Domain + "source-bundle-digest",
		"pvc":    Domain + "source-bundle-pvc",
		"path":   Domain + "source-bundle-path",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source bundle annotations = %#v, want %#v", got, want)
	}
}
