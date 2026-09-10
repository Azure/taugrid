// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scraper

import (
	"testing"
	"time"
)

func TestParseExplicitSampleTimestamps(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`gpu_errors{UUID="a"} 0 1700000000123`,
		`gpu_errors 0 1700000000123`,
	} {
		metric, err := parseLine(input)
		if err != nil {
			t.Fatal(err)
		}
		if !metric.Timestamp.Equal(time.UnixMilli(1700000000123)) {
			t.Fatalf("timestamp lost: %+v", metric)
		}
	}
	if _, err := parseLine("gpu_errors 0 invalid"); err == nil {
		t.Fatal("malformed explicit timestamp must not become fresh data")
	}
	metric, err := parseLine("gpu_errors 0")
	if err != nil || !metric.Timestamp.IsZero() {
		t.Fatalf("untimestamped exporter sample changed: %+v, %v", metric, err)
	}
}
