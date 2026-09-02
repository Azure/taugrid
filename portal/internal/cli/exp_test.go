// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/parquet-go/parquet-go"
	"google.golang.org/protobuf/encoding/protowire"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

func TestExpServeKustoDiscoveryFlags(t *testing.T) {
	opts := defaultExpServeOptions()
	if opts.kustoSince != "" {
		t.Fatalf("legacy kustoSince default = %q, want empty", opts.kustoSince)
	}
	if opts.kustoDiscoverySince != "90d" || opts.kustoMaxDiscoverySince != "365d" || opts.kustoTargetSince != "365d" {
		t.Fatalf("unexpected Kusto discovery defaults: %+v", opts)
	}

	store := ""
	cmd := newExpServeCmd(&store)
	for _, name := range []string{"allowed-project", "featured-project", "discovery-since", "max-discovery-since", "target-since"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("serve command missing --%s", name)
		}
	}
	if got := cmd.Flags().Lookup("kusto-since").DefValue; got != "" {
		t.Fatalf("--kusto-since default = %q, want empty", got)
	}
}

func TestExpLocalStoreWorkflow(t *testing.T) {
	store := filepath.Join(t.TempDir(), "project-alpha-store")
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "experiment-alpha",
		"--project", "project-alpha",
		"--group", "reference-group",
		"--idempotency-key", "init-project-alpha",
		"-o", "json",
	)
	if err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}

	var initResult expstore.InitResult
	if err := json.Unmarshal([]byte(out), &initResult); err != nil {
		t.Fatalf("parse init json: %v\n%s", err, out)
	}
	if initResult.Experiment.ExperimentID != "experiment-alpha" ||
		initResult.Experiment.Project != "project-alpha" ||
		initResult.RunGroup.RunGroupID != "reference-group" {
		t.Fatalf("init json=%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "list", "--kind", "groups", "--json")
	if err != nil {
		t.Fatalf("exp list failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "reference-group") {
		t.Fatalf("list output missing group:\n%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "status", "experiment-alpha", "--json")
	if err != nil {
		t.Fatalf("exp status failed: %v\nstderr:\n%s", err, stderr)
	}
	// An experiment's arms are the groups its runs occupy, so a freshly
	// initialized experiment reports none. This used to report 1: run_groups
	// carried an experiment_id, which asserted that an arm label belongs to one
	// experiment. It does not -- "reference-group" is reusable across
	// experiments -- so ownership now lives on the run, and "runs": 0 with
	// "run_groups": 1 is no longer representable.
	if !strings.Contains(out, `"target_type": "experiment"`) ||
		!strings.Contains(out, `"runs": 0`) || !strings.Contains(out, `"run_groups": 0`) {
		t.Fatalf("status output unexpected:\n%s", out)
	}
	// The group itself is still registered; it just is not an arm of the
	// experiment until a run lands in it.
	groupsOut, stderr, err := executeExpCommand("experiment", "--store", store, "list", "--kind", "groups", "--json")
	if err != nil {
		t.Fatalf("exp list groups failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(groupsOut, "reference-group") {
		t.Fatalf("group registration lost:\n%s", groupsOut)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "sql", "select experiment_id, project from experiments", "--format", "csv")
	if err != nil {
		t.Fatalf("exp sql failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "experiment_id,project") || !strings.Contains(out, "experiment-alpha,project-alpha") {
		t.Fatalf("sql csv unexpected:\n%s", out)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"seed":1,"lr":0.001}`), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(t.TempDir(), "reward.png")
	if err := os.WriteFile(artifactPath, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	trackArgs := []string{
		"experiment", "--store", store,
		"track", "manual-seed-1",
		"--group", "reference-group",
		"--owner", "agent",
		"--config", configPath,
		"--artifact", "plot:reward-curve=" + artifactPath,
		"--tag", "seed=1",
		"--tag", "tau_workspace=research-workspace",
		"--metric", "train/return=42",
		"--step", "10",
		"--idempotency-key", "track-manual-seed-1",
		"--json",
	}
	out, stderr, err = executeExpCommand(trackArgs...)
	if err != nil {
		t.Fatalf("exp track failed: %v\nstderr:\n%s", err, stderr)
	}
	var trackResult struct {
		RunID       string `json:"run_id"`
		Configs     int    `json:"configs"`
		Artifacts   int    `json:"artifacts"`
		Tags        int    `json:"tags"`
		MetricFiles int    `json:"metric_files"`
		MetricRows  int    `json:"metric_rows"`
		Reused      bool   `json:"reused"`
		MetricFile  struct {
			Path string `json:"path"`
		} `json:"metric_file"`
	}
	if err := json.Unmarshal([]byte(out), &trackResult); err != nil {
		t.Fatalf("parse track json: %v\n%s", err, out)
	}
	if trackResult.RunID != "manual-seed-1" || trackResult.Configs != 1 || trackResult.Artifacts != 1 || trackResult.Tags != 2 || trackResult.MetricFiles != 1 || trackResult.MetricRows != 1 || trackResult.MetricFile.Path == "" {
		t.Fatalf("unexpected track json:\n%s", out)
	}
	metricRows, err := parquet.ReadFile[expstore.MetricRow](filepath.Join(store, filepath.FromSlash(trackResult.MetricFile.Path)))
	if err != nil {
		t.Fatalf("read tracked metric file: %v", err)
	}
	if len(metricRows) != 1 {
		t.Fatalf("tracked metric rows = %d, want 1", len(metricRows))
	}
	var metricTags map[string]string
	if err := json.Unmarshal([]byte(metricRows[0].Tags), &metricTags); err != nil {
		t.Fatalf("parse tracked metric tags: %v", err)
	}
	if metricTags["tau_workspace"] != "research-workspace" || metricTags["seed"] != "1" ||
		metricTags["tau.metric.write_version"] != expTrackVersion {
		t.Fatalf("tracked metric tags = %#v", metricTags)
	}
	out, stderr, err = executeExpCommand(trackArgs...)
	if err != nil {
		t.Fatalf("exp track reuse failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"reused": true`) {
		t.Fatalf("track should reuse idempotency key:\n%s", out)
	}
	defaultTrackArgs := []string{
		"experiment", "--store", store,
		"track", "manual-default",
		"--metric", "eval/score=7",
		"--idempotency-key", "track-manual-default",
		"--json",
	}
	if _, stderr, err = executeExpCommand(defaultTrackArgs...); err != nil {
		t.Fatalf("default exp track failed: %v\nstderr:\n%s", err, stderr)
	}
	out, stderr, err = executeExpCommand(defaultTrackArgs...)
	if err != nil {
		t.Fatalf("default exp track reuse failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"reused": true`) {
		t.Fatalf("default track should reuse idempotency key:\n%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "search", "--query", "manual-seed", "--metric-filter", "train/return>=42", "--lifecycle", "succeeded", "--json")
	if err != nil {
		t.Fatalf("exp search failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"run_id": "manual-seed-1"`) || !strings.Contains(out, `"lifecycle_state": "succeeded"`) || !strings.Contains(out, `"train/return"`) {
		t.Fatalf("exp search output missing tracked successful run:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "runs", "--query", "manual-seed", "--metric-filter", "train/return>=42", "--lifecycle", "succeeded", "--json")
	if err != nil {
		t.Fatalf("exp runs alias failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"run_id": "manual-seed-1"`) || !strings.Contains(out, `"lifecycle_state": "succeeded"`) {
		t.Fatalf("exp runs alias output missing tracked successful run:\n%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "experiments", "search", "--query", "project-alpha", "--lifecycle", "succeeded", "--json")
	if err != nil {
		t.Fatalf("exp experiments search failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"experiment_id": "experiment-alpha"`) || !strings.Contains(out, `"run_count": 2`) {
		t.Fatalf("experiment search output missing experiment:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "experiments", "list", "--query", "project-alpha", "--lifecycle", "succeeded", "--json")
	if err != nil {
		t.Fatalf("exp experiments list alias failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"experiment_id": "experiment-alpha"`) || !strings.Contains(out, `"run_count": 2`) {
		t.Fatalf("experiment list alias output missing experiment:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "list", "--kind", "experiments", "--json")
	if err != nil {
		t.Fatalf("exp list --kind experiments failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"experiment_id": "experiment-alpha"`) || !strings.Contains(out, `"lifecycle_counts": "succeeded=2"`) {
		t.Fatalf("experiment kind list output unexpected:\n%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "experiments", "tag-run", "manual-seed-1", "--experiment", "manual-comparison", "--name", "Manual comparison")
	if err != nil {
		t.Fatalf("exp experiments tag-run failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"experiment_id": "manual-comparison"`) {
		t.Fatalf("tag-run output unexpected:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "experiments", "search", "--query", "Manual comparison", "--json")
	if err != nil {
		t.Fatalf("exp experiments search tagged run failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"experiment_id": "manual-comparison"`) || !strings.Contains(out, `"run_count": 1`) {
		t.Fatalf("tagged experiment search output unexpected:\n%s", out)
	}

	out, stderr, err = executeExpCommand("experiment", "--store", store, "status", "manual-seed-1", "--json")
	if err != nil {
		t.Fatalf("exp status tracked run failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{`"target_type": "run"`, `"configs": 1`, `"metric_files": 1`, `"artifacts": 1`} {
		if !strings.Contains(out, want) {
			t.Fatalf("tracked run status missing %s:\n%s", want, out)
		}
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "sql", "select r.run_id, c.format, a.type, t.value from runs r join configs c using (run_id) join artifacts a using (run_id) join tags t on t.scope_id = r.run_id where r.run_id = 'manual-seed-1'", "--format", "csv")
	if err != nil {
		t.Fatalf("exp sql tracked records failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "manual-seed-1,json,plot,1") {
		t.Fatalf("tracked sql csv unexpected:\n%s", out)
	}
	metadataArtifactPath := filepath.Join(t.TempDir(), "prediction-step-7.png")
	if err := os.WriteFile(metadataArtifactPath, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataArtifactSpec, err := json.Marshal(map[string]string{
		"type":      "image",
		"name":      "media/prediction-gallery step 7",
		"uri":       metadataArtifactPath,
		"caption":   "validation examples",
		"direction": "output",
		"alias":     "latest-validation-gallery",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"track", "metadata-seed-1",
		"--artifact", string(metadataArtifactSpec),
		"--runtime", `{"python":"3.13"}`,
		"--dependencies", `{"packages":[{"name":"lightning","version":"2"}]}`,
		"--log-uri", "logs/metadata-seed-1.txt",
		"--idempotency-key", "track-metadata-seed-1",
		"--json",
	); err != nil {
		t.Fatalf("exp track metadata artifact failed: %v\nstderr:\n%s", err, stderr)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "sql", "select run_id, type, name, caption, direction, alias from artifacts where run_id = 'metadata-seed-1'", "--format", "csv")
	if err != nil {
		t.Fatalf("exp sql metadata artifact failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "metadata-seed-1,image,media/prediction-gallery step 7,validation examples,output,latest-validation-gallery") {
		t.Fatalf("metadata artifact sql csv unexpected:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "sql", "select runtime, dependencies, log_uri from run_context where run_id = 'metadata-seed-1'", "--format", "csv")
	if err != nil {
		t.Fatalf("exp sql repro context failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "python") || !strings.Contains(out, "3.13") || !strings.Contains(out, "logs/metadata-seed-1.txt") {
		t.Fatalf("metadata repro context csv unexpected:\n%s", out)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "stellar", "manual-seed-1", "-o", "json")
	if err != nil {
		t.Fatalf("exp stellar tracked run failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{`"configs": [`, configPath, `"artifacts": [`, artifactPath, `"metric_name": "train/return"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("tracked stellar json missing %s:\n%s", want, out)
		}
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "dashboard", "manual-seed-1", "-o", "tui")
	if err != nil {
		t.Fatalf("exp dashboard tui failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{"Stellar dashboard: manual-seed-1 (run)", "SUMMARY", "STATUS", "METRICS", "CHART train/return", "RUNS", "open_dashboard", portalbin.ExperimentCmd + " --store", " open 'manual-seed-1'"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tracked dashboard tui missing %s:\n%s", want, out)
		}
	}
	// The verb moved out of tau, so a dashboard that still prints "tau
	// experiment ..." hands the user a command that no longer resolves.
	if strings.Contains(out, "tau experiment ") {
		t.Fatalf("dashboard tui offers a command on the tau binary, which no longer has the experiment verb:\n%s", out)
	}

	packet := filepath.Join(t.TempDir(), "packet")
	out, stderr, err = executeExpCommand("experiment", "--store", store, "export", "--out", packet, "-o", "json")
	if err != nil {
		t.Fatalf("exp export failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"destination": "`+packet+`"`) {
		t.Fatalf("export json unexpected:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(packet, expstore.AppendLogDir, "configs.jsonl")); err != nil {
		t.Fatalf("export missing configs mirror: %v", err)
	}
}

func TestExperimentImportJSONLUsesRunConfigExperimentDefaults(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "tau.yaml"), []byte(`name: config-defaults
experiment:
  project: nanogpt-fineweb
  title: NanoGPT API surface
  group: safe-stack
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "manifest-default",
		"--project", "manifest-project",
		"--group", "manifest-group",
	); err != nil {
		t.Fatalf("experiment init failed: %v\nstderr:\n%s", err, stderr)
	}
	history := filepath.Join(dir, "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"val_loss":3.27}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"import", "jsonl",
		"--history", history,
		"--dry-run",
		"--json",
	)
	if err != nil {
		t.Fatalf("jsonl import dry-run failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		`"run_id": "config-defaults"`,
		`"project": "nanogpt-fineweb"`,
		`"experiment_id": "nanogpt-api-surface"`,
		`"run_group_id": "safe-stack"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config default output missing %s:\n%s", want, out)
		}
	}

	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"import", "jsonl",
		"--run", "explicit-run",
		"--history", history,
		"--project", "explicit-project",
		"--experiment", "explicit-experiment",
		"--group", "explicit-group",
		"--dry-run",
		"--json",
	)
	if err != nil {
		t.Fatalf("jsonl explicit override dry-run failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		`"run_id": "explicit-run"`,
		`"project": "explicit-project"`,
		`"experiment_id": "explicit-experiment"`,
		`"run_group_id": "explicit-group"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explicit override output missing %s:\n%s", want, out)
		}
	}
}

func TestExpOpenBuildsBrowserURL(t *testing.T) {
	store := filepath.Join(t.TempDir(), "browser-open-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "project-alpha-browser-open",
		"--project", "project-alpha",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}

	previousOpen := openBrowserURL
	t.Cleanup(func() {
		openBrowserURL = previousOpen
	})
	stopAfterCapture := fmt.Errorf("stop after capture")
	var opened string
	openBrowserURL = func(_ context.Context, targetURL string) error {
		opened = targetURL
		return stopAfterCapture
	}

	_, _, err := executeExpCommand(
		"experiment", "--store", store,
		"open", "project-alpha-browser-open",
		"--addr", "127.0.0.1:0",
		"--metric", "train/return",
	)
	if err != stopAfterCapture {
		t.Fatalf("exp open err=%v, want sentinel", err)
	}
	for _, want := range []string{
		"http://127.0.0.1:",
		"/stellar?",
		"target=project-alpha-browser-open",
		"metric=train%2Freturn",
	} {
		if !strings.Contains(opened, want) {
			t.Fatalf("opened URL missing %q: %s", want, opened)
		}
	}
}

func TestExpOpenInfersOutcomeMetric(t *testing.T) {
	store := filepath.Join(t.TempDir(), "browser-open-metric-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "stellar-primary-metric",
		"--project", "stellar-primary-metric",
		"--group", "vision-r3",
		"-o", "json",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	history := filepath.Join(t.TempDir(), "metrics-history.jsonl")
	if err := os.WriteFile(history, []byte(strings.Join([]string{
		`{"_step":1,"_timestamp":1770000000,"train/loss":0.42,"eval/macro_auprc":0.61}`,
		`{"_step":2,"_timestamp":1770000060,"train/loss":0.38,"eval/macro_auprc":0.67}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"import", "jsonl",
		"--run", "stellar-primary-metric-run-1",
		"--group", "vision-r3",
		"--history", history,
		"--source", "stellar-primary-metric-jsonl",
		"--idempotency-key", "jsonl-primary-metric",
		"-o", "json",
	); err != nil {
		t.Fatalf("exp import jsonl failed: %v\nstderr:\n%s", err, stderr)
	}

	previousOpen := openBrowserURL
	t.Cleanup(func() {
		openBrowserURL = previousOpen
	})
	stopAfterCapture := fmt.Errorf("stop after capture")
	var opened string
	openBrowserURL = func(_ context.Context, targetURL string) error {
		opened = targetURL
		return stopAfterCapture
	}

	_, _, err := executeExpCommand(
		"experiment", "--store", store,
		"open", "stellar-primary-metric",
		"--addr", "127.0.0.1:0",
	)
	if err != stopAfterCapture {
		t.Fatalf("exp open err=%v, want sentinel", err)
	}
	if !strings.Contains(opened, "metric=eval%2Fmacro_auprc") {
		t.Fatalf("opened URL should infer primary outcome metric, got %s", opened)
	}

	tui, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"dashboard", "stellar-primary-metric",
		"-o", "tui",
	)
	if err != nil {
		t.Fatalf("exp dashboard tui failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(tui, "open 'stellar-primary-metric' --metric 'eval/macro_auprc'") {
		t.Fatalf("dashboard TUI should link primary outcome metric:\n%s", tui)
	}
}

func TestStellarBrowserURLNormalizesWildcardBindAddress(t *testing.T) {
	got, err := stellarBrowserURL("0.0.0.0:8080", "run one", "eval/score")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8080/stellar?metric=eval%2Fscore&target=run+one"
	if got != want {
		t.Fatalf("url=%q, want %q", got, want)
	}
}

func TestExpStoreEnvResolvesWithoutStoreFlag(t *testing.T) {
	store := filepath.Join(t.TempDir(), "project-alpha-store")
	t.Setenv(expstore.ExpStoreEnv, store)
	t.Setenv(expstore.ExpStoreRootEnv, "")
	t.Setenv(expstore.ExpContextEnv, "")
	t.Setenv(expstore.ExpTeamEnv, "")
	t.Setenv(expstore.ExpProjectEnv, "")

	if _, stderr, err := executeExpCommand(
		"experiment",
		"init", "experiment-alpha",
		"--project", "project-alpha",
	); err != nil {
		t.Fatalf("exp init via env store failed: %v\nstderr:\n%s", err, stderr)
	}
	out, stderr, err := executeExpCommand("experiment", "list", "--kind", "experiments", "--json")
	if err != nil {
		t.Fatalf("exp list via env store failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "experiment-alpha") {
		t.Fatalf("list output missing experiment:\n%s", out)
	}
	out, stderr, err = executeExpCommand(
		"experiment",
		"track", "env-seed",
		"--metric", "eval/score=1",
		"--idempotency-key", "track-env-seed",
		"--json",
	)
	if err != nil {
		t.Fatalf("exp track via env store failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"run_id": "env-seed"`) {
		t.Fatalf("track output missing env-seed:\n%s", out)
	}
}

func TestExpImportJSONLExposesResearchMetricOptions(t *testing.T) {
	store := filepath.Join(t.TempDir(), "captioner-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "captioner-richer-metrics",
		"--project", "captioner",
		"--group", "a100-rerun",
		"-o", "json",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	history := filepath.Join(t.TempDir(), "metrics-history.jsonl")
	if err := os.WriteFile(history, []byte(strings.Join([]string{
		`{"_step":1,"_timestamp":1770000000,"train/loss":0.42,"train/lr":0.0002,"train/grad_norm":1.7,"train/step_time_s":3.1,"train/examples_seen":64,"train/input_tokens":1024,"train/tokens":1024,"gpu/memory_allocated_gb":21.5,"gpu/memory_reserved_gb":24,"gpu/max_memory_allocated_gb":23.75,"checkpoint/file_count":8,"checkpoint/bytes":4096,"feature/image_text_alignment":0.61,"inference/time_s":0.2}`,
		`{"_step":2,"train/loss":0.38,"train/lr":0.0001,"train/tokens":2048,"gpu/memory_allocated_gb":22.1}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"import", "jsonl",
		"--run", "captioner2-stellar-a100-base-rich-v3",
		"--experiment", "captioner-richer-metrics",
		"--group", "a100-rerun",
		"--history", history,
		"--source", "captioner-jsonl",
		"--tag", "dataset=vision",
		"--tag", "tau_workspace=research-workspace",
		"--idempotency-key", "jsonl-captioner-rich",
		"-o", "json",
	)
	if err != nil {
		t.Fatalf("exp import jsonl failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"rows": 18`) || !strings.Contains(out, `"history_files"`) {
		t.Fatalf("jsonl import output unexpected:\n%s", out)
	}
	var importResult struct {
		ExperimentID string `json:"experiment_id"`
		MetricFile   struct {
			Path string `json:"path"`
		} `json:"metric_file"`
	}
	if err := json.Unmarshal([]byte(out), &importResult); err != nil {
		t.Fatalf("parse jsonl import output: %v\n%s", err, out)
	}
	if importResult.ExperimentID != "captioner-richer-metrics" ||
		!strings.Contains(filepath.ToSlash(importResult.MetricFile.Path), "/experiment=captioner-richer-metrics/") {
		t.Fatalf("jsonl import did not retain the experiment assignment: %+v", importResult)
	}
	rows, err := parquet.ReadFile[expstore.MetricRow](filepath.Join(store, filepath.FromSlash(importResult.MetricFile.Path)))
	if err != nil {
		t.Fatal(err)
	}
	var metricTags map[string]string
	if err := json.Unmarshal([]byte(rows[0].Tags), &metricTags); err != nil {
		t.Fatal(err)
	}
	if metricTags["dataset"] != "vision" || metricTags["tau_workspace"] != "research-workspace" {
		t.Fatalf("jsonl import metric tags = %#v", metricTags)
	}
	opened, err := expstore.Open(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	runTags, err := opened.RunTags(context.Background(), []string{"captioner2-stellar-a100-base-rich-v3"})
	if err != nil {
		t.Fatal(err)
	}
	if runTags["captioner2-stellar-a100-base-rich-v3"]["dataset"] != "vision" ||
		runTags["captioner2-stellar-a100-base-rich-v3"]["tau_workspace"] != "research-workspace" {
		t.Fatalf("jsonl import run tags = %#v", runTags)
	}
	out, stderr, err = executeExpCommand("experiment", "--store", store, "stellar", "captioner-richer-metrics", "--metric", "gpu/memory_allocated_gb", "-o", "json")
	if err != nil {
		t.Fatalf("exp stellar json failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		`"metric_name": "gpu/memory_allocated_gb"`,
		`"card": "Systems"`,
		`"name": "train/grad_norm"`,
		`"name": "checkpoint/bytes"`,
		`"name": "feature/image_text_alignment"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Stellar JSON missing %s:\n%s", want, out)
		}
	}

	if _, _, err := executeExpCommand(
		"experiment", "--store", store,
		"import", "jsonl",
		"--run", "captioner-invalid-tag",
		"--history", history,
		"--tag", "missing-equals",
		"--dry-run",
	); err == nil || !strings.Contains(err.Error(), "expected key=value") {
		t.Fatalf("expected malformed --tag error, got %v", err)
	}
}

func TestExpStoreRootContextTeamProjectDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tau-exp")
	t.Setenv(expstore.ExpStoreEnv, "")
	t.Setenv(expstore.ExpStoreRootEnv, root)
	t.Setenv(expstore.ExpContextEnv, "kind-taugrid")
	t.Setenv(expstore.ExpTeamEnv, "research")
	t.Setenv(expstore.ExpProjectEnv, "project-alpha")

	out, stderr, err := executeExpCommand(
		"experiment",
		"init", "experiment-alpha",
		"--project", "project-alpha",
		"-o", "json",
	)
	if err != nil {
		t.Fatalf("exp init via default store failed: %v\nstderr:\n%s", err, stderr)
	}
	wantStore := filepath.Join(root, "kind-taugrid", "research", "project-alpha")
	if !strings.Contains(out, `"store_path": "`+wantStore+`"`) {
		t.Fatalf("init output missing resolved store %q:\n%s", wantStore, out)
	}
}

func TestExpStoreMissingConfigErrorIsActionable(t *testing.T) {
	t.Setenv(expstore.ExpStoreEnv, "")
	t.Setenv(expstore.ExpStoreRootEnv, "")
	t.Setenv(expstore.ExpContextEnv, "")
	t.Setenv(expstore.ExpTeamEnv, "")
	t.Setenv(expstore.ExpProjectEnv, "")

	_, _, err := executeExpCommand("experiment", "list")
	if err == nil {
		t.Fatal("expected missing store configuration error")
	}
	for _, want := range []string{"--store", expstore.ExpStoreEnv, expstore.ExpStoreRootEnv, expstore.ExpContextEnv, expstore.ExpTeamEnv, expstore.ExpProjectEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing error %q in %q", want, err.Error())
		}
	}
}

func TestExpExportADXCommandWritesLocalProjection(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "project-alpha-store")
	store, _, err := expstore.Init(ctx, storePath, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce candidate model sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(128)
	gpuCount := int64(1)
	metricRel := "metrics/project=project-alpha/run=seed-1/part.parquet"
	metricStep := int64(1)
	metricPath := filepath.Join(store.Root, filepath.FromSlash(metricRel))
	if err := os.MkdirAll(filepath.Dir(metricPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(metricPath, []expstore.MetricRow{{
		Project:    "project-alpha",
		RunGroupID: "reference-group",
		RunID:      "seed-1",
		MetricName: "train/return",
		Step:       &metricStep,
		Value:      42,
		Source:     "track",
		Tags:       "{}",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "agent",
			CreatedAt:  "2026-05-16T00:00:00Z",
			ResultURI:  "artifacts/seed-1",
		},
		RunContext: &expstore.RunContextRecord{
			RunID:        "seed-1",
			Cluster:      "kind-taugrid",
			Namespace:    "ray",
			Team:         "research",
			Profile:      "research-train-gpu",
			Lane:         "training",
			ClusterQueue: "taugrid-training",
			GPUClass:     "a100",
			GPUCount:     &gpuCount,
		},
		Artifacts: []expstore.ArtifactRecord{{
			ArtifactID: "artifact-seed-1",
			RunID:      "seed-1",
			Type:       "log",
			URI:        "artifacts/seed-1/train.log",
			Name:       "train.log",
			SizeBytes:  &size,
			CreatedAt:  "2026-05-16T00:00:00Z",
		}},
		MetricFiles: []expstore.MetricFileRecord{{
			FileID:        "metrics-seed-1",
			Path:          metricRel,
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    "reference-group",
			RunID:         "seed-1",
			RowCount:      2,
			CreatedAt:     "2026-05-16T00:00:01Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservation(ctx, expstore.RecordObservationOptions{
		Observation: expstore.ObservationRecord{
			ObservationID: "obs-seed-1",
			Author:        "researcher",
			Source:        "human",
			Type:          "decision",
			ScopeType:     "run",
			ScopeID:       "seed-1",
			Text:          "Keep seed-1.",
			CreatedAt:     "2026-05-16T01:00:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "adx")
	out, stderr, err := executeExpCommand("experiment", "--store", storePath, "export", "adx", "--out", outDir, "--format", "jsonl", "-o", "json")
	if err != nil {
		t.Fatalf("exp export adx failed: %v\nstderr:\n%s", err, stderr)
	}
	var result expstore.ADXExportResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse adx export json: %v\n%s", err, out)
	}
	if result.ProjectionVersion != expstore.ADXProjectionVersion || result.Mode != "local-files" {
		t.Fatalf("unexpected adx export result: %+v", result)
	}
	runsJSONL, err := os.ReadFile(filepath.Join(outDir, "TauExpRuns.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runsJSONL), `"run_id":"seed-1"`) || !strings.Contains(string(runsJSONL), `"source_store_id":"tau-exp-`) {
		t.Fatalf("runs JSONL missing projected row metadata:\n%s", runsJSONL)
	}
	schemaKQL, err := os.ReadFile(filepath.Join(outDir, "schema.kql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schemaKQL), ".create-merge table TauExpRuns") {
		t.Fatalf("schema KQL missing runs table:\n%s", schemaKQL)
	}

	csvDir := filepath.Join(t.TempDir(), "adx-csv")
	if _, stderr, err := executeExpCommand("experiment", "--store", storePath, "export", "adx", "--out", csvDir, "--format", "csv"); err != nil {
		t.Fatalf("exp export adx csv failed: %v\nstderr:\n%s", err, stderr)
	}
	runsCSV, err := os.ReadFile(filepath.Join(csvDir, "TauExpRuns.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(runsCSV), "exported_at,source_store_id,source_store_path,source_schema_version,projection_version,run_id") {
		t.Fatalf("runs CSV missing stable header:\n%s", runsCSV)
	}

	dryRun, stderr, err := executeExpCommand("experiment", "--store", storePath, "export", "adx", "--dry-run", "-o", "json")
	if err != nil {
		t.Fatalf("exp export adx dry-run failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(dryRun, `"mode": "dry-run"`) || !strings.Contains(dryRun, `"name": "TauExpObservations"`) {
		t.Fatalf("dry-run json unexpected:\n%s", dryRun)
	}
}

func TestExpSQLRejectsMutation(t *testing.T) {
	store := filepath.Join(t.TempDir(), "project-alpha-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "experiment-alpha",
		"--project", "project-alpha",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	_, _, err := executeExpCommand("experiment", "--store", store, "sql", "delete from experiments")
	if err == nil {
		t.Fatal("expected mutating SQL to fail")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error should mention read-only, got %v", err)
	}
}

func TestExpOffloadMetricsCommand(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "sample-project-metrics",
		"--project", "sample-project",
		"--group", "reference-group",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"track", "seed-1",
		"--project", "sample-project",
		"--group", "reference-group",
		"--metric", "train/return=42",
		"--step", "1",
		"--idempotency-key", "track-seed-1",
	); err != nil {
		t.Fatalf("exp track failed: %v\nstderr:\n%s", err, stderr)
	}

	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	outDir := filepath.Join(t.TempDir(), "spool")
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"offload", "metrics",
		"--out", outDir,
		"--remote-write-endpoint", server.URL,
		"--json",
	)
	if err != nil {
		t.Fatalf("exp offload metrics failed: %v\nstderr:\n%s", err, stderr)
	}
	var result struct {
		Rows           int    `json:"rows"`
		MetricsFile    string `json:"metrics_file"`
		CheckpointFile string `json:"checkpoint_file"`
		RemoteWrite    *struct {
			Samples int  `json:"samples"`
			Reused  bool `json:"reused"`
		} `json:"remote_write"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse metrics offload json: %v\n%s", err, out)
	}
	if result.Rows != 1 || result.MetricsFile == "" || result.CheckpointFile == "" || result.RemoteWrite == nil || result.RemoteWrite.Samples != 1 {
		t.Fatalf("unexpected metrics offload result:\n%s", out)
	}
	metrics, err := os.ReadFile(filepath.Join(outDir, "TauExpMetrics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metrics), `"metric_name":"train/return"`) || !strings.Contains(string(metrics), `"source_store_id":"`) {
		t.Fatalf("unexpected native metrics spool:\n%s", metrics)
	}
	checkpointBefore, err := os.ReadFile(filepath.Join(outDir, "metrics_offload_checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}

	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"offload", "metrics",
		"--out", outDir,
		"--remote-write-endpoint", server.URL,
		"--json",
	)
	if err != nil {
		t.Fatalf("second exp offload metrics failed: %v\nstderr:\n%s", err, stderr)
	}
	var second struct {
		RemoteWrite *struct {
			Reused bool `json:"reused"`
		} `json:"remote_write"`
	}
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("parse second metrics offload json: %v\n%s", err, out)
	}
	checkpointAfter, err := os.ReadFile(filepath.Join(outDir, "metrics_offload_checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checkpointExportedAt(t, checkpointBefore), checkpointExportedAt(t, checkpointAfter)) {
		t.Fatalf("exported_at changed across metrics offload checkpoint rewrites:\nbefore=%s\nafter=%s", checkpointBefore, checkpointAfter)
	}
	if second.RemoteWrite == nil || !second.RemoteWrite.Reused || atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("remote-write checkpoint should skip unchanged replay: requests=%d out=%s", atomic.LoadInt32(&requests), out)
	}
}

func TestOpenMetricsOffloadStoreInitializesOnlineStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "store")
	storeHandle, err := openMetricsOffloadStore(context.Background(), &storePath, metricsOffloadOptions{
		History:    []string{"history.jsonl"},
		Project:    "vit-enc-vision",
		RunGroupID: "demo-experiment",
	})
	if err != nil {
		t.Fatalf("open metrics offload store: %v", err)
	}
	defer storeHandle.Close()

	manifest := storeHandle.Manifest()
	if manifest.Project != "vit-enc-vision" {
		t.Fatalf("store manifest not initialized from metrics sidecar args: %+v", manifest)
	}
}

func TestExpOffloadMetricsWatchUsesSidecarEnvDefaults(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/return":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "spool")
	t.Setenv(expstore.ExpStoreEnv, store)
	t.Setenv("TAU_METRICS_HISTORY", history)
	t.Setenv(metricsOffloadRunEnv, "vision-demo")
	t.Setenv(metricsOffloadProjectEnv, "vit-enc-vision")
	t.Setenv(metricsOffloadGroupEnv, "demo-experiment")
	t.Setenv(metricsOffloadTagsEnv, `{"dataset":"vision","recipe":"vit-enc"}`)
	t.Setenv(metricsOffloadOutEnv, outDir)
	t.Setenv(metricsOffloadIntervalEnv, "5s")

	out, stderr, err := executeExpCommand("experiment", "offload", "metrics", "--watch", "--max-iterations", "1", "--json")
	if err != nil {
		t.Fatalf("online metrics offload with env defaults failed: %v\nstderr:\n%s", err, stderr)
	}
	var results []metricsOffloadResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("parse online metrics watch json: %v\n%s", err, out)
	}
	if len(results) != 1 || results[0].ImportRows != 1 || results[0].Rows != 1 {
		t.Fatalf("unexpected env-default online metrics result: %+v", results)
	}
	opened, err := expstore.Open(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	tags, err := opened.RunTags(context.Background(), []string{"vision-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if tags["vision-demo"]["dataset"] != "vision" || tags["vision-demo"]["recipe"] != "vit-enc" {
		t.Fatalf("sidecar env tags not recorded: %+v", tags)
	}
}

func TestExpOffloadMetricsOnlineJSONLTailsOnlyNewLines(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "tau.yaml"), []byte(`name: online-defaults
experiment:
  project: config-project
  title: Config experiment
  group: config-group
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "online-metrics",
		"--project", "sample-project",
		"--group", "reference-group",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/return":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "online-spool")
	runOnline := func() metricsOffloadResult {
		out, stderr, err := executeExpCommand(
			"experiment", "--store", store,
			"offload", "metrics",
			"--history", history,
			"--out", outDir,
			"--json",
		)
		if err != nil {
			t.Fatalf("online metrics offload failed: %v\nstderr:\n%s", err, stderr)
		}
		var result metricsOffloadResult
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("parse online metrics json: %v\n%s", err, out)
		}
		return result
	}

	first := runOnline()
	if first.ImportRows != 1 || first.Rows != 1 || len(first.ImportedMetricFiles) != 1 {
		t.Fatalf("unexpected first online result: %+v", first)
	}
	firstMetrics, err := os.ReadFile(first.MetricsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"run_id":"online-defaults"`, `"project":"config-project"`, `"run_group_id":"config-group"`} {
		if !strings.Contains(string(firstMetrics), want) {
			t.Fatalf("online metrics offload missing config default %s:\n%s", want, firstMetrics)
		}
	}
	checkpointRaw, err := os.ReadFile(filepath.Join(outDir, "metrics_jsonl_checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpointShape map[string]json.RawMessage
	if err := json.Unmarshal(checkpointRaw, &checkpointShape); err != nil {
		t.Fatalf("parse JSONL checkpoint: %v\n%s", err, checkpointRaw)
	}
	if len(checkpointShape) != 3 {
		t.Fatalf("checkpoint top-level field count = %d, want 3 in %s", len(checkpointShape), checkpointRaw)
	}
	var schemaVersion string
	if err := json.Unmarshal(checkpointShape["schema_version"], &schemaVersion); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != metricsJSONLCheckpointSchemaVersion {
		t.Fatalf("checkpoint schema version = %q, want %q", schemaVersion, metricsJSONLCheckpointSchemaVersion)
	}
	var checkpointFiles map[string]json.RawMessage
	if err := json.Unmarshal(checkpointShape["files"], &checkpointFiles); err != nil {
		t.Fatal(err)
	}
	if _, ok := checkpointFiles[history]; !ok {
		t.Fatalf("checkpoint files missing history path %q: %s", history, checkpointShape["files"])
	}
	if _, ok := checkpointShape["updated_at"]; !ok {
		t.Fatalf("checkpoint missing updated_at: %s", checkpointRaw)
	}
	f, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"_step":2,"_timestamp":1770000001,"train/return":43}` + "\n" + `{"_step":3,"_timestamp":1770000002,"train/return":44`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	second := runOnline()
	if second.ImportRows != 1 || second.Rows != 1 {
		t.Fatalf("second online import should only process the new complete line: %+v", second)
	}
	third := runOnline()
	if third.ImportRows != 0 || third.Rows != 0 {
		t.Fatalf("partial trailing line should not be imported: %+v", third)
	}
	f, err = os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fourth := runOnline()
	if fourth.ImportRows != 1 || fourth.Rows != 1 {
		t.Fatalf("completed trailing line should import once: %+v", fourth)
	}
	replacement := fmt.Sprintf(`{"_step":4,"_timestamp":1770000003,"train/return":45,"padding":"%s"}`+"\n", strings.Repeat("x", 4096))
	if err := os.WriteFile(history, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	replaced := runOnline()
	if replaced.ImportRows != 1 || replaced.Rows != 1 {
		t.Fatalf("same-path replacement should reset the tail checkpoint and import new rows: %+v", replaced)
	}

	fullOut := filepath.Join(t.TempDir(), "full-spool")
	out, stderr, err := executeExpCommand("experiment", "--store", store, "offload", "metrics", "--out", fullOut, "--json")
	if err != nil {
		t.Fatalf("full metrics offload failed: %v\nstderr:\n%s", err, stderr)
	}
	var full struct {
		Rows int `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &full); err != nil {
		t.Fatalf("parse full metrics offload: %v\n%s", err, out)
	}
	if full.Rows != 4 {
		t.Fatalf("online tailing should have imported exactly four metric rows, got %d\n%s", full.Rows, out)
	}
}

func TestMetricsOffloadImmutableChunksPublishOnceAndFreshSessionsBaseline(t *testing.T) {
	historyDir := t.TempDir()
	historyPattern := filepath.Join(historyDir, "metrics-history-attempt-*", "*.jsonl")
	chunkPath := func(attempt, chunk int) string {
		return filepath.Join(historyDir, fmt.Sprintf("metrics-history-attempt-%d", attempt), fmt.Sprintf("chunk-%06d.jsonl", chunk))
	}
	openChunk := func(attempt, chunk, step int) (*os.File, string, string) {
		t.Helper()
		final := chunkPath(attempt, chunk)
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			t.Fatal(err)
		}
		temp := final + ".tmp"
		f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf("{\"_step\":%d,\"_timestamp\":17700000%02d,\"train/loss\":%d}\n", step, step, step)
		if _, err := f.WriteString(raw); err != nil {
			f.Close()
			t.Fatal(err)
		}
		return f, temp, final
	}
	publishChunk := func(attempt, chunk, step int) string {
		t.Helper()
		f, temp, final := openChunk(attempt, chunk, step)
		if err := f.Sync(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, final); err != nil {
			t.Fatal(err)
		}
		return final
	}
	publishChunk(0, 1, 99)
	inProgress, temp, final := openChunk(0, 2, 1)

	sessionA := metricsOffloadOptions{
		History:                 []string{historyPattern},
		RunID:                   "same-config-name",
		Project:                 "pretraining",
		RunGroupID:              "bounded",
		Source:                  "stellar-online",
		Out:                     filepath.Join(t.TempDir(), "session-a"),
		BaselineExistingHistory: true,
		ReadyFile:               filepath.Join(t.TempDir(), "session-a-ready"),
	}
	if err := prepareMetricsOffloadWatch(sessionA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionA.ReadyFile); err != nil {
		t.Fatalf("fresh session did not publish readiness after baseline: %v", err)
	}
	storeA := newTestMetricsOffloadStore(t, "session-a")
	ignored, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if ignored.ImportRows != 0 {
		t.Fatalf("open nonmatching temp file or baselined chunk was imported: %+v", ignored)
	}
	if err := inProgress.Sync(); err != nil {
		inProgress.Close()
		t.Fatal(err)
	}
	if err := inProgress.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, final); err != nil {
		t.Fatal(err)
	}
	first, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if first.ImportRows != 1 {
		t.Fatalf("atomically published chunk was not imported exactly once: %+v", first)
	}
	repeated, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ImportRows != 0 {
		t.Fatalf("published chunk replayed in the same telemetry session: %+v", repeated)
	}

	publishChunk(0, 3, 2)
	unique, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if unique.ImportRows != 1 {
		t.Fatalf("new unique chunk was not imported exactly once: %+v", unique)
	}

	if err := prepareMetricsOffloadWatch(sessionA); err != nil {
		t.Fatal(err)
	}
	retry, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ImportRows != 0 {
		t.Fatalf("retry replayed previously published chunks: %+v", retry)
	}
	publishChunk(1, 1, 3)
	retryNew, err := runMetricsOffloadOnce(context.Background(), storeA, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if retryNew.ImportRows != 1 {
		t.Fatalf("retry attempt's unique chunk was not imported once: %+v", retryNew)
	}

	sessionB := sessionA
	sessionB.Out = filepath.Join(t.TempDir(), "session-b")
	sessionB.ReadyFile = filepath.Join(t.TempDir(), "session-b-ready")
	if err := prepareMetricsOffloadWatch(sessionB); err != nil {
		t.Fatal(err)
	}
	storeB := newTestMetricsOffloadStore(t, "session-b")
	baselined, err := runMetricsOffloadOnce(context.Background(), storeB, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if baselined.ImportRows != 0 {
		t.Fatalf("fresh session imported chunks published before readiness: %+v", baselined)
	}
	publishChunk(2, 1, 4)
	fresh, err := runMetricsOffloadOnce(context.Background(), storeB, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ImportRows != 1 {
		t.Fatalf("fresh session did not import only the post-readiness chunk: %+v", fresh)
	}
	freshRepeat, err := runMetricsOffloadOnce(context.Background(), storeB, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if freshRepeat.ImportRows != 0 {
		t.Fatalf("fresh session replayed its published chunk: %+v", freshRepeat)
	}
}

func TestMetricsOffloadOnlinePreservesRequiredStepAndTimestamp(t *testing.T) {
	const step = int64(9007199254740993)
	const timestamp = int64(1770000000)
	history := filepath.Join(t.TempDir(), "chunk-000001.jsonl")
	row := fmt.Sprintf(`{"_step":%d,"_timestamp":%d,"train/loss":1.25}`, step, timestamp)
	if err := os.WriteFile(history, []byte(row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		requestBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result, err := runMetricsOffloadOnce(context.Background(), newTestMetricsOffloadStore(t, "valid-online"), metricsOffloadOptions{
		History:    []string{history},
		RunID:      "valid-online",
		Project:    "pretraining",
		RunGroupID: "bounded",
		Source:     "stellar-online",
		Out:        filepath.Join(t.TempDir(), "out"),
		RemoteWrite: remoteWriteConfig{
			Endpoint: server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteWriteSamples != 1 {
		t.Fatalf("remote-write samples = %d, want 1", result.RemoteWriteSamples)
	}
	labels, timestampMS := decodeFirstRemoteWriteSeriesForTest(t, requestBody)
	if labels["step"] != strconv.FormatInt(step, 10) || timestampMS != timestamp*1000 {
		t.Fatalf("valid row labels=%v timestamp=%d, want step %d at %d", labels, timestampMS, step, timestamp*1000)
	}
}

func TestMetricsOffloadOnlineRejectsRowsWithoutHostedFields(t *testing.T) {
	tests := []struct {
		name      string
		row       string
		wantError string
	}{
		{name: "missing step", row: `{"_timestamp":1770000000,"train/loss":1.25}`, wantError: "missing required numeric _step field"},
		{name: "missing timestamp", row: `{"_step":7,"train/loss":1.25}`, wantError: "missing required numeric _timestamp field"},
		{name: "step wrong type", row: `{"_step":"7","_timestamp":1770000000,"train/loss":1.25}`, wantError: "_step must be numeric"},
		{name: "timestamp wrong type", row: `{"_step":7,"_timestamp":"1770000000","train/loss":1.25}`, wantError: "_timestamp must be numeric"},
		{name: "non-finite timestamp", row: `{"_step":7,"_timestamp":1e309,"train/loss":1.25}`, wantError: "_timestamp must be a finite number"},
		{name: "invalid timestamp", row: `{"_step":7,"_timestamp":0,"train/loss":1.25}`, wantError: "_timestamp must be a valid positive Unix epoch-seconds value"},
		{name: "non-finite metric", row: `{"_step":7,"_timestamp":1770000000,"train/loss":1e309}`, wantError: `metric "train/loss" must be a finite number`},
		{name: "trailing invalid JSON", row: `{"_step":7,"_timestamp":1770000000,"train/loss":1.25} garbage`, wantError: "row must contain exactly one JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := filepath.Join(t.TempDir(), "chunk-000001.jsonl")
			if err := os.WriteFile(history, []byte(tt.row+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&requests, 1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			storeName := strings.ToLower(strings.ReplaceAll(tt.name, " ", "-"))
			result, err := runMetricsOffloadOnce(context.Background(), newTestMetricsOffloadStore(t, storeName), metricsOffloadOptions{
				History:    []string{history},
				RunID:      "invalid-online",
				Project:    "pretraining",
				RunGroupID: "bounded",
				Source:     "stellar-online",
				Out:        filepath.Join(t.TempDir(), "out"),
				RemoteWrite: remoteWriteConfig{
					Endpoint: server.URL,
				},
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), "line 1") {
				t.Fatalf("error = %v, want line 1 error containing %q", err, tt.wantError)
			}
			if result.RemoteWriteSamples != 0 || result.Rows != 0 || atomic.LoadInt32(&requests) != 0 {
				t.Fatalf("invalid row produced telemetry: result=%+v requests=%d", result, requests)
			}
		})
	}
}

func TestMetricsOffloadWatchSkipsEmptyAndNonScalarHistoryUntilScalarsArrive(t *testing.T) {
	historyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(historyDir, "empty.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(historyDir, "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":0,"_timestamp":1770000000,"phase":"starting"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out")
	completionFile := filepath.Join(t.TempDir(), "done")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan error, 1)
	go func() {
		checkpoint := filepath.Join(out, "metrics_jsonl_checkpoint.json")
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(checkpoint); err == nil {
				f, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
				if err == nil {
					_, err = f.WriteString(`{"_step":1,"_timestamp":1770000001,"train/loss":1.25}` + "\n")
					if closeErr := f.Close(); err == nil {
						err = closeErr
					}
				}
				if err == nil {
					err = os.WriteFile(completionFile, nil, 0o644)
				}
				published <- err
				return
			}
			select {
			case <-ctx.Done():
				published <- ctx.Err()
				return
			case <-ticker.C:
			}
		}
	}()

	results, err := runMetricsOffloadWatch(ctx, newTestMetricsOffloadStore(t, "transient-empty-history"), metricsOffloadOptions{
		History:        []string{filepath.Join(historyDir, "*.jsonl")},
		RunID:          "transient-empty-history",
		Project:        "pretraining",
		RunGroupID:     "bounded",
		Source:         "stellar-online",
		Out:            out,
		CompletionFile: completionFile,
	}, 10*time.Millisecond, 100, nil)
	cancel()
	publishErr := <-published
	if err != nil {
		t.Fatalf("watch exited on non-scalar history: %v", err)
	}
	if publishErr != nil {
		t.Fatal(publishErr)
	}
	if len(results) < 2 || results[0].ImportRows != 0 {
		t.Fatalf("non-scalar history was not treated as an empty iteration: %+v", results)
	}
	var imported int
	for _, result := range results {
		imported += result.ImportRows
	}
	if imported != 1 || !results[len(results)-1].Completed {
		t.Fatalf("later scalar history was not offloaded on completion: %+v", results)
	}
}

func TestMetricsOffloadCompletionFinalizesUnterminatedRow(t *testing.T) {
	history := filepath.Join(t.TempDir(), "chunk-000001.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":7,"_timestamp":1770000000,"train/loss":1.25}`), 0o644); err != nil {
		t.Fatal(err)
	}
	completionFile := filepath.Join(t.TempDir(), "done")
	if err := os.WriteFile(completionFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	results, err := runMetricsOffloadWatch(context.Background(), newTestMetricsOffloadStore(t, "final-unterminated"), metricsOffloadOptions{
		History:        []string{history},
		RunID:          "final-unterminated",
		Project:        "pretraining",
		RunGroupID:     "bounded",
		Source:         "stellar-online",
		Out:            filepath.Join(t.TempDir(), "out"),
		CompletionFile: completionFile,
		RemoteWrite: remoteWriteConfig{
			Endpoint: server.URL,
		},
	}, time.Hour, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Completed || results[0].ImportRows != 1 || results[0].Rows != 2 || results[0].RemoteWriteSamples != 2 {
		t.Fatalf("unexpected final drain result: %+v", results)
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Fatalf("final drain requests = %d, want scalar plus terminal status", requests)
	}
}

func TestMetricsOffloadOnlineValidationPreventsFalseTerminalStatus(t *testing.T) {
	history := filepath.Join(t.TempDir(), "chunk-000001.jsonl")
	if err := os.WriteFile(history, []byte(`{"step":7,"train/loss":1.25}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completionFile := filepath.Join(t.TempDir(), "done")
	if err := os.WriteFile(completionFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	results, err := runMetricsOffloadWatch(context.Background(), newTestMetricsOffloadStore(t, "invalid-complete"), metricsOffloadOptions{
		History:        []string{history},
		RunID:          "invalid-complete",
		Project:        "pretraining",
		RunGroupID:     "bounded",
		Source:         "stellar-online",
		Out:            filepath.Join(t.TempDir(), "out"),
		CompletionFile: completionFile,
		RemoteWrite: remoteWriteConfig{
			Endpoint: server.URL,
		},
	}, time.Millisecond, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "missing required numeric _step field") {
		t.Fatalf("error = %v, want missing _step validation failure", err)
	}
	if len(results) != 0 || atomic.LoadInt32(&requests) != 0 {
		t.Fatalf("invalid completed history emitted a terminal result: results=%+v requests=%d", results, requests)
	}
}

func TestExpOffloadMetricsWatchEmitsCompletionStatusMarker(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "online-metrics-status",
		"--project", "sample-project",
		"--group", "reference-group",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/return":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completionFile := filepath.Join(t.TempDir(), "done.json")
	if err := os.WriteFile(completionFile, []byte(`{"state":"failed","reason":"OOMKilled","message":"worker pod exited","completed_at":"2026-02-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	outDir := filepath.Join(t.TempDir(), "online-spool")
	doneFile := filepath.Join(t.TempDir(), "published")
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"offload", "metrics",
		"--history", history,
		"--run", "seed-1",
		"--project", "sample-project",
		"--group", "reference-group",
		"--tag", "tau_workspace=sample",
		"--out", outDir,
		"--watch",
		"--interval", "1ms",
		"--completion-file", completionFile,
		"--done-file", doneFile,
		"--remote-write-endpoint", server.URL,
		"--json",
	)
	if err != nil {
		t.Fatalf("online metrics watch failed: %v\nstderr:\n%s", err, stderr)
	}
	var results []metricsOffloadResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("parse watch metrics json: %v\n%s", err, out)
	}
	if len(results) != 1 || !results[0].Completed || results[0].StatusState != "failed" || results[0].Rows != 2 || results[0].RemoteWriteSamples != 2 {
		t.Fatalf("unexpected watch completion result: %+v\n%s", results, out)
	}
	statusRaw, err := os.ReadFile(results[0].StatusMetricsFile)
	if err != nil {
		t.Fatal(err)
	}
	var statusRow struct {
		MetricName string  `json:"metric_name"`
		Value      float64 `json:"value"`
		Tags       string  `json:"tags"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(statusRaw), &statusRow); err != nil {
		t.Fatalf("parse status marker: %v\n%s", err, statusRaw)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(statusRow.Tags), &tags); err != nil {
		t.Fatalf("parse status marker tags: %v\n%s", err, statusRow.Tags)
	}
	if statusRow.MetricName != expkusto.RunStatusMetricName || statusRow.Value != -1 || tags[expkusto.RunStatusStateTag] != "failed" || tags[expkusto.RunStatusReasonTag] != "OOMKilled" || tags["tau_workspace"] != "sample" {
		t.Fatalf("unexpected status marker row: row=%+v tags=%+v", statusRow, tags)
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Fatalf("expected metric chunk and status marker remote writes, got %d", atomic.LoadInt32(&requests))
	}
	if raw, err := os.ReadFile(doneFile); err != nil || string(raw) != "done\n" {
		t.Fatalf("terminal publication acknowledgement = %q, %v", raw, err)
	}
}

func TestExpOffloadMetricsWatchEmitsSucceededStatusMarker(t *testing.T) {
	store := newTestMetricsOffloadStore(t, "online-metrics-success")
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/return":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	completionFile := filepath.Join(t.TempDir(), "done.json")
	if err := os.WriteFile(completionFile, []byte(`{"state":"succeeded","reason":"rayjob-entrypoint-exit","completed_at":"2026-02-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := runMetricsOffloadWatch(context.Background(), store, metricsOffloadOptions{
		History:        []string{history},
		RunID:          "seed-1",
		Project:        "sample-project",
		RunGroupID:     "reference-group",
		Out:            filepath.Join(t.TempDir(), "online-spool"),
		CompletionFile: completionFile,
		Source:         "stellar-online",
	}, time.Hour, 0, nil)
	if err != nil {
		t.Fatalf("online metrics watch failed: %v", err)
	}
	if len(results) != 1 || !results[0].Completed || results[0].StatusState != "succeeded" || results[0].Rows != 2 {
		t.Fatalf("unexpected watch completion result: %+v", results)
	}
	statusRow, tags := readMetricsStatusRowForTest(t, results[0].StatusMetricsFile)
	if statusRow.MetricName != expkusto.RunStatusMetricName || statusRow.Value != 1 || tags[expkusto.RunStatusStateTag] != "succeeded" || tags[expkusto.RunStatusReasonTag] != "rayjob-entrypoint-exit" {
		t.Fatalf("unexpected status marker row: row=%+v tags=%+v", statusRow, tags)
	}
}

func TestPublishMetricsOffloadDoneAcknowledgesGracefulShutdownStatus(t *testing.T) {
	doneFile := filepath.Join(t.TempDir(), "done")
	err := publishMetricsOffloadDone(doneFile, []metricsOffloadResult{{
		Completed:   false,
		StatusState: "cancelled",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(doneFile); err != nil || string(raw) != "done\n" {
		t.Fatalf("graceful shutdown acknowledgement = %q, %v", raw, err)
	}
}

func TestExpOffloadMetricsStatusRetryIsContentAddressed(t *testing.T) {
	store := newTestMetricsOffloadStore(t, "online-metrics-retry")
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/loss":1.0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completionFile := filepath.Join(t.TempDir(), "done.json")
	outDir := filepath.Join(t.TempDir(), "online-spool")
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	run := func(statusJSON string) metricsOffloadResult {
		t.Helper()
		if err := os.WriteFile(completionFile, []byte(statusJSON+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		results, err := runMetricsOffloadWatch(context.Background(), store, metricsOffloadOptions{
			History:        []string{history},
			RunID:          "seed-retry",
			Project:        "sample-project",
			RunGroupID:     "retry",
			Tags:           []string{"tau_workspace=sample", "tau_retry_attempt=2"},
			Out:            outDir,
			CompletionFile: completionFile,
			Source:         "stellar-online",
			ArtifactURI:    "/data/sample/seed-retry",
			CheckpointURI:  "/data/sample/seed-retry/checkpoints",
			RemoteWrite: remoteWriteConfig{
				Endpoint: server.URL,
			},
		}, time.Hour, 0, nil)
		if err != nil {
			t.Fatalf("online metrics retry failed: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("unexpected retry results: %+v", results)
		}
		return results[0]
	}

	failed := run(`{"state":"failed","reason":"OOMKilled","completed_at":"2026-02-01T00:00:00Z"}`)
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("initial run requests = %d, want scalar + failed status", got)
	}
	failedRow, failedTags := readMetricsStatusRowForTest(t, failed.StatusMetricsFile)
	if failedTags[expkusto.RunStatusStateTag] != "failed" ||
		failedTags[expkusto.RunStatusArtifactURITag] != "/data/sample/seed-retry" ||
		failedTags[expkusto.RunStatusCheckpointURITag] != "/data/sample/seed-retry/checkpoints" {
		t.Fatalf("unexpected failed status tags: %+v", failedTags)
	}

	succeeded := run(`{"state":"succeeded","reason":"job-entrypoint-exit","completed_at":"2026-02-01T00:05:00Z"}`)
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("retry requests = %d, want one new succeeded status without scalar replay", got)
	}
	succeededRow, succeededTags := readMetricsStatusRowForTest(t, succeeded.StatusMetricsFile)
	if succeededTags[expkusto.RunStatusStateTag] != "succeeded" {
		t.Fatalf("unexpected succeeded status tags: %+v", succeededTags)
	}
	if failedRow.MetricFileID == succeededRow.MetricFileID {
		t.Fatalf("failed and succeeded observations reused checkpoint identity %q", failedRow.MetricFileID)
	}

	repeated := run(`{"state":"succeeded","reason":"job-entrypoint-exit","completed_at":"2026-02-01T00:05:00Z"}`)
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("identical retry requests = %d, want no duplicate remote write", got)
	}
	repeatedRow, _ := readMetricsStatusRowForTest(t, repeated.StatusMetricsFile)
	if repeatedRow.MetricFileID != succeededRow.MetricFileID {
		t.Fatalf("identical terminal observation changed identity: %q != %q", repeatedRow.MetricFileID, succeededRow.MetricFileID)
	}
}

func TestExpOffloadMetricsWatchShutdownEmitsCancelledStatusWithoutSentinel(t *testing.T) {
	store := newTestMetricsOffloadStore(t, "online-metrics-shutdown")
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/return":42}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	shutdown := make(chan metricsCompletionStatus, 1)
	shutdown <- metricsSidecarShutdownStatus("SIGTERM")
	results, err := runMetricsOffloadWatch(context.Background(), store, metricsOffloadOptions{
		History:        []string{history},
		RunID:          "seed-1",
		Project:        "sample-project",
		RunGroupID:     "reference-group",
		Out:            filepath.Join(t.TempDir(), "online-spool"),
		CompletionFile: filepath.Join(t.TempDir(), "missing-completion.json"),
		Source:         "stellar-online",
		RemoteWrite: remoteWriteConfig{
			Endpoint: server.URL,
		},
	}, time.Hour, 0, shutdown)
	if err != nil {
		t.Fatalf("online metrics watch shutdown failed: %v", err)
	}
	if len(results) != 1 || results[0].Completed || results[0].StatusState != "cancelled" || results[0].Rows != 2 || results[0].RemoteWriteSamples != 2 {
		t.Fatalf("unexpected watch shutdown result: %+v", results)
	}
	statusRow, tags := readMetricsStatusRowForTest(t, results[0].StatusMetricsFile)
	if statusRow.MetricName != expkusto.RunStatusMetricName || statusRow.Value != -2 || tags[expkusto.RunStatusStateTag] != "cancelled" || tags[expkusto.RunStatusReasonTag] != "sidecar-shutdown" || !strings.Contains(tags[expkusto.RunStatusMessageTag], "SIGTERM") {
		t.Fatalf("unexpected shutdown status marker row: row=%+v tags=%+v", statusRow, tags)
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Fatalf("expected metric chunk and shutdown status marker remote writes, got %d", atomic.LoadInt32(&requests))
	}
}

func TestExpOffloadShutdownWaitsForWorkloadCompletion(t *testing.T) {
	store := newTestMetricsOffloadStore(t, "online-metrics-shutdown-wait")
	history := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(history, []byte(`{"_step":1,"_timestamp":1770000000,"train/loss":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completion := filepath.Join(t.TempDir(), "completion.json")
	go func() {
		time.Sleep(50 * time.Millisecond)
		file, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = file.WriteString(`{"_step":2,"_timestamp":1770000001,"train/loss":0.5}` + "\n")
			_ = file.Close()
		}
		_ = os.WriteFile(completion, []byte(`{"state":"succeeded","reason":"workload-entrypoint-exit"}`+"\n"), 0o644)
	}()
	shutdown := make(chan metricsCompletionStatus, 1)
	shutdown <- metricsSidecarShutdownStatus("SIGTERM")
	results, err := runMetricsOffloadWatch(context.Background(), store, metricsOffloadOptions{
		History:                []string{history},
		RunID:                  "seed-wait",
		Project:                "sample-project",
		RunGroupID:             "reference-group",
		Out:                    filepath.Join(t.TempDir(), "online-spool"),
		CompletionFile:         completion,
		Source:                 "stellar-online",
		ShutdownCompletionWait: time.Second,
	}, time.Hour, 0, shutdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Completed || results[0].StatusState != "succeeded" || results[0].Rows != 3 {
		t.Fatalf("shutdown wait result = %+v", results)
	}
}

func checkpointExportedAt(t *testing.T, raw []byte) []byte {
	t.Helper()
	var checkpoint struct {
		ExportedAt string `json:"exported_at"`
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatalf("parse checkpoint: %v\n%s", err, raw)
	}
	return []byte(checkpoint.ExportedAt)
}

func TestExpOffloadMetricsAgentManifestCommand(t *testing.T) {
	out, stderr, err := executeExpCommand(
		"experiment",
		"offload", "metrics-agent",
		"--name", "sample-training-metrics-agent",
		"--namespace", "research",
		"--image", "tau:test",
		"--pvc", "blob-training",
		"--store", "/data/tau-exp/sample-training",
		"--history", "/data/outputs/sample-training/history.jsonl",
		"--run", "sample-training-seed-1",
		"--project", "sample-project",
		"--experiment", "sample-training-scaling",
		"--group", "reference-group",
		"--tag", "dataset=sample-training",
		"--completion-file", "/data/outputs/sample-training/done",
		"--max-iterations", "5",
		"--node-selector", "kubernetes.azure.com/mode=system",
	)
	if err != nil {
		t.Fatalf("exp offload metrics-agent failed: %v\nstderr:\n%s", err, stderr)
	}
	manifest := parseAgentDeploymentManifest(t, out)
	container := requireAgentDeployment(t, manifest, "sample-training-metrics-agent", "research", "tau-metrics-agent", "metrics-agent", "tau-metrics", "blob-training")
	requireArgsContain(t, container.Args,
		"--store", "/data/tau-exp/sample-training",
		"offload", "metrics",
		"--history", "/data/outputs/sample-training/history.jsonl",
		"--experiment", "sample-training-scaling",
		"--tag", "dataset=sample-training",
		"--completion-file", "/data/outputs/sample-training/done",
		"--max-iterations", "5",
		"--remote-write-endpoint", "http://${NODE_IP}:3100/receive",
	)
	requireVolumeMount(t, container, "tmp", "/tmp")
	requireVolumeMount(t, container, "tau-metrics", "/data")
	if !container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("readOnlyRootFilesystem = false, want true")
	}
	if got := manifest.Spec.Template.Spec.NodeSelector["kubernetes.azure.com/mode"]; got != "system" {
		t.Fatalf("node selector = %q, want system", got)
	}
}

func TestExpOffloadArtifactsCommand(t *testing.T) {
	store := filepath.Join(t.TempDir(), "artifact-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "image-resnet",
		"--project", "vision",
		"--group", "h200",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	artifactPath := filepath.Join(t.TempDir(), "confusion-matrix.png")
	if err := os.WriteFile(artifactPath, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"track", "resnet-run-1",
		"--project", "vision",
		"--group", "h200",
		"--artifact", "image:confusion-matrix.png="+artifactPath,
		"--json",
	); err != nil {
		t.Fatalf("exp track failed: %v\nstderr:\n%s", err, stderr)
	}
	objectRoot := filepath.Join(t.TempDir(), "objects")
	spool := filepath.Join(t.TempDir(), "spool")
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"offload", "artifacts",
		"--run", "resnet-run-1",
		"--out", spool,
		"--object-root", objectRoot,
		"--json",
	)
	if err != nil {
		t.Fatalf("exp offload artifacts failed: %v\nstderr:\n%s", err, stderr)
	}
	var result struct {
		Uploaded   int    `json:"uploaded"`
		Verified   int    `json:"verified"`
		Indexed    int    `json:"indexed"`
		Failed     int    `json:"failed"`
		Checkpoint string `json:"checkpoint"`
		Artifacts  []struct {
			ObjectURI string `json:"object_uri"`
			Status    string `json:"status"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse artifact offload json: %v\n%s", err, out)
	}
	if result.Uploaded != 1 || result.Verified != 1 || result.Indexed != 1 || result.Failed != 0 || len(result.Artifacts) != 1 || result.Artifacts[0].Status != "uploaded" {
		t.Fatalf("unexpected artifact offload result:\n%s", out)
	}
	if result.Checkpoint == "" {
		t.Fatalf("checkpoint missing:\n%s", out)
	}
	if _, err := os.Stat(result.Checkpoint); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}
	if !strings.HasPrefix(result.Artifacts[0].ObjectURI, "file://") {
		t.Fatalf("object URI = %q, want file://", result.Artifacts[0].ObjectURI)
	}
	ctx := context.Background()
	storeHandle, err := expstore.Open(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer storeHandle.Close()
	artifacts, err := storeHandle.ArtifactsForRun(ctx, "resnet-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].DurableRef == "" || artifacts[0].ContentType == "" || artifacts[0].Digest == "" || artifacts[0].SizeBytes == nil {
		t.Fatalf("artifact durable metadata missing after offload: %+v", artifacts)
	}
}

func TestExpOffloadArtifactsWatchContinuesOnPartialFailure(t *testing.T) {
	store := filepath.Join(t.TempDir(), "artifact-store")
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"init", "image-resnet",
		"--project", "vision",
		"--group", "h200",
	); err != nil {
		t.Fatalf("exp init failed: %v\nstderr:\n%s", err, stderr)
	}
	artifactPath := filepath.Join(t.TempDir(), "missing-after-track.png")
	if err := os.WriteFile(artifactPath, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"track", "resnet-run-1",
		"--project", "vision",
		"--group", "h200",
		"--artifact", "image:missing-after-track.png="+artifactPath,
		"--json",
	); err != nil {
		t.Fatalf("exp track failed: %v\nstderr:\n%s", err, stderr)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"offload", "artifacts",
		"--run", "resnet-run-1",
		"--out", filepath.Join(t.TempDir(), "spool"),
		"--object-root", filepath.Join(t.TempDir(), "objects"),
		"--watch",
		"--max-iterations", "1",
		"--json",
	)
	if err != nil {
		t.Fatalf("watch should report partial failures without exiting: %v\nstderr:\n%s", err, stderr)
	}
	var results []struct {
		Failed  int `json:"failed"`
		Indexed int `json:"indexed"`
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("parse watch artifact offload json: %v\n%s", err, out)
	}
	if len(results) != 1 || results[0].Failed != 1 || results[0].Indexed != 0 {
		t.Fatalf("unexpected watch partial result:\n%s", out)
	}
}

func TestExpOffloadArtifactsAgentManifestCommand(t *testing.T) {
	out, stderr, err := executeExpCommand(
		"experiment",
		"offload", "artifacts-agent",
		"--name", "resnet-artifacts-agent",
		"--namespace", "research",
		"--image", "tau:test",
		"--pvc", "blob-training",
		"--store", "/data/tau-exp/resnet",
		"--run", "resnet-run-1",
		"--out", "/data/tau-artifacts-offload",
		"--object-root", "/data/tau-object-store",
		"--object-base-uri", "file:///data/tau-object-store",
		"--completion-file", "/data/tau-artifacts.done",
		"--max-iterations", "5",
		"--node-selector", "kubernetes.azure.com/mode=system",
	)
	if err != nil {
		t.Fatalf("exp offload artifacts-agent failed: %v\nstderr:\n%s", err, stderr)
	}
	manifest := parseAgentDeploymentManifest(t, out)
	container := requireAgentDeployment(t, manifest, "resnet-artifacts-agent", "research", "tau-artifacts-agent", "artifacts-agent", "tau-artifacts", "blob-training")
	requireArgsContain(t, container.Args,
		"--store", "/data/tau-exp/resnet",
		"offload", "artifacts",
		"--run", "resnet-run-1",
		"--out", "/data/tau-artifacts-offload",
		"--object-root", "/data/tau-object-store",
		"--object-base-uri", "file:///data/tau-object-store",
		"--completion-file", "/data/tau-artifacts.done",
		"--max-iterations", "5",
	)
	requireVolumeMount(t, container, "tmp", "/tmp")
	requireVolumeMount(t, container, "tau-artifacts", "/data")
	if !container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("readOnlyRootFilesystem = false, want true")
	}
	if got := manifest.Spec.Template.Spec.NodeSelector["kubernetes.azure.com/mode"]; got != "system" {
		t.Fatalf("node selector = %q, want system", got)
	}
}

func TestExpOffloadArtifactsAgentManifestAzBlobCommand(t *testing.T) {
	out, stderr, err := executeExpCommand(
		"experiment",
		"offload", "artifacts-agent",
		"--name", "resnet-artifacts-agent",
		"--namespace", "research",
		"--image", "tau:test",
		"--pvc", "blob-training",
		"--store", "/data/tau-exp/resnet",
		"--run", "resnet-run-1",
		"--out", "/data/tau-artifacts-offload",
		"--object-store", "azblob",
		"--account", "acct",
		"--container", "tau-artifacts",
		"--account-url", "https://acct.blob.core.windows.net",
		"--object-base-uri", "azblob://acct.blob.core.windows.net/tau-artifacts",
		"--completion-file", "/data/tau-artifacts.done",
		"--max-iterations", "5",
	)
	if err != nil {
		t.Fatalf("exp offload artifacts-agent azblob failed: %v\nstderr:\n%s", err, stderr)
	}
	manifest := parseAgentDeploymentManifest(t, out)
	container := requireAgentDeployment(t, manifest, "resnet-artifacts-agent", "research", "tau-artifacts-agent", "artifacts-agent", "tau-artifacts", "blob-training")
	requireArgsContain(t, container.Args,
		"--store", "/data/tau-exp/resnet",
		"offload", "artifacts",
		"--run", "resnet-run-1",
		"--out", "/data/tau-artifacts-offload",
		"--object-store", "azblob",
		"--account", "acct",
		"--container", "tau-artifacts",
		"--account-url", "https://acct.blob.core.windows.net",
		"--object-base-uri", "azblob://acct.blob.core.windows.net/tau-artifacts",
		"--completion-file", "/data/tau-artifacts.done",
		"--max-iterations", "5",
	)
	for _, arg := range container.Args {
		if arg == "--object-root" {
			t.Fatalf("azblob agent args should not include --object-root: %+v", container.Args)
		}
	}
}

func parseAgentDeploymentManifest(t *testing.T, raw string) agentDeploymentManifest {
	t.Helper()
	var manifest agentDeploymentManifest
	if err := yaml.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, raw)
	}
	return manifest
}

func requireAgentDeployment(t *testing.T, manifest agentDeploymentManifest, name, namespace, appName, containerName, volumeName, claimName string) agentContainer {
	t.Helper()
	if manifest.APIVersion != "apps/v1" || manifest.Kind != "Deployment" {
		t.Fatalf("manifest type = %s %s, want apps/v1 Deployment", manifest.APIVersion, manifest.Kind)
	}
	if manifest.Metadata.Name != name || manifest.Metadata.Namespace != namespace {
		t.Fatalf("metadata = %s/%s, want %s/%s", manifest.Metadata.Namespace, manifest.Metadata.Name, namespace, name)
	}
	if got := manifest.Metadata.Labels["app.kubernetes.io/name"]; got != appName {
		t.Fatalf("app label = %q, want %q", got, appName)
	}
	container := firstAgentContainer(t, manifest)
	if len(container.Args) == 0 || container.Args[0] != "experiment" {
		t.Fatalf("agent args must use the public experiment root: %+v", container.Args)
	}
	if container.Name != containerName {
		t.Fatalf("container name = %q, want %q", container.Name, containerName)
	}
	requirePersistentVolumeClaim(t, manifest, volumeName, claimName)
	return container
}

func firstAgentContainer(t *testing.T, manifest agentDeploymentManifest) agentContainer {
	t.Helper()
	containers := manifest.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	return containers[0]
}

func requireArgsContain(t *testing.T, args []string, wants ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, arg := range args {
		seen[arg] = true
	}
	for _, want := range wants {
		if !seen[want] {
			t.Fatalf("args missing %q in %#v", want, args)
		}
	}
}

func requireVolumeMount(t *testing.T, container agentContainer, name, mountPath string) {
	t.Helper()
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			if mount.MountPath != mountPath {
				t.Fatalf("mount %s path = %q, want %q", name, mount.MountPath, mountPath)
			}
			return
		}
	}
	t.Fatalf("mount %s missing in %#v", name, container.VolumeMounts)
}

func requirePersistentVolumeClaim(t *testing.T, manifest agentDeploymentManifest, name, claimName string) {
	t.Helper()
	for _, volume := range manifest.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			if volume.PersistentVolumeClaim == nil {
				t.Fatalf("volume %s has no persistentVolumeClaim", name)
			}
			if volume.PersistentVolumeClaim.ClaimName != claimName {
				t.Fatalf("volume %s claim = %q, want %q", name, volume.PersistentVolumeClaim.ClaimName, claimName)
			}
			return
		}
	}
	t.Fatalf("volume %s missing in %#v", name, manifest.Spec.Template.Spec.Volumes)
}

func TestExpKustoMetricsQueryCommand(t *testing.T) {
	out, stderr, err := executeExpCommand(
		"experiment",
		"kusto", "metrics-query",
		"--project", "sample-project",
		"--group", "reference-group",
		"--run", "seed-1",
		"--metric", "train/return",
		"--target-points", "10000",
	)
	if err != nil {
		t.Fatalf("exp kusto metrics-query failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"TauExpMetrics",
		"| where ['project'] == 'sample-project'",
		"| where run_group_id == 'reference-group'",
		"| where run_id in ('seed-1')",
		"step_bucket=bin(step, step_bin)",
		"summarize arg_min(value, *)",
		"summarize arg_max(value, *)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("query missing %q:\n%s", want, out)
		}
	}

	out, stderr, err = executeExpCommand(
		"experiment",
		"kusto", "metrics-query",
		"--project", "sample-project",
		"--group", "reference-group",
		"--run", "seed-1",
		"--metric", "train/return",
		"--ingestion", "remote-write",
	)
	if err != nil {
		t.Fatalf("exp kusto remote-write metrics-query failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"ExperimentMetrics",
		"Labels.metric_name",
		"| where ['project'] == 'sample-project'",
		"| where run_group_id == 'reference-group'",
		"| where run_id in ('seed-1')",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("remote-write query missing %q:\n%s", want, out)
		}
	}

	out, stderr, err = executeExpCommand(
		"experiment",
		"kusto", "metrics-query",
		"--project", "sample-project",
		"--ingestion", "remote-write",
		"--json",
	)
	if err != nil {
		t.Fatalf("exp kusto metrics-query json failed: %v\nstderr:\n%s", err, stderr)
	}
	var queryResult struct {
		Endpoint string `json:"endpoint"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal([]byte(out), &queryResult); err != nil {
		t.Fatalf("parse kusto metrics-query json: %v\n%s", err, out)
	}
	if queryResult.Endpoint != "https://example.kusto.windows.net" || queryResult.Database != "Metrics" {
		t.Fatalf("unexpected kusto defaults: %+v", queryResult)
	}

	out, stderr, err = executeExpCommand(
		"experiment",
		"kusto", "schema",
		"--ingestion", "all",
	)
	if err != nil {
		t.Fatalf("exp kusto schema failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		".create-merge table TauExpMetrics",
		"TauExpMetricsDashboardRows",
		".create-merge table ExperimentMetrics",
		"ExperimentMetricsDashboardRows",
		"Prometheus metric experiment_metrics",
		"Cluster: string",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("schema output missing %q:\n%s", want, out)
		}
	}
}

func TestExpStellarCommand(t *testing.T) {
	store := setupRLStellarStore(t)

	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"observe",
		"--scope", "run:baseline-seed-1",
		"--type", "decision",
		"--text", "Prefer ablation after seed envelope improved.",
		"--author", "researcher",
		"--source", "human",
		"--evidence", `{"metric":"train/return","command":"tau experiment compare experiment-alpha --metric train/return --format jsonl"}`,
		"--idempotency-key", "note-baseline-seed-1",
		"--json",
	)
	if err != nil {
		t.Fatalf("exp observe failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"created": true`) {
		t.Fatalf("observe json unexpected:\n%s", out)
	}
	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"observe",
		"--scope", "run:baseline-seed-1",
		"--type", "decision",
		"--text", "Prefer ablation after seed envelope improved.",
		"--author", "researcher",
		"--source", "human",
		"--evidence", `{"metric":"train/return","command":"tau experiment compare experiment-alpha --metric train/return --format jsonl"}`,
		"--idempotency-key", "note-baseline-seed-1",
		"--json",
	)
	if err != nil {
		t.Fatalf("exp observe reuse failed: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, `"reused": true`) {
		t.Fatalf("observe should reuse idempotency key:\n%s", out)
	}

	html, stderr, err := executeExpCommand("experiment", "--store", store, "stellar", "experiment-alpha", "-o", "html")
	if err != nil {
		t.Fatalf("exp stellar failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"Tau experiment",
		"Research loop summary",
		"Current answer",
		"Best evidence",
		"Next action",
		"Run-group compare",
		"Best current group is candidate-group on train/return",
		"Outlier seeds",
		"Event markers",
		"Runtime/config diffs",
		"Canonical evidence cards",
		"Outcome",
		"World model",
		"world_model/perplexity",
		"train/loss",
		"Behavior",
		"Systems",
		"Observation notebook",
		"Copy SQL",
		"Copy next command",
		"Export PNG",
		"Export Parquet packet",
		"reference-group",
		"candidate-group",
		"baseline-seed-3",
		"below group envelope",
		"World-model loss spike aligns with the reward collapse.",
		"tau submit candidate-training --group candidate-group --set seed=7",
		"Prefer ablation after seed envelope improved.",
		"Queue wait",
		"a100",
		"not collected",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stellar html missing %q\n%s", want, html)
		}
	}
	jsonOut, stderr, err := executeExpCommand("experiment", "--store", store, "stellar", "experiment-alpha", "-o", "json")
	if err != nil {
		t.Fatalf("exp stellar json failed: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		`"schema_version": "tau.exp.cockpit.v0"`,
		`"status": "answered"`,
		`"best_group_id": "candidate-group"`,
		`"next_command": "tau submit candidate-training --group candidate-group --set seed=7"`,
		`"outliers": [`,
		`"event_markers": [`,
		`"runtime_diffs": [`,
	} {
		if !strings.Contains(jsonOut, want) {
			t.Fatalf("stellar json missing %s:\n%s", want, jsonOut)
		}
	}
	if !strings.Contains(jsonOut, `"seed_coverage": "6 runs across 2 run groups (candidate-group=3, reference-group=3)"`) {
		t.Fatalf("stellar json unexpected:\n%s", jsonOut)
	}
}

func TestExpCompareAndPlotCommands(t *testing.T) {
	store := setupComparePlotStore(t)
	out, stderr, err := executeExpCommand(
		"experiment", "--store", store,
		"compare", "experiment-alpha",
		"--metric", "train/return",
		"--json",
	)
	if err != nil {
		t.Fatalf("exp compare json failed: %v\nstderr:\n%s", err, stderr)
	}
	wantJSON := `{
  "schema_version": "tau.exp.compare.v0",
  "target": "experiment-alpha",
  "target_type": "experiment",
  "metric_name": "train/return",
  "direction": "max",
  "best_group_id": "candidate-group",
  "best_run_id": "seed-3",
  "run_groups": 2,
  "runs": 3,
  "metric_files": 3,
  "groups": [
    {
      "run_group_id": "candidate-group",
      "run_count": 1,
      "latest_step": 2,
      "min": 30,
      "p25": 30,
      "median": 30,
      "p75": 30,
      "max": 30,
      "best_value": 30,
      "best_run_id": "seed-3"
    },
    {
      "run_group_id": "reference-group",
      "run_count": 2,
      "latest_step": 2,
      "min": 20,
      "p25": 20,
      "median": 22,
      "p75": 22,
      "max": 22,
      "best_value": 22,
      "best_run_id": "seed-2"
    }
  ],
  "run_values": [
    {
      "run_id": "seed-3",
      "run_group_id": "candidate-group",
      "state": "succeeded",
      "owner": "jsonl-import",
      "latest_step": 2,
      "value": 30
    },
    {
      "run_id": "seed-1",
      "run_group_id": "reference-group",
      "state": "succeeded",
      "owner": "jsonl-import",
      "latest_step": 2,
      "value": 20
    },
    {
      "run_id": "seed-2",
      "run_group_id": "reference-group",
      "state": "succeeded",
      "owner": "jsonl-import",
      "latest_step": 2,
      "value": 22
    }
  ]
}
`
	if out != wantJSON {
		t.Fatalf("compare json mismatch\nwant:\n%s\ngot:\n%s", wantJSON, out)
	}

	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"compare", "experiment-alpha",
		"--metric", "train/return",
		"--format", "csv",
	)
	if err != nil {
		t.Fatalf("exp compare csv failed: %v\nstderr:\n%s", err, stderr)
	}
	wantCSV := "metric_name,direction,run_group_id,run_count,latest_step,min,p25,median,p75,max,best_value,best_run_id,winner\n" +
		"train/return,max,candidate-group,1,2,30,30,30,30,30,30,seed-3,true\n" +
		"train/return,max,reference-group,2,2,20,20,22,22,22,22,seed-2,false\n"
	if out != wantCSV {
		t.Fatalf("compare csv mismatch\nwant:\n%s\ngot:\n%s", wantCSV, out)
	}

	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"compare", "experiment-alpha",
		"--metric", "train/return",
		"--format", "jsonl",
	)
	if err != nil {
		t.Fatalf("exp compare jsonl failed: %v\nstderr:\n%s", err, stderr)
	}
	wantJSONL := "{\"best_run_id\":\"seed-3\",\"best_value\":30,\"direction\":\"max\",\"latest_step\":2,\"max\":30,\"median\":30,\"metric_name\":\"train/return\",\"min\":30,\"p25\":30,\"p75\":30,\"run_count\":1,\"run_group_id\":\"candidate-group\",\"winner\":true}\n" +
		"{\"best_run_id\":\"seed-2\",\"best_value\":22,\"direction\":\"max\",\"latest_step\":2,\"max\":22,\"median\":22,\"metric_name\":\"train/return\",\"min\":20,\"p25\":20,\"p75\":22,\"run_count\":2,\"run_group_id\":\"reference-group\",\"winner\":false}\n"
	if out != wantJSONL {
		t.Fatalf("compare jsonl mismatch\nwant:\n%s\ngot:\n%s", wantJSONL, out)
	}

	plotPath := filepath.Join(t.TempDir(), "train-return.svg")
	out, stderr, err = executeExpCommand(
		"experiment", "--store", store,
		"plot", "experiment-alpha",
		"--metric", "train/return",
		"--out", plotPath,
		"--json",
	)
	if err != nil {
		t.Fatalf("exp plot failed: %v\nstderr:\n%s", err, stderr)
	}
	var plotResult struct {
		SchemaVersion string `json:"schema_version"`
		Target        string `json:"target"`
		MetricName    string `json:"metric_name"`
		Out           string `json:"out"`
		Bytes         int    `json:"bytes"`
		Series        int    `json:"series"`
	}
	if err := json.Unmarshal([]byte(out), &plotResult); err != nil {
		t.Fatalf("parse plot json: %v\n%s", err, out)
	}
	if plotResult.SchemaVersion != "tau.exp.plot.v0" || plotResult.Target != "experiment-alpha" ||
		plotResult.MetricName != "train/return" || plotResult.Out != plotPath || plotResult.Bytes == 0 || plotResult.Series != 3 {
		t.Fatalf("plot json unexpected:\n%s", out)
	}
	svg, err := os.ReadFile(plotPath)
	if err != nil {
		t.Fatalf("plot artifact was not written: %v", err)
	}
	for _, want := range []string{"<svg", "Tau experiment plot: train/return", "seed-1", "seed-2", "seed-3"} {
		if !strings.Contains(string(svg), want) {
			t.Fatalf("plot svg missing %q\n%s", want, string(svg))
		}
	}
}

func setupRLStellarStore(t *testing.T) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "project-alpha-store")
	for _, tc := range []struct {
		group string
	}{
		{group: "reference-group"},
		{group: "candidate-group"},
	} {
		if _, stderr, err := executeExpCommand(
			"experiment", "--store", store,
			"init", "experiment-alpha",
			"--project", "project-alpha",
			"--group", tc.group,
		); err != nil {
			t.Fatalf("exp init %s failed: %v\nstderr:\n%s", tc.group, err, stderr)
		}
	}
	metricSet := func(returnValue, wmLoss, perplexity, trainLoss, entropy, gpuUtil float64) []cliScalar {
		return []cliScalar{
			{tag: "train/return", step: 1, wallTime: 10, value: returnValue - 10},
			{tag: "train/return", step: 2, wallTime: 20, value: returnValue},
			{tag: "world_model/loss", step: 2, wallTime: 20, value: wmLoss},
			{tag: "world_model/perplexity", step: 2, wallTime: 20, value: perplexity},
			{tag: "train/loss", step: 2, wallTime: 20, value: trainLoss},
			{tag: "policy/entropy", step: 2, wallTime: 20, value: entropy},
			{tag: "system/gpu_util", step: 2, wallTime: 20, value: gpuUtil},
		}
	}
	type runFixture struct {
		id       string
		group    string
		seed     int
		values   []cliScalar
		profile  string
		config   string
		queue    float64
		gpuHours float64
		events   []expstore.EventRecord
	}
	runs := []runFixture{
		{id: "baseline-seed-1", group: "reference-group", seed: 1, values: metricSet(64, 0.82, 22.4, 1.8, 1.10, 82), profile: "training-s", config: "config-baseline", queue: 12.5, gpuHours: 0.72},
		{id: "baseline-seed-2", group: "reference-group", seed: 2, values: metricSet(68, 0.76, 20.1, 1.6, 1.16, 84), profile: "training-s", config: "config-baseline", queue: 11.0, gpuHours: 0.74},
		{id: "baseline-seed-3", group: "reference-group", seed: 3, values: metricSet(25, 2.40, 61.8, 4.3, 0.42, 38), profile: "training-s", config: "config-baseline", queue: 31.0, gpuHours: 0.52, events: []expstore.EventRecord{{
			EventID:  "event-baseline-seed-3-oom",
			RunID:    "baseline-seed-3",
			Time:     "2026-05-16T02:30:00Z",
			Type:     "oom",
			Source:   "kubernetes",
			Severity: "warning",
			Message:  "Container restarted after world-model memory spike.",
		}}},
		{id: "ablation-seed-1", group: "candidate-group", seed: 1, values: metricSet(78, 0.64, 16.2, 1.2, 1.22, 86), profile: "training-m", config: "config-ablation", queue: 19.0, gpuHours: 0.81},
		{id: "ablation-seed-2", group: "candidate-group", seed: 2, values: metricSet(83, 0.59, 14.9, 1.1, 1.25, 88), profile: "training-m", config: "config-ablation", queue: 20.5, gpuHours: 0.83},
		{id: "ablation-seed-3", group: "candidate-group", seed: 3, values: metricSet(86, 0.57, 14.1, 1.0, 1.28, 89), profile: "training-m", config: "config-ablation", queue: 21.0, gpuHours: 0.84, events: []expstore.EventRecord{{
			EventID:  "event-ablation-seed-3-checkpoint",
			RunID:    "ablation-seed-3",
			Time:     "2026-05-16T03:10:00Z",
			Type:     "checkpoint",
			Source:   "tau",
			Severity: "info",
			Message:  "Best checkpoint uploaded.",
		}}},
	}
	for _, run := range runs {
		history := filepath.Join(t.TempDir(), run.id+".jsonl")
		if err := writeCLIJSONLScalars(history, run.values); err != nil {
			t.Fatal(err)
		}
		if _, stderr, err := executeExpCommand(
			"experiment", "--store", store,
			"import", "jsonl",
			"--run", run.id,
			"--group", run.group,
			"--history", history,
			"--idempotency-key", "jsonl-"+run.id,
			"--json",
		); err != nil {
			t.Fatalf("import %s failed: %v\nstderr:\n%s", run.id, err, stderr)
		}
	}

	ctx := context.Background()
	storeHandle, err := expstore.Open(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer storeHandle.Close()
	resultURIs := map[string]string{}
	runRows, err := storeHandle.Query(ctx, "select run_id, result_uri from runs")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range runRows.Rows {
		resultURIs[fmt.Sprint(row["run_id"])] = fmt.Sprint(row["result_uri"])
	}
	gpuCount := int64(1)
	for _, run := range runs {
		resultURI := resultURIs[run.id]
		if _, err := storeHandle.RecordRunData(ctx, expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      run.id,
				Project:    "project-alpha",
				RunGroupID: run.group,
				State:      "succeeded",
				Owner:      "jsonl-import",
				ResultURI:  resultURI,
			},
			Configs: []expstore.ConfigRecord{{
				ConfigHash:     run.config,
				RunID:          run.id,
				Format:         "yaml",
				URI:            "artifacts/" + run.id + "/config.yaml",
				NormalizedJSON: fmt.Sprintf(`{"seed":%d,"group":"%s"}`, run.seed, run.group),
				IndexedFields:  fmt.Sprintf(`{"replay_buffer":"%s"}`, run.config),
			}},
			Artifacts: []expstore.ArtifactRecord{{
				ArtifactID: "video-" + run.id,
				RunID:      run.id,
				Type:       "video",
				URI:        "artifacts/" + run.id + "/rollout.mp4",
				Name:       "rollout.mp4",
				CreatedAt:  "2026-05-16T03:00:00Z",
				Preview:    `{"kind":"video"}`,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := storeHandle.EnrichRunData(ctx, expstore.EnrichRunDataOptions{
			Run: expstore.RunRecord{
				RunID:       run.id,
				Project:     "project-alpha",
				RunGroupID:  run.group,
				State:       "succeeded",
				Owner:       "jsonl-import",
				ConfigHash:  run.config,
				CodeSHA:     "abc123-" + run.group,
				ImageDigest: "sha256-" + run.group,
				TauCommand:  fmt.Sprintf("tau submit candidate-training --group %s --set seed=%d", run.group, run.seed),
				ResultURI:   resultURI,
			},
			RunContext: &expstore.RunContextRecord{
				RunID:            run.id,
				Cluster:          "aks-project-alpha",
				Namespace:        "tau",
				Team:             "agentic",
				Profile:          run.profile,
				Lane:             "training",
				ClusterQueue:     "taugrid-training",
				KueueWorkload:    run.id + "-workload",
				GPUClass:         "a100",
				GPUCount:         &gpuCount,
				QueueWaitSeconds: &run.queue,
				GPUHours:         &run.gpuHours,
			},
			Events: run.events,
			Tags: []expstore.TagRecord{
				{ScopeType: "run", ScopeID: run.id, Key: "seed", Value: fmt.Sprint(run.seed)},
				{ScopeType: "run", ScopeID: run.id, Key: "rl.workload", Value: "candidate-training-project-alpha"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, obs := range []expstore.ObservationRecord{
		{
			ObservationID: "obs-experiment-decision",
			Author:        "researcher",
			Source:        "human",
			Type:          "decision",
			ScopeType:     "experiment",
			ScopeID:       "experiment-alpha",
			Text:          "Ablation is the current winner, but run one more seed before closing the experiment.",
			Evidence:      `{"metric":"train/return","run_group":"candidate-group"}`,
			CreatedAt:     "2026-05-16T04:00:00Z",
		},
		{
			ObservationID: "obs-baseline-seed-3-exclusion",
			Author:        "agent",
			Source:        "agent",
			Type:          "exclusion",
			ScopeType:     "run",
			ScopeID:       "baseline-seed-3",
			Text:          "World-model loss spike aligns with the reward collapse.",
			Evidence:      `{"metric":"world_model/loss","event_id":"event-baseline-seed-3-oom"}`,
			CreatedAt:     "2026-05-16T04:05:00Z",
		},
		{
			ObservationID: "obs-next-seed",
			Author:        "agent",
			Source:        "agent",
			Type:          "next-experiment",
			ScopeType:     "experiment",
			ScopeID:       "experiment-alpha",
			Text:          "Run one additional ablation seed to tighten the envelope before declaring the reproduction answered.",
			Evidence:      `{"command":"tau submit candidate-training --group candidate-group --set seed=7"}`,
			CreatedAt:     "2026-05-16T04:10:00Z",
		},
	} {
		if _, err := storeHandle.RecordObservation(ctx, expstore.RecordObservationOptions{Observation: obs}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func setupComparePlotStore(t *testing.T) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "project-alpha-store")
	for _, group := range []string{"reference-group", "candidate-group"} {
		if _, stderr, err := executeExpCommand(
			"experiment", "--store", store,
			"init", "experiment-alpha",
			"--project", "project-alpha",
			"--group", group,
		); err != nil {
			t.Fatalf("exp init %s failed: %v\nstderr:\n%s", group, err, stderr)
		}
	}
	importRun := func(runID, group string, values []cliScalar) {
		t.Helper()
		history := filepath.Join(t.TempDir(), runID+".jsonl")
		if err := writeCLIJSONLScalars(history, values); err != nil {
			t.Fatal(err)
		}
		if _, stderr, err := executeExpCommand(
			"experiment", "--store", store,
			"import", "jsonl",
			"--run", runID,
			"--group", group,
			"--history", history,
			"--idempotency-key", "jsonl-compare-"+runID,
			"--json",
		); err != nil {
			t.Fatalf("import %s failed: %v\nstderr:\n%s", runID, err, stderr)
		}
	}
	importRun("seed-1", "reference-group", []cliScalar{
		{tag: "train/return", step: 1, wallTime: 10, value: 10},
		{tag: "train/return", step: 2, wallTime: 20, value: 20},
	})
	importRun("seed-2", "reference-group", []cliScalar{
		{tag: "train/return", step: 1, wallTime: 10, value: 12},
		{tag: "train/return", step: 2, wallTime: 20, value: 22},
	})
	importRun("seed-3", "candidate-group", []cliScalar{
		{tag: "train/return", step: 1, wallTime: 10, value: 14},
		{tag: "train/return", step: 2, wallTime: 20, value: 30},
	})
	return store
}

func TestExpCaptureExistingRunRecordPreservesMetadata(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce candidate model sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "jsonl-import",
			ResultURI:  "metrics/project=project-alpha/run=seed-1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	run, ok, err := existingRunRecord(ctx, store, "seed-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected existing run")
	}
	if run.Owner != "jsonl-import" || run.ResultURI != "metrics/project=project-alpha/run=seed-1" {
		t.Fatalf("existing run metadata not preserved: %+v", run)
	}
}

func executeExpCommand(args ...string) (string, string, error) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

func newTestMetricsOffloadStore(t *testing.T, name string) *expstore.Store {
	t.Helper()
	store, _, err := expstore.Init(context.Background(), filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        name,
		Project:     "sample-project",
		Description: "Can the metrics sidecar mark terminal status?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatalf("expstore init: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})
	return store
}

type testMetricsStatusRow struct {
	MetricFileID string  `json:"metric_file_id"`
	MetricName   string  `json:"metric_name"`
	Value        float64 `json:"value"`
	Tags         string  `json:"tags"`
}

func readMetricsStatusRowForTest(t *testing.T, path string) (testMetricsStatusRow, map[string]string) {
	t.Helper()
	statusRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var statusRow testMetricsStatusRow
	if err := json.Unmarshal(bytes.TrimSpace(statusRaw), &statusRow); err != nil {
		t.Fatalf("parse status marker: %v\n%s", err, statusRaw)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(statusRow.Tags), &tags); err != nil {
		t.Fatalf("parse status marker tags: %v\n%s", err, statusRow.Tags)
	}
	return statusRow, tags
}

func decodeFirstRemoteWriteSeriesForTest(t *testing.T, compressed []byte) (map[string]string, int64) {
	t.Helper()
	payload, err := snappy.Decode(nil, compressed)
	if err != nil {
		t.Fatal(err)
	}
	field, typ, n := protowire.ConsumeTag(payload)
	if n < 0 || field != 1 || typ != protowire.BytesType {
		t.Fatalf("unexpected write request field=%d type=%v: %v", field, typ, protowire.ParseError(n))
	}
	series, n := protowire.ConsumeBytes(payload[n:])
	if n < 0 {
		t.Fatalf("consume series: %v", protowire.ParseError(n))
	}
	labels := map[string]string{}
	var timestamp int64
	for len(series) > 0 {
		field, typ, n = protowire.ConsumeTag(series)
		if n < 0 {
			t.Fatalf("consume series tag: %v", protowire.ParseError(n))
		}
		series = series[n:]
		value, n := protowire.ConsumeBytes(series)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("consume series field=%d type=%v: %v", field, typ, protowire.ParseError(n))
		}
		series = series[n:]
		switch field {
		case 1:
			name, labelValue := remoteWriteLabelForTest(t, value)
			labels[name] = labelValue
		case 2:
			timestamp = remoteWriteSampleTimestampForTest(t, value)
		}
	}
	return labels, timestamp
}

func remoteWriteLabelForTest(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var name, value string
	for len(raw) > 0 {
		field, typ, n := protowire.ConsumeTag(raw)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("consume label tag field=%d type=%v: %v", field, typ, protowire.ParseError(n))
		}
		raw = raw[n:]
		text, n := protowire.ConsumeString(raw)
		if n < 0 {
			t.Fatalf("consume label value: %v", protowire.ParseError(n))
		}
		raw = raw[n:]
		if field == 1 {
			name = text
		} else if field == 2 {
			value = text
		}
	}
	return name, value
}

func remoteWriteSampleTimestampForTest(t *testing.T, raw []byte) int64 {
	t.Helper()
	for len(raw) > 0 {
		field, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			t.Fatalf("consume sample tag: %v", protowire.ParseError(n))
		}
		raw = raw[n:]
		if field == 2 && typ == protowire.VarintType {
			value, n := protowire.ConsumeVarint(raw)
			if n < 0 {
				t.Fatalf("consume sample timestamp: %v", protowire.ParseError(n))
			}
			return int64(value)
		}
		n = protowire.ConsumeFieldValue(field, typ, raw)
		if n < 0 {
			t.Fatalf("consume sample field=%d type=%v: %v", field, typ, protowire.ParseError(n))
		}
		raw = raw[n:]
	}
	t.Fatal("remote-write sample missing timestamp")
	return 0
}

type cliScalar struct {
	tag      string
	step     int64
	wallTime float64
	value    float64
}

// writeCLIJSONLScalars emits scalars as a Stellar-style JSONL history file,
// collapsing all scalars sharing a step into one row.
func writeCLIJSONLScalars(path string, scalars []cliScalar) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var order []int64
	rows := map[int64]map[string]any{}
	for _, scalar := range scalars {
		row, ok := rows[scalar.step]
		if !ok {
			row = map[string]any{"_step": scalar.step, "_timestamp": scalar.wallTime}
			rows[scalar.step] = row
			order = append(order, scalar.step)
		}
		row[scalar.tag] = scalar.value
	}
	enc := json.NewEncoder(f)
	for _, step := range order {
		if err := enc.Encode(rows[step]); err != nil {
			return err
		}
	}
	return nil
}
