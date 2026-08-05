package cli

import "testing"

func TestCleanStorageFlagRejectsWhitespace(t *testing.T) {
	if got, err := cleanStorageFlag("--data-pvc", "blob-training"); err != nil || got != "blob-training" {
		t.Fatalf("valid PVC name should pass, got %q err=%v", got, err)
	}
	if got, err := cleanStorageFlag("--data-pvc", ""); err != nil || got != "" {
		t.Fatalf("empty value should pass through, got %q err=%v", got, err)
	}
	for _, bad := range []string{" blob-training", "blob-training ", "   "} {
		if _, err := cleanStorageFlag("--data-pvc", bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}
