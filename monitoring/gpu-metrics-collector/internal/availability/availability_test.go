// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package availability

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/state"
)

func dcgmTarget() scraper.ScrapeTarget {
	return scraper.ScrapeTarget{
		Name:                  "dcgm-exporter",
		URL:                   "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics",
		Required:              true,
		AvailabilityCondition: "DcgmExporterUnavailable",
		UnavailableFor:        2 * time.Minute,
		AvailableFor:          1 * time.Minute,
	}
}

func failure(target scraper.ScrapeTarget, msg string) scraper.TargetStatus {
	return scraper.TargetStatus{Target: target, OK: false, SafeURL: scraper.SafeURL(target.URL), Err: msg}
}

func success(target scraper.ScrapeTarget) scraper.TargetStatus {
	return scraper.TargetStatus{Target: target, OK: true, SafeURL: scraper.SafeURL(target.URL)}
}

// establish drives the tracker through a full success window so it owns the
// condition, mirroring a collector that has been running normally. A fresh
// tracker deliberately publishes nothing until it has proven the state.
func establish(t *testing.T, tr *Tracker, target scraper.ScrapeTarget, start time.Time) time.Time {
	t.Helper()
	tr.Evaluate([]scraper.TargetStatus{success(target)}, start)
	at := start.Add(target.AvailableWindow())
	if got := only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, at), target.AvailabilityCondition); got.Firing {
		t.Fatalf("expected an established False, got %+v", got)
	}
	return at
}

func only(t *testing.T, results []rules.Result, cond string) rules.Result {
	t.Helper()
	for _, r := range results {
		if r.ConditionType == cond {
			return r
		}
	}
	t.Fatalf("no result for condition %q", cond)
	return rules.Result{}
}

func TestHealthyTargetReportsExplicitFalse(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	// A fresh tracker stays silent until reachability is proven for
	// availableFor; only then does it publish the explicit False.
	if results := tr.Evaluate([]scraper.TargetStatus{success(target)}, start); len(results) != 0 {
		t.Fatalf("unproven tracker published %+v", results)
	}
	now := start.Add(time.Minute)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, now), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("healthy target must not fire")
	}
	if got.Reason != "DcgmExporterUnavailableOk" {
		t.Errorf("unexpected reason %q", got.Reason)
	}
	if !strings.Contains(got.Message, "reachable") ||
		!strings.Contains(got.Message, "nvidia-dcgm-exporter.gpu-operator.svc:9400") {
		t.Errorf("unexpected message %q", got.Message)
	}
}

func TestTransientFailureBelowThresholdDoesNotFire(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := establish(t, tr, target, time.Now())

	// One failed scrape, then recovery, well inside the 2m failure window.
	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("a single failed scrape must not set the condition")
	}
	if !strings.Contains(got.Message, "condition sets after 2m0s") {
		t.Errorf("pending message should state the threshold, got %q", got.Message)
	}

	got = only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(15*time.Second)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("recovered target must not fire")
	}

	// The failure timer must have reset: another failure 5m later still needs
	// the full window before firing.
	got = only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(5*time.Minute)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("failure timer must restart after a successful scrape")
	}
}

func TestSustainedFailureFiresAfterWindow(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := establish(t, tr, target, time.Now())

	for _, offset := range []time.Duration{0, 30 * time.Second, 90 * time.Second} {
		got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(offset)), "DcgmExporterUnavailable")
		if got.Firing {
			t.Fatalf("fired early at %s", offset)
		}
	}

	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(2*time.Minute)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("expected the condition to fire after the failure window")
	}
	if got.Reason != "DcgmExporterUnavailable" {
		t.Errorf("unexpected reason %q", got.Reason)
	}
	if !strings.Contains(got.Message, `"dcgm-exporter"`) ||
		!strings.Contains(got.Message, "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics") ||
		!strings.Contains(got.Message, "connection refused") ||
		!strings.Contains(got.Message, "unavailable for 2m0s") {
		t.Errorf("firing message missing target, URL, duration, or cause: %q", got.Message)
	}
}

func TestNon2xxFailureFiresWithStatusContext(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	tr.Evaluate([]scraper.TargetStatus{failure(target, "unexpected status 503")}, start)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "unexpected status 503")}, start.Add(3*time.Minute)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("sustained 503 responses must set the condition")
	}
	if !strings.Contains(got.Message, "unexpected status 503") {
		t.Errorf("message should carry the status error, got %q", got.Message)
	}
}

