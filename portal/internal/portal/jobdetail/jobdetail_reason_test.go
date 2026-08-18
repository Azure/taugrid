// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobdetail

import "testing"

// RayJob failures have always been legible in the UI because KubeRay puts the
// cause on status.reason/status.message and parseRayJob forwards it. Batch Jobs
// carry the equivalent text on their terminal condition, but parseJob used to
// read only {type,status} and drop it — making batch Jobs the one workload kind
// whose terminal cause was invisible.

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

// Suspended is not an outcome. Reporting it as the terminal explanation would
// describe a queued Job as though it had finished.
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

// A condition that flipped back to False is history, not the current outcome.
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
