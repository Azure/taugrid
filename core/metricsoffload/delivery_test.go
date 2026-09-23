// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metricsoffload

import (
	"strings"
	"testing"
	"time"
)

func TestTerminalDrainTimeoutCoversAttemptsAndBackoff(t *testing.T) {
	got, err := TerminalDrainTimeout(4, 2*time.Second, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := 20*time.Minute + 44*time.Second
	if got != want {
		t.Fatalf("terminal drain timeout = %s, want %s", got, want)
	}

	got, err = TerminalDrainTimeout(MaxADXAttempts, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want = 109*time.Minute + time.Second
	if got != want {
		t.Fatalf("exhausted default delivery timeout = %s, want %s", got, want)
	}
}

func TestTerminalDrainTimeoutRejectsUnboundedDelivery(t *testing.T) {
	_, err := TerminalDrainTimeout(MaxADXAttempts, time.Minute, 12*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("terminal drain timeout error = %v, want maximum bound", err)
	}
}
