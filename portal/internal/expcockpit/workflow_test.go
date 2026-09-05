// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"testing"

	"github.com/Azure/taugrid/portal/internal/frontendtest"
)

func TestFrontendResearchWorkflow(t *testing.T) {
	frontendtest.RunNode(t, "workflow_test.mjs")
}
