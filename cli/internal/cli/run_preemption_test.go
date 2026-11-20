// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/rayjobrender"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/topology"
)

type preemptionPolicyRunner struct {
	output string
	err    error
	calls  [][]string
}

func TestRenderedMultiNodeJobPreemptionPolicy(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"Never", "PreemptLowerPriority"} {
		t.Run(policy, func(t *testing.T) {
			manifest, err := jobrender.Render(profile.Profile{
				Runtime: profile.Runtime{Image: "registry.example.com/runtime:test"},
			}, jobrender.Options{
				Name: "multi-job", Namespace: "tau", ScriptPath: script,
				Launcher: "torchrun", Nodes: 2, ProcessesPerNode: 1,
				QueueName: "demo-queue", PriorityTier: "default", PodPreemptionPolicy: policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(manifest), "kind: Service") || !strings.Contains(string(manifest), "kind: Job") {
				t.Fatalf("expected Service and Job stream:\n%s", manifest)
			}
			runner := &preemptionPolicyRunner{output: `{"metadata":{"name":"taugrid-default"},"preemptionPolicy":"` + policy + `"}`}
			if err := validateRenderedPodPreemptionPolicy(context.Background(), runner, manifest, policy); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runner.calls, [][]string{{"get", "priorityclass", "taugrid-default", "-o", "json"}}) {
				t.Fatalf("unexpected class lookup: %v", runner.calls)
			}
			mismatch := "Never"
			if policy == mismatch {
				mismatch = "PreemptLowerPriority"
			}
			if err := validateRenderedPodPreemptionPolicy(context.Background(), runner, manifest, mismatch); err == nil || !strings.Contains(err.Error(), "Kubernetes requires these policies to match") {
				t.Fatalf("expected mismatched rendered class rejection, got %v", err)
			}
		})
	}
}

func TestRenderedPreemptionPolicyRejectsInvalidWorkloadStream(t *testing.T) {
	for _, tc := range []struct {
		manifest string
		calls    int
	}{
		{manifest: "apiVersion: v1\nkind: Service\n"},
		{manifest: "apiVersion: v1\nkind: Service\n---\nspec: [\n"},
		{manifest: "apiVersion: batch/v1\nkind: Job\nspec:\n  template:\n    spec:\n      priorityClassName: taugrid-default\n---\nspec: [\n", calls: 1},
	} {
		runner := &preemptionPolicyRunner{output: `{"metadata":{"name":"taugrid-default"},"preemptionPolicy":"Never"}`}
		if err := validateRenderedPodPreemptionPolicy(context.Background(), runner, []byte(tc.manifest), "Never"); err == nil {
			t.Fatalf("accepted workload stream %q", tc.manifest)
		}
		if len(runner.calls) != tc.calls {
			t.Fatalf("unexpected class lookup: %v", runner.calls)
		}
	}
}

func TestRenderedPriorityTierPreemptionPolicy(t *testing.T) {
	for _, tier := range []string{"default", "priority"} {
		for _, engine := range []string{"job", "rayjob"} {
			t.Run(engine+"/"+tier, func(t *testing.T) {
				p := profile.Profile{Runtime: profile.Runtime{Image: "registry.example.com/runtime:test"}}
				var manifest []byte
				var err error
				if engine == "job" {
					manifest, err = jobrender.Render(p, jobrender.Options{
						Name: "tier-job", Namespace: "nightly", Command: []string{"true"}, QueueName: "jobqueue",
						PriorityTier: tier, PodPreemptionPolicy: "PreemptLowerPriority",
					})
				} else {
					manifest, err = rayjobrender.Render(rayjobrender.Options{
						Name: "tier-ray", Namespace: "nightly", ScriptName: "train.py", Script: []byte("print('ok')\n"),
						Workers: 1, Profile: p, TopologyOptions: topology.Options{
							PriorityTier: tier, PodPreemptionPolicy: "PreemptLowerPriority",
						},
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				class := "taugrid-" + tier
				runner := &preemptionPolicyRunner{output: `{"metadata":{"name":"` + class + `"},"preemptionPolicy":"PreemptLowerPriority"}`}
				if err := validateRenderedPodPreemptionPolicy(context.Background(), runner, manifest, "PreemptLowerPriority"); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(runner.calls, [][]string{{"get", "priorityclass", class, "-o", "json"}}) {
					t.Fatalf("unexpected class lookup: %v", runner.calls)
				}
				if err := validateRenderedPodPreemptionPolicy(context.Background(), runner, manifest, "Never"); err == nil {
					t.Fatal("mismatched rendered class policy accepted")
				}
			})
		}
	}
}

func (r *preemptionPolicyRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	r.calls = append(r.calls, args)
	return r.output, r.err
}

func TestValidatePodPreemptionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		class     string
		requested string
		output    string
		err       error
		want      string
	}{
		{name: "unchanged default"},
		{name: "matching never", class: "nightly", requested: "Never", output: `{"metadata":{"name":"nightly"},"preemptionPolicy":"Never"}`},
		{name: "matching lower", class: "normal", requested: "PreemptLowerPriority", output: `{"metadata":{"name":"normal"}}`},
		{name: "cannot override class", class: "normal", requested: "Never", output: `{"metadata":{"name":"normal"},"preemptionPolicy":"PreemptLowerPriority"}`, want: "Kubernetes requires these policies to match"},
		{name: "omitted class policy defaults lower", class: "normal", requested: "Never", output: `{"metadata":{"name":"normal"}}`, want: `preemptionPolicy="PreemptLowerPriority"`},
		{name: "wrong response identity", class: "normal", requested: "PreemptLowerPriority", output: `{"metadata":{"name":"other"}}`, want: `expected "normal"`},
		{name: "missing named class", requested: "Never", want: "requires a named pod PriorityClass"},
		{name: "API error", class: "nightly", requested: "Never", err: errors.New("access denied"), want: "access denied"},
		{name: "malformed API response", class: "nightly", requested: "Never", output: "invalid", want: "decode pod PriorityClass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &preemptionPolicyRunner{output: tc.output, err: tc.err}
			err := validatePodPreemptionPolicy(context.Background(), runner, tc.class, tc.requested)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if tc.requested == "" || tc.class == "" {
				if len(runner.calls) != 0 {
					t.Fatalf("unexpected API calls: %v", runner.calls)
				}
			} else if !reflect.DeepEqual(runner.calls, [][]string{{"get", "priorityclass", tc.class, "-o", "json"}}) {
				t.Fatalf("unexpected API calls: %v", runner.calls)
			}
		})
	}
}
