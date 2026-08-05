package storageprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScriptAvoidsAwkDependency(t *testing.T) {
	script := Script()
	if strings.Contains(script, "awk ") {
		t.Fatalf("storage preflight should not require awk:\n%s", script)
	}
}

func TestScriptWarnsWhenDfUnavailable(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	binDir := t.TempDir()
	for _, name := range []string{"mkdir", "rm"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not available", name)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	hotDir := filepath.Join(t.TempDir(), "hot")
	if err := os.Mkdir(hotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "-c", Script())
	cmd.Env = []string{
		"PATH=" + binDir,
		"TAU_HOT_DIR=" + hotDir,
		"TAU_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage preflight should continue without df; err=%v output=%s", err, out)
	}
	if !strings.Contains(string(out), "tau storage warning: df not found; skipping free-space check for "+hotDir) {
		t.Fatalf("missing df warning:\n%s", out)
	}
}
