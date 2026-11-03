// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHotSizeConfig(t *testing.T, engine string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: hot-size
engine: `+engine+`
entrypoint: train.py
runtime:
  image: example.com/research/train:v1
storage:
  hot_size: 500Gi
policy:
  namespace: tests
  queue: team-a
  disable_default_priorities: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRunConfigHotSizeDryRun(t *testing.T) {
	for _, tc := range []struct {
		engine string
		kind   string
	}{
		{engine: "job", kind: "kind: Job"},
		{engine: "rayjob", kind: "kind: RayJob"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			config := writeHotSizeConfig(t, tc.engine)
			rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
			for _, want := range []string{tc.kind, "name: tau-hot", "sizeLimit: 500Gi", "mountPath: /mnt"} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("%s dry-run missing %q:\n%s", tc.engine, want, rendered)
				}
			}
		})
	}
}
