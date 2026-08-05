package cli

import "testing"

func TestDefaultKubeContextReadsTauContext(t *testing.T) {
	t.Setenv("TAU_CONTEXT", "kind-taugrid")

	if got := defaultKubeContext(); got != "kind-taugrid" {
		t.Fatalf("defaultKubeContext() = %q, want kind-taugrid", got)
	}
}

func TestDefaultKubeContextFallsBackToCurrentContext(t *testing.T) {
	t.Setenv("TAU_CONTEXT", "")

	if got := defaultKubeContext(); got != "" {
		t.Fatalf("defaultKubeContext() = %q, want empty current-context fallback", got)
	}
}
