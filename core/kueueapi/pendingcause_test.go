// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kueueapi

import "testing"

func TestPendingCause_PrefersQuotaReservedMessageRegardlessOfOrder(t *testing.T) {
	quota := Condition{Type: "QuotaReserved", Status: "False", Reason: "Pending", Message: "couldn't assign flavors to pod set main"}
	admitted := Condition{Type: "Admitted", Status: "False", Reason: "Pending", Message: "generic pending"}

	for _, tc := range []struct {
		name  string
		conds []Condition
	}{
		{"quota first", []Condition{quota, admitted}},
		{"admitted first", []Condition{admitted, quota}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := PendingCause(tc.conds)
			if message != quota.Message {
				t.Errorf("message = %q, want the QuotaReserved message", message)
			}
			if reason != "Pending" {
				t.Errorf("reason = %q, want %q", reason, "Pending")
			}
		})
	}
}

// Kueue often sets only QuotaReserved while a workload waits for flavor
// assignment; there is no Admitted condition to fall back on.
func TestPendingCause_QuotaReservedOnly(t *testing.T) {
	reason, message := PendingCause([]Condition{
		{Type: "QuotaReserved", Status: "False", Reason: "Pending", Message: "couldn't assign flavors"},
	})
	if reason != "Pending" || message != "couldn't assign flavors" {
		t.Errorf("got reason=%q message=%q", reason, message)
	}
}

func TestPendingCause_FallsBackToOtherNonTrueCondition(t *testing.T) {
	reason, message := PendingCause([]Condition{
		{Type: "Deactivated", Status: "False", Reason: "Deactivated", Message: "workload deactivated"},
	})
	if reason != "Deactivated" || message != "workload deactivated" {
		t.Errorf("got reason=%q message=%q", reason, message)
	}
}

func TestPendingCause_IgnoresSatisfiedConditions(t *testing.T) {
	reason, message := PendingCause([]Condition{
		{Type: "QuotaReserved", Status: "True", Reason: "Admitted", Message: "quota reserved"},
	})
	if reason != "" || message != "" {
		t.Errorf("got reason=%q message=%q, want both empty", reason, message)
	}
}

// Reason and message must come from the same condition. Kueue writes both in
// one operation: UnsetQuotaReservationWithCondition sets QuotaReserved=False
// with the caller's reason ("Inadmissible" here) and then calls
// SyncAdmittedCondition, which derives Admitted=False with its own fixed
// vocabulary. Mixing the two produced a row whose REASON came from Admitted
// while the detail line came from QuotaReserved.
func TestPendingCause_ReasonAndMessageComeFromOneCondition(t *testing.T) {
	reason, message := PendingCause([]Condition{
		{Type: "Admitted", Status: "False", Reason: "NoReservation", Message: "The workload has no reservation"},
		{Type: "QuotaReserved", Status: "False", Reason: "Inadmissible", Message: "LocalQueue jobqueue doesn't exist"},
	})
	if reason != "Inadmissible" || message != "LocalQueue jobqueue doesn't exist" {
		t.Errorf("got reason=%q message=%q, want both from QuotaReserved", reason, message)
	}
}

// Falling back to Admitted must take its message too, not pair Admitted's
// reason with an empty QuotaReserved message.
func TestPendingCause_FallbackKeepsAdmittedPairIntact(t *testing.T) {
	reason, message := PendingCause([]Condition{
		{Type: "Admitted", Status: "False", Reason: "UnsatisfiedChecks", Message: "The workload has not all checks ready"},
	})
	if reason != "UnsatisfiedChecks" || message != "The workload has not all checks ready" {
		t.Errorf("got reason=%q message=%q", reason, message)
	}
}
