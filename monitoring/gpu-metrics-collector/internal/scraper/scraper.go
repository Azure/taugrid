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

type Metric struct {
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp time.Time
}

type ScrapeTarget struct {
	Name                  string        `yaml:"name"`
	URL                   string        `yaml:"url"`
	Required              bool          `yaml:"required,omitempty"`
	AvailabilityCondition string        `yaml:"availabilityCondition,omitempty"`
	UnavailableFor        time.Duration `yaml:"unavailableFor,omitempty"`
	AvailableFor          time.Duration `yaml:"availableFor,omitempty"`
}

const (
	DefaultUnavailableFor = 2 * time.Minute
	DefaultAvailableFor   = 1 * time.Minute
)

func (t ScrapeTarget) UnavailableWindow() time.Duration {
	if t.UnavailableFor > 0 {
		return t.UnavailableFor
	}
	return DefaultUnavailableFor
}

func (t ScrapeTarget) AvailableWindow() time.Duration {
	if t.AvailableFor > 0 {
		return t.AvailableFor
	}
	return DefaultAvailableFor
}

type TargetStatus struct {
	Target  ScrapeTarget
	OK      bool
	SafeURL string
	Err     string
}

type ScrapeResult struct {
	Metrics  []Metric
	Statuses []TargetStatus
}

type Scraper struct {
	targets []ScrapeTarget
	client  *http.Client
}

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

func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted-url>"
	}
	safe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return safe.String()
}

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
			continue
		}
		metrics = append(metrics, m)
	}

	return metrics, scanner.Err()
}

func parseLine(line string) (Metric, error) {
	var m Metric

	labelStart := strings.IndexByte(line, '{')
	labelEnd := strings.IndexByte(line, '}')

	if labelStart >= 0 && labelEnd > labelStart {
		m.Name = line[:labelStart]
		m.Labels = parseLabels(line[labelStart+1 : labelEnd])
		line = strings.TrimSpace(line[labelEnd+1:])
	} else {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return m, fmt.Errorf("invalid line")
		}
		m.Name = parts[0]
		line = strings.Join(parts[1:], " ")
	}

	parts := strings.Fields(line)
	if len(parts) == 0 {
		return m, fmt.Errorf("missing value")
	}

	val, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return m, fmt.Errorf("parsing value: %w", err)
	}
	m.Value = val
	if len(parts) > 1 {
		millis, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return m, fmt.Errorf("parsing timestamp: %w", err)
		}
		m.Timestamp = time.UnixMilli(millis)
	}

	return m, nil
}

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

func unquoteLabel(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(s)
	}
	return s
}
