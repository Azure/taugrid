// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"reflect"
	"testing"
)

func TestRunnerBaseArgsIncludeIsolatedKubeconfig(t *testing.T) {
	runner := NewWithKubeconfig("aks-flex", "/tmp/tau-kubeconfig")
	want := []string{"--kubeconfig", "/tmp/tau-kubeconfig", "--context", "aks-flex"}
	if got := runner.baseArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs = %#v, want %#v", got, want)
	}
}
