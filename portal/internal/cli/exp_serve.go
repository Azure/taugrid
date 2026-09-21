// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

type expServeOptions struct {
	addr                   string
	defaultTarget          string
	metric                 string
	source                 string
	kustoMetricsFile       string
	kustoProject           string
	workspace              string
	kustoWorkspace         string
	kustoAllowedProjects   []string
	kustoFeaturedProjects  []string
	kustoEndpoint          string
	kustoDatabase          string
	kustoIngestion         string
	kustoSince             string
	kustoDiscoverySince    string
	kustoMaxDiscoverySince string
	kustoTargetSince       string
	kustoQueryCommand      string
	kustoQueryArgs         []string
	kustoTargetPoints      int
	maxRuns                int
	maxMetricRows          int
	timeout                time.Duration
	open                   bool
}

var openBrowserURL = openSystemBrowserURL

func newExpServeCmd(storePath *string) *cobra.Command {
	opts := defaultExpServeOptions()
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve Stellar over HTTP",
		Long: `Serve a thin HTTP API and Stellar shell over the resolved experiment store.

The service is read-only for the configured expstore.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			server, err := newExpServerFromOptions(storePath, opts)
			if err != nil {
				return err
			}
			return serveExpServer(cmd, server, opts, opts.defaultTarget, opts.open)
		},
	}
	addExpServeFlags(cmd, &opts, true)
	return cmd
}

func newExpOpenCmd(storePath *string) *cobra.Command {
	opts := defaultExpServeOptions()
	cmd := &cobra.Command{
		Use:   "open TARGET",
		Short: "Open a Stellar dashboard in the default browser",
		Long: `Open a Stellar dashboard in the user's default browser and serve it locally.

This is the one-command browser path for agents after they discover a target
with ` + portalbin.ExperimentCmd + ` search or ` + portalbin.ExperimentCmd + ` experiments list. The
command keeps serving
Stellar in the foreground until it is interrupted. When --metric is omitted,
Tau links the browser to the target's primary outcome metric when one is
available.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.defaultTarget = args[0]
			server, err := newExpServerFromOptions(storePath, opts)
			if err != nil {
				return err
			}
			return serveExpServer(cmd, server, opts, args[0], true)
		},
	}
	addExpServeFlags(cmd, &opts, false)
	return cmd
}

func defaultExpServeOptions() expServeOptions {
	return expServeOptions{
		addr:                   expapi.DefaultAddr,
		source:                 "local",
		kustoIngestion:         "projection",
		kustoDiscoverySince:    "90d",
		kustoMaxDiscoverySince: "365d",
		kustoTargetSince:       "365d",
		kustoTargetPoints:      12000,
		maxRuns:                expapi.DefaultMaxRuns,
		maxMetricRows:          expapi.DefaultMaxMetricRows,
		timeout:                expapi.DefaultRequestTimeout,
	}
}

