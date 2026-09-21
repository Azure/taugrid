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

func TestParseNanosecondBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		end   string
		valid bool
	}{
		{"positive nanosecond", "2026-09-01T00:00:00.000000001Z", true},
		{"exact thirty days", "2026-10-01T00:00:00Z", true},
		{"over thirty days", "2026-10-01T00:00:00.000000001Z", false},
		{"equal offset instant", "2026-09-01T02:00:00+02:00", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(url.Values{"start": {"2026-09-01T00:00:00Z"}, "end": {test.end}}, false)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}
