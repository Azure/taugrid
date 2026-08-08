// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/portal/internal/portal/jobs"
	"github.com/Azure/taugrid/portal/internal/portal/kubeclient"
	"github.com/Azure/taugrid/portal/internal/portalapi"
)

// newPortalCmd builds the `taugrid-portal portal` command group. The portal is the
// unified observability surface; `serve` runs the HTTP server that hosts the
// portal shell plus the mounted Stellar experience.
func newPortalCmd() *cobra.Command {
	var storePath string
	cmd := &cobra.Command{
		Use:   "portal",
		Short: "Unified observability portal",
		Long: `taugrid-portal portal — a single web entry point that aggregates and cross-links the
runtime's dashboards (Experiments, Jobs/Queue, Cluster Health, Ray, Cost).

The portal is read-only and hosts the Stellar experience unchanged under
/stellar.`,
	}
	cmd.PersistentFlags().StringVar(&storePath, "store", "", "experiment store root (used by the mounted Stellar experience)")
	cmd.AddCommand(newPortalServeCmd(&storePath))
	return cmd
}

func newPortalServeCmd(storePath *string) *cobra.Command {
	// Reuse Stellar's serve options so the portal stays flag-compatible with
	// `tau experiment serve` for everything Stellar needs (source, Kusto adapter,
	// limits). The portal binds its own default addr.
	opts := defaultExpServeOptions()
	opts.addr = portalapi.DefaultAddr
	// Portal-specific flags for the Jobs/Queue board's Kubernetes access.
	var (
		kubeconfig     string
		namespace      string
		policyPath     string
		jobsScopeMode  string
		operatorScopes []string
		clusterName    string
		directory      string
		userHeader     string
		groupsHeader   string
		historyTable   string
		historyLimit   int
		historyEnabled bool

		kueueVizEnabled         bool
		kueueVizNamespace       = "kueue-system"
		kueueVizBackendService  = "kueue-kueueviz-backend"
		kueueVizFrontendService = "kueue-kueueviz-frontend"
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the unified portal over HTTP",
		Long: `Serve the portal shell, the /api/portal/* board APIs, and the mounted Stellar
experience over a single HTTP listener.

Data access is read-only. Kusto-backed boards reuse the same
--kusto-query-command contract as Stellar. The Jobs/Queue board reads Kueue
objects via client-go (in-cluster ServiceAccount, or --kubeconfig locally); if
Kubernetes is unreachable the portal still serves every other board.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if historyEnabled && strings.TrimSpace(opts.kustoQueryCommand) == "" && strings.TrimSpace(opts.kustoEndpoint) == "" {
				return fmt.Errorf("--run-history-enabled requires --kusto-endpoint or --kusto-query-command")
			}
			var workspaceDirectory portalapi.WorkspaceDirectory
			if strings.TrimSpace(directory) != "" {
				loaded, err := portalapi.LoadWorkspaceDirectory(directory)
				if err != nil {
					return err
				}
				workspaceDirectory = loaded
			}
			parsedOperatorScopes, err := parseJobsOperatorScopes(operatorScopes)
			if err != nil {
				return err
			}
			jobsOpts := portalapi.JobsOptions{
				ScopeMode:      portalapi.JobsScopeMode(jobsScopeMode),
				OperatorScopes: parsedOperatorScopes,
				PolicyPath:     policyPath,
			}
			// The Jobs, Ray, Nodes, and Runs boards share one Kubernetes reader:
			// a portal on a host without cluster access still serves the shell,
			// Stellar, and the Kusto boards. /api/portal/{jobs,ray,nodes,runs}
			// then return 503 until access is configured.
			rayOpts, runsOpts := legacyKubernetesBoardOptions(namespace)
			var nodesOpts portalapi.NodesOptions
			if client, err := kubeclient.New(kubeconfig); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: Jobs, Ray, Nodes, and Runs boards disabled (no Kubernetes access): %v\n", err)
			} else {
				jobsOpts.Reader = client
				rayOpts.Reader = client
				nodesOpts.Reader = client
				runsOpts.Reader = client
			}
			// The Kusto-backed boards (Cluster Health, Cost, Node Utilization)
			// reuse Stellar's shell-out contract (--kusto-query-command). Without
			// it they are disabled and their APIs return 503; the rest of the
			// portal serves.
			var clusterOpts portalapi.ClusterOptions
			var costOpts portalapi.CostOptions
			var nodeUtilOpts portalapi.NodeUtilOptions
			// Kusto transport selection: an explicit --kusto-query-command keeps
			// the Stellar shell-out adapter; otherwise a bare --kusto-endpoint
			// selects the native azure-kusto-go SDK path (DefaultAzureCredential),
			// which avoids the busybox/IMDS adapter entirely.
			var querier kustoquery.Querier
			if strings.TrimSpace(opts.kustoQueryCommand) != "" {
				querier = kustoquery.Client{
					Command:  opts.kustoQueryCommand,
					Args:     opts.kustoQueryArgs,
					Endpoint: opts.kustoEndpoint,
					Database: opts.kustoDatabase,
				}
			} else if strings.TrimSpace(opts.kustoEndpoint) != "" {
				querier = kustoquery.SDKClient{
					Endpoint: opts.kustoEndpoint,
					Database: opts.kustoDatabase,
				}
			}
			if querier != nil {
				clusterOpts.Querier = querier
				clusterOpts.Cluster = clusterName
				costOpts.Querier = querier
				costOpts.Cluster = clusterName
				nodeUtilOpts.Querier = querier
				nodeUtilOpts.Cluster = clusterName
				if historyEnabled {
					runsOpts.History = runs.NewKustoHistoryReader(querier)
				}
				runsOpts.HistoryTable = historyTable
				runsOpts.HistoryLimit = historyLimit
			} else {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: Cluster Health, Cost, and Node Utilization boards disabled (set --kusto-endpoint, or --kusto-query-command for a custom adapter)")
			}
			// The Kueue (Live) board reverse-proxies the KueueViz dashboard's
			// fixed backend/frontend Services. When --kueueviz is unset the board
			// is disabled and /api/portal/kueueviz* returns 503; the rest of the
			// portal serves. Reachability is over in-cluster DNS (default
			// transport), same as the Ray/Jobs boards.
			kueueVizOpts := portalapi.KueueVizOptions{
				Enabled:         kueueVizEnabled,
				Namespace:       kueueVizNamespace,
				BackendService:  kueueVizBackendService,
				FrontendService: kueueVizFrontendService,
			}
			server, err := portalapi.NewServer(portalapi.Options{
				Stellar:            opts.toExpapiOptions(storePath),
				Jobs:               jobsOpts,
				Cluster:            clusterOpts,
				Cost:               costOpts,
				Ray:                rayOpts,
				Nodes:              nodesOpts,
				Runs:               runsOpts,
				NodeUtil:           nodeUtilOpts,
				WorkspaceDirectory: workspaceDirectory,
				Identity: portalapi.IdentityOptions{
					UserHeader:   userHeader,
					GroupsHeader: groupsHeader,
				},
				KueueViz: kueueVizOpts,
			})
			if err != nil {
				return err
			}
			return servePortalServer(cmd, server, opts)
		},
	}
	// includeOpen=false: the portal has no single browser target to auto-open.
	addExpServeFlags(cmd, &opts, false)
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig for the Jobs board when running out-of-cluster (default: in-cluster ServiceAccount, then $KUBECONFIG)")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "optional legacy namespace for the Ray and Runs boards; empty reads cluster-wide and managed workspaces override it")
	cmd.Flags().StringVar(&policyPath, "policy", "", "topology policy file for the Jobs board (default: TAU_TOPOLOGY_POLICY, in-tree, or embedded Azure policy)")
	cmd.Flags().StringVar(&jobsScopeMode, "jobs-scope-mode", string(portalapi.JobsScopeDisabled), "computed Jobs board scope mode: disabled, workspace, or operator")
	cmd.Flags().StringSliceVar(&operatorScopes, "jobs-operator-scope", nil, "trusted operator Jobs scope as team=namespace/localQueue (repeatable; operator mode only)")
	cmd.Flags().StringVar(&clusterName, "cluster", "", "legacy/default cluster scope for Kusto-backed boards (required when durable run history is configured)")
	cmd.Flags().StringVar(&directory, "workspace-directory", "", "metadata-only JSON workspace directory; enables trusted Entra identity headers and server-resolved workspace scope")
	cmd.Flags().StringVar(&userHeader, "workspace-user-header", "", "trusted authenticated user header (default: X-MS-CLIENT-PRINCIPAL-NAME)")
	cmd.Flags().StringVar(&groupsHeader, "workspace-groups-header", "", "trusted authenticated groups header (default: X-MS-CLIENT-PRINCIPAL-GROUPS; comma or semicolon separated)")
	cmd.Flags().BoolVar(&historyEnabled, "run-history-enabled", false, "enable durable Kusto run history (requires a deployed lifecycle recorder)")
	cmd.Flags().StringVar(&historyTable, "run-history-table", "TauExpRunLifecycle", "Kusto table containing durable Tau lifecycle history")
	cmd.Flags().IntVar(&historyLimit, "run-history-limit", 200, "maximum durable run history rows per workspace")
	cmd.Flags().BoolVar(&kueueVizEnabled, "kueueviz", false, "enable the Kueue (Live) board reverse-proxying the KueueViz dashboard (default: disabled, board returns 503)")
	cmd.Flags().StringVar(&kueueVizNamespace, "kueueviz-namespace", kueueVizNamespace, "namespace of the KueueViz backend/frontend Services")
	cmd.Flags().StringVar(&kueueVizBackendService, "kueueviz-backend-service", kueueVizBackendService, "KueueViz backend Service name (serves the /ws/* WebSocket endpoints)")
	cmd.Flags().StringVar(&kueueVizFrontendService, "kueueviz-frontend-service", kueueVizFrontendService, "KueueViz frontend Service name (serves the SPA and assets)")
	return cmd
}

func legacyKubernetesBoardOptions(namespace string) (portalapi.RayOptions, portalapi.RunsOptions) {
	return portalapi.RayOptions{Namespace: namespace}, portalapi.RunsOptions{Namespace: namespace}
}

func parseJobsOperatorScopes(values []string) ([]jobs.Scope, error) {
	scopes := make([]jobs.Scope, 0, len(values))
	for i, value := range values {
		team, target, ok := strings.Cut(strings.TrimSpace(value), "=")
		if !ok {
			return nil, fmt.Errorf("--jobs-operator-scope %d must use team=namespace/localQueue", i+1)
		}
		namespace, queueName, ok := strings.Cut(target, "/")
		if !ok || strings.Contains(queueName, "/") {
			return nil, fmt.Errorf("--jobs-operator-scope %d must use team=namespace/localQueue", i+1)
		}
		scopes = append(scopes, jobs.Scope{Team: team, Namespace: namespace, Queue: queueName})
	}
	if len(scopes) > 0 {
		if err := jobs.ValidateScopes(scopes); err != nil {
			return nil, fmt.Errorf("invalid --jobs-operator-scope: %w", err)
		}
	}
	return scopes, nil
}

func servePortalServer(cmd *cobra.Command, server *portalapi.Server, opts expServeOptions) error {
	addr := strings.TrimSpace(opts.addr)
	if addr == "" {
		addr = portalapi.DefaultAddr
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	actualAddr := listener.Addr().String()
	fmt.Fprintf(cmd.ErrOrStderr(), "serving taugrid-portal portal at http://%s/portal\n", actualAddr)
	return server.Serve(cmd.Context(), listener)
}
