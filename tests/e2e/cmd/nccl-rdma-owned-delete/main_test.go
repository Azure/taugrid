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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

func TestDeleteOwnedResourceSendsUIDPreconditionAndForegroundBody(t *testing.T) {
	const namespace = "approved-nccl-rdma"
	const uid = types.UID("31c57b2e-b633-4073-ad3f-6a9db75d94cf")
	gvr := schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		require.Equal(t, "/apis/batch/v1/namespaces/approved-nccl-rdma/jobs/e2e-nccl-rdma-2x1xh200", r.URL.Path)
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
	require.NoError(t, deleteOwnedResource(
		context.Background(), client, gvr, namespace, "e2e-nccl-rdma-2x1xh200", uid, time.Second,
	))
}

func TestDeleteOwnedResourceTreatsAlreadyDeletedAsSuccess(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	err := deleteOwnedResource(
		context.Background(),
		client,
		schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
		"taugrid-rdma-diagnostic",
		"e2e-nccl-rdma-2x1xh200",
		types.UID("successful-create-uid"),
		time.Second,
	)
	require.NoError(t, err)
}

func TestDeleteOwnedResourceHonorsOverallTimeout(t *testing.T) {
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
	err = deleteOwnedResource(
		context.Background(),
		client,
		schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		"approved-nccl-rdma",
		"nccl-rdma-probe",
		types.UID("31c57b2e-b633-4073-ad3f-6a9db75d94cf"),
		50*time.Millisecond,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "context deadline exceeded")
	require.Less(t, time.Since(start), time.Second)
}

func TestDeleteOwnedResourceDoesNotDeleteReplacementUID(t *testing.T) {
	const ownedUID = types.UID("31c57b2e-b633-4073-ad3f-6a9db75d94cf")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var options metav1.DeleteOptions
		require.NoError(t, json.NewDecoder(r.Body).Decode(&options))
		require.NotNil(t, options.Preconditions)
		require.Equal(t, ownedUID, *options.Preconditions.UID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, err := w.Write([]byte(`{
			"apiVersion":"v1",
			"kind":"Status",
			"status":"Failure",
			"reason":"Conflict",
			"message":"UID precondition failed",
			"code":409
		}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	err = deleteOwnedResource(
		context.Background(),
		client,
		schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
		"approved-nccl-rdma",
		"nccl-rdma-auth",
		ownedUID,
		time.Second,
	)
	require.Error(t, err)
	require.Equal(t, 1, requests, "cleanup must not retry without the successful CREATE response UID")
}

func TestBoundedRESTTimeout(t *testing.T) {
	require.Equal(t, 2*time.Second, boundedRESTTimeout(2*time.Second))
	require.Equal(t, maxRESTRequestTimeout, boundedRESTTimeout(time.Minute))
}
