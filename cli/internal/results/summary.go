// Package results turns eval/train JSON artifacts into the small, stable
// researcher-facing summary/dashboard shape used by Tau read commands.
package results

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
)

// Summary is the normalized result shape for dashboards and machine-readable
// run summaries. It is intentionally smaller than the source artifact.
type Summary struct {
	SchemaVersion int          `json:"schema_version"`
	Kind          string       `json:"kind"`
	Status        string       `json:"status"`
	GeneratedAt   string       `json:"generated_at,omitempty"`
	Score         *float64     `json:"score,omitempty"`
	ScoreMetric   string       `json:"score_metric,omitempty"`
	ScoreBetter   string       `json:"score_better,omitempty"`
	Headline      []Metric     `json:"headline,omitempty"`
	Metrics       []Metric     `json:"metrics,omitempty"`
	Artifacts     []Artifact   `json:"artifacts,omitempty"`
	Error         *ResultError `json:"error,omitempty"`
}

// Metric is one displayable scalar in a result summary.
type Metric struct {
	Name   string   `json:"name"`
	Value  *float64 `json:"value,omitempty"`
	Text   string   `json:"text,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Better string   `json:"better,omitempty"`
}

// Display returns a human-friendly value for HTML tables.
func (m Metric) Display() string {
	if m.Text != "" {
		return m.Text
	}
	if m.Value == nil {
		return "-"
	}
	return fmt.Sprintf("%.4g", *m.Value)
}

// Artifact is a path or command-visible file associated with the run.
type Artifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ResultError is the compact failure shape surfaced in dashboards.
type ResultError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Summarize normalizes known Tau result JSON contracts.
func Summarize(raw []byte) (Summary, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return Summary{}, fmt.Errorf("parse result json: %w", err)
	}
	switch {
	case stringValue(m["score_metric"]) != "":
		return summarizeFinetune(m), nil
	default:
		return summarizeGeneric(m), nil
	}
}

// SummaryJSON returns the normalized summary as formatted JSON.
func SummaryJSON(raw []byte) ([]byte, error) {
	s, err := Summarize(raw)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// HTML returns a standalone dashboard document for a result artifact.
func HTML(raw []byte) ([]byte, error) {
	s, err := Summarize(raw)
	if err != nil {
		return nil, err
	}
	formattedRaw := raw
	var rawObj any
	if json.Unmarshal(raw, &rawObj) == nil {
		if pretty, err := json.MarshalIndent(rawObj, "", "  "); err == nil {
			formattedRaw = pretty
		}
	}
	data := struct {
		Summary Summary
		Raw     string
	}{
		Summary: s,
		Raw:     string(formattedRaw),
	}
	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func summarizeFinetune(m map[string]any) Summary {
	s := baseSummary("tau-finetune")
	s.Score = numberPtr(m["score"])
	s.ScoreMetric = stringValue(m["score_metric"])
	s.ScoreBetter = stringValue(m["score_better"])
	if errObj, ok := m["error"].(map[string]any); ok {
		s.Status = "FAILED"
		s.Error = &ResultError{Kind: stringValue(errObj["kind"]), Message: stringValue(errObj["message"])}
	} else if s.Score == nil {
		s.Status = "OK (no score)"
	}
	if s.Score != nil {
		s.Headline = append(s.Headline, Metric{Name: s.ScoreMetric, Value: s.Score, Better: s.ScoreBetter})
	}
	if train, ok := m["train_log_summary"].(map[string]any); ok {
		s.Metrics = appendMetric(s.Metrics, "train.final_loss", train["final_loss"], "")
		s.Metrics = appendMetric(s.Metrics, "train.best_step", train["best_step"], "")
		s.Metrics = appendMetric(s.Metrics, "train.steps_completed", train["steps_completed"], "")
	}
	s.Metrics = appendMetric(s.Metrics, "runtime.wall_seconds", m["wall_seconds"], "s")
	s.Metrics = appendMetric(s.Metrics, "runtime.gpu_hours", m["gpu_hours"], "GPU-h")
	if p := stringValue(m["weights_path"]); p != "" {
		s.Artifacts = append(s.Artifacts, Artifact{Name: "weights", Path: p})
	}
	if p := stringValue(m["manifest_path"]); p != "" {
		s.Artifacts = append(s.Artifacts, Artifact{Name: "manifest", Path: p})
	}
	return s
}

func summarizeGeneric(m map[string]any) Summary {
	s := baseSummary("generic-json")
	if generated := stringValue(m["generated_at"]); generated != "" {
		s.GeneratedAt = generated
	}
	if status := stringValue(m["status"]); status != "" {
		s.Status = status
	}
	return s
}

func baseSummary(kind string) Summary {
	return Summary{
		SchemaVersion: 1,
		Kind:          kind,
		Status:        "OK",
	}
}

func appendMetric(metrics []Metric, name string, value any, unit string) []Metric {
	if value == nil {
		return metrics
	}
	if f := numberPtr(value); f != nil {
		return append(metrics, Metric{Name: name, Value: f, Unit: unit})
	}
	if text := stringValue(value); text != "" {
		return append(metrics, Metric{Name: name, Text: text, Unit: unit})
	}
	return metrics
}

func stringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return ""
	}
}

func numberPtr(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case float32:
		f := float64(x)
		return &f
	case int:
		f := float64(x)
		return &f
	case int64:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Tau result: {{.Summary.Kind}}</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 2rem; max-width: 1100px; line-height: 1.45; }
    .card { border: 1px solid #8885; border-radius: 12px; padding: 1rem; margin: 1rem 0; }
    table { border-collapse: collapse; width: 100%; }
    th, td { text-align: left; border-bottom: 1px solid #8884; padding: 0.45rem 0.6rem; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
    pre { overflow: auto; padding: 1rem; border-radius: 8px; background: #8881; }
    .status { font-weight: 700; }
  </style>
</head>
<body>
  <h1>Tau result: {{.Summary.Kind}}</h1>
  <p class="status">Status: {{.Summary.Status}}</p>
  {{with .Summary.Error}}<div class="card"><strong>{{.Kind}}</strong><br>{{.Message}}</div>{{end}}
  {{if .Summary.Headline}}
  <div class="card">
    <h2>Headline</h2>
    <table><tbody>
    {{range .Summary.Headline}}<tr><th>{{.Name}}</th><td>{{.Display}}</td><td>{{.Unit}}</td><td>{{.Better}}</td></tr>{{end}}
    </tbody></table>
  </div>
  {{end}}
  {{if .Summary.Metrics}}
  <div class="card">
    <h2>Metrics</h2>
    <table><thead><tr><th>Name</th><th>Value</th><th>Unit</th></tr></thead><tbody>
    {{range .Summary.Metrics}}<tr><td>{{.Name}}</td><td>{{.Display}}</td><td>{{.Unit}}</td></tr>{{end}}
    </tbody></table>
  </div>
  {{end}}
  {{if .Summary.Artifacts}}
  <div class="card">
    <h2>Artifacts</h2>
    <table><thead><tr><th>Name</th><th>Path</th></tr></thead><tbody>
    {{range .Summary.Artifacts}}<tr><td>{{.Name}}</td><td><code>{{.Path}}</code></td></tr>{{end}}
    </tbody></table>
  </div>
  {{end}}
  <details class="card">
    <summary>Raw JSON</summary>
    <pre>{{.Raw}}</pre>
  </details>
</body>
</html>
`))
