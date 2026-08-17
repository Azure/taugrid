// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"
)

// The CRDs shipped by the tau-core-controller Helm chart are hand-maintained
// byte-for-byte copies of the controller-gen output under config/crd/bases.
// Nothing regenerates or syncs them, so a field added to the API types reaches
// the chart only if someone remembers to copy the file. When they do not, the
// installed CRD silently prunes the new field and the feature fails in a way
// that looks like the API server ignoring valid input.
//
// This test is the missing sync check.
func TestChartCRDsMatchGeneratedCRDs(t *testing.T) {
	const (
		generatedDir = "../../config/crd/bases"
		chartDir     = "../../../../charts/tau-core-controller/crds"
	)

	entries, err := os.ReadDir(generatedDir)
	if err != nil {
		t.Fatalf("read generated CRD dir: %v", err)
	}

	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		seen++
		generated, err := os.ReadFile(filepath.Join(generatedDir, entry.Name()))
		if err != nil {
			t.Fatalf("read generated %s: %v", entry.Name(), err)
		}
		chart, err := os.ReadFile(filepath.Join(chartDir, entry.Name()))
		if err != nil {
			t.Fatalf("chart is missing CRD %s: %v\nrun: cp %s/%s %s/%s",
				entry.Name(), err, generatedDir, entry.Name(), chartDir, entry.Name())
		}
		if string(generated) != string(chart) {
			t.Errorf("chart CRD %s has drifted from controller-gen output.\nrun: make manifests && cp %s/%s %s/%s",
				entry.Name(), generatedDir, entry.Name(), chartDir, entry.Name())
		}
	}

	if seen == 0 {
		t.Fatalf("no generated CRDs found in %s; the sync check would silently pass", generatedDir)
	}
}
