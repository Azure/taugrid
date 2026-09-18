// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package historyrange

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseRejectsRepeatedAndMixedParameters(t *testing.T) {
	for _, raw := range []string{
		"window=1h&window=24h",
		"start=2026-09-16T00%3A00%3A00Z&start=2026-09-16T01%3A00%3A00Z&end=2026-09-16T02%3A00%3A00Z",
		"window=1h&start=2026-09-16T00%3A00%3A00Z&end=2026-09-16T01%3A00%3A00Z",
	} {
		values, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(values, true); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", raw)
		}
	}
}

func TestParseAllowsLegacySinceOnlyWhenEnabled(t *testing.T) {
	values := url.Values{"since": {"24h"}}
	got, err := Parse(values, true)
	if err != nil || got.Since != "24h" {
		t.Fatalf("Parse allow since = %+v, %v", got, err)
	}
	if _, err := Parse(values, false); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Parse disallow since error = %v", err)
	}
}
