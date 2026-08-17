// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/runhistory"
)

type runHistorySourceFactory func() (runhistory.Source, error)

func newRunHistoryCmd() *cobra.Command {
	return newRunHistoryCmdWithFactories(func() (runhistory.Source, error) {
		return runhistory.NewInClusterSource()
	}, runhistory.NewKustoWriter)
}

func newRunHistoryCmdWithFactories(sourceFactory runHistorySourceFactory, writerFactory runhistory.WriterFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "history", Short: "Record durable metadata-only run lifecycle history"}
	cmd.AddCommand(newRunHistoryRecordCmd(sourceFactory, writerFactory))
	return cmd
}

func newRunHistoryRecordCmd(sourceFactory runHistorySourceFactory, writerFactory runhistory.WriterFactory) *cobra.Command {
	var (
		endpoint      string
		database      string
		table         string
		namespace     string
		cluster       string
		workspaceID   string
		resultScope   string
		interval      time.Duration
		ingestTimeout time.Duration
		once          bool
		healthAddr    string
	)
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Continuously ingest Tau Job, RayJob, and Kueue lifecycle metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if endpoint == "" {
				return fmt.Errorf("--kusto-endpoint is required")
			}
			if namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			source, err := sourceFactory()
			if err != nil {
				return err
			}
			writer, err := writerFactory(runhistory.WriterConfig{Endpoint: endpoint, Database: database, Table: table, Timeout: ingestTimeout})
			if err != nil {
				return err
			}
			runContext, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			health := &runhistory.Health{}
			healthErrors, err := runhistory.StartHealthServer(runContext, healthAddr, health)
			if err != nil {
				return fmt.Errorf("start health server: %w", err)
			}
			reconciler := &runhistory.Reconciler{
				Source: source, Writer: writer, Cluster: cluster,
				WorkspaceID: workspaceID, ResultScope: resultScope, WriterRetries: 2,
				Log: cmd.ErrOrStderr(),
			}
			runDone := make(chan error, 1)
			go func() {
				runDone <- runhistory.Run(runContext, reconciler, namespace, interval, once, health)
			}()
			select {
			case err := <-runDone:
				return err
			case err, ok := <-healthErrors:
				if !ok {
					return fmt.Errorf("health server stopped unexpectedly")
				}
				return fmt.Errorf("health server: %w", err)
			}
		},
	}
	cmd.Flags().StringVar(&endpoint, "kusto-endpoint", "", "ADX endpoint for managed lifecycle ingestion")
	cmd.Flags().StringVar(&database, "kusto-database", "Metrics", "ADX database for lifecycle records")
	cmd.Flags().StringVar(&table, "kusto-table", "TauExpRunLifecycle", "ADX table for lifecycle records")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace containing Tau workloads")
	cmd.Flags().StringVar(&cluster, "cluster", "", "stable cluster identifier recorded on every row")
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "default workspace identity for workloads without a workspace stamp")
	cmd.Flags().StringVar(&resultScope, "result-scope", "", "default result scope for workloads without a result-scope stamp")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "reconciliation interval")
	cmd.Flags().DurationVar(&ingestTimeout, "ingest-timeout", 2*time.Minute, "maximum time to wait for ADX ingestion acknowledgement")
	cmd.Flags().BoolVar(&once, "once", false, "reconcile once and exit after the ingestion acknowledgement")
	cmd.Flags().StringVar(&healthAddr, "health-addr", ":8080", "health server listen address")
	return cmd
}
