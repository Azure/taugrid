// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package podsecurity

import "testing"

func TestApplyRestrictedCoversAllContainers(t *testing.T) {
	pod := map[string]any{
		"initContainers": []any{map[string]any{"name": "init"}},
		"containers": []any{
			map[string]any{"name": "main"},
			map[string]any{"name": "sidecar", "securityContext": map[string]any{"readOnlyRootFilesystem": true}},
		},
	}
	group := int64(65532)
	if err := ApplyRestricted(pod, &group); err != nil {
		t.Fatal(err)
	}
	if pod["securityContext"].(map[string]any)["fsGroup"] != group {
		t.Fatalf("pod securityContext = %v", pod["securityContext"])
	}
	for _, field := range []string{"initContainers", "containers"} {
		for _, raw := range pod[field].([]any) {
			context := raw.(map[string]any)["securityContext"].(map[string]any)
			if context["runAsNonRoot"] != true || context["allowPrivilegeEscalation"] != false {
				t.Fatalf("%s securityContext = %v", field, context)
			}
			if context["runAsUser"] != defaultRestrictedID || context["runAsGroup"] != defaultRestrictedID {
				t.Fatalf("%s numeric identity = %v", field, context)
			}
			if context["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
				t.Fatalf("%s seccompProfile = %v", field, context["seccompProfile"])
			}
			drop := context["capabilities"].(map[string]any)["drop"].([]any)
			if len(drop) != 1 || drop[0] != "ALL" {
				t.Fatalf("%s capabilities = %v", field, context["capabilities"])
			}
		}
	}
}

func TestApplyRestrictedPreservesExplicitNonRootIdentity(t *testing.T) {
	pod := map[string]any{
		"containers": []any{map[string]any{
			"name": "main",
			"securityContext": map[string]any{
				"runAsUser":  int64(1000),
				"runAsGroup": int64(2000),
			},
		}},
	}
	if err := ApplyRestricted(pod, nil); err != nil {
		t.Fatal(err)
	}
	context := pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
	if context["runAsUser"] != int64(1000) || context["runAsGroup"] != int64(2000) {
		t.Fatalf("explicit identity was replaced: %v", context)
	}
}

func TestApplyRestrictedRejectsRootContainer(t *testing.T) {
	pod := map[string]any{
		"containers": []any{map[string]any{
			"name":            "root",
			"securityContext": map[string]any{"runAsUser": int64(0)},
		}},
	}
	if err := ApplyRestricted(pod, nil); err == nil {
		t.Fatal("expected root container rejection")
	}
}

func TestApplyRestrictedDoesNotMutateInputSecurityContext(t *testing.T) {
	securityContext := map[string]any{
		"readOnlyRootFilesystem": true,
		"capabilities":           map[string]any{"drop": []any{"NET_RAW"}},
	}
	pod := map[string]any{
		"containers": []any{map[string]any{
			"name":            "main",
			"securityContext": securityContext,
		}},
	}
	if err := ApplyRestricted(pod, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := securityContext["runAsNonRoot"]; exists {
		t.Fatalf("input securityContext was mutated: %v", securityContext)
	}
	drop := securityContext["capabilities"].(map[string]any)["drop"].([]any)
	if len(drop) != 1 || drop[0] != "NET_RAW" {
		t.Fatalf("input capabilities were mutated: %v", securityContext["capabilities"])
	}
}
