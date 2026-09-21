// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portal_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
)

func TestPortalLiveADXOptIn(t *testing.T) {
	for _, sample := range []struct {
		name   string
		global string
		portal string
		fail   bool
	}{
		{name: "cluster-only", global: "1"},
		{name: "explicit-missing-config", global: "1", portal: "1", fail: true},
		{name: "offline", global: "0", portal: "1"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPortalLiveADXBrowser$", "-test.v")
			command.Env = append(portalEnvironment(), "AI_RUNTIME_E2E="+sample.global, "TAU_PORTAL_LIVE_E2E="+sample.portal)
			output, err := command.CombinedOutput()
			if sample.fail {
				require.Error(t, err, "%s", output)
				require.Contains(t, string(output), "TAU_PORTAL_ADX_ENDPOINT is required")
			} else {
				require.NoError(t, err, "%s", output)
				require.Contains(t, string(output), "--- SKIP: TestPortalLiveADXBrowser")
			}
		})
	}
}

func TestPortalLiveADXBrowser(t *testing.T) {
	e2e.SkipUnlessE2E(t)
	if os.Getenv("TAU_PORTAL_LIVE_E2E") != "1" {
		t.Skip("set TAU_PORTAL_LIVE_E2E=1 to enable live Portal ADX/browser acceptance")
	}
	for _, name := range []string{
		"TAU_PORTAL_ADX_ENDPOINT", "TAU_PORTAL_ADX_DATABASE", "TAU_PORTAL_WORKSPACE",
		"TAU_PORTAL_CLUSTER", "TAU_PORTAL_REMOTE_WRITE_URL", "TAU_PORTAL_PLAYWRIGHT_MODULE",
		"TAU_PORTAL_CHROMIUM", "TAU_PORTAL_EVIDENCE_DIR",
	} {
		require.NotEmpty(t, os.Getenv(name), "%s is required for live acceptance", name)
	}
	portal := buildPortal(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "live-adx.mjs", portal.binary)
	output, err := command.CombinedOutput()
	t.Logf("%s", output)
	require.NoError(t, err, "live ADX/browser acceptance failed; see TAU_PORTAL_EVIDENCE_DIR")
}
