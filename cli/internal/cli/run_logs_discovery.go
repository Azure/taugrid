// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

type runLogsRoute struct {
	Workspace       string
	WorkspaceUID    string
	SystemNamespace string
	KubeContext     string
	Kubeconfig      string
	Namespace       string
}

func (r runLogsRoute) label() string {
	parts := make([]string, 0, 3)
	if workspace := strings.TrimSpace(r.Workspace); workspace != "" {
		parts = append(parts, "workspace="+workspace)
	}
	if kubeContext := strings.TrimSpace(r.KubeContext); kubeContext != "" {
		parts = append(parts, "context="+kubeContext)
	}
	parts = append(parts, "namespace="+strings.TrimSpace(r.Namespace))
	return strings.Join(parts, ", ")
}

func (r runLogsRoute) key() string {
	return strings.Join([]string{
		strings.TrimSpace(r.Kubeconfig),
		strings.TrimSpace(r.KubeContext),
		strings.TrimSpace(r.Namespace),
	}, "\x00")
}

func (r runLogsRoute) logicalKey() string {
	if workspaceUID := strings.TrimSpace(r.WorkspaceUID); workspaceUID != "" {
		return "uid\x00" + workspaceUID
	}
	return "route\x00" + r.key()
}

type runLogsDiscoveryHooks struct {
	connected    func() (runLogsRoute, error)
	cached       func() ([]runLogsRoute, error)
	probe        func(context.Context, runLogsRoute, string) (bool, error)
	execute      func(context.Context, io.Writer, runLogsRoute, string, runLogsOptions) error
	probeTimeout time.Duration
}

const (
	defaultRunLogsProbeTimeout = 15 * time.Second
	defaultRunLogsProbeBudget  = 30 * time.Second
	maxRunLogsProbeConcurrency = 8
)

type runLogsProbeHooks struct {
	fetchSnapshot func(context.Context) (status.Snapshot, error)
	listJobPods   func(context.Context) (string, error)
}

func probeRunLogsWithHooks(ctx context.Context, hooks runLogsProbeHooks) (bool, error) {
	snapshot, err := hooks.fetchSnapshot(ctx)
	if snapshot.JobFound || snapshot.RayJob.Found {
		return true, err
	}
	if err != nil {
		return false, err
	}
	podName, err := hooks.listJobPods(ctx)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(podName) != "", nil
}

func cachedRunLogsWorkspaceRoute(
	ctx context.Context,
	workspace, name string,
	hooks runLogsDiscoveryHooks,
) (runLogsRoute, bool, error) {
	cached, err := hooks.cached()
	if err != nil {
		return runLogsRoute{}, false, fmt.Errorf("discover configured Tau workspace connections: %w", err)
	}
	workspace = strings.TrimSpace(workspace)
	var matches []runLogsRoute
	for _, route := range cached {
		if strings.TrimSpace(route.Workspace) == workspace {
			matches = append(matches, route)
		}
	}
	switch len(matches) {
	case 0:
		return runLogsRoute{}, false, nil
	case 1:
		return matches[0], true, nil
	}
	probed, _, probeErrors := probeCachedRunLogsRoutes(ctx, hooks, matches, map[string]struct{}{}, name)
	probed = deduplicateRunLogsRoutes(probed)
	switch len(probed) {
	case 0:
		detail := ""
		if len(probeErrors) > 0 {
			detail = "; discovery errors: " + strings.Join(probeErrors, "; ")
		}
		return runLogsRoute{}, false, fmt.Errorf(
			"run %q was not found in configured Tau workspace %q%s; pass --context and --namespace to select a target explicitly",
			name,
			workspace,
			detail,
		)
	case 1:
		return probed[0], true, nil
	default:
		locations := make([]string, 0, len(probed))
		for _, route := range probed {
			locations = append(locations, route.label())
		}
		return runLogsRoute{}, false, fmt.Errorf(
			"workspace %q has multiple configured Tau connections containing run %q (%s); pass --context and --namespace to select one",
			workspace,
			name,
			strings.Join(locations, "; "),
		)
	}
}

