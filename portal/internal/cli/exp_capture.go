// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/portal/internal/expcapture"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func newExpCaptureCmd(storePath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture",
		Short: "Capture cluster runtime context into the experiment store",
	}
	cmd.AddCommand(newExpCaptureRunProfileCmd(storePath))
	return cmd
}

func newExpCaptureRunProfileCmd(storePath *string) *cobra.Command {
	var (
		namespace      string
		kubeContext    string
		project        string
		groupID        string
		owner          string
		state          string
		idempotencyKey string
		output         string
		jsonOutput     bool
	)
	cmd := &cobra.Command{
		Use:   "run-profile JOB",
		Short: "Persist tau run status --run-profile context for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			ns := namespace
			if ns == "" {
				ns = "default"
			}
			r := kube.New(kubeContext)
			snap, err := status.Fetch(cmd.Context(), r, ns, args[0])
			if err != nil {
				return err
			}
			record, err := expcapture.RunData(snap, status.CostProfile{}, status.ExperimentRunDataOptions{
				Project:    project,
				RunGroupID: groupID,
				Owner:      owner,
				State:      state,
				Cluster:    kubeContext,
			})
			if err != nil {
				return err
			}
			if idempotencyKey == "" {
				idempotencyKey = "capture-run-profile-" + record.Run.RunID
			}
			record.IdempotencyKey = idempotencyKey

			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			if existing, ok, err := existingRunRecord(cmd.Context(), store, record.Run.RunID); err != nil {
				return err
			} else if ok {
				record.Run = existing
			}
			result, err := store.RecordRunData(cmd.Context(), record)
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeRunProfileCaptureTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace containing the run Job (default: default)")
	cmd.Flags().StringVar(&kubeContext, "context", kube.DefaultContext(), kube.ContextHelp())
	cmd.Flags().StringVar(&project, "project", "", "project id for newly captured runs (default: store manifest project)")
	cmd.Flags().StringVar(&groupID, "group", "", "run group id for newly captured runs (default: default)")
	cmd.Flags().StringVar(&owner, "owner", "", "run owner for newly captured runs (default: tau-status)")
	cmd.Flags().StringVar(&state, "state", "", "run state override for newly captured runs (default: derived from Job state)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key (default: capture-run-profile-<run-id>)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func existingRunRecord(ctx context.Context, store *expstore.Store, runID string) (expstore.RunRecord, bool, error) {
	statusResult, err := store.Status(ctx, runID)
	if err != nil {
		if errors.Is(err, expstore.ErrNotFound) {
			return expstore.RunRecord{}, false, nil
		}
		return expstore.RunRecord{}, false, err
	}
	if statusResult.TargetType != "run" || statusResult.Run == nil {
		return expstore.RunRecord{}, false, nil
	}
	return runRecordFromStatusRow(statusResult.Run), true, nil
}

func runRecordFromStatusRow(row map[string]any) expstore.RunRecord {
	return expstore.RunRecord{
		RunID:        expString(row, "run_id"),
		Project:      expString(row, "project"),
		RunGroupID:   expString(row, "run_group_id"),
		ParentRunID:  expString(row, "parent_run_id"),
		State:        expString(row, "state"),
		Owner:        expString(row, "owner"),
		CreatedAt:    expString(row, "created_at"),
		StartedAt:    expString(row, "started_at"),
		CompletedAt:  expString(row, "completed_at"),
		ConfigHash:   expString(row, "config_hash"),
		CodeSHA:      expString(row, "code_sha"),
		ImageDigest:  expString(row, "image_digest"),
		TauCommand:   expString(row, "tau_command"),
		ResultURI:    expString(row, "result_uri"),
		IndexVersion: expString(row, "index_version"),
	}
}

func expString(row map[string]any, key string) string {
	value := row[key]
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func writeRunProfileCaptureTable(w io.Writer, result expstore.RecordRunDataResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "run\t%s\n", result.RunID)
	fmt.Fprintf(tw, "created_run\t%t\n", result.CreatedRun)
	fmt.Fprintf(tw, "run_context\t%t\n", result.RunContext)
	fmt.Fprintf(tw, "reused\t%t\n", result.Reused)
	if result.IdempotencyKey != "" {
		fmt.Fprintf(tw, "idempotency_key\t%s\n", result.IdempotencyKey)
	}
	return tw.Flush()
}
