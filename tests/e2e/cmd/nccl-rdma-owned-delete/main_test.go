// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestDeleteOwnedMPIJobSendsUIDPreconditionAndForegroundBody(t *testing.T) {
	const namespace = "approved-nccl-rdma"
	const uid = types.UID("31c57b2e-b633-4073-ad3f-6a9db75d94cf")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		require.Equal(t, "/apis/kubeflow.org/v2beta1/namespaces/approved-nccl-rdma/mpijobs/"+mpiJobName, r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var options metav1.DeleteOptions
		require.NoError(t, json.Unmarshal(body, &options))
		require.NotNil(t, options.Preconditions)
		require.NotNil(t, options.Preconditions.UID)
		require.Equal(t, uid, *options.Preconditions.UID)
		require.NotNil(t, options.PropagationPolicy)
		require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)

		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success","code":200}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	require.NoError(t, deleteOwnedMPIJob(context.Background(), client, namespace, uid, time.Second))
}

func TestDeleteOwnedMPIJobHonorsOverallTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)

	start := time.Now()
	err = deleteOwnedMPIJob(
		context.Background(),
		client,
		"approved-nccl-rdma",
		types.UID("31c57b2e-b633-4073-ad3f-6a9db75d94cf"),
		50*time.Millisecond,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "context deadline exceeded")
	require.Less(t, time.Since(start), time.Second)
}

func TestBoundedRESTTimeout(t *testing.T) {
	require.Equal(t, 2*time.Second, boundedRESTTimeout(2*time.Second))
	require.Equal(t, maxRESTRequestTimeout, boundedRESTTimeout(time.Minute))
}
