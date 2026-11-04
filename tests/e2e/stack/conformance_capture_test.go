// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

func conformanceRayCluster(ctx context.Context, client dynamic.Interface, namespace, job string) (string, error) {
	var cluster string
	err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		obj, err := client.Resource(e2e.RayJobGVR).Namespace(namespace).Get(ctx, job, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		cluster, _, err = unstructured.NestedString(obj.Object, "status", "rayClusterName")
		return cluster != "", err
	})
	return cluster, err
}

func fineWebCallerOwnsCleanup() (bool, error) {
	switch os.Getenv("FINEWEB_CLEANUP_OWNER") {
	case "", "test":
		return false, nil
	case "caller":
		return true, nil
	default:
		return false, fmt.Errorf("FINEWEB_CLEANUP_OWNER must be test or caller")
	}
}

func boundedDuration(raw string, fallback, maximum time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > maximum {
		return 0, fmt.Errorf("invalid timeout %q: require a positive duration <= %s", raw, maximum)
	}
	return d, nil
}

func conformanceExecutionBudget(fineweb bool) (time.Duration, error) {
	admission, err := boundedDuration(os.Getenv("CONFORMANCE_ADMISSION_TIMEOUT"), 30*time.Minute, 90*time.Minute)
	if err != nil {
		return 0, err
	}
	completion, capture := 120*time.Minute, 15*time.Minute
	if fineweb {
		if raw := os.Getenv("FINEWEB_RAYJOB_TIMEOUT_MINUTES"); raw != "" {
			minutes, err := strconv.Atoi(raw)
			if err != nil || minutes <= 0 || minutes > 180 {
				return 0, fmt.Errorf("FINEWEB_RAYJOB_TIMEOUT_MINUTES must be in (0,180]")
			}
			completion = time.Duration(minutes) * time.Minute
		}
		capture, err = boundedDuration(os.Getenv("FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT"), 15*time.Minute, 30*time.Minute)
		if err != nil {
			return 0, err
		}
	}
	minimum := int(completion/time.Minute) + 130
	for _, extra := range []time.Duration{admission - 30*time.Minute, capture - 15*time.Minute} {
		if extra > 0 {
			minimum += int((extra + time.Minute - 1) / time.Minute)
		}
	}
	goTimeout, err := boundedDuration(os.Getenv("CONFORMANCE_GO_TEST_TIMEOUT"), 245*time.Minute, 365*time.Minute)
	if err != nil {
		return 0, fmt.Errorf("CONFORMANCE_GO_TEST_TIMEOUT: %w", err)
	}
	budget := goTimeout + 5*time.Minute
	if budget < time.Duration(minimum)*time.Minute {
		return 0, fmt.Errorf("CONFORMANCE_GO_TEST_TIMEOUT must be >= %dm; caller execution needs >= %dm", minimum-5, minimum)
	}
	return budget, nil
}

func conformanceContext(t *testing.T, fineweb bool) context.Context {
	t.Helper()
	budget, err := conformanceExecutionBudget(fineweb)
	if err != nil {
		t.Fatal(err)
	}
	duration := budget - 20*time.Minute
	if deadline, ok := t.Deadline(); ok {
		if remaining := time.Until(deadline) - 8*time.Minute; remaining < duration {
			duration = remaining
		}
	}
	if duration <= 0 {
		t.Fatal("Go test timeout leaves no execution/cleanup budget; use the conformance harness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	t.Cleanup(cancel)
	return ctx
}

func registerRayJobCleanup(t *testing.T, fixture string, cleanup func()) {
	t.Helper()
	if fixture == "fineweb-rayjob-16xh200-ib.yaml" {
		caller, err := fineWebCallerOwnsCleanup()
		if err != nil {
			t.Fatal(err)
		}
		if caller {
			t.Log("FINEWEB_CALLER_CLEANUP: caller must attempt artifact publication then always clean up, including on failure/cancel")
			return
		}
	}
	t.Cleanup(cleanup)
}
