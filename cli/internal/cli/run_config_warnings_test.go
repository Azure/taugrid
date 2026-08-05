package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end guard for the warning path: an unknown key in a managed manifest
// must reach the user. The parser ignores the key either way, so this message is
// the only evidence the author gets that their directive does nothing.
func TestReadRunConfigReportsUnknownManagedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(path, []byte(`schema_version: 1
name: typo-config
run:
  entrypoint: train.py
compute:
  gpus: 8
scheduling:
  node_selector:
    example.invalid/gpu-series: nd-h200-v5
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, warnings, err := readRunConfig(path)
	if err != nil {
		t.Fatalf("readRunConfig: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning for the unknown scheduling key")
	}

	var buf bytes.Buffer
	emitConfigWarnings(&buf, warnings)
	got := buf.String()
	if !strings.Contains(got, `"scheduling"`) {
		t.Errorf("emitted output does not name the key: %q", got)
	}
	if !strings.Contains(got, `did you mean "policy"?`) {
		t.Errorf("emitted output does not suggest policy: %q", got)
	}
}

func TestReadRunConfigStaysQuietForValidManagedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(path, []byte(`schema_version: 1
name: clean-config
run:
  entrypoint: train.py
compute:
  gpus: 8
policy:
  node_selector:
    example.invalid/gpu-series: nd-h200-v5
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, warnings, err := readRunConfig(path)
	if err != nil {
		t.Fatalf("readRunConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("valid config warned: %v", warnings)
	}
}
