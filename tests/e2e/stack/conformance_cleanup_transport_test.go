// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type cleanupTransport func(*http.Request) (*http.Response, error)

func (f cleanupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestConformanceNamedCleanup(t *testing.T) {
	for _, mode := range []string{"direct", "manager", "absent", "delete-notfound", "forbidden", "transient", "replacement", "workloads", "pods", "retained", "stall-get", "stall-delete", "stall-list"} {
		t.Run(mode, func(t *testing.T) {
			deleted, deleteCalls, workloadCalls, podCalls := mode == "absent", 0, 0, 0
			manager := mode != "direct" && mode != "pods" && mode != "retained"
			config := &rest.Config{Host: "https://offline.invalid", QPS: 10000, Burst: 10000}
			config.Transport = cleanupTransport(func(r *http.Request) (*http.Response, error) {
				_, bounded := r.Context().Deadline()
				require.True(t, bounded)
				if r.Context().Err() != nil {
					return nil, r.Context().Err()
				}
				stall := (mode == "stall-get" && strings.Contains(r.URL.Path, "/rayjobs/")) ||
					(mode == "stall-delete" && r.Method == "DELETE") ||
					(mode == "stall-list" && strings.HasSuffix(r.URL.Path, "/workloads"))
				if stall {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				status, body := 200, ""
				switch {
				case strings.Contains(r.URL.Path, "/rayjobs/") && r.Method == "DELETE":
					deleteCalls++
					var options metav1.DeleteOptions
					require.NoError(t, json.NewDecoder(r.Body).Decode(&options))
					require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
					require.Equal(t, types.UID("owned-uid"), *options.Preconditions.UID)
					switch {
					case mode == "delete-notfound":
						deleted = true
						status, body = 404, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`
					case mode == "forbidden":
						status, body = 403, `{"kind":"Status","apiVersion":"v1","reason":"Forbidden","message":"denied","code":403}`
					case mode == "transient" && deleteCalls < 3:
						status, body = 503, `{"kind":"Status","apiVersion":"v1","reason":"ServiceUnavailable","message":"retry","code":503}`
					default:
						deleted = true
						body = `{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`
					}
				case strings.Contains(r.URL.Path, "/rayjobs/"):
					body = `{"kind":"RayJob","apiVersion":"ray.io/v1","metadata":{"uid":"owned-uid"},"status":{"rayClusterName":"owned-cluster"}}`
					if deleted {
						status, body = 404, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`
						if mode == "replacement" {
							status, body = 200, `{"kind":"RayJob","apiVersion":"ray.io/v1","metadata":{"uid":"replacement"}}`
						}
					}
				case strings.HasSuffix(r.URL.Path, "/workloads"):
					workloadCalls++
					body = `{"kind":"WorkloadList","apiVersion":"kueue.x-k8s.io/v1beta2","items":[]}`
					if mode == "workloads" && workloadCalls < 3 {
						body = fmt.Sprintf(`{"kind":"WorkloadList","apiVersion":"kueue.x-k8s.io/v1beta2","items":[{"metadata":{"ownerReferences":[{"kind":"RayJob","name":%q,"uid":"owned-uid"}]}}]}`, rayJobNameNanoGPT)
					}
				case strings.HasSuffix(r.URL.Path, "/rayclusters"):
					require.False(t, manager)
					require.NotEmpty(t, r.URL.Query().Get("labelSelector"))
					body = `{"kind":"RayClusterList","apiVersion":"ray.io/v1","items":[]}`
				case strings.HasSuffix(r.URL.Path, "/pods"):
					require.False(t, manager)
					require.NotEmpty(t, r.URL.Query().Get("labelSelector"))
					podCalls++
					body = `{"kind":"PodList","apiVersion":"v1","items":[]}`
					if (mode == "pods" && podCalls < 3) || mode == "retained" {
						body = `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"owned-worker"}}]}`
					}
				default:
					return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			kube, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			dyn, err := dynamic.NewForConfig(config)
			require.NoError(t, err)
			timeout := time.Second
			if strings.HasPrefix(mode, "stall-") || mode == "retained" {
				timeout = 30 * time.Millisecond
			}
			if mode == "direct" {
				expired, cancel := context.WithCancel(context.Background())
				cancel()
				require.ErrorIs(t, deleteConformanceRayJob(expired, kube, dyn, "shared", rayJobNameNanoGPT, false, time.Millisecond), context.Canceled)
			}
			start := time.Now()
			err = cleanupOwnedConformanceRayJob(kube, dyn, "shared", rayJobNameNanoGPT, manager, timeout, time.Millisecond)
			switch {
			case mode == "forbidden":
				require.True(t, apierrors.IsForbidden(err), "%v", err)
				require.Equal(t, 1, deleteCalls)
			case mode == "replacement":
				require.ErrorContains(t, err, "replaced")
				require.Equal(t, 1, deleteCalls)
			case mode == "retained":
				require.ErrorContains(t, err, "deadline")
				require.Positive(t, podCalls)
				require.Less(t, time.Since(start), time.Second)
			case strings.HasPrefix(mode, "stall-"):
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Less(t, time.Since(start), time.Second)
			default:
				require.NoError(t, err)
				if mode == "absent" {
					require.Zero(t, deleteCalls)
				}
				if mode == "transient" {
					require.Equal(t, 3, deleteCalls)
				}
				if mode == "workloads" {
					require.Equal(t, 3, workloadCalls)
				}
				if mode == "pods" {
					require.GreaterOrEqual(t, podCalls, 3)
				}
			}
		})
	}
}
