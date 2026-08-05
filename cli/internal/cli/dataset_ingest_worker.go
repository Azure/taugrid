package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/datasetingest"
)

// newDatasetIngestWorkerCmd returns the hidden ingest-worker subcommand. It is
// invoked by the batch/v1 Job rendered by `tau data dataset ingest` (workspace
// mode). Its interface is stable: flags must not change without bumping the
// worker image.
//
// The command:
//  1. Reads the registry (file:// for local-mode E2E; az:// for in-cluster).
//  2. Reads the source-root and destination.
//  3. Runs RunWorker.
//  4. Writes a stable JSON result to stdout and exits 0 on success, non-zero
//     on failure.
func newDatasetIngestWorkerCmd() *cobra.Command {
	var rf registryFlags
	var sourceRoot, destination string
	cmd := &cobra.Command{
		Use:    ingestWorkerCmdName + " NAME@VERSION",
		Short:  "Run the dataset ingest worker (invoked by the ingest Job; not for direct use)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseDatasetRef(args[0])
			if err != nil {
				return err
			}
			if ref.version == "" {
				return fmt.Errorf("ingest-worker requires NAME@VERSION")
			}

			if err := datasetingest.ValidateAzureURL(sourceRoot); err != nil {
				return fmt.Errorf("--source-root: %w", err)
			}
			if destination != "" {
				if err := datasetingest.ValidateAzureURL(destination); err != nil {
					return fmt.Errorf("--destination: %w", err)
				}
				if !isFileIngestURI(destination) && !strings.HasPrefix(destination, "az://") {
					return fmt.Errorf("--destination must use file:// or az://")
				}
			}

			reg, err := rf.inClusterRegistryClient()
			if err != nil {
				return fmt.Errorf("registry: %w", err)
			}

			rec, err := reg.Get(cmd.Context(), ref.name, ref.version)
			if err != nil {
				return fmt.Errorf("load record: %w", err)
			}
			destination, err = resolveIngestDestination(rec, destination)
			if err != nil {
				return err
			}
			source, sink, locker, err := buildWorkerComponents(cmd.Context(), sourceRoot, destination)
			if err != nil {
				return err
			}

			result, err := datasetingest.RunWorker(cmd.Context(), ref.name, ref.version, datasetingest.WorkerConfig{
				Registry:  reg,
				Source:    source,
				Sink:      sink,
				Locker:    locker,
				AttemptID: fmt.Sprintf("worker-%d", time.Now().UnixNano()),
			})
			if err != nil {
				// Write a failure status JSON to stdout for the orchestrator.
				failStatus := dataset.IngestStatus{
					SchemaVersion: dataset.IngestStatusSchemaVersion,
					Name:          ref.name,
					Version:       ref.version,
					State:         dataset.IngestStateFailed,
					FailureSummary: func() string {
						if len(err.Error()) > 512 {
							return err.Error()[:512]
						}
						return err.Error()
					}(),
				}
				_ = writeJSON(cmd.OutOrStdout(), buildDatasetIngestResult(rec, failStatus))
				return err
			}

			return writeJSON(cmd.OutOrStdout(), buildDatasetIngestResult(rec, result.Status))
		},
	}
	cmd.Flags().StringVar(&sourceRoot, "source-root", "", "source URI: file:///local/dir, az://account/container[/prefix], or public https://...")
	cmd.Flags().StringVar(&destination, "destination", "", "destination URI: file:///local/dir or az://account/container[/prefix] (default: record)")
	_ = cmd.MarkFlagRequired("source-root")
	rf.bind(cmd, "pvc")
	return cmd
}

// simpleRef holds the parsed name and version from a dataset arg.
type simpleRef struct {
	name    string
	version string
}

// parseDatasetRef parses "NAME@VERSION" without importing the dataset package.
// The full dataset.ParseRef is used in other commands; we duplicate the split
// here to keep the worker command free of unnecessary dependencies.
func parseDatasetRef(arg string) (simpleRef, error) {
	parts := strings.SplitN(arg, "@", 2)
	name := parts[0]
	if name == "" {
		return simpleRef{}, fmt.Errorf("invalid dataset ref %q: name must not be empty", arg)
	}
	var version string
	if len(parts) == 2 {
		version = parts[1]
	}
	return simpleRef{name: name, version: version}, nil
}