func addExpServeFlags(cmd *cobra.Command, opts *expServeOptions, includeOpen bool) {
	cmd.Flags().StringVar(&opts.addr, "addr", expapi.DefaultAddr, "HTTP listen address")
	if includeOpen {
		cmd.Flags().StringVar(&opts.defaultTarget, "default-target", "", "default Stellar target when target query parameter is omitted")
		cmd.Flags().BoolVar(&opts.open, "open", false, "open the Stellar dashboard URL in the default browser after binding the server")
	}
	cmd.Flags().StringVar(&opts.metric, "metric", "", "default metric name for the primary dashboard chart")
	cmd.Flags().StringVar(&opts.source, "source", "local", "Stellar datasource: local, kusto, or auto")
	cmd.Flags().StringVar(&opts.kustoMetricsFile, "kusto-metrics-file", "", "Kusto Stellar row JSONL/JSON exported from Kusto for --source=kusto or --source=auto fallback")
	cmd.Flags().StringVar(&opts.kustoProject, "kusto-project", "", "deprecated project-label compatibility metadata; use --allowed-project for hard discovery scoping or request project= filters")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "TauWorkspace this Stellar server serves; required, since Stellar refuses unscoped reads")
	cmd.Flags().StringVar(&opts.kustoWorkspace, "kusto-workspace", "", "deprecated alias for --workspace")
	_ = cmd.Flags().MarkDeprecated("kusto-workspace", "use --workspace; scoping now applies to every source, not just Kusto")
	cmd.Flags().StringArrayVar(&opts.kustoAllowedProjects, "allowed-project", nil, "project label allowed in Kusto discovery; repeat for team-scoped deployments, omit to discover all projects")
	cmd.Flags().StringArrayVar(&opts.kustoFeaturedProjects, "featured-project", nil, "project label to feature in Stellar without filtering discovery; repeatable")
	cmd.Flags().StringVar(&opts.kustoEndpoint, "kusto-endpoint", "", "Kusto endpoint; queried natively via azure-kusto-go unless --kusto-query-command is set, in which case it only fills {endpoint} placeholders")
	cmd.Flags().StringVar(&opts.kustoDatabase, "kusto-database", "", "Kusto database metadata passed to --kusto-query-command placeholders")
	cmd.Flags().StringVar(&opts.kustoIngestion, "kusto-ingestion", "projection", "Kusto ingestion shape: projection or remote-write")
	cmd.Flags().StringVar(&opts.kustoSince, "kusto-since", "", "deprecated fallback lookback for live Kusto query generation; prefer --discovery-since and --target-since")
	cmd.Flags().StringVar(&opts.kustoDiscoverySince, "discovery-since", "90d", "default lookback for broad Kusto experiment discovery")
	cmd.Flags().StringVar(&opts.kustoMaxDiscoverySince, "max-discovery-since", "365d", "maximum allowed lookback for unscoped broad Kusto experiment discovery")
	cmd.Flags().StringVar(&opts.kustoTargetSince, "target-since", "365d", "default lookback for targeted live Kusto dashboard queries")
	cmd.Flags().IntVar(&opts.kustoTargetPoints, "kusto-target-points", 12000, "target downsampled points for live Kusto metrics queries")
	cmd.Flags().StringVar(&opts.kustoQueryCommand, "kusto-query-command", "", "executable that runs generated KQL and emits Stellar row JSONL/JSON or Kusto REST JSON; KQL is passed on stdin unless an arg contains {query}")
	cmd.Flags().StringArrayVar(&opts.kustoQueryArgs, "kusto-query-arg", nil, "argument for --kusto-query-command; supports {endpoint}, {database}, and {query} placeholders")
	cmd.Flags().IntVar(&opts.maxRuns, "max-runs", expapi.DefaultMaxRuns, "maximum runs returned in each Stellar snapshot")
	cmd.Flags().IntVar(&opts.maxMetricRows, "max-metric-rows", expapi.DefaultMaxMetricRows, "maximum declared metric rows scanned per Stellar snapshot")
	cmd.Flags().DurationVar(&opts.timeout, "request-timeout", expapi.DefaultRequestTimeout, "maximum time per HTTP request")
}

// nativeKustoQuery returns the azure-kusto-go transport for Stellar, or nil when
// it does not apply. An explicit --kusto-query-command always wins, so existing
// shell-adapter deployments keep their behaviour; otherwise a bare
// --kusto-endpoint is enough to reach ADX with DefaultAzureCredential, matching
// how the other portal boards already pick kustoquery.SDKClient.
func (opts expServeOptions) nativeKustoQuery() func(ctx context.Context, query string) (string, error) {
	if strings.TrimSpace(opts.kustoQueryCommand) != "" {
		return nil
	}
	endpoint := strings.TrimSpace(opts.kustoEndpoint)
	if endpoint == "" {
		return nil
	}
	database := strings.TrimSpace(opts.kustoDatabase)
	return func(ctx context.Context, query string) (string, error) {
		return kustoquery.RunADXQuery(ctx, endpoint, database, query)
	}
}

func newExpServerFromOptions(storePath *string, opts expServeOptions) (*expapi.Server, error) {
	return expapi.NewServer(opts.toExpapiOptions(storePath))
}

