// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"strings"
	"testing"
)

func TestDirectConfigReplacesTopologyPolicyWithWorkloadProfileSnapshot(t *testing.T) {
	cfg, err := parse([]byte(`policy:
  workload_profile_snapshot: profiles.yaml
`), "tau.yaml")
	if err != nil {
		t.Fatalf("new snapshot field: %v", err)
	}
	if cfg.Policy.WorkloadProfileSnapshot != "profiles.yaml" {
		t.Fatalf("snapshot = %q", cfg.Policy.WorkloadProfileSnapshot)
	}

	_, err = parse([]byte(`policy:
  topology_policy: legacy.yaml
`), "tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "topology_policy") {
		t.Fatalf("legacy topology_policy error = %v", err)
	}
}
