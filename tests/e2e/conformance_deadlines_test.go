// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/tests/e2e/bundle"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type conformanceTransport func(*http.Request) (*http.Response, error)

func (f conformanceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestConformanceWaitsBoundStalledRequests(t *testing.T) {
	for _, name := range []string{"admission-get", "admission-list", "completion", "logs", "ready", "deleted", "pods-deleted"} {
		t.Run(name, func(t *testing.T) {
			timeout := pollInterval + 100*time.Millisecond
			if name == "deleted" || name == "pods-deleted" {
				timeout = 100 * time.Millisecond
			}
			stalled := false
			diagnostics := 0
			config := &rest.Config{Host: "https://offline.invalid", Transport: conformanceTransport(func(r *http.Request) (*http.Response, error) {
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("API request has no deadline")
					return nil, fmt.Errorf("unbounded API request")
				}
				if stalled {
					diagnostics++
					if r.Context().Err() != nil {
						t.Error("diagnostics inherited the expired execution context")
					}
					return nil, fmt.Errorf("offline diagnostics unavailable")
				}
				if name == "admission-list" && strings.Contains(r.URL.Path, "/rayjobs/") {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{"apiVersion":"ray.io/v1","kind":"RayJob","metadata":{"name":"owned","uid":"owned-uid"}}`))}, nil
				}
				stalled = true
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			kube, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			dyn, err := dynamic.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			tc := &TestContext{T: t, ctx: context.Background(), kubeClient: kube, dynamicClient: dyn, bundle: bundle.New(t)}
			start := time.Now()
			switch name {
			case "admission-get", "admission-list":
				_, err = tc.WaitForWorkloadAdmittedByRayJob("shared", "owned", timeout)
			case "completion":
				err = tc.WaitForRayJobStatus("shared", "owned", "SUCCEEDED", timeout)
			case "logs":
				_, err = tc.WaitForFullPodContainerLogsByLabelContaining("shared", "owned=true", "driver", []string{"sentinel"}, timeout)
			case "ready":
				err = tc.WaitForRunningPodsByLabel("shared", "owned=true", 16, timeout)
			case "deleted":
				err = tc.WaitForRayJobDeleted("shared", "owned", timeout)
			case "pods-deleted":
				err = tc.WaitForNoPodsByLabel("shared", "owned=true", timeout)
			}
			if !errors.Is(err, context.DeadlineExceeded) || !stalled || time.Since(start) > timeout+time.Second {
				t.Fatalf("wait failed to bound requests: err=%v stalled=%v elapsed=%s", err, stalled, time.Since(start))
			}
			if name != "deleted" && diagnostics == 0 {
				t.Error("failed wait did not attempt independent diagnostics")
			}
		})
	}
}

func TestConformanceAdmissionDeadlineRetainsDiagnostics(t *testing.T) {
	config := &rest.Config{Host: "https://offline.invalid", Transport: conformanceTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"kind":"RayJob","apiVersion":"ray.io/v1","metadata":{"name":"owned","uid":"owned-uid"}}`
		if strings.HasSuffix(r.URL.Path, "/workloads") {
			body = `{"kind":"WorkloadList","apiVersion":"kueue.x-k8s.io/v1beta2","items":[{"metadata":{"name":"owned-workload","ownerReferences":[{"kind":"RayJob","name":"owned","uid":"owned-uid"}]},"status":{"conditions":[{"type":"Admitted","status":"False","reason":"Pending","message":"insufficient quota"}]}}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	tc := &TestContext{T: t, ctx: context.Background(), kubeClient: kube, dynamicClient: dyn, bundle: bundle.New(t)}
	_, err = tc.WaitForWorkloadAdmittedByRayJob("shared", "owned", pollInterval+100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending admission must fail with its deadline, got %v", err)
	}
	for _, text := range []string{"owned-workload", "Admitted=False", "reason=Pending", "insufficient quota"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("missing admission diagnostics %q in %v", text, err)
		}
	}
}

func TestConformanceLogContainerSelection(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			config := &rest.Config{Host: "https://offline.invalid", Transport: conformanceTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"owned-head"}}]}`
				if strings.HasSuffix(r.URL.Path, "/log") {
					if r.URL.Query().Get("container") != "driver" {
						t.Error("named log container was not selected")
					}
					if (r.URL.Query().Get("tailLines") == "") != full {
						t.Error("full/tailed log selection was not preserved")
					}
					body = "sentinel"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			kube, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			tc := &TestContext{T: t, ctx: context.Background(), kubeClient: kube}
			var logs string
			if full {
				logs, err = tc.WaitForFullPodContainerLogsByLabelContaining("shared", "owned=true", "driver", []string{"sentinel"}, 3*time.Second)
			} else {
				logs, err = tc.WaitForPodContainerLogsByLabelContaining("shared", "owned=true", "driver", []string{"sentinel"}, 3*time.Second)
			}
			if err != nil || logs != "sentinel" {
				t.Fatalf("logs=%q err=%v", logs, err)
			}
		})
	}
}
