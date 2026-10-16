// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func warningsFor(t *testing.T, raw string) []string {
	t.Helper()
	_, warnings, err := parseWithDiagnostics([]byte(raw), "tau.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return warnings
}

func TestManagedConfigWarnsOnNestedTypo(t *testing.T) {
	for input, want := range map[string]string{
		"nodeSelector":  "policy.node_selector",
		"node_selecter": "policy.node_selector",
	} {
		warnings := warningsFor(t, `schema_version: 1
name: nested-typo
run:
  entrypoint: train.py
policy:
  `+input+`:
    example.invalid/gpu-series: nd-h200-v5
`)
		if len(warnings) != 1 {
			t.Fatalf("policy.%s: warnings = %v, want exactly one", input, warnings)
		}
		if !strings.Contains(warnings[0], "policy."+input) {
			t.Errorf("warning does not name the nested key: %q", warnings[0])
		}
		if !strings.Contains(warnings[0], `did you mean "`+want+`"?`) {
			t.Errorf("warning does not suggest %q: %q", want, warnings[0])
		}
	}
}

func TestToleratedNonSchemaKeysStayPinned(t *testing.T) {
	opaqueRoots := 0
	for path, descend := range managedPassthroughPaths {
		if descend {
			continue
		}
		if idx := strings.LastIndex(path, "."); idx >= 0 {
			if parentDescends, ok := managedPassthroughPaths[path[:idx]]; ok && parentDescends {
				continue
			}
		}
		opaqueRoots++
	}
	if opaqueRoots > 20 {
		t.Fatalf("managedPassthroughPaths has %d opaque roots; if it keeps growing, runconfig and manifest should share one schema instead", opaqueRoots)
	}
}

func TestManagedConfigWithOnlyKnownKeysWarnsNothing(t *testing.T) {
	warnings := warningsFor(t, `schema_version: 1
name: known-keys
run:
  entrypoint: train.py
  workload_kind: rayjob
compute:
  gpus: 8
  workers: 2
runtime:
  image: example.invalid/ray:latest
policy:
  profile: research-training
  node_selector:
    example.invalid/gpu-series: nd-h200-v5
storage:
  data_pvc: blob-training
`)
	if len(warnings) != 0 {
		t.Fatalf("valid managed config warned: %v", warnings)
	}
}

func TestManagedConfigWarnsOnMisspelledSchedulingDirective(t *testing.T) {
	warnings := warningsFor(t, `schema_version: 1
name: typo-config
run:
  entrypoint: train.py
compute:
  gpus: 8
scheduling:
  node_selector:
    example.invalid/gpu-series: nd-h200-v5
`)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	got := warnings[0]
	for _, want := range []string{`"scheduling"`, "tau.yaml", "ignored", `did you mean "policy"?`} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q missing %q", got, want)
		}
	}
}

func TestUnrelatedKeyGetsNoMisleadingSuggestion(t *testing.T) {
	if got, ok := suggestFieldPath("quantum_entanglement_settings"); ok {
		t.Fatalf("unrelated key suggested %q; a wrong hint is worse than none", got)
	}
}

func TestFreeFormMapKeysAreNotValidated(t *testing.T) {
	warnings := warningsFor(t, `schema_version: 1
name: free-form
run:
  entrypoint: train.py
policy:
  node_selector:
    definitely-not-a-config-field: value
    another.weird/label: value
runtime:
  env_secret:
    HF_TOKEN: my-secret:token
`)
	if len(warnings) != 0 {
		t.Fatalf("free-form map keys warned: %v", warnings)
	}
}

func looksLikeTauConfig(raw []byte) bool {
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return false
	}
	for _, marker := range []string{"schema_version", "run", "engine"} {
		if _, ok := top[marker]; ok {
			return true
		}
	}
	return false
}

func TestLooksLikeTauConfigPartitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"direct config", "schema_version: 1\nname: x\nrun:\n  engine: job\n", true},
		{"engine at top level", "name: x\nengine: rayjob\nentrypoint: t.py\n", true},
		{"run at top level", "name: x\nrun:\n  engine: job\n", true},
		{"k8s manifest", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n", false},
		{"kind cluster", "apiVersion: kind.x-k8s.io/v1alpha4\nkind: Cluster\n", false},
		{"multi-doc manifest", "---\napiVersion: v1\nkind: Namespace\n---\napiVersion: v1\nkind: Pod\n", false},
		{"not a mapping", "- a\n- b\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeTauConfig([]byte(tc.raw)); got != tc.want {
				t.Fatalf("looksLikeTauConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShippedExampleConfigsWarnNothing(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("examples root missing (%v); if examples moved, repoint this "+
			"guard rather than letting it pass vacuously", err)
	}
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !looksLikeTauConfig(raw) {
			return nil
		}
		checked++
		_, warnings, err := parseWithDiagnostics(raw, path)
		if err != nil {
			t.Errorf("%s failed to parse: %v", path, err)
			return nil
		}
		if len(warnings) != 0 {
			t.Errorf("%s warned: %v", path, warnings)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking examples: %v", err)
	}
	if checked == 0 {
		t.Fatal("no example configs were checked; the guard is vacuous")
	}
}

func TestDirectConfigStillHardErrorsOnUnknownKey(t *testing.T) {
	_, _, err := parseWithDiagnostics([]byte(`schema_version: 1
name: direct-typo
run:
  engine: job
  entrypoint: train.py
scheduling:
  node_selector:
    a: b
`), "tau.yaml")
	if err == nil {
		t.Fatal("direct config with an unknown key must fail, not warn")
	}
	if !strings.Contains(err.Error(), "scheduling") {
		t.Fatalf("error should name the offending key: %v", err)
	}
}

func TestNestedPassthroughTypoWarns(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "rdma enabled typo",
			raw: "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
				"runtime:\n  rdma:\n    enabeld: true\n",
			want: "runtime.rdma.enabeld",
		},
		{
			name: "removed rdma resource name",
			raw: "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
				"runtime:\n  rdma:\n    enabled: true\n    resource_name: rdma/x\n",
			want: "runtime.rdma.resource_name",
		},
		{
			name: "mount readOnly typo",
			raw: "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
				"storage:\n  mounts:\n    - name: a\n      mountPath: /a\n      pvc: p\n      readOlny: true\n",
			want: "storage.mounts.readOlny",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := UnknownKeys([]byte(tc.raw))
			if err != nil {
				t.Fatalf("UnknownKeys: %v", err)
			}
			var got []string
			for _, key := range keys {
				got = append(got, key.Path)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("want exactly [%s], got %v", tc.want, got)
			}
		})
	}
}

func TestValidNestedPassthroughIsSilent(t *testing.T) {
	raw := "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
		"runtime:\n  rdma:\n    enabled: true\n" +
		"storage:\n  data_pvc: blob-training\n  mounts:\n" +
		"    - name: a\n      mountPath: /a\n      pvc: p\n      readOnly: true\n" +
		"    - name: b\n      mountPath: /b\n      pvc: q\n"
	keys, err := UnknownKeys([]byte(raw))
	if err != nil {
		t.Fatalf("UnknownKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("valid manifest warned: %v", keys)
	}
}

func TestRepeatedListTypoReportsOnce(t *testing.T) {
	raw := "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
		"storage:\n  mounts:\n" +
		"    - name: a\n      mountPath: /a\n      pvc: p\n      readOlny: true\n" +
		"    - name: b\n      mountPath: /b\n      pvc: q\n      readOlny: true\n"
	keys, err := UnknownKeys([]byte(raw))
	if err != nil {
		t.Fatalf("UnknownKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Path != "storage.mounts.readOlny" {
		t.Fatalf("want one storage.mounts.readOlny, got %v", keys)
	}
}
