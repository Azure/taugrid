// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package onboarding

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestClassifyOnboardingFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		owner  FailureOwner
		action string
	}{
		{
			name: "descriptor", err: workspaceconnection.ErrDescriptorNotFound,
			owner: FailureOwnerResearcher, action: "workspace.connection.yaml",
		},
		{
			name: "azure forbidden", err: &azcore.ResponseError{StatusCode: 403},
			owner: FailureOwnerPlatform, action: "Cluster User",
		},
		{
			name: "workspace readiness", err: errors.New("workspace sample is not Ready"),
			owner: FailureOwnerPlatform, action: "workspace owner",
		},
		{
			name: "legacy cluster credentials", err: errors.New("AKS cluster-user kubeconfig is not Entra exec authentication"),
			owner: FailureOwnerPlatform, action: "AKS Entra authentication",
		},
		{
			name: "queue timeout", err: errors.New("wait for workload: deadline exceeded"),
			owner: FailureOwnerTransient, action: "run status",
		},
		{
			name: "immutable field with imagePullPolicy in body",
			err: errors.New(
				`kubectl [apply -n demo -f -]: exit status 1: ` +
					`The Job "train" is invalid: spec.template: Invalid value: ` +
					`{"spec":{"containers":[{"imagePullPolicy":"IfNotPresent"}]}}: field is immutable`,
			),
			owner: FailureOwnerResearcher, action: "tau run cancel",
		},
		{
			name:  "real ImagePullBackOff",
			err:   errors.New("pod demo/train-xyz: container waiting reason ImagePullBackOff"),
			owner: FailureOwnerPlatform, action: "workload image",
		},
		{
			name:  "real ErrImagePull",
			err:   errors.New("pod demo/train-xyz: container waiting reason ErrImagePull"),
			owner: FailureOwnerPlatform, action: "workload image",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.err)
			if got.Owner != test.owner || !strings.Contains(got.Action, test.action) {
				t.Fatalf("guidance = %#v", got)
			}
		})
	}
}

func TestClassifyFilesystemErrorsNeverSuggestsNetwork(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "missing file",
			err:  &os.PathError{Op: "stat", Path: "/repo/tau.yaml", Err: syscall.ENOENT},
		},
		{
			name: "permission denied",
			err:  &os.PathError{Op: "open", Path: "/repo/tau.yaml", Err: syscall.EACCES},
		},
		{
			name: "resource temporarily unavailable",
			err:  &os.PathError{Op: "read", Path: "/repo/tau.yaml", Err: syscall.EAGAIN},
		},
		{
			name: "interrupted system call",
			err:  &os.PathError{Op: "read", Path: "/repo/tau.yaml", Err: syscall.EINTR},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guidance := Classify(test.err)
			action := strings.ToLower(guidance.Action)
			if strings.Contains(action, "network") || strings.Contains(action, "dns") {
				t.Fatalf("filesystem error received network guidance: %#v", guidance)
			}
		})
	}
}

func TestClassifyConcreteNetworkErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "DNS error without keyword fallback",
			err:  &net.DNSError{Err: "lookup failed", Name: "cluster.example"},
		},
		{
			name: "connection reset operation",
			err:  &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		},
		{
			name: "invalid network address",
			err:  &net.AddrError{Err: "invalid address", Addr: "cluster.example"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guidance := Classify(test.err)
			if !strings.Contains(strings.ToLower(guidance.Action), "network") {
				t.Fatalf("network error received non-network guidance: %#v", guidance)
			}
		})
	}
}

func TestExplainPreservesCause(t *testing.T) {
	cause := workspaceconnection.ErrInteractiveRequired
	err := Explain(cause)
	if !errors.Is(err, cause) ||
		!strings.Contains(err.Error(), "Owner: Researcher action required") ||
		!strings.Contains(err.Error(), "Run `tau workspace connection` in an interactive terminal") {
		t.Fatalf("guided error = %v", err)
	}
	if Explain(err) != err {
		t.Fatalf("Explain should not double-wrap GuidedError")
	}
}
