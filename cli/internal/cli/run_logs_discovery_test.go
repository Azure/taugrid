// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
)

func TestRunLogsDiscoveryPrefersConnectedWorkspace(t *testing.T) {
	connected := runLogsRoute{Workspace: "current", KubeContext: "current-context", Namespace: "current-ns"}
	cachedCalled := false
	var executed runLogsRoute

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return connected, nil },
			cached: func() ([]runLogsRoute, error) {
				cachedCalled = true
				return nil, nil
			},
			probe: func(_ context.Context, route runLogsRoute, name string) (bool, error) {
				return route == connected && name == "train", nil
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, opts runLogsOptions) error {
				executed = route
				if opts.Tail != 60 {
					t.Fatalf("tail = %d, want 60", opts.Tail)
				}
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if cachedCalled {
		t.Fatal("cached connections must not be searched after the connected workspace matches")
	}
	if executed != connected {
		t.Fatalf("executed route = %#v, want %#v", executed, connected)
	}
}

func TestRunLogsDiscoveryFallsBackToCachedConnection(t *testing.T) {
	target := runLogsRoute{Workspace: "research", KubeContext: "research-context", Namespace: "research-ns"}
	var executed runLogsRoute
	var warnings bytes.Buffer

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&warnings,
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{target}, nil },
			probe: func(_ context.Context, route runLogsRoute, _ string) (bool, error) {
				return route == target, nil
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != target {
		t.Fatalf("executed route = %#v, want %#v", executed, target)
	}
	if warnings.Len() != 0 {
		t.Fatalf("successful cache fallback printed warnings: %s", warnings.String())
	}
}

func TestRunLogsDiscoveryKeepsOlderUsableRouteForSameWorkspace(t *testing.T) {
	newest := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "newest", Kubeconfig: "/tmp/newest", Namespace: "research-ns",
	}
	older := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "older", Kubeconfig: "/tmp/older", Namespace: "research-ns",
	}
	var executed runLogsRoute

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{newest, older}, nil },
			probe: func(_ context.Context, route runLogsRoute, _ string) (bool, error) {
				if route == newest {
					return false, errors.New("newest kubeconfig is unusable")
				}
				return route == older, nil
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != older {
		t.Fatalf("executed route = %#v, want older usable route %#v", executed, older)
	}
}

func TestRunLogsDiscoveryCollapsesUsableRoutesForSameWorkspace(t *testing.T) {
	newest := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "newest", Kubeconfig: "/tmp/newest", Namespace: "research-ns",
	}
	older := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "older", Kubeconfig: "/tmp/older", Namespace: "research-ns",
	}
	var executed runLogsRoute

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{newest, older}, nil },
			probe:     func(context.Context, runLogsRoute, string) (bool, error) { return true, nil },
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != newest {
		t.Fatalf("executed route = %#v, want freshest usable route %#v", executed, newest)
	}
}

func TestRunLogsDiscoveryWarnsWhenConnectedProbeFailsBeforeFallback(t *testing.T) {
	connected := runLogsRoute{Workspace: "current", KubeContext: "current-context", Namespace: "current-ns"}
	target := runLogsRoute{Workspace: "research", KubeContext: "research-context", Namespace: "research-ns"}
	var warnings bytes.Buffer

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&warnings,
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return connected, nil },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{target}, nil },
			probe: func(_ context.Context, route runLogsRoute, _ string) (bool, error) {
				if route == connected {
					return false, errors.New("authorization denied")
				}
				return route == target, nil
			},
			execute: func(context.Context, io.Writer, runLogsRoute, string, runLogsOptions) error {
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	for _, want := range []string{"warning: run discovery skipped", connected.label(), "authorization denied"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warning %q missing %q", warnings.String(), want)
		}
	}
}

func TestRunLogsDiscoverySkipsTimedOutCachedRoute(t *testing.T) {
	stale := runLogsRoute{Workspace: "stale", KubeContext: "offline", Namespace: "stale-ns"}
	target := runLogsRoute{Workspace: "research", KubeContext: "research-context", Namespace: "research-ns"}
	var warnings bytes.Buffer
	var executed runLogsRoute

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&warnings,
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected:    func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:       func() ([]runLogsRoute, error) { return []runLogsRoute{stale, target}, nil },
			probeTimeout: 10 * time.Millisecond,
			probe: func(ctx context.Context, route runLogsRoute, _ string) (bool, error) {
				if route == stale {
					<-ctx.Done()
					return false, ctx.Err()
				}
				return route == target, nil
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != target {
		t.Fatalf("executed route = %#v, want %#v", executed, target)
	}
	for _, want := range []string{stale.label(), "probe timed out"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warning %q missing %q", warnings.String(), want)
		}
	}
}