func TestRecoveryClearsOnlyAfterRecoveryWindow(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(3*time.Minute)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("expected firing before recovery")
	}

	got = only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(3*time.Minute+15*time.Second)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("one successful scrape must not clear the condition")
	}
	if !strings.Contains(got.Message, "condition clears after 1m0s") {
		t.Errorf("recovering message should state the recovery window, got %q", got.Message)
	}

	got = only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(4*time.Minute+15*time.Second)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("expected the condition to clear after the recovery window")
	}

	// A failure during recovery restarts the recovery clock.
	tr2 := New([]scraper.ScrapeTarget{target})
	tr2.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start)
	tr2.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(3*time.Minute))
	tr2.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(3*time.Minute+30*time.Second))
	tr2.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(3*time.Minute+45*time.Second))
	got = only(t, tr2.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(4*time.Minute+30*time.Second)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("recovery clock must restart after an intervening failure")
	}
}

func TestOptionalTargetsProduceNoCondition(t *testing.T) {
	t.Parallel()

	dcgm := dcgmTarget()
	nodeExporter := scraper.ScrapeTarget{Name: "node-exporter", URL: "http://localhost:9100/metrics"}
	npd := scraper.ScrapeTarget{Name: "node-problem-detector", URL: "http://localhost:20261/metrics"}

	tr := New([]scraper.ScrapeTarget{dcgm, nodeExporter, npd})
	if tr.Tracked() != 1 {
		t.Fatalf("expected 1 tracked condition, got %d", tr.Tracked())
	}

	start := time.Now()
	tr.Evaluate([]scraper.TargetStatus{success(dcgm)}, start)
	// Optional targets fail while the required one succeeds.
	results := tr.Evaluate([]scraper.TargetStatus{
		success(dcgm),
		failure(nodeExporter, "connection refused"),
		failure(npd, "connection refused"),
	}, start.Add(10*time.Minute))
	if len(results) != 1 {
		t.Fatalf("expected only the required target's result, got %d", len(results))
	}
	if results[0].Firing {
		t.Error("optional target failures must not fire the required condition")
	}
}

func TestRequiredFailureNotMaskedByOtherHealthyTargets(t *testing.T) {
	t.Parallel()

	dcgm := dcgmTarget()
	nodeExporter := scraper.ScrapeTarget{Name: "node-exporter", URL: "http://localhost:9100/metrics"}

	tr := New([]scraper.ScrapeTarget{dcgm, nodeExporter})
	start := time.Now()

	tr.Evaluate([]scraper.TargetStatus{failure(dcgm, "connection refused"), success(nodeExporter)}, start)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{
		failure(dcgm, "connection refused"),
		success(nodeExporter),
	}, start.Add(3*time.Minute)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("a healthy node-exporter must not mask a lost DCGM exporter")
	}
}

func TestIndependentConditionsPerRequiredTarget(t *testing.T) {
	t.Parallel()

	dcgm := dcgmTarget()
	other := scraper.ScrapeTarget{
		Name:                  "secondary-exporter",
		URL:                   "http://localhost:19400/metrics",
		Required:              true,
		AvailabilityCondition: "SecondaryExporterUnavailable",
		UnavailableFor:        2 * time.Minute,
		AvailableFor:          time.Minute,
	}

	tr := New([]scraper.ScrapeTarget{dcgm, other})
	start := time.Now()
	tr.Evaluate([]scraper.TargetStatus{failure(dcgm, "connection refused"), success(other)}, start)
	results := tr.Evaluate([]scraper.TargetStatus{failure(dcgm, "connection refused"), success(other)}, start.Add(3*time.Minute))

	if !only(t, results, "DcgmExporterUnavailable").Firing {
		t.Error("failing target should fire its own condition")
	}
	if only(t, results, "SecondaryExporterUnavailable").Firing {
		t.Error("healthy target must not inherit another target's failure")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start)
	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(3*time.Minute))

	saved := tr.ExportState()
	if !saved["DcgmExporterUnavailable"].Firing {
		t.Fatal("exported state should record the firing condition")
	}

	savedAt := start.Add(3 * time.Minute)
	restartedAt := savedAt.Add(20 * time.Second)
	restarted := New([]scraper.ScrapeTarget{target})
	restarted.RestoreState(saved, savedAt, restartedAt)

	// The first successful scrape after a restart must not clear the condition
	// before the recovery window elapses.
	got := only(t, restarted.Evaluate([]scraper.TargetStatus{success(target)}, restartedAt), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("restored condition cleared before the recovery window")
	}
	got = only(t, restarted.Evaluate([]scraper.TargetStatus{success(target)}, restartedAt.Add(time.Minute)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("restored condition should clear after the recovery window")
	}
}