func runLogsWithDiscovery(
	ctx context.Context,
	out, errOut io.Writer,
	name string,
	opts runLogsOptions,
	hooks runLogsDiscoveryHooks,
) error {
	if err := validateRunLogsOptions(opts); err != nil {
		return err
	}

	var diagnostics []string
	var warnings []string
	seen := map[string]struct{}{}
	if connected, err := hooks.connected(); err != nil {
		diagnostics = append(diagnostics, "connected workspace: "+err.Error())
	} else {
		seen[connected.key()] = struct{}{}
		found, probeErr := probeRunLogsRoute(ctx, hooks, connected, name)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if found {
			return hooks.execute(ctx, out, connected, name, opts)
		}
		if probeErr != nil {
			diagnostic := connected.label() + ": " + probeErr.Error()
			diagnostics = append(diagnostics, diagnostic)
			warnings = append(warnings, diagnostic)
		}
	}

	cached, err := hooks.cached()
	if err != nil {
		return fmt.Errorf("discover configured Tau workspace connections: %w", err)
	}
	matches := make([]runLogsRoute, 0, 1)
	searched := make([]string, 0, len(cached))
	cachedMatches, cachedSearched, cachedErrors := probeCachedRunLogsRoutes(ctx, hooks, cached, seen, name)
	matches = append(matches, cachedMatches...)
	matches = deduplicateRunLogsRoutes(matches)
	searched = append(searched, cachedSearched...)
	diagnostics = append(diagnostics, cachedErrors...)
	warnings = append(warnings, cachedErrors...)
	if err := ctx.Err(); err != nil {
		return err
	}

	switch len(matches) {
	case 1:
		for _, diagnostic := range warnings {
			fmt.Fprintf(errOut, "warning: run discovery skipped %s\n", diagnostic)
		}
		return hooks.execute(ctx, out, matches[0], name, opts)
	case 0:
		detail := ""
		if len(searched) > 0 {
			detail = "; searched " + strings.Join(searched, "; ")
		}
		if len(diagnostics) > 0 {
			detail += "; discovery errors: " + strings.Join(diagnostics, "; ")
		}
		return fmt.Errorf(
			"run %q was not found in the connected workspace or configured Tau workspace connections%s; pass --context and --namespace to select a target explicitly",
			name,
			detail,
		)
	default:
		locations := make([]string, 0, len(matches))
		for _, route := range matches {
			locations = append(locations, route.label())
		}
		return fmt.Errorf(
			"run %q exists in multiple configured Tau workspaces (%s); pass --workspace, or --context and --namespace, to select one",
			name,
			strings.Join(locations, "; "),
		)
	}
}

func deduplicateRunLogsRoutes(routes []runLogsRoute) []runLogsRoute {
	unique := make([]runLogsRoute, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		key := route.logicalKey()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, route)
	}
	return unique
}

func probeCachedRunLogsRoutes(
	ctx context.Context,
	hooks runLogsDiscoveryHooks,
	routes []runLogsRoute,
	seen map[string]struct{},
	name string,
) ([]runLogsRoute, []string, []string) {
	unique := make([]runLogsRoute, 0, len(routes))
	for _, route := range routes {
		if _, duplicate := seen[route.key()]; duplicate {
			continue
		}
		seen[route.key()] = struct{}{}
		unique = append(unique, route)
	}
	if len(unique) == 0 {
		return nil, nil, nil
	}

	budget := defaultRunLogsProbeBudget
	if hooks.probeTimeout > 0 {
		budget = 2 * hooks.probeTimeout
	}
	searchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type probeResult struct {
		found bool
		err   error
	}
	results := make([]probeResult, len(unique))
	jobs := make(chan int, len(unique))
	for i := range unique {
		jobs <- i
	}
	close(jobs)

	workers := min(maxRunLogsProbeConcurrency, len(unique))
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i].found, results[i].err = probeRunLogsRoute(searchCtx, hooks, unique[i], name)
			}
		}()
	}
	wg.Wait()

	matches := make([]runLogsRoute, 0, len(unique))
	searched := make([]string, 0, len(unique))
	probeErrors := make([]string, 0, len(unique))
	for i, route := range unique {
		searched = append(searched, route.label())
		if results[i].found {
			matches = append(matches, route)
		}
		if results[i].err != nil {
			probeErrors = append(probeErrors, route.label()+": "+results[i].err.Error())
		}
	}
	return matches, searched, probeErrors
}