// toExpapiOptions maps the shared serve flags onto expapi.Options. Both
// `taugrid-portal experiment serve` and `taugrid-portal portal serve` build their Stellar server from this,
// so the two commands stay flag-compatible.
func (opts expServeOptions) toExpapiOptions(storePath *string) expapi.Options {
	return expapi.Options{
		StorePath:              storePathValue(storePath),
		DefaultTarget:          opts.defaultTarget,
		DefaultMetric:          opts.metric,
		Source:                 opts.source,
		KustoMetricsFile:       opts.kustoMetricsFile,
		KustoProject:           opts.kustoProject,
		Workspace:              opts.workspace,
		KustoWorkspace:         opts.kustoWorkspace,
		KustoAllowedProjects:   opts.kustoAllowedProjects,
		KustoFeaturedProjects:  opts.kustoFeaturedProjects,
		KustoEndpoint:          opts.kustoEndpoint,
		KustoDatabase:          opts.kustoDatabase,
		KustoIngestion:         opts.kustoIngestion,
		KustoSince:             opts.kustoSince,
		KustoDiscoverySince:    opts.kustoDiscoverySince,
		KustoMaxDiscoverySince: opts.kustoMaxDiscoverySince,
		KustoTargetSince:       opts.kustoTargetSince,
		KustoQueryCommand:      opts.kustoQueryCommand,
		KustoQueryArgs:         opts.kustoQueryArgs,
		KustoNativeQuery:       opts.nativeKustoQuery(),
		KustoTargetPoints:      opts.kustoTargetPoints,
		MaxRuns:                opts.maxRuns,
		MaxMetricRows:          opts.maxMetricRows,
		RequestTimeout:         opts.timeout,
	}
}

func serveExpServer(cmd *cobra.Command, server *expapi.Server, opts expServeOptions, target string, openBrowser bool) error {
	addr := strings.TrimSpace(opts.addr)
	if addr == "" {
		addr = expapi.DefaultAddr
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	actualAddr := listener.Addr().String()
	metric, err := resolveStellarBrowserMetric(cmd.Context(), server.StoreRoot(), target, opts)
	if err != nil {
		_ = listener.Close()
		return err
	}
	browserURL, err := stellarBrowserURL(actualAddr, target, metric)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if openBrowser {
		if err := openBrowserURL(cmd.Context(), browserURL); err != nil {
			_ = listener.Close()
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "opened %s stellar at %s\n", portalbin.ExperimentCmd, browserURL)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "serving %s stellar at http://%s (store=%s)\n", portalbin.ExperimentCmd, actualAddr, server.StoreRoot())
	return server.Serve(cmd.Context(), listener)
}

func resolveStellarBrowserMetric(ctx context.Context, storeRoot, target string, opts expServeOptions) (string, error) {
	if metric := strings.TrimSpace(opts.metric); metric != "" {
		return metric, nil
	}
	if strings.TrimSpace(target) == "" || strings.HasPrefix(storeRoot, "kusto://") {
		return "", nil
	}
	store, err := expstore.Open(ctx, storeRoot)
	if err != nil {
		return "", err
	}
	defer store.Close()
	snapshot, err := expcockpit.BuildSnapshot(ctx, store, expcockpit.Options{
		Target:        target,
		MaxRuns:       opts.maxRuns,
		MaxMetricRows: opts.maxMetricRows,
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(snapshot.Compare.MetricName) != "" {
		return snapshot.Compare.MetricName, nil
	}
	return snapshot.Chart.MetricName, nil
}

func stellarBrowserURL(addr, target, metric string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = expapi.DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse --addr %q: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	values := url.Values{}
	if target = strings.TrimSpace(target); target != "" {
		values.Set("target", target)
	}
	if metric = strings.TrimSpace(metric); metric != "" {
		values.Set("metric", metric)
	}
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   "/stellar",
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func openSystemBrowserURL(ctx context.Context, targetURL string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{targetURL}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", targetURL}
	default:
		name = "xdg-open"
		args = []string{targetURL}
	}
	command := exec.CommandContext(ctx, name, args...)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