func TestRunLogsDiscoveryBoundsConcurrentCachedProbes(t *testing.T) {
	timeout := 40 * time.Millisecond
	routes := []runLogsRoute{
		{Workspace: "stale-1", KubeContext: "offline-1", Namespace: "ns-1"},
		{Workspace: "stale-2", KubeContext: "offline-2", Namespace: "ns-2"},
		{Workspace: "stale-3", KubeContext: "offline-3", Namespace: "ns-3"},
		{Workspace: "target", KubeContext: "online", Namespace: "target-ns"},
	}
	started := time.Now()
	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected:    func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:       func() ([]runLogsRoute, error) { return routes, nil },
			probeTimeout: timeout,
			probe: func(ctx context.Context, route runLogsRoute, _ string) (bool, error) {
				if route.Workspace == "target" {
					return true, nil
				}
				<-ctx.Done()
				return false, ctx.Err()
			},
			execute: func(context.Context, io.Writer, runLogsRoute, string, runLogsOptions) error {
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 3*timeout {
		t.Fatalf("cached probes took %s; want one bounded search window, not one timeout per route", elapsed)
	}
}

func TestRunLogsDiscoveryProbesFallbackQueuedAfterTimedOutBatch(t *testing.T) {
	timeout := 10 * time.Millisecond
	routes := make([]runLogsRoute, maxRunLogsProbeConcurrency+1)
	for i := range maxRunLogsProbeConcurrency {
		routes[i] = runLogsRoute{
			Workspace:   fmt.Sprintf("stale-%d", i),
			KubeContext: fmt.Sprintf("offline-%d", i),
			Namespace:   fmt.Sprintf("ns-%d", i),
		}
	}
	target := runLogsRoute{Workspace: "target", KubeContext: "online", Namespace: "target-ns"}
	routes[len(routes)-1] = target
	var executed runLogsRoute

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected:    func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:       func() ([]runLogsRoute, error) { return routes, nil },
			probeTimeout: timeout,
			probe: func(ctx context.Context, route runLogsRoute, _ string) (bool, error) {
				if route == target {
					return true, nil
				}
				<-ctx.Done()
				return false, ctx.Err()
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != target {
		t.Fatalf("executed route = %#v, want queued fallback %#v", executed, target)
	}
}

func TestRunLogsDiscoveryKeepsCachedMatchFromPartialSnapshot(t *testing.T) {
	target := runLogsRoute{Workspace: "research", KubeContext: "online", Namespace: "target-ns"}
	var warnings bytes.Buffer
	var executed runLogsRoute
	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&warnings,
		"train",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{target}, nil },
			probe: func(context.Context, runLogsRoute, string) (bool, error) {
				return true, errors.New("workloads forbidden")
			},
			execute: func(_ context.Context, _ io.Writer, route runLogsRoute, _ string, _ runLogsOptions) error {
				executed = route
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("runLogsWithDiscovery: %v", err)
	}
	if executed != target {
		t.Fatalf("executed route = %#v, want %#v", executed, target)
	}
	if !strings.Contains(warnings.String(), "workloads forbidden") {
		t.Fatalf("warning %q does not retain the partial snapshot error", warnings.String())
	}
}

func TestProbeRunLogsRoutePreservesMatchAtDeadline(t *testing.T) {
	timeout := 5 * time.Millisecond
	found, err := probeRunLogsRoute(context.Background(), runLogsDiscoveryHooks{
		probeTimeout: timeout,
		probe: func(ctx context.Context, _ runLogsRoute, _ string) (bool, error) {
			<-ctx.Done()
			return true, ctx.Err()
		},
	}, runLogsRoute{}, "train")
	if !found {
		t.Fatal("positive workload identity was discarded at the deadline")
	}
	if err == nil || !strings.Contains(err.Error(), "probe timed out") {
		t.Fatalf("error = %v, want deadline warning", err)
	}
}

func TestRunLogsDiscoveryRejectsAmbiguousCachedMatches(t *testing.T) {
	first := runLogsRoute{Workspace: "vision", KubeContext: "east", Namespace: "team-a"}
	second := runLogsRoute{Workspace: "language", KubeContext: "west", Namespace: "team-b"}

	err := runLogsWithDiscovery(
		context.Background(),
		&bytes.Buffer{},
		&bytes.Buffer{},
		"shared-name",
		runLogsOptions{Tail: 60},
		runLogsDiscoveryHooks{
			connected: func() (runLogsRoute, error) { return runLogsRoute{}, errors.New("not connected") },
			cached:    func() ([]runLogsRoute, error) { return []runLogsRoute{first, second}, nil },
			probe:     func(context.Context, runLogsRoute, string) (bool, error) { return true, nil },
			execute: func(context.Context, io.Writer, runLogsRoute, string, runLogsOptions) error {
				t.Fatal("ambiguous discovery must not execute logs")
				return nil
			},
		},
	)
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	for _, want := range []string{"multiple configured Tau workspaces", "workspace=vision", "workspace=language", "--workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestValidateCachedRunLogsRouteRejectsWorkspaceDrift(t *testing.T) {
	route := runLogsRoute{
		Workspace:       "research",
		WorkspaceUID:    "expected-uid",
		SystemNamespace: "tau-system",
		Namespace:       "research-ns",
	}
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{
			name: "identity changed",
			json: `{"metadata":{"uid":"replacement-uid","generation":1},"spec":{"target":{"namespace":"research-ns"}},"status":{"phase":"Ready","observedGeneration":1}}`,
			want: "identity changed",
		},
		{
			name: "namespace changed",
			json: `{"metadata":{"uid":"expected-uid","generation":1},"spec":{"target":{"namespace":"other-ns"}},"status":{"phase":"Ready","observedGeneration":1}}`,
			want: "namespace changed",
		},
		{
			name: "not ready",
			json: `{"metadata":{"uid":"expected-uid"},"spec":{"target":{"namespace":"research-ns"}},"status":{"phase":"Pending"}}`,
			want: "not Ready",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
				wantArgs := "-n tau-system get workspace.tau.azure.com research -o json"
				if got := strings.Join(args, " "); got != wantArgs {
					t.Fatalf("args = %q, want %q", got, wantArgs)
				}
				return tc.json, nil
			})
			err := validateCachedRunLogsRoute(context.Background(), runner, route)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProbeRunLogsFindsPodsAfterJobTTLDeletion(t *testing.T) {
	found, err := probeRunLogsWithHooks(context.Background(), runLogsProbeHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Namespace: "research", Name: "completed"}, nil
		},
		listJobPods: func(context.Context) (string, error) {
			return "completed-pod", nil
		},
	})
	if err != nil {
		t.Fatalf("probeRunLogsWithHooks: %v", err)
	}
	if !found {
		t.Fatal("lingering Job pod should make the route discoverable")
	}
}

