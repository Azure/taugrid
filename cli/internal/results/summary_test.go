// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"strings"
	"testing"
)

func TestSummarizeFinetune(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "score": 0.812,
	  "score_metric": "accuracy",
	  "score_better": "higher",
	  "wall_seconds": 42.0,
	  "gpu_hours": 0.5,
	  "weights_path": "/data/checkpoints/finetunes/x/checkpoints/best.safetensors",
	  "manifest_path": "/data/checkpoints/finetunes/x/manifest.yaml",
	  "train_log_summary": {
	    "final_loss": 0.0123,
	    "best_step": 7,
	    "steps_completed": 9
	  }
	}`)
	s, err := Summarize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != "tau-finetune" || s.Status != "OK" {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if s.Score == nil || *s.Score != 0.812 || s.ScoreMetric != "accuracy" {
		t.Fatalf("unexpected score summary: %+v", s)
	}
	if len(s.Artifacts) != 2 {
		t.Fatalf("artifacts=%v want weights+manifest", s.Artifacts)
	}
}

func TestSummarizeFinetuneNoScore(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "score_metric": "accuracy",
	  "wall_seconds": 12.0
	}`)
	s, err := Summarize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "OK (no score)" {
		t.Fatalf("status=%q want OK (no score)", s.Status)
	}
}

func TestSummarizeFinetuneError(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "score_metric": "accuracy",
	  "error": {"kind": "OOM", "message": "CUDA OOM at step 42"}
	}`)
	s, err := Summarize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "FAILED" || s.Error == nil || s.Error.Kind != "OOM" {
		t.Fatalf("unexpected error summary: %+v", s)
	}
}

func TestSummarizeGenericFallback(t *testing.T) {
	raw := []byte(`{"status": "DONE", "generated_at": "2026-04-27T00:00:00Z"}`)
	s, err := Summarize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != "generic-json" {
		t.Fatalf("kind=%q want generic-json", s.Kind)
	}
	if s.Status != "DONE" {
		t.Fatalf("status=%q want DONE", s.Status)
	}
}

func TestSummaryJSONAndHTML(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "score": 0.5,
	  "score_metric": "f1",
	  "score_better": "higher"
	}`)
	summary, err := SummaryJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), `"kind": "tau-finetune"`) {
		t.Fatalf("summary JSON missing kind:\n%s", summary)
	}
	html, err := HTML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "Tau result: tau-finetune") ||
		!strings.Contains(string(html), "f1") {
		t.Fatalf("HTML missing expected content:\n%s", html)
	}
}
