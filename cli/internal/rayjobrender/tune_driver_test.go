// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTuneDriverForwardsWorkerMetricsAndSelectsBestResult(t *testing.T) {
	run := runTuneDriverHarness(t, "quality_score", "quality_score")
	if run.err != nil {
		t.Fatalf("generated Tune driver failed: %v\n%s", run.err, run.output)
	}

	loads, err := os.ReadFile(run.loadsPath)
	if err != nil {
		t.Fatalf("read module load trace: %v", err)
	}
	if got := strings.Count(strings.ReplaceAll(string(loads), "\r\n", "\n"), "loaded\n"); got != 3 {
		t.Fatalf("researcher module loaded %d time(s), want 3 (head validation plus two Torch worker reloads)", got)
	}

	var trials []struct {
		Config  map[string]float64 `json:"config"`
		Reports []map[string]any   `json:"reports"`
	}
	trace, err := os.ReadFile(run.tracePath)
	if err != nil {
		t.Fatalf("read outer Tune result trace: %v", err)
	}
	if err := json.Unmarshal(trace, &trials); err != nil {
		t.Fatalf("decode outer Tune result trace: %v\n%s", err, trace)
	}
	if len(trials) != 2 {
		t.Fatalf("outer Tune trial count = %d, want 2: %s", len(trials), trace)
	}
	for _, trial := range trials {
		if len(trial.Reports) != 3 {
			t.Fatalf("trial %v forwarded %d reports, want 3: %v", trial.Config, len(trial.Reports), trial.Reports)
		}
		lr := trial.Config["lr"]
		for step, report := range trial.Reports {
			want := lr + float64(step)
			if got, _ := report["quality_score"].(float64); got != want {
				t.Fatalf("trial lr=%v step=%d quality_score=%v, want %v", lr, step, report["quality_score"], want)
			}
			if got, _ := report["checkpoint_path"].(string); got == "" {
				t.Fatalf("trial lr=%v step=%d lost checkpoint propagation: %v", lr, step, report)
			}
		}
	}
	output := string(run.output)
	for _, want := range []string{"Best config: {'lr': 0.1}", "Best quality_score: 2.1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("generated driver output missing %q:\n%s", want, output)
		}
	}
}

func TestTuneDriverPreservesMissingMetricFailure(t *testing.T) {
	run := runTuneDriverHarness(t, "worker_only_metric", "quality_score")
	if run.err == nil {
		t.Fatalf("generated driver accepted a trial that never reported the configured metric:\n%s", run.output)
	}
	if !strings.Contains(string(run.output), "did not include the specified metric(s) 'quality_score'") {
		t.Fatalf("missing-metric failure was weakened or replaced: %v\n%s", run.err, run.output)
	}
}

type tuneDriverHarnessRun struct {
	output     []byte
	err        error
	loadsPath  string
	tracePath  string
	resultPath string
}

