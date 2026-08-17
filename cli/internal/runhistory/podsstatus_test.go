// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pods_status is the operator's first diagnostic when batch Job failure
// summaries look wrong: "available" means the pod read worked and found
// nothing, anything else means the recorder could not see. The usual cause of
// the latter is a Role without the pods read verb, which produces no other
// symptom — the recorder stays healthy and quietly writes weaker records.
//
// That makes the field useless unless it escapes the process, which is what
// these tests pin.

func readyzBody(t *testing.T, health *Health) map[string]string {
	t.Helper()
	server := httptest.NewServer(health.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /readyz: %v", err)
	}
	return body
}

func TestReadyzExposesPodsStatus(t *testing.T) {
	health := &Health{}
	health.MarkSuccess(Result{
		RayJobsStatus:   "available",
		WorkloadsStatus: "available",
		PodsStatus:      "unavailable",
	}, "2026-08-17T10:00:00Z")

	body := readyzBody(t, health)
	if got := body["pods_status"]; got != "unavailable" {
		t.Errorf("pods_status = %q, want unavailable", got)
	}
	// The pre-existing fields must keep working.
	if body["rayjobs_status"] != "available" || body["workloads_status"] != "available" {
		t.Errorf("existing readyz fields changed: %+v", body)
	}
	if body["last_reconciled"] != "2026-08-17T10:00:00Z" {
		t.Errorf("last_reconciled = %q", body["last_reconciled"])
	}
}

func TestReadyzReportsPodsAvailable(t *testing.T) {
	health := &Health{}
	health.MarkSuccess(Result{RayJobsStatus: "available", WorkloadsStatus: "available", PodsStatus: "available"}, "t")

	if got := readyzBody(t, health)["pods_status"]; got != "available" {
		t.Errorf("pods_status = %q, want available", got)
	}
}

// Losing pod visibility must produce a log line, since an operator reading logs
// is the other half of the diagnostic path.
func TestPodsUnavailableIsLoggedOnce(t *testing.T) {
	var log bytes.Buffer
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded

	reconciler := newTestReconciler(source, &fakeWriter{})
	reconciler.Log = &log

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
			t.Fatal(err)
		}
	}

	if got := strings.Count(log.String(), "pod reads unavailable"); got != 1 {
		t.Errorf("logged the same degradation %d times, want exactly 1:\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "only the Job condition reason") {
		t.Errorf("log did not explain the consequence:\n%s", log.String())
	}
}

func TestPodsRecoveryIsLogged(t *testing.T) {
	var log bytes.Buffer
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded

	reconciler := newTestReconciler(source, &fakeWriter{})
	reconciler.Log = &log
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	source.podErr = nil
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(log.String(), "pod reads recovered") {
		t.Errorf("recovery was not logged:\n%s", log.String())
	}
}

// A healthy recorder must not log on every poll.
func TestSteadyStateDoesNotLog(t *testing.T) {
	var log bytes.Buffer
	reconciler := newTestReconciler(failedJobSource(nil), &fakeWriter{})
	reconciler.Log = &log

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
			t.Fatal(err)
		}
	}

	if log.Len() != 0 {
		t.Errorf("steady state logged:\n%s", log.String())
	}
}
