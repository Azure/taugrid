// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sampleExposition = `DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu="0"} 0
`

func statusFor(t *testing.T, res ScrapeResult, name string) TargetStatus {
	t.Helper()
	for _, s := range res.Statuses {
		if s.Target.Name == name {
			return s
		}
	}
	t.Fatalf("no status reported for target %q", name)
	return TargetStatus{}
}

func TestScrapeReportsSuccessfulTarget(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleExposition))
	}))
	defer srv.Close()

	s := New([]ScrapeTarget{{Name: "dcgm-exporter", URL: srv.URL + "/metrics", Required: true, AvailabilityCondition: "DcgmExporterUnavailable"}})
	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(res.Metrics))
	}
	got := statusFor(t, res, "dcgm-exporter")
	if !got.OK {
		t.Errorf("expected OK status, got err %q", got.Err)
	}
	if got.Err != "" {
		t.Errorf("expected empty error, got %q", got.Err)
	}
}

func TestScrapeReportsNon2xxWithoutMetrics(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := New([]ScrapeTarget{{Name: "dcgm-exporter", URL: srv.URL + "/metrics"}})
	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Metrics) != 0 {
		t.Fatalf("expected no metrics, got %d", len(res.Metrics))
	}
	got := statusFor(t, res, "dcgm-exporter")
	if got.OK {
		t.Fatal("expected failure status for 503 response")
	}
	if !strings.Contains(got.Err, "503") {
		t.Errorf("expected status code in error, got %q", got.Err)
	}
}

func TestScrapeReportsConnectionFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL + "/metrics"
	srv.Close() // Nothing is listening now.

	s := New([]ScrapeTarget{{Name: "dcgm-exporter", URL: url}})
	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := statusFor(t, res, "dcgm-exporter")
	if got.OK {
		t.Fatal("expected failure status against a closed listener")
	}
	if got.Err == "" {
		t.Fatal("expected a non-empty connection error")
	}
}

func TestScrapeReportsTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	s := New([]ScrapeTarget{{Name: "dcgm-exporter", URL: srv.URL + "/metrics"}})
	s.client.Timeout = 50 * time.Millisecond

	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := statusFor(t, res, "dcgm-exporter")
	if got.OK {
		t.Fatal("expected failure status on timeout")
	}
	if !strings.Contains(got.Err, "timeout") && !strings.Contains(got.Err, "deadline") {
		t.Errorf("expected a timeout error, got %q", got.Err)
	}
}

func TestScrapeKeepsHealthyTargetsIndependent(t *testing.T) {
	t.Parallel()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleExposition))
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	badURL := bad.URL + "/metrics"
	bad.Close()

	s := New([]ScrapeTarget{
		{Name: "dcgm-exporter", URL: badURL, Required: true, AvailabilityCondition: "DcgmExporterUnavailable"},
		{Name: "node-exporter", URL: good.URL + "/metrics"},
	})
	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(res.Statuses))
	}
	if statusFor(t, res, "dcgm-exporter").OK {
		t.Error("failed required target must not report OK")
	}
	if !statusFor(t, res, "node-exporter").OK {
		t.Error("healthy optional target should report OK")
	}
	// A successful optional target must not mask the required target's loss.
	if len(res.Metrics) != 1 {
		t.Errorf("expected only the healthy target's metrics, got %d", len(res.Metrics))
	}
}

func TestSafeURLStripsCredentialsAndQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "http://localhost:19400/metrics", "http://localhost:19400/metrics"},
		{"service", "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics", "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"},
		{"credentials", "http://user:s3cret@localhost:19400/metrics", "http://localhost:19400/metrics"},
		{"query", "https://exporter.svc:9400/metrics?token=abc123", "https://exporter.svc:9400/metrics"},
		{"fragment", "http://exporter.svc:9400/metrics#frag", "http://exporter.svc:9400/metrics"},
		{"unparseable", "://nope", "<redacted-url>"},
	}
	for _, tc := range tests {
		if got := SafeURL(tc.raw); got != tc.want {
			t.Errorf("%s: SafeURL(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestScrapeErrorNeverLeaksSecrets(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	base := srv.URL
	srv.Close()

	raw := strings.Replace(base, "http://", "http://user:s3cret@", 1) + "/metrics?token=abc123"
	s := New([]ScrapeTarget{{Name: "dcgm-exporter", URL: raw}})
	res, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := statusFor(t, res, "dcgm-exporter")
	if got.OK {
		t.Fatal("expected failure status")
	}
	for _, secret := range []string{"s3cret", "abc123", "user:s3cret"} {
		if strings.Contains(got.Err, secret) {
			t.Errorf("error %q leaked %q", got.Err, secret)
		}
		if strings.Contains(got.SafeURL, secret) {
			t.Errorf("safe URL %q leaked %q", got.SafeURL, secret)
		}
	}
}

func TestScrapeTargetWindowDefaults(t *testing.T) {
	t.Parallel()

	var zero ScrapeTarget
	if got := zero.UnavailableWindow(); got != DefaultUnavailableFor {
		t.Errorf("UnavailableWindow() = %s, want %s", got, DefaultUnavailableFor)
	}
	if got := zero.AvailableWindow(); got != DefaultAvailableFor {
		t.Errorf("AvailableWindow() = %s, want %s", got, DefaultAvailableFor)
	}

	set := ScrapeTarget{UnavailableFor: 90 * time.Second, AvailableFor: 30 * time.Second}
	if got := set.UnavailableWindow(); got != 90*time.Second {
		t.Errorf("UnavailableWindow() = %s, want 90s", got)
	}
	if got := set.AvailableWindow(); got != 30*time.Second {
		t.Errorf("AvailableWindow() = %s, want 30s", got)
	}
}
