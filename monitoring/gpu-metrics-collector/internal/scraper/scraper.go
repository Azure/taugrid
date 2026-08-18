// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scraper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Metric represents a single scraped metric sample.
type Metric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ScrapeTarget defines an endpoint to scrape.
//
// A target may declare itself required, meaning its loss is a node-level health
// signal rather than a log line. Required targets must name the Node condition
// that reports their availability so a remediation system can distinguish "no
// errors reported" from "no metrics collected".
type ScrapeTarget struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Required marks the target as mandatory for this node's health signal.
	Required bool `yaml:"required,omitempty"`
	// AvailabilityCondition is the Node condition type set while a required
	// target is unreachable. It reports endpoint reachability only and must not
	// reuse a condition type owned by a rule or by another target.
	AvailabilityCondition string `yaml:"availabilityCondition,omitempty"`
	// UnavailableFor is how long scrapes must fail continuously before the
	// condition is set. Zero uses DefaultUnavailableFor.
	UnavailableFor time.Duration `yaml:"unavailableFor,omitempty"`
	// AvailableFor is how long scrapes must succeed continuously before a set
	// condition is cleared. Zero uses DefaultAvailableFor.
	AvailableFor time.Duration `yaml:"availableFor,omitempty"`
}

// Default debounce windows for required-target availability. They are long
// enough that a single missed scrape cannot flap a Node condition.
const (
	DefaultUnavailableFor = 2 * time.Minute
	DefaultAvailableFor   = 1 * time.Minute
)

// UnavailableWindow returns the configured failure debounce, or the default.
func (t ScrapeTarget) UnavailableWindow() time.Duration {
	if t.UnavailableFor > 0 {
		return t.UnavailableFor
	}
	return DefaultUnavailableFor
}

// AvailableWindow returns the configured recovery debounce, or the default.
func (t ScrapeTarget) AvailableWindow() time.Duration {
	if t.AvailableFor > 0 {
		return t.AvailableFor
	}
	return DefaultAvailableFor
}

// TargetStatus is the redacted outcome of scraping one target. Err is safe to
// publish: it never carries credentials or query parameters from the URL.
type TargetStatus struct {
	Target  ScrapeTarget
	OK      bool
	SafeURL string
	Err     string
}

// ScrapeResult carries the merged metric set plus per-target outcomes.
type ScrapeResult struct {
	Metrics  []Metric
	Statuses []TargetStatus
}

// Scraper collects metrics from Prometheus exposition endpoints.
type Scraper struct {
	targets []ScrapeTarget
	client  *http.Client
}

// New creates a Scraper for the given targets.
func New(targets []ScrapeTarget) *Scraper {
	return &Scraper{
		targets: targets,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// Scrape fetches metrics from all targets concurrently, returning a unified
// metric set plus the per-target outcome. Targets that are unreachable are
// logged and skipped; their loss is reported through ScrapeResult.Statuses so
// a required target's absence cannot be mistaken for a healthy node.
func (s *Scraper) Scrape(ctx context.Context) (ScrapeResult, error) {
	type result struct {
		metrics []Metric
		err     error
		target  ScrapeTarget
	}

	ch := make(chan result, len(s.targets))
	for _, t := range s.targets {
		go func(t ScrapeTarget) {
			metrics, err := s.scrapeTarget(ctx, t)
			ch <- result{metrics: metrics, err: err, target: t}
		}(t)
	}

	out := ScrapeResult{Statuses: make([]TargetStatus, 0, len(s.targets))}
	for range s.targets {
		r := <-ch
		safeURL := SafeURL(r.target.URL)
		status := TargetStatus{Target: r.target, OK: r.err == nil, SafeURL: safeURL}
		if r.err != nil {
			status.Err = redactError(r.err, r.target.URL, safeURL)
			slog.Warn("scrape target unavailable",
				"target", r.target.Name,
				"url", safeURL,
				"required", r.target.Required,
				"error", status.Err)
		} else {
			out.Metrics = append(out.Metrics, r.metrics...)
		}
		out.Statuses = append(out.Statuses, status)
	}
	return out, nil
}

// SafeURL renders a target URL without credentials, query, or fragment so it
// can be published in a Node condition message. Unparseable URLs collapse to a
// placeholder rather than leaking their raw contents.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted-url>"
	}
	safe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return safe.String()
}

// redactError renders a scrape error without the raw target URL. net/http wraps
// failures in *url.Error, whose message embeds the full URL including any
// userinfo and query string.
func redactError(err error, rawURL, safeURL string) string {
	msg := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		msg = urlErr.Err.Error()
	}
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, safeURL)
	}
	if u, parseErr := url.Parse(rawURL); parseErr == nil {
		if u.User != nil {
			msg = strings.ReplaceAll(msg, u.User.String(), "<redacted>")
		}
		if u.RawQuery != "" {
			msg = strings.ReplaceAll(msg, u.RawQuery, "<redacted>")
		}
	}
	return truncate(msg, maxErrorLength)
}

// maxErrorLength bounds the error text copied into a Node condition message.
const maxErrorLength = 200

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (s *Scraper) scrapeTarget(ctx context.Context, target ScrapeTarget) ([]Metric, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return parseExposition(resp.Body)
}

// parseExposition parses the Prometheus text exposition format.
func parseExposition(r io.Reader) ([]Metric, error) {
	var metrics []Metric
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		m, err := parseLine(line)
		if err != nil {
			continue // Skip unparseable lines.
		}
		metrics = append(metrics, m)
	}

	return metrics, scanner.Err()
}

// parseLine parses a single Prometheus exposition line.
// Format: metric_name{label="value",...} value [timestamp]
func parseLine(line string) (Metric, error) {
	var m Metric

	// Split name{labels} from value.
	labelStart := strings.IndexByte(line, '{')
	labelEnd := strings.IndexByte(line, '}')

	if labelStart >= 0 && labelEnd > labelStart {
		m.Name = line[:labelStart]
		m.Labels = parseLabels(line[labelStart+1 : labelEnd])
		line = strings.TrimSpace(line[labelEnd+1:])
	} else {
		// No labels — split on first space.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return m, fmt.Errorf("invalid line")
		}
		m.Name = parts[0]
		line = parts[1]
	}

	// Parse value (ignore optional timestamp).
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return m, fmt.Errorf("missing value")
	}

	val, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return m, fmt.Errorf("parsing value: %w", err)
	}
	m.Value = val

	return m, nil
}

// parseLabels parses label="value",... into a map.
// Label values follow the Prometheus text format: quoted strings with
// escape sequences \", \\, and \n.
func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key := pair[:eq]
		val := unquoteLabel(pair[eq+1:])
		labels[key] = val
	}
	return labels
}

// unquoteLabel strips surrounding quotes and expands Prometheus escape sequences.
func unquoteLabel(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(s)
	}
	return s
}
