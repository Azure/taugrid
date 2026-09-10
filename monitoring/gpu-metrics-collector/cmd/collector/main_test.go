// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/availability"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/conditions"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
)

func TestCollectPublishesActualCoverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		code   int
		body   string
		status corev1.ConditionStatus
	}{
		{"reachable but empty exporter", 200, "", corev1.ConditionUnknown},
		{"partial device coverage", 200, "gpu_errors{UUID=\"a\"} 0\n", corev1.ConditionUnknown},
		{"unreachable exporter", 503, "", corev1.ConditionUnknown},
		{"complete zero readings", 200, "gpu_errors{UUID=\"a\"} 0\ngpu_errors{UUID=\"b\"} 0\n", corev1.ConditionFalse},
		{"known fault despite missing other GPU", 200, "gpu_errors{UUID=\"a\"} 1\n", corev1.ConditionTrue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			targets := []scraper.ScrapeTarget{{
				Name: "dcgm", URL: server.URL, Required: true,
				AvailabilityCondition: "DcgmExporterUnavailable",
			}}
			engine := rules.NewEngine([]rules.Rule{{
				Name: "errors", MetricName: "gpu_errors", ConditionType: "GPUError",
				Mode: "instant", MinSamples: 2, SampleLabel: "UUID",
			}})
			client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node"}})
			writer := conditions.NewWriter(client, "gpu-node")
			tracker := availability.New(targets, []string{"GPUError"})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := collect(ctx, scraper.New(targets), engine, tracker, writer); err != nil {
				t.Fatal(err)
			}
			node, err := client.CoreV1().Nodes().Get(ctx, "gpu-node", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, condition := range node.Status.Conditions {
				if condition.Type == "GPUError" {
					if condition.Status != tt.status {
						t.Fatalf("node condition %s, want %s", condition.Status, tt.status)
					}
					return
				}
			}
			t.Fatal("collector failed to publish the GPU condition")
		})
	}
}
