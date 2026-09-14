// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func writeServeAppArgs(t *testing.T, contents string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "app-args.yaml")
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadServeAppArgs(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"empty object", "{}", "{}"},
		{"JSON", `{"llm_configs":[{"engine_kwargs":{"tensor_parallel_size":8,"gpu_memory_utilization":0.8,"enforce_eager":true}}]}`,
			`{"llm_configs":[{"engine_kwargs":{"tensor_parallel_size":8,"gpu_memory_utilization":0.8,"enforce_eager":true}}]}`},
		{"YAML", "model: example\nsettings:\n  tokens: 1048576\n  enabled: true\n  optional: null\n",
			`{"model":"example","settings":{"tokens":1048576,"enabled":true,"optional":null}}`},
		{"quoted scalars", "text: 'true'\nversion: '2.58.0'\n",
			`{"text":"true","version":"2.58.0"}`},
		{"size limit inclusive", "{}" + strings.Repeat(" ", maxServeAppArgsBytes-2), "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args, err := loadServeAppArgs(writeServeAppArgs(t, test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if args == nil {
				t.Fatal("an empty object must stay distinct from absent application arguments")
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s, want %s", raw, test.want)
			}
		})
	}
}

func TestLoadServeAppArgsRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"empty file", "", "object"},
		{"null", "null", "not null"},
		{"scalar", "8", "object"},
		{"array", "[]", "object"},
		{"malformed", "args: [", "object"},
		{"duplicate JSON key", `{"model":"first","model":"second"}`, "already defined"},
		{"duplicate nested key", "args:\n  model: first\n  model: second\n", "already defined"},
		{"multiple documents", "{}\n---\n{}", "exactly one document"},
		{"trailing null document", "{}\n---\n", "exactly one document"},
		{"non-string nested key", "args:\n  1: value\n", "JSON-compatible"},
		{"NaN", "value: .nan", "JSON-compatible"},
		{"infinity", "value: .inf", "JSON-compatible"},
		{"oversize", "{}" + strings.Repeat(" ", maxServeAppArgsBytes-1), "1 MiB"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args, err := loadServeAppArgs(writeServeAppArgs(t, test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) || args != nil {
				t.Fatalf("args=%#v err=%v, want %q", args, err, test.want)
			}
		})
	}
	if _, err := loadServeAppArgs(filepath.Join(t.TempDir(), "missing.json")); err == nil ||
		!strings.Contains(err.Error(), "read --app-args") {
		t.Fatalf("missing file: %v", err)
	}
}

func TestServeAppArgsRejectsConflictsBeforeConnecting(t *testing.T) {
	preventServeClusterAccess(t)
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"deployment", []string{"--kind=deployment"}, "requires --kind=rayservice"},
		{"no builder", nil, "explicit --import-path"},
		{"empty builder", []string{"--import-path="}, "explicit --import-path"},
		{"empty filename", []string{"--app-args="}, "file path"},
		{"missing file", []string{"--import-path=example:builder"}, "read --app-args"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"deploy", "model", "--profile=fixture", "--app-args=/must-not-exist/app-args.json"}
			out, _, err := executeServeCommand(t, append(args, test.flags...)...)
			if err == nil || !strings.Contains(err.Error(), test.want) || out != "" {
				t.Fatalf("out=%q err=%v, want %q", out, err, test.want)
			}
		})
	}
	for _, flag := range []string{
		"--replicas=1", "--min-replicas=1", "--max-replicas=0",
		"--target-qps=0", "--scale-down-delay=0", "--args=",
	} {
		t.Run(flag, func(t *testing.T) {
			out, _, err := executeServeCommand(t,
				"deploy", "model", "--profile=fixture", "--import-path=ray.serve.llm:build_openai_app",
				"--app-args=/must-not-read-this-file", flag,
			)
			if err == nil || !strings.Contains(err.Error(), "--app-args conflicts with") || out != "" {
				t.Fatalf("out=%q err=%v", out, err)
			}
		})
	}
	out, _, err := executeServeCommand(t,
		"deploy", "model", "--profile=fixture", "--import-path=example:builder",
		"--app-args="+writeServeAppArgs(t, "[]"),
	)
	if err == nil || !strings.Contains(err.Error(), "object") || out != "" {
		t.Fatalf("invalid configuration reached cluster access: out=%q err=%v", out, err)
	}
}

