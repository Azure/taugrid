// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAPIKindAvailable(t *testing.T) {
	groupKind := tauv1alpha1.GroupVersion.WithKind(tauv1alpha1.KindTauTeam).GroupKind()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{tauv1alpha1.GroupVersion})

	available, err := apiKindAvailable(mapper, groupKind)
	if err != nil {
		t.Fatalf("apiKindAvailable() error = %v", err)
	}
	if available {
		t.Fatal("apiKindAvailable() = true before registering mapping")
	}

	mapper.Add(tauv1alpha1.GroupVersion.WithKind(tauv1alpha1.KindTauTeam), meta.RESTScopeNamespace)
	available, err = apiKindAvailable(mapper, groupKind)
	if err != nil {
		t.Fatalf("apiKindAvailable() error = %v", err)
	}
	if !available {
		t.Fatal("apiKindAvailable() = false after registering mapping")
	}
}

func TestResolveSystemNamespaceFlag(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		system  string
		legacy  string
		want    string
		wantErr bool
	}{
		{name: "default", want: "tau-system"},
		{name: "system flag", system: "custom-system", want: "custom-system"},
		{name: "legacy alias", legacy: "custom-system", want: "custom-system"},
		{name: "matching aliases", system: "custom-system", legacy: "custom-system", want: "custom-system"},
		{name: "conflict", system: "system-a", legacy: "system-b", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := resolveSystemNamespaceFlag(testCase.system, testCase.legacy)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("resolveSystemNamespaceFlag() error = %v, wantErr %t", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Fatalf("resolveSystemNamespaceFlag() = %q, want %q", got, testCase.want)
			}
		})
	}
}