func TestRestartDowntimeDoesNotCountTowardFailureWindow(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	// One failed scrape is persisted with a non-zero failure timer.
	start = establish(t, tr, target, start)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("a single failed scrape must not fire")
	}
	saved := tr.ExportState()

	// The node reboots for five minutes, longer than the failure window.
	savedAt := start
	restartedAt := start.Add(5 * time.Minute)
	restarted := New([]scraper.ScrapeTarget{target})
	restarted.RestoreState(saved, savedAt, restartedAt)

	got = only(t, restarted.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, restartedAt), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("collector downtime must not count as continuous scrape failure")
	}

	// Progress made before the restart is preserved: the remaining window is
	// what was left, not the full window again.
	got = only(t, restarted.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, restartedAt.Add(2*time.Minute)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("expected firing once the remaining failure window elapsed")
	}
}

func TestRestartDowntimeDoesNotCountTowardRecoveryWindow(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start)
	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(3*time.Minute))
	// One successful scrape starts the recovery clock, then the collector dies.
	tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(3*time.Minute+10*time.Second))
	saved := tr.ExportState()

	savedAt := start.Add(3*time.Minute + 10*time.Second)
	restartedAt := savedAt.Add(10 * time.Minute)
	restarted := New([]scraper.ScrapeTarget{target})
	restarted.RestoreState(saved, savedAt, restartedAt)

	got := only(t, restarted.Evaluate([]scraper.TargetStatus{success(target)}, restartedAt), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("collector downtime must not count as continuous scrape success")
	}
}

func TestDeconfiguredConditionIsClearedOnce(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	now := time.Now()
	tr.RestoreState(map[string]state.Availability{
		"DcgmExporterUnavailable":  {Firing: false, HealthySince: now.Add(-time.Hour)},
		"RenamedExporterCondition": {Firing: true, FailingSince: now.Add(-time.Hour)},
	}, now.Add(-time.Minute), now)

	results := tr.Evaluate([]scraper.TargetStatus{success(target)}, now)
	got := only(t, results, "RenamedExporterCondition")
	if got.Firing {
		t.Fatal("a de-configured condition must be published as False, not left True")
	}
	if got.Reason != "RenamedExporterConditionOk" {
		t.Errorf("unexpected reason %q", got.Reason)
	}
	if !strings.Contains(got.Message, "no longer configured") {
		t.Errorf("unexpected message %q", got.Message)
	}
}

func TestRestoreDoesNotResurrectUnknownConditions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tr := New([]scraper.ScrapeTarget{dcgmTarget()})
	establish(t, tr, dcgmTarget(), now.Add(-2*time.Minute))
	tr.RestoreState(map[string]state.Availability{
		"RemovedCondition": {Firing: true, FailingSince: now.Add(-time.Hour), Established: true},
	}, now.Add(-time.Minute), now)
	if tr.Tracked() != 1 {
		t.Fatalf("expected only configured conditions, got %d", tr.Tracked())
	}
	results := tr.Evaluate([]scraper.TargetStatus{success(dcgmTarget())}, now)
	if only(t, results, "DcgmExporterUnavailable").Firing {
		t.Error("stale persisted condition must not resurrect")
	}
	if only(t, results, "RemovedCondition").Firing {
		t.Error("a removed condition must be cleared, not left True")
	}
}

