// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobdetail

import "testing"

func TestParseJobSurfacesTerminalFailureReason(t *testing.T) {
	raw := []byte(`{
	  "metadata":{"name":"train","creationTimestamp":"2026-07-16T10:00:00Z"},
	  "status":{"failed":1,"conditions":[
	    {"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}
	  ]}
	}`)

	got, ok := parseJob(raw)
	if !ok {
		t.Fatal("parseJob rejected a valid Job")
	}
	if got.detail.Reason != "BackoffLimitExceeded" {
		t.Errorf("Reason = %q, want BackoffLimitExceeded", got.detail.Reason)
	}
	if got.detail.Message != "Job has reached the specified backoff limit" {
		t.Errorf("Message = %q", got.detail.Message)
	}
}

func TestParseJobSurfacesTerminalCompletionReason(t *testing.T) {
	raw := []byte(`{
	  "metadata":{"name":"train","creationTimestamp":"2026-07-16T10:00:00Z"},
	  "status":{"succeeded":1,"conditions":[
	    {"type":"Complete","status":"True","reason":"CompletionsReached","message":"Reached expected number of succeeded pods"}
	  ]}
	}`)

	got, ok := parseJob(raw)
	if !ok {
		t.Fatal("parseJob rejected a valid Job")
	}
	if got.detail.Reason != "CompletionsReached" {
		t.Errorf("Reason = %q, want CompletionsReached", got.detail.Reason)
	}
}

func TestParseJobIgnoresNonTerminalConditions(t *testing.T) {
	raw := []byte(`{
	  "metadata":{"name":"train","creationTimestamp":"2026-07-16T10:00:00Z"},
	  "status":{"conditions":[
	    {"type":"Suspended","status":"True","reason":"JobSuspended","message":"Job suspended"}
	  ]}
	}`)

	got, ok := parseJob(raw)
	if !ok {
		t.Fatal("parseJob rejected a valid Job")
	}
	if got.detail.Reason != "" || got.detail.Message != "" {
		t.Errorf("non-terminal condition leaked into the outcome: %q / %q", got.detail.Reason, got.detail.Message)
	}
}

func TestParseJobIgnoresFalseTerminalConditions(t *testing.T) {
	raw := []byte(`{
	  "metadata":{"name":"train","creationTimestamp":"2026-07-16T10:00:00Z"},
	  "status":{"conditions":[
	    {"type":"Failed","status":"False","reason":"Stale","message":"stale"}
	  ]}
	}`)

	got, ok := parseJob(raw)
	if !ok {
		t.Fatal("parseJob rejected a valid Job")
	}
	if got.detail.Reason != "" {
		t.Errorf("Reason = %q, want empty", got.detail.Reason)
	}
}
