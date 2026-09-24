// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadlogs

import (
	"strings"
	"testing"
)

func TestSanitizeBoundsAndRedacts(t *testing.T) {
	content, truncated, redacted := Sanitize([]byte("Authorization: Bearer secret\nsig=private\nok=value\x00\n"), 45)
	if !truncated || !redacted {
		t.Fatalf("truncated=%v redacted=%v content=%q", truncated, redacted, content)
	}
	if strings.Contains(content, "secret") || strings.Contains(content, "private") || strings.ContainsRune(content, '\x00') {
		t.Fatalf("sensitive or control content remained: %q", content)
	}
}