func TestDefaultWindowsApplyWhenUnset(t *testing.T) {
	t.Parallel()

	target := scraper.ScrapeTarget{
		Name:                  "dcgm-exporter",
		URL:                   "http://localhost:19400/metrics",
		Required:              true,
		AvailabilityCondition: "DcgmExporterUnavailable",
	}
	tr := New([]scraper.ScrapeTarget{target})
	start := establish(t, tr, target, time.Now())

	tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start)
	got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(scraper.DefaultUnavailableFor-time.Second)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("fired before the default failure window")
	}
	got = only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(scraper.DefaultUnavailableFor)), "DcgmExporterUnavailable")
	if !got.Firing {
		t.Fatal("expected firing at the default failure window")
	}
}

// The following cover the unseeded-restart hazard: the collector restarts while
// an outage is ongoing, but its snapshot is missing, corrupt, or stale, so
// state.Load returns nil. The tracker starts with firing=false and must not let
// a still-failing scrape clear the True condition the server already holds.

func TestUnseededRestartDuringOutageNeverClears(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	start := time.Now()

	// state.Load returns (nil, nil) for a missing, corrupt, or stale snapshot,
	// so in every case RestoreState is never called and the tracker is fresh.
	for _, snapshot := range []string{"missing", "corrupt", "stale"} {
		t.Run(snapshot, func(t *testing.T) {
			tr := New([]scraper.ScrapeTarget{target})

			// The outage continues across the restart. Nothing may be published
			// yet: publishing False here would clear the server's True.
			for _, offset := range []time.Duration{0, 15 * time.Second, 90 * time.Second} {
				results := tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(offset))
				if len(results) != 0 {
					t.Fatalf("at %s an unseeded restart published %+v; it must stay silent", offset, results)
				}
			}

			// Once this process has proven the failure for the full window it
			// owns the condition again and re-asserts True.
			got := only(t, tr.Evaluate([]scraper.TargetStatus{failure(target, "connection refused")}, start.Add(2*time.Minute)), "DcgmExporterUnavailable")
			if !got.Firing {
				t.Fatal("expected the condition to be re-asserted True after unavailableFor")
			}
		})
	}
}

func TestUnseededRestartClearsOnlyAfterProvenRecovery(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	// Restart with no usable snapshot, and the target is reachable again. The
	// clear must still wait for availableFor of proven success rather than
	// trusting one scrape.
	if results := tr.Evaluate([]scraper.TargetStatus{success(target)}, start); len(results) != 0 {
		t.Fatalf("one successful scrape after an unseeded restart published %+v", results)
	}
	if results := tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(30*time.Second)); len(results) != 0 {
		t.Fatalf("published before availableFor elapsed: %+v", results)
	}

	got := only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(time.Minute)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("expected False once recovery was proven for availableFor")
	}
	if !strings.Contains(got.Message, "reachable") {
		t.Errorf("unexpected message %q", got.Message)
	}
}

func TestUnseededRestartRecoveryInterruptedByFailureStaysSilent(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	// A flapping endpoint after an unseeded restart never accumulates a full
	// success window, so the tracker must never clear the server's condition.
	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * 30 * time.Second)
		statuses := []scraper.TargetStatus{success(target)}
		if i%2 == 1 {
			statuses = []scraper.TargetStatus{failure(target, "connection refused")}
		}
		if results := tr.Evaluate(statuses, at); len(results) != 0 {
			t.Fatalf("flapping target published %+v at %s", results, at.Sub(start))
		}
	}
}

func TestSeededRestartStillReportsImmediately(t *testing.T) {
	t.Parallel()

	target := dcgmTarget()
	tr := New([]scraper.ScrapeTarget{target})
	start := time.Now()

	// Establish a healthy, published condition and snapshot it.
	tr.Evaluate([]scraper.TargetStatus{success(target)}, start)
	if got := only(t, tr.Evaluate([]scraper.TargetStatus{success(target)}, start.Add(time.Minute)), "DcgmExporterUnavailable"); got.Firing {
		t.Fatal("expected an established False")
	}
	saved := tr.ExportState()

	// A usable snapshot is continuity, so the restarted process may speak to
	// the condition on its very first cycle.
	savedAt := start.Add(time.Minute)
	restarted := New([]scraper.ScrapeTarget{target})
	restarted.RestoreState(saved, savedAt, savedAt.Add(15*time.Second))

	got := only(t, restarted.Evaluate([]scraper.TargetStatus{success(target)}, savedAt.Add(15*time.Second)), "DcgmExporterUnavailable")
	if got.Firing {
		t.Fatal("a seeded restart should keep reporting False")
	}
}
