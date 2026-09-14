// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderApplicationBuilderArguments(t *testing.T) {
	for _, args := range []map[string]any{
		{},
		{
			"llm_configs": []any{
				map[string]any{
					"engine_kwargs": map[string]any{
						"max_model_len":          1048576,
						"gpu_memory_utilization": 0.8,
						"enforce_eager":          true,
					},
					"deployment_config": map[string]any{"num_replicas": 1},
				},
			},
			"description": "line one\nline two",
		},
	} {
		options := distributedRayOptions()
		options.ImportPath = "ray.serve.llm:build_openai_app"
		options.ReplicasSet = false
		options.AppArgs = args
		raw, err := Render(distributedRayProfile(), options)
		if err != nil {
			t.Fatal(err)
		}
		object := decodeOne(t, raw)
		var config map[string]any
		if err := yaml.Unmarshal([]byte(getPath(t, object, "spec", "serveConfigV2").(string)), &config); err != nil {
			t.Fatal(err)
		}
		app := config["applications"].([]any)[0].(map[string]any)
		if !reflect.DeepEqual(app["args"], args) {
			t.Fatalf("nested argument types were not preserved: got %#v, want %#v", app["args"], args)
		}
		if app["deployments"] != nil {
			t.Fatal("builder-owned LLM deployments must not receive a hard-coded default deployment override")
		}
		workers := getPath(t, object, "spec", "rayClusterConfig", "workerGroupSpecs").([]any)
		if workers[0].(map[string]any)["replicas"] != 8 {
			t.Fatal("application arguments changed the worker pool")
		}
	}
}

func TestRenderRejectsInvalidApplicationArguments(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Options)
		want   string
	}{
		{"missing builder", func(o *Options) { o.ImportPath = "" }, "explicit import path"},
		{"replicas", func(o *Options) { o.ReplicasSet = true }, "deployment overrides"},
		{"autoscaling", func(o *Options) { o.Autoscaling = &AutoscalingOptions{MaxReplicas: 2} }, "deployment overrides"},
		{"legacy arguments", func(o *Options) { o.Args = []string{"--model"} }, "legacy args"},
		{"non-JSON value", func(o *Options) { o.AppArgs["invalid"] = math.NaN() }, "JSON-compatible"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := distributedRayOptions()
			options.ImportPath = "example:builder"
			options.ReplicasSet = false
			options.AppArgs = map[string]any{}
			test.change(&options)
			raw, err := Render(distributedRayProfile(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(raw) != 0 {
				t.Fatalf("raw=%s err=%v, want %q", raw, err, test.want)
			}
		})
	}
}