func probeRunLogsRoute(ctx context.Context, hooks runLogsDiscoveryHooks, route runLogsRoute, name string) (bool, error) {
	timeout := hooks.probeTimeout
	if timeout <= 0 {
		timeout = defaultRunLogsProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	found, err := hooks.probe(probeCtx, route, name)
	if probeCtx.Err() == context.DeadlineExceeded {
		return found, fmt.Errorf("probe timed out after %s: %w", timeout, probeCtx.Err())
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return found, err
}

func defaultRunLogsDiscoveryHooks(cmd *cobra.Command, connection *runLifecycleConnectionFlags) runLogsDiscoveryHooks {
	return runLogsDiscoveryHooks{
		connected: func() (runLogsRoute, error) {
			resolvedContext, namespace, restore, err := connection.resolve(cmd)
			if err != nil {
				return runLogsRoute{}, err
			}
			route := runLogsRoute{
				Workspace:   connection.workspace,
				KubeContext: resolvedContext,
				Kubeconfig:  activeKubeconfigPath(),
				Namespace:   namespace,
			}
			restore()
			return route, nil
		},
		cached: func() ([]runLogsRoute, error) {
			connections, err := workspaceconnection.ListCachedConnections(strings.TrimSpace(os.Getenv("TAU_CONFIG_DIR")))
			if err != nil {
				return nil, err
			}
			routes := make([]runLogsRoute, 0, len(connections))
			for _, connection := range connections {
				routes = append(routes, runLogsRoute{
					Workspace:       connection.Workspace,
					WorkspaceUID:    connection.WorkspaceUID,
					SystemNamespace: connection.SystemNamespace,
					KubeContext:     connection.ContextName,
					Kubeconfig:      connection.KubeconfigPath,
					Namespace:       connection.Namespace,
				})
			}
			return routes, nil
		},
		probe: func(ctx context.Context, route runLogsRoute, name string) (bool, error) {
			r := kube.NewWithKubeconfig(route.KubeContext, route.Kubeconfig)
			if err := validateCachedRunLogsRoute(ctx, r, route); err != nil {
				return false, err
			}
			return probeRunLogsWithHooks(ctx, runLogsProbeHooks{
				fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
					return status.FetchRunLogs(ctx, r, route.Namespace, name)
				},
				listJobPods: func(ctx context.Context) (string, error) {
					return r.Raw(ctx, []string{
						"-n", route.Namespace,
						"get", "pods",
						"-l", "job-name=" + name,
						"-o", "name",
					}, nil)
				},
			})
		},
		execute: func(ctx context.Context, out io.Writer, route runLogsRoute, name string, opts runLogsOptions) error {
			opts.Namespace = route.Namespace
			r := kube.NewWithKubeconfig(route.KubeContext, route.Kubeconfig)
			if err := validateCachedRunLogsRoute(ctx, r, route); err != nil {
				return err
			}
			return runLogsCommandWithHooks(ctx, out, r, name, opts, runLogsHooks{})
		},
	}
}

func validateCachedRunLogsRoute(ctx context.Context, runner kubeRawRunner, route runLogsRoute) error {
	if route.WorkspaceUID == "" {
		return nil
	}
	raw, err := runner.Raw(ctx, []string{
		"-n", route.SystemNamespace,
		"get", "workspace.tau.azure.com", route.Workspace,
		"-o", "json",
	}, nil)
	if err != nil {
		return fmt.Errorf("revalidate cached workspace %q: %w", route.Workspace, err)
	}
	workspace, err := tauworkspace.Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("revalidate cached workspace %q: %w", route.Workspace, err)
	}
	if workspace.Metadata.UID != route.WorkspaceUID {
		return fmt.Errorf(
			"cached workspace %q identity changed (expected UID %q, got %q); reconnect the workspace",
			route.Workspace,
			route.WorkspaceUID,
			workspace.Metadata.UID,
		)
	}
	namespace := tauworkspace.ResolvedNamespace(workspace)
	if namespace != route.Namespace {
		return fmt.Errorf(
			"cached workspace %q namespace changed (expected %q, got %q); reconnect the workspace",
			route.Workspace,
			route.Namespace,
			namespace,
		)
	}
	if !tauworkspace.Ready(workspace) {
		return fmt.Errorf("cached workspace %q is not Ready; reconnect the workspace", route.Workspace)
	}
	return nil
}
