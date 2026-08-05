package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootSilencesCobraErrorPrinting asserts the root does not let cobra print
// the error itself. main() prints `error: %v` and exits 1; without
// SilenceErrors cobra prints its own `Error: %v` first and every failure is
// reported twice.
func TestRootSilencesCobraErrorPrinting(t *testing.T) {
	root := NewRoot()
	if !root.SilenceErrors {
		t.Fatal("root must set SilenceErrors so main() is the only error printer")
	}
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"run", "--config", "/nonexistent-path-for-test.yaml"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a failure to exercise the error path")
	}
	if stderr.Len() != 0 {
		t.Fatalf("cobra wrote to stderr; main() would print the error again:\n%s", stderr.String())
	}
	for _, section := range []string{"Owner:", "Action:"} {
		if count := strings.Count(err.Error(), section); count != 1 {
			t.Fatalf("guided error rendered %q %d times, want 1:\n%s", section, count, err)
		}
	}
}

func TestRepoGenRootSilencesCobraErrorPrinting(t *testing.T) {
	if !NewRepoGenRoot().SilenceErrors {
		t.Fatal("tau-gen root must set SilenceErrors so main() is the only error printer")
	}
}
