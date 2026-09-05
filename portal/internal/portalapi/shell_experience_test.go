// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"testing"

	"github.com/Azure/taugrid/portal/internal/frontendtest"
)

func TestShellExperience(t *testing.T) {
	frontendtest.RunNode(t, "shell_experience_test.mjs")
}
