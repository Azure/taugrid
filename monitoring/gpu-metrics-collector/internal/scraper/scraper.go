package scraper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
type ScrapeTarget struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
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

// Scrape fetches metrics from all targets concurrently, returning a unified metric set.
// Targets that are unreachable are logged and skipped.
func (s *Scraper) Scrape(ctx context.Context) ([]Metric, error) {
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

	var all []Metric
	for range s.targets {
		r := <-ch
		if r.err != nil {
			slog.Warn("scrape target unavailable", "target", r.target.Name, "url", r.target.URL, "error", r.err)
			continue
		}
		all = append(all, r.metrics...)
	}
	return all, nil
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