// buildWorkerComponents selects the correct ByteSource, StagedSink, and
// VersionLocker based on the source-root and destination URIs.
func buildWorkerComponents(
	ctx context.Context,
	sourceRoot, destination string,
) (datasetingest.ByteSource, datasetingest.StagedSink, datasetingest.VersionLocker, error) {
	// Validate schemes.
	srcIsFile := strings.HasPrefix(sourceRoot, "file://") || (!strings.Contains(sourceRoot, "://"))
	dstIsFile := strings.HasPrefix(destination, "file://") || (!strings.Contains(destination, "://"))

	if srcIsFile && dstIsFile {
		srcDir := strings.TrimPrefix(sourceRoot, "file://")
		dstDir := strings.TrimPrefix(destination, "file://")
		dstDir = strings.TrimRight(dstDir, "/")
		if srcDir == "" || dstDir == "" {
			return nil, nil, nil, fmt.Errorf("--source-root and --destination must be non-empty for file:// mode")
		}
		return datasetingest.FileSource{Root: srcDir},
			datasetingest.FileSink{Root: dstDir},
			datasetingest.FileLocker{Dir: dstDir},
			nil
	}

	// Azure mode: AzureSource + AzureSink + AzureLocker.
	if strings.HasPrefix(sourceRoot, "az://") && strings.HasPrefix(destination, "az://") {
		srcAcct, srcCtr, srcPrefix, err := parseAzURL(sourceRoot)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("--source-root: %w", err)
		}
		dstAcct, dstCtr, dstPrefix, err := parseAzURL(destination)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("--destination: %w", err)
		}

		cred, err := datasetingest.NewAzureDefaultCred()
		if err != nil {
			return nil, nil, nil, err
		}

		src, err := datasetingest.NewAzureSource(ctx, srcAcct, srcCtr, srcPrefix, cred)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("azure source: %w", err)
		}
		sink, err := datasetingest.NewAzureSink(ctx, dstAcct, dstCtr, dstPrefix, cred)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("azure sink: %w", err)
		}
		locker, err := datasetingest.NewAzureLocker(ctx, dstAcct, dstCtr, ".tau-ingest-locks", cred)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("azure locker: %w", err)
		}
		return src, sink, locker, nil
	}

	if strings.HasPrefix(sourceRoot, "https://") {
		src, err := datasetingest.NewHTTPSSource(sourceRoot, nil)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("HTTPS source: %w", err)
		}
		if dstIsFile {
			dstDir := strings.TrimRight(strings.TrimPrefix(destination, "file://"), "/")
			if dstDir == "" {
				return nil, nil, nil, fmt.Errorf("--destination must be non-empty for file:// mode")
			}
			return src, datasetingest.FileSink{Root: dstDir}, datasetingest.FileLocker{Dir: dstDir}, nil
		}
		if strings.HasPrefix(destination, "az://") {
			dstAcct, dstCtr, dstPrefix, err := parseAzURL(destination)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("--destination: %w", err)
			}
			cred, err := datasetingest.NewAzureDefaultCred()
			if err != nil {
				return nil, nil, nil, err
			}
			sink, err := datasetingest.NewAzureSink(ctx, dstAcct, dstCtr, dstPrefix, cred)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("azure sink: %w", err)
			}
			locker, err := datasetingest.NewAzureLocker(ctx, dstAcct, dstCtr, ".tau-ingest-locks", cred)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("azure locker: %w", err)
			}
			return src, sink, locker, nil
		}
	}

	return nil, nil, nil, fmt.Errorf(
		"incompatible source/destination schemes: --source-root=%q --destination=%q\n"+
			"source may be file://, az://, or https://; destination must be file:// or az://",
		sourceRoot, destination,
	)
}
