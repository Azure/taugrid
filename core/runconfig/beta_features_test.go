// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBetaFeaturesAcceptOnlyUniqueMultiKueueAcknowledgement(t *testing.T) {
	for _, managed := range []bool{false, true} {
		prefix := ""
		if managed {
			prefix = "schema_version: 1\n"
		}
		cfg, err := parse([]byte(prefix+`name: beta-run
execution:
  beta_features: [multikueue]
`), "tau.yaml")
		if err != nil {
			t.Fatalf("managed=%t valid acknowledgement: %v", managed, err)
		}
		if len(cfg.Execution.BetaFeatures) != 1 ||
			cfg.Execution.BetaFeatures[0] != BetaFeatureMultiKueue {
			t.Fatalf("managed=%t beta features = %#v", managed, cfg.Execution.BetaFeatures)
		}
	}

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "unknown",
			raw:  "execution:\n  beta_features: [future]\n",
			want: "unsupported",
		},
		{
			name: "duplicate",
			raw:  "execution:\n  beta_features: [multikueue, multikueue]\n",
			want: "duplicate",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parse([]byte(test.raw), "tau.yaml")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestBetaFeaturesSchemaIsTypedEnumList(t *testing.T) {
	data, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	execution := properties["execution"].(map[string]any)
	executionProperties := execution["properties"].(map[string]any)
	betaFeatures := executionProperties["beta_features"].(map[string]any)
	if betaFeatures["type"] != "array" {
		t.Fatalf("execution.beta_features schema = %#v", betaFeatures)
	}
	items := betaFeatures["items"].(map[string]any)
	enum, ok := items["enum"].([]any)
	if !ok || len(enum) != 1 || enum[0] != string(BetaFeatureMultiKueue) {
		t.Fatalf("execution.beta_features items = %#v", items)
	}
	if _, hasArrayEnum := betaFeatures["enum"]; hasArrayEnum {
		t.Fatalf("execution.beta_features incorrectly puts enum on array: %#v", betaFeatures)
	}
}

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