func TestServeAppArgsValidationOrder(t *testing.T) {
	preventServeClusterAccess(t)
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{
			name:  "kind before filename",
			flags: []string{"--kind=deployment", "--app-args="},
			want:  "--app-args requires --kind=rayservice",
		},
		{
			name:  "filename before builder",
			flags: []string{"--app-args=", "--replicas=1"},
			want:  "--app-args requires a file path",
		},
		{
			name:  "builder before deployment overrides",
			flags: []string{"--replicas=1"},
			want:  "--app-args requires an explicit --import-path for the application builder",
		},
		{
			name:  "replicas before autoscaling",
			flags: []string{"--import-path=example:builder", "--max-replicas=2", "--replicas=1"},
			want:  "--app-args conflicts with --replicas; configure builder-owned deployments in the application arguments",
		},
		{
			name:  "deployment overrides before file read",
			flags: []string{"--import-path=example:builder", "--args="},
			want:  "--app-args conflicts with --args; configure builder-owned deployments in the application arguments",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"deploy", "model", "--profile=fixture", "--app-args=/must-not-read-this-file"}
			out, _, err := executeServeCommand(t, append(args, test.flags...)...)
			if err == nil || err.Error() != test.want || out != "" {
				t.Fatalf("out=%q err=%v, want %q", out, err, test.want)
			}
		})
	}
}

func TestServeNativeVLLMAppArgsOffline(t *testing.T) {
	preventServeClusterAccess(t)
	const contents = `{
		"llm_configs": [{
			"model_loading_config": {"model_id": "example/model", "model_source": "/models/example"},
			"deployment_config": {"num_replicas": 1},
			"engine_kwargs": {"tensor_parallel_size": 8, "max_model_len": 1048576, "enforce_eager": true}
		}]
	}`
	appArgs := writeServeAppArgs(t, contents)
	out, stderr, err := executeServeCommand(t,
		"deploy", "model-api", "--kind=rayservice",
		"--profile=h100-nvl-8node", "--namespace=taugrid-default",
		"--workload-profile-snapshot="+writeServeSnapshot(t, eightRankServeSnapshot(t)),
		"--dry-run=client", "--image=example.invalid/ray-vllm:fixture",
		"--nodes=8", "--gpus=1", "--ray-version=2.58.0",
		"--import-path=ray.serve.llm:build_openai_app", "--app-args="+appArgs,
	)
	if err != nil {
		t.Fatalf("offline render: %v\n%s", err, stderr)
	}
	var object struct {
		Spec struct {
			ServeConfigV2 string `json:"serveConfigV2"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &object); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Applications []struct {
			ImportPath  string         `json:"import_path"`
			Args        map[string]any `json:"args"`
			Deployments []any          `json:"deployments"`
		} `json:"applications"`
	}
	if err := yaml.Unmarshal([]byte(object.Spec.ServeConfigV2), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Applications) != 1 {
		t.Fatalf("expected one application: %#v", config)
	}
	app := config.Applications[0]
	if app.ImportPath != "ray.serve.llm:build_openai_app" || app.Deployments != nil {
		t.Fatalf("builder must own deployment names and replicas: %#v", app)
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(contents), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(app.Args, expected) {
		t.Fatalf("builder arguments changed while rendering:\ngot %#v\nwant %#v", app.Args, expected)
	}
	for _, unwanted := range []string{"sglang", "LeaderWorkerSet", "LWS_"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("vLLM output contains %q", unwanted)
		}
	}
}