func TestCachedRunLogsWorkspaceRouteSelectsNamedWorkspace(t *testing.T) {
	target := runLogsRoute{Workspace: "research", KubeContext: "research-context", Namespace: "research-ns"}
	got, found, err := cachedRunLogsWorkspaceRoute(context.Background(), "research", "train", runLogsDiscoveryHooks{
		cached: func() ([]runLogsRoute, error) {
			return []runLogsRoute{
				{Workspace: "other", KubeContext: "other-context", Namespace: "other-ns"},
				target,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("cachedRunLogsWorkspaceRoute: %v", err)
	}
	if !found || got != target {
		t.Fatalf("route = %#v, found=%v; want %#v", got, found, target)
	}
}

func TestCachedRunLogsWorkspaceRouteFallsBackWithinLogicalWorkspace(t *testing.T) {
	newest := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "newest", Kubeconfig: "/tmp/newest", Namespace: "research-ns",
	}
	older := runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "older", Kubeconfig: "/tmp/older", Namespace: "research-ns",
	}
	got, found, err := cachedRunLogsWorkspaceRoute(context.Background(), "research", "train", runLogsDiscoveryHooks{
		cached: func() ([]runLogsRoute, error) {
			return []runLogsRoute{newest, older}, nil
		},
		probe: func(_ context.Context, route runLogsRoute, _ string) (bool, error) {
			if route == newest {
				return false, errors.New("newest kubeconfig is unusable")
			}
			return route == older, nil
		},
	})
	if err != nil {
		t.Fatalf("cachedRunLogsWorkspaceRoute: %v", err)
	}
	if !found || got != older {
		t.Fatalf("route = %#v, found=%v; want older usable route %#v", got, found, older)
	}
}

func TestRootRegistersDiscoveringLogsCommand(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"logs"})
	if err != nil {
		t.Fatalf("find tau logs: %v", err)
	}
	if cmd == nil || cmd.Name() != "logs" {
		t.Fatalf("tau logs command = %#v", cmd)
	}
	if flag := cmd.Flags().Lookup("tail"); flag == nil || flag.DefValue != "200" {
		t.Fatalf("tau logs --tail flag = %#v", flag)
	}
}
