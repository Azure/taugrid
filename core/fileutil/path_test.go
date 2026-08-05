package fileutil

import "testing"

func TestSafePathComponent(t *testing.T) {
	tests := map[string]string{
		"":               "default",
		"run/id:123":     "run-id-123",
		"..":             ShortStringHash(".."),
		"already_safe.1": "already_safe.1",
	}
	for input, want := range tests {
		if got := SafePathComponent(input); got != want {
			t.Fatalf("SafePathComponent(%q) = %q, want %q", input, got, want)
		}
	}
}