func runTuneDriverHarness(t *testing.T, reportedMetric, configuredMetric string) tuneDriverHarnessRun {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the generated-driver test")
	}

	root := t.TempDir()
	writeTuneDriverTestFile(t, root, "ray/__init__.py", `
_initialized = False

def is_initialized():
    return _initialized

def init():
    global _initialized
    _initialized = True

from . import tune
`)
	writeTuneDriverTestFile(t, root, "ray/tune/__init__.py", `
import itertools
import json
import os
import pathlib

_active_reports = None

class _Grid:
    def __init__(self, values):
        self.values = list(values)

def grid_search(values):
    return _Grid(values)

def report(metrics):
    if _active_reports is None:
        raise RuntimeError("tune.report called outside a Tune trial")
    _active_reports.append(dict(metrics))

class TuneConfig:
    def __init__(self, **kwargs):
        self.metric = kwargs.get("metric")
        self.mode = kwargs.get("mode", "min")

class _Result:
    def __init__(self, config, reports):
        self.config = config
        self.reports = reports
        self.metrics = reports[-1]

class _Results:
    def __init__(self, results, metric, mode):
        self.results = results
        self.metric = metric
        self.mode = mode

    def get_best_result(self, metric=None, mode=None):
        metric = metric or self.metric
        mode = mode or self.mode
        reverse = mode == "max"
        return sorted(self.results, key=lambda result: result.metrics[metric], reverse=reverse)[0]

class Tuner:
    def __init__(self, trainable, param_space, tune_config):
        self.trainable = trainable
        self.param_space = param_space
        self.tune_config = tune_config

    def fit(self):
        keys = list(self.param_space)
        choices = [
            value.values if isinstance(value, _Grid) else [value]
            for value in self.param_space.values()
        ]
        results = []
        trace = []
        global _active_reports
        for values in itertools.product(*choices):
            config = dict(zip(keys, values))
            _active_reports = []
            self.trainable(config)
            reports = list(_active_reports)
            metric = self.tune_config.metric
            if not reports or metric not in reports[-1]:
                raise ValueError(
                    f"Trial returned a result which did not include the specified metric(s) '{metric}'"
                )
            results.append(_Result(config, reports))
            trace.append({"config": config, "reports": reports})
        pathlib.Path(os.environ["TAU_TEST_TUNE_TRACE"]).write_text(
            json.dumps(trace), encoding="utf-8"
        )
        return _Results(results, self.tune_config.metric, self.tune_config.mode)
`)
	writeTuneDriverTestFile(t, root, "ray/train/__init__.py", `
_active_reporter = None

def report(metrics, checkpoint=None):
    if _active_reporter is None:
        raise RuntimeError("ray.train.report called outside trainer.fit")
    _active_reporter(dict(metrics), checkpoint)

class ScalingConfig:
    def __init__(self, **kwargs):
        self.kwargs = kwargs

class RunConfig:
    def __init__(self, callbacks=None, **kwargs):
        self.callbacks = list(callbacks or [])
        self.kwargs = kwargs
`)
	writeTuneDriverTestFile(t, root, "ray/train/torch.py", `
import ray.train as train

class TorchConfig:
    def __init__(self, **kwargs):
        self.kwargs = kwargs

class TorchTrainer:
    def __init__(self, train_loop_per_worker, train_loop_config, run_config, **kwargs):
        self.train_loop_per_worker = train_loop_per_worker
        self.train_loop_config = train_loop_config
        self.run_config = run_config

    def fit(self):
        reports = []
        checkpoints = []

        def collect(metrics, checkpoint):
            reports.append(dict(metrics))
            checkpoints.append(checkpoint)
            for callback in self.run_config.callbacks:
                callback.after_report(None, [metrics], checkpoint)

        train._active_reporter = collect
        try:
            self.train_loop_per_worker(self.train_loop_config)
        finally:
            train._active_reporter = None
        return type(
            "Result",
            (),
            {
                "metrics": reports[-1] if reports else {},
                "checkpoint": checkpoints[-1] if checkpoints else None,
            },
        )()
`)
	writeTuneDriverTestFile(t, root, "ray/tune/integration/__init__.py", "")
	writeTuneDriverTestFile(t, root, "ray/tune/integration/ray_train.py", `
from ray import tune

class TuneReportCallback:
    def after_report(self, run_context, metrics, checkpoint):
        report = dict(metrics[0])
        if checkpoint is not None:
            report["checkpoint_path"] = checkpoint
        tune.report(report)
`)
	trainPath := writeTuneDriverTestFile(t, root, "researcher_train.py", fmt.Sprintf(`
import os
import ray.train

with open(os.environ["TAU_TEST_MODULE_LOADS"], "a", encoding="utf-8") as stream:
    stream.write("loaded\n")

def train_func(config):
    with open(os.environ["TAU_TEST_TRAIN_RESULT"], "a", encoding="utf-8") as stream:
        stream.write(str(config["lr"]) + "\n")
    for step in range(3):
        ray.train.report(
            {%q: config["lr"] + step, "step": step},
            checkpoint=f"checkpoint-{config['lr']}-{step}",
        )
`, reportedMetric))
	driverPath := writeTuneDriverTestFile(t, root, tuneDriverFilename, tuneDriverScript)
	loadsPath := filepath.Join(root, "module-loads.txt")
	resultPath := filepath.Join(root, "train-result.txt")
	tracePath := filepath.Join(root, "tune-trace.json")

	cmd := exec.Command(python, driverPath)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+root,
		"TAU_TEST_MODULE_LOADS="+loadsPath,
		"TAU_TEST_TRAIN_RESULT="+resultPath,
		"TAU_TEST_TUNE_TRACE="+tracePath,
		"TAU_TUNE_TRAIN_MODULE=researcher_train",
		"TAU_TUNE_TRAIN_PATH="+trainPath,
		`TAU_TUNE_PARAM_SPACE={"lr":[0.2,0.1]}`,
		"TAU_TUNE_METRIC="+configuredMetric,
		"TAU_TUNE_MODE=min",
		"TAU_NUM_WORKERS=1",
		"TAU_DIST_BACKEND=gloo",
	)
	output, runErr := cmd.CombinedOutput()
	return tuneDriverHarnessRun{
		output:     output,
		err:        runErr,
		loadsPath:  loadsPath,
		tracePath:  tracePath,
		resultPath: resultPath,
	}
}

func writeTuneDriverTestFile(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
