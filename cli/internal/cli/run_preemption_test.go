// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type preemptionPolicyRunner struct {
	output string
	err    error
	calls  [][]string
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
