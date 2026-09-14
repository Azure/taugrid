// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestDeriveStateFailsClosed(t *testing.T) {
	observed := time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC)
	validUntil := observed.Add(24 * time.Hour)
	for _, tc := range []struct {
		name       string
		now        time.Time
		status     string
		running    bool
		verified   bool
		wantState  string
		wantFresh  string
		wantAgeSec int64
	}{
		{name: "running", now: observed.Add(time.Minute), running: true, wantState: StateRunning, wantFresh: FreshnessNotApplicable, wantAgeSec: 60},
		{name: "unverified", now: observed.Add(time.Minute), status: "pass", wantState: StateUnknown, wantFresh: FreshnessUnknown, wantAgeSec: 60},
		{name: "fresh pass", now: validUntil.Add(-time.Nanosecond), status: "pass", verified: true, wantState: StatePassed, wantFresh: FreshnessFresh, wantAgeSec: 86399},
		{name: "boundary pass", now: validUntil, status: "pass", verified: true, wantState: StatePassed, wantFresh: FreshnessFresh, wantAgeSec: 86400},
		{name: "stale pass", now: validUntil.Add(time.Nanosecond), status: "pass", verified: true, wantState: StateStale, wantFresh: FreshnessStale, wantAgeSec: 86400},
		{name: "fresh fail", now: observed.Add(time.Minute), status: "fail", verified: true, wantState: StateFailed, wantFresh: FreshnessFresh, wantAgeSec: 60},
		{name: "verified unknown", now: observed.Add(time.Minute), status: "unknown", verified: true, wantState: StateUnknown, wantFresh: FreshnessFresh, wantAgeSec: 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, freshness, age := DeriveState(tc.now, observed, validUntil, tc.status, tc.running, tc.verified)
			if state != tc.wantState || freshness != tc.wantFresh || age == nil || *age != tc.wantAgeSec {
				t.Fatalf("DeriveState = %q %q %v, want %q %q %d", state, freshness, age, tc.wantState, tc.wantFresh, tc.wantAgeSec)
			}
		})
	}
}

func TestDeriveStateUnknownTime(t *testing.T) {
	state, freshness, age := DeriveState(time.Now(), time.Time{}, time.Time{}, "pass", false, true)
	if state != StateUnknown || freshness != FreshnessUnknown || age != nil {
		t.Fatalf("DeriveState = %q %q %v", state, freshness, age)
	}
}

func TestCursorRoundTripAndValidation(t *testing.T) {
	want := Cursor{
		SortAt:       time.Date(2026, 9, 14, 20, 0, 0, 123, time.UTC),
		ValidationID: "nccl-rdma-0123456789abcdef",
	}
	encoded, err := EncodeCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SortAt.Equal(want.SortAt) || got.ValidationID != want.ValidationID {
		t.Fatalf("cursor = %+v, want %+v", got, want)
	}
	for _, invalid := range []string{"not-base64", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`))} {
		if _, err := DecodeCursor(invalid); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("DecodeCursor(%q) error = %v", invalid, err)
		}
	}
}
