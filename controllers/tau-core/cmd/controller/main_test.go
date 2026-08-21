// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

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
