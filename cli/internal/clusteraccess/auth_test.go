// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"context"
	"testing"
)

func TestUserCredentialFactoryFallsBackWithoutAzureCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, mode := range []string{AuthModeInteractive, AuthModeDeviceCode} {
		t.Run(mode, func(t *testing.T) {
			result, err := (UserCredentialFactory{Mode: mode}).Credential(
				context.Background(),
				"11111111-1111-1111-1111-111111111111",
			)
			if err != nil {
				t.Fatalf("Credential: %v", err)
			}
			if result.Token == nil || result.KubeloginMode != mode {
				t.Fatalf("credential result = %#v, want kubelogin mode %q", result, mode)
			}
		})
	}
}

func TestUserCredentialFactoryDefaultsToInteractiveWithoutAzureCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	result, err := (UserCredentialFactory{}).Credential(
		context.Background(),
		"11111111-1111-1111-1111-111111111111",
	)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if result.Token == nil || result.KubeloginMode != AuthModeInteractive {
		t.Fatalf("credential result = %#v, want interactive fallback", result)
	}
}
