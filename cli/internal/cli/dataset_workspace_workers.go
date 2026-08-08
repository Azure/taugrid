// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/dataset"
)

const (
	registerWorkerCmdName = "register-worker"
	statusWorkerCmdName   = "status-worker"

	// datasetRecordMountPath is where the transport ConfigMap is mounted in the
	// register worker pod, and datasetRecordFileName is the key inside it.
	datasetRecordMountPath = "/etc/tau-dataset"
	datasetRecordFileName  = "record.json"
)

// datasetRegisterResult is the stable JSON emitted by the register worker (and
// by workspace-mode register) so the orchestrator can report idempotence.
type datasetRegisterResult struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	Digest        string `json:"digest"`
	// Created is true when this call registered a new record, false when an
	// identical record already existed (idempotent no-op).
	Created bool `json:"created"`
}

const datasetRegisterResultSchemaVersion = 1

// newDatasetRegisterWorkerCmd returns the hidden register-worker subcommand.
// It is invoked by the batch/v1 Job rendered by `tau data dataset register`
// (workspace mode). It reads the immutable record from a mounted ConfigMap and
// registers it idempotently under the workspace's workload identity using the
// SDK-backed registry backend (no `az` binary required).
func newDatasetRegisterWorkerCmd() *cobra.Command {
	var rf registryFlags
	var recordFile string
	cmd := &cobra.Command{
		Use:    registerWorkerCmdName,
		Short:  "Register a dataset record from a mounted file (invoked by the register Job; not for direct use)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(recordFile)
			if err != nil {
				return fmt.Errorf("read record file %s: %w", recordFile, err)
			}
			var rec dataset.Record
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("parse record file %s: %w", recordFile, err)
			}

			reg, err := rf.inClusterRegistryClient()
			if err != nil {
				return fmt.Errorf("registry: %w", err)
			}

			written, created, err := reg.EnsureRegister(cmd.Context(), rec)
			if err != nil {
				return err
			}
			// Establish the ingest-status baseline so `dataset status` works.
			if _, _, err := reg.InitIngestStatus(cmd.Context(), written); err != nil {
				return fmt.Errorf("init ingest status: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), datasetRegisterResult{
				SchemaVersion: datasetRegisterResultSchemaVersion,
				Name:          written.Name,
				Version:       written.Version,
				Digest:        written.Digest,
				Created:       created,
			})
		},
	}
	cmd.Flags().StringVar(&recordFile, "record-file", datasetRecordMountPath+"/"+datasetRecordFileName, "path to the mounted record JSON")
	rf.bind(cmd, "pvc")
	return cmd
}

// newDatasetStatusWorkerCmd returns the hidden status-worker subcommand. It is
// invoked by the Job rendered by `tau data dataset status --workspace` so a
// researcher without direct Azure RBAC can read a project's ingest status via
// the workspace workload identity. It prints the IngestStatus JSON to stdout.
func newDatasetStatusWorkerCmd() *cobra.Command {
	var rf registryFlags
	cmd := &cobra.Command{
		Use:    statusWorkerCmdName + " NAME@VERSION",
		Short:  "Read a dataset ingest status (invoked by the status Job; not for direct use)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseDatasetRef(args[0])
			if err != nil {
				return err
			}
			if ref.version == "" {
				return fmt.Errorf("status-worker requires NAME@VERSION")
			}
			reg, err := rf.inClusterRegistryClient()
			if err != nil {
				return fmt.Errorf("registry: %w", err)
			}
			status, err := reg.GetIngestStatus(cmd.Context(), ref.name, ref.version)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), status)
		},
	}
	rf.bind(cmd, "pvc")
	return cmd
}
