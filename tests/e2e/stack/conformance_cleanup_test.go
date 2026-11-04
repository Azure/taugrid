// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func cleanupOwnedConformanceRayJob(kube kubernetes.Interface, dyn dynamic.Interface, namespace, name string, manager bool, timeout, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return deleteConformanceRayJob(ctx, kube, dyn, namespace, name, manager, interval)
}

func transientConformanceError(err error) bool {
	var network net.Error
	return apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		(errors.As(err, &network) && network.Timeout())
}

func retryConformance(ctx context.Context, interval time.Duration, operation func(context.Context) (bool, error)) error {
	var last error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		done, err := operation(ctx)
		last = err
		if err != nil && transientConformanceError(err) {
			return false, nil
		}
		return done, err
	})
	if err != nil && last != nil {
		return fmt.Errorf("%w (last API error: %v)", err, last)
	}
	return err
}

func deleteConformanceRayJob(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, namespace, name string, manager bool, interval time.Duration) error {
	switch name {
	case rayJobName, rayJobNameGPU, rayJobNameTrain, rayJobNameTrainGPU, rayJobNameFineWeb, rayJobNameNanoGPT:
	default:
		return fmt.Errorf("refusing non-conformance RayJob name %q", name)
	}
	jobs := dyn.Resource(e2e.RayJobGVR).Namespace(namespace)
	var uid types.UID
	clusters := map[string]struct{}{}
	err := retryConformance(ctx, interval, func(ctx context.Context) (bool, error) {
		job, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		uid = job.GetUID()
		if uid == "" {
			return false, fmt.Errorf("owned RayJob has no UID")
		}
		if !manager {
			cluster, _, err := unstructured.NestedString(job.Object, "status", "rayClusterName")
			if err != nil {
				return false, err
			}
			if cluster != "" {
				clusters[cluster] = struct{}{}
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !manager {
		err = retryConformance(ctx, interval, func(ctx context.Context) (bool, error) {
			items, err := dyn.Resource(e2e.RayClusterGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("ray.io/originated-from-cr-name=%s,ray.io/originated-from-crd=RayJob", name),
			})
			if err != nil {
				return false, err
			}
			for _, item := range items.Items {
				clusters[item.GetName()] = struct{}{}
			}
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	if uid != "" {
		err = retryConformance(ctx, interval, func(ctx context.Context) (bool, error) {
			propagation := metav1.DeletePropagationForeground
			err := jobs.Delete(ctx, name, metav1.DeleteOptions{
				PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &uid},
			})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return err == nil, err
		})
		if err != nil {
			return fmt.Errorf("delete owned RayJob: %w", err)
		}
	}
	return retryConformance(ctx, interval, func(ctx context.Context) (bool, error) {
		job, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if uid == "" || job.GetUID() != uid {
				return false, fmt.Errorf("RayJob replaced during cleanup; refusing replacement")
			}
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		workloads, err := dyn.Resource(e2e.WorkloadGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, workload := range workloads.Items {
			for _, owner := range workload.GetOwnerReferences() {
				if owner.Kind == "RayJob" && owner.Name == name && (uid == "" || owner.UID == uid) {
					return false, nil
				}
			}
		}
		if manager {
			return true, nil
		}
		selectors := []string{
			fmt.Sprintf("batch.kubernetes.io/job-name=%s", name),
			fmt.Sprintf("ray.io/originated-from-cr-name=%s,ray.io/originated-from-crd=RayJob", name),
		}
		for cluster := range clusters {
			selectors = append(selectors, "ray.io/cluster="+cluster)
		}
		for _, selector := range selectors {
			pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return false, err
			}
			if len(pods.Items) != 0 {
				return false, nil
			}
		}
		return true, nil
	})
}
