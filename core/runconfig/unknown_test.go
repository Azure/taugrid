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

// Nested typos are the same class of bug as top-level ones and must be caught
// one level down, where a camelCase slip is easy to make and impossible to see.
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

// Every tolerated key weakens the check, so the list is pinned. Provenance and
// commentary belong in YAML comments; growing this list should require an
// argument, not a quiet append.
func TestToleratedNonSchemaKeysStayPinned(t *testing.T) {
	if len(documentedPassthroughPaths) != 0 {
		t.Fatalf("documentedPassthroughPaths grew to %v; a key that looks live but does nothing should be a comment instead", documentedPassthroughPaths)
	}
	// managedPassthroughPaths is allowed to exist, but only for keys the
	// manifest schema really owns. The reflection guard in cli/internal/manifest
	// proves each entry is real; this bounds how many there can be.
	//
	// Count opaque roots, not entries. Only an opaque root weakens the check --
	// it drops a whole subtree. Descend markers and the children they enumerate
	// make the checker stricter, so counting them would let the bound be spent
	// on entries that improve it, and a genuinely new opaque root could then
	// slip in under the headroom. This keeps the bound measuring what it was
	// written to measure.
	opaqueRoots := 0
	for path, descend := range managedPassthroughPaths {
		if descend {
			continue
		}
		if idx := strings.LastIndex(path, "."); idx >= 0 {
			if parentDescends, ok := managedPassthroughPaths[path[:idx]]; ok && parentDescends {
				// A child of a structured subtree: it narrows the check.
				continue
			}
		}
		opaqueRoots++
	}
	if opaqueRoots > 20 {
		t.Fatalf("managedPassthroughPaths has %d opaque roots; if it keeps growing, runconfig and manifest should share one schema instead", opaqueRoots)
	}
}

// A valid config produces no warning.
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
  preset: azure.research.training.l
  node_selector:
    example.invalid/gpu-series: nd-h200-v5
storage:
  data_pvc: blob-training
`)
	if len(warnings) != 0 {
		t.Fatalf("valid managed config warned: %v", warnings)
	}
}

// The regression this whole file exists for: policy.node_selector misspelled as
// scheduling.node_selector used to be dropped in silence, so the workload
// scheduled anywhere instead of onto the intended GPU flavor.
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

// A wrong parent with a correct leaf must point at the real full path, since
// that is the shape of the mistake that cost a live debugging cycle.
func TestUnknownNestedKeySuggestsFullPath(t *testing.T) {
	suggestion, ok := suggestFieldPath("scheduling.node_selector")
	if !ok {
		t.Fatal("expected a suggestion for scheduling.node_selector")
	}
	if suggestion != "policy.node_selector" {
		t.Fatalf("suggestion = %q, want policy.node_selector", suggestion)
	}
}

func TestUnknownKeySuggestionsCoverCommonTypos(t *testing.T) {
	for input, want := range map[string]string{
		"complute":             "compute",
		"policy.node_selecter": "policy.node_selector",
		"runtime.imag":         "runtime.image",
		"polciy":               "policy",
	} {
		got, ok := suggestFieldPath(input)
		if !ok || got != want {
			t.Errorf("suggestFieldPath(%q) = (%q, %v), want %q", input, got, ok, want)
		}
	}
}

func TestUnrelatedKeyGetsNoMisleadingSuggestion(t *testing.T) {
	if got, ok := suggestFieldPath("quantum_entanglement_settings"); ok {
		t.Fatalf("unrelated key suggested %q; a wrong hint is worse than none", got)
	}
}

// Free-form map values carry user data, not schema. Validating their keys as
// field names would warn on every legitimate node label and env var.
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

// looksLikeTauConfig selects run configs by a top-level marker key rather than
// by filename. Kubernetes manifests under cli/examples are top-level mappings
// too, so presence of a marker -- not parseability -- is the discriminator;
// "parses as a config" would be circular, since the thing under test is whether
// the config parses without warnings.
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

// The probe must accept configs regardless of filename and reject Kubernetes
// manifests regardless of extension. Without this, a probe that returned true
// unconditionally would still make TestShippedExampleConfigsWarnNothing pass
// its checked>0 guard while asserting on files that are not configs.
func TestLooksLikeTauConfigPartitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"direct config", "schema_version: 1\nname: x\nrun:\n  engine: job\n", true},
		{"engine at top level", "name: x\nengine: ray\nentrypoint: t.py\n", true},
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

// Every shipped example must warn nothing. Manifest-owned keys (resource_naming,
// compute.cpus, compute.worker_memory) are legitimate and must stay silent.
//
// This walks the tree rather than globbing examples/*/tau.yaml: examples nest
// (aks-cpu-quickstart/stellar-demo/tau.yaml), and a one-level glob silently
// checks fewer files than it appears to -- an unguarded config that looks
// guarded, which is the failure mode this whole change exists to prevent.
//
// Selection is by content, not by filename. A name filter is the same class of
// error as the one-level glob on a different axis: kind-smoke/tau-ray.yaml is a
// real shipped config and tau.yaml matches everything except it. Widening to
// *.yaml is wrong too -- kind-cluster.yaml and kind-kueue-lanes.yaml are
// Kubernetes manifests that are not configs and must not be asserted on. So the
// discriminator is a top-level marker key, which partitions the tree exactly.
//
// A missing root is fatal, not skipped, for the same reason. cli and core are
// one repo joined by a replace directive and core is never published, so
// cli/examples is always present relative to here; if it ever is not, the
// directory was renamed and this guard silently stopped guarding. A skip
// reports as a pass, which is the identical failure one layer out.
func TestShippedExampleConfigsWarnNothing(t *testing.T) {
	root := filepath.Join("..", "..", "cli", "examples")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("examples root missing (%v); if cli/examples moved, repoint this "+
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

// Direct configs stay strict. The warning path must not soften them.
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

// TestNestedPassthroughTypoWarns covers the fail-open one level down: these
// subtrees are manifest-owned, but they are structured, and a dropped key in
// either one silently changes scheduling or mount semantics.
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
			name: "rdma resource_name typo",
			raw: "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
				"runtime:\n  rdma:\n    enabled: true\n    resource_nmae: rdma/x\n",
			want: "runtime.rdma.resource_nmae",
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

// TestValidNestedPassthroughIsSilent is the other half: descending must not
// turn correct manifests into warning spam.
func TestValidNestedPassthroughIsSilent(t *testing.T) {
	raw := "schema_version: 1\nname: x\nworkflow:\n  script: t.py\n" +
		"runtime:\n  rdma:\n    enabled: true\n    resource_name: rdma/rdma_shared_device_a\n    count: 1\n" +
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

// TestRepeatedListTypoReportsOnce keeps one mistake from becoming N warnings.
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
