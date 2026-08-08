// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/dataset"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func newDatasetRegisterCmd() *cobra.Command {
	var rf registryFlags
	var (
		purpose, from, manifest, assurance, output string
		account, container, prefix                 string
		tokenizer, format                          string
		sequencePacking                            bool
		rlKind, rlPolicyRun, rlReward, rlSchema    string
		evalTask, evalSplit, evalMetric            string
		srcKind, srcRepo, srcRevision, srcConfig   string
		tags                                       []string
		workspace, workerImage                     string
		wait, dryRun                               bool
	)
	cmd := &cobra.Command{
		Use:   "register NAME@VERSION",
		Short: "Register an immutable dataset version",
		Long: `Register an immutable dataset record. There are two ways to supply the file
list:

  --from <local-dir|az://account/container/prefix>
      Scan the dataset's bytes, compute per-file sha256 (and, for known
      pretraining formats, token counts), and record them. Bytes are read with
      the caller's Azure RBAC (az --auth-mode login) or from a local directory.
      Assurance defaults to "verified".

  --manifest <file.json>
      Record a caller-supplied file list ({source, components[],
      files[]{path,bytes,sha256,token_count,source,domain,split}}) without
      downloading the bytes. Use this to catalog large
      external datasets whose per-file sha256 is already known (e.g. a Hugging
      Face dataset's LFS object ids). Assurance defaults to "manifest-supplied".
      --account/--container/--prefix point at where the bytes live (or will be
      ingested) in the dedicated dataset account.

register refuses to overwrite an existing NAME@VERSION; only aliases move. The
record is written to the registry backend selected by --registry (default: the
in-cluster blob-training PVC; az://account/container for a no-cluster,
server-immutability write; file://dir for a local seed catalog).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			if ref.Version == "" {
				return fmt.Errorf("register requires NAME@VERSION (an exact version, not an alias)")
			}
			if !validPurpose(purpose) {
				return fmt.Errorf("--purpose must be one of: pretrain, rl, eval")
			}
			if from == "" && manifest == "" {
				return fmt.Errorf("one of --from or --manifest is required")
			}
			if from != "" && manifest != "" {
				return fmt.Errorf("--from and --manifest are mutually exclusive")
			}

			source := dataset.Source{Kind: srcKind, Repo: srcRepo, Revision: srcRevision, Config: srcConfig}
			var files []dataset.File
			var components []dataset.Component

			if manifest != "" {
				man, err := loadDatasetManifest(manifest)
				if err != nil {
					return err
				}
				if assurance == "" {
					assurance = dataset.AssuranceManifestSupplied
				}
				// Manifest provenance fills any unset --source-* flag.
				if source.Kind == "" {
					source.Kind = man.Source.Kind
				}
				if source.Repo == "" {
					source.Repo = man.Source.Repo
				}
				if source.Revision == "" {
					source.Revision = man.Source.Revision
				}
				if source.Config == "" {
					source.Config = man.Source.Config
				}
				components = man.Components
				for _, mf := range man.Files {
					files = append(files, dataset.File{
						Path:       mf.Path,
						Bytes:      mf.Bytes,
						SHA256:     mf.SHA256,
						TokenCount: mf.TokenCount,
						Source:     mf.Source,
						Domain:     mf.Domain,
						Split:      mf.Split,
					})
				}
			} else {
				if assurance == "" {
					assurance = dataset.AssuranceVerified
				}
				// If --from is an az URL and explicit pointer flags are unset,
				// default the record's byte-location pointer to it.
				if strings.HasPrefix(from, "az://") {
					a, c, p, perr := parseAzURL(from)
					if perr != nil {
						return perr
					}
					if account == "" {
						account = a
					}
					if container == "" {
						container = c
					}
					if prefix == "" {
						prefix = p
					}
				}
				src, err := dataSourceForRegister(from, account, container, prefix)
				if err != nil {
					return err
				}
				scanned, err := src.scan(cmd.Context())
				if err != nil {
					return err
				}
				if len(scanned) == 0 {
					return fmt.Errorf("no files found at %s", src.describe())
				}
				for _, f := range scanned {
					file := dataset.File{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256}
					if purpose == dataset.PurposePretrain && format == dataset.FormatTokenizedBinUint16 && assurance == dataset.AssuranceVerified {
						if f.Bytes%2 != 0 {
							return fmt.Errorf("file %s has odd byte count %d but format is %s (uint16)", f.Path, f.Bytes, format)
						}
						file.TokenCount = f.Bytes / 2
					}
					files = append(files, file)
				}
			}

			if len(files) == 0 {
				return fmt.Errorf("no files to register")
			}

			var totalTokens int64
			for _, f := range files {
				totalTokens += f.TokenCount
			}

			tagMap, err := parseTagPairs(tags)
			if err != nil {
				return err
			}

			rec := dataset.Record{
				SchemaVersion: dataset.SchemaVersion,
				Name:          ref.Name,
				Version:       ref.Version,
				Purpose:       purpose,
				Account:       account,
				Container:     container,
				Prefix:        prefix,
				Assurance:     assurance,
				CreatedAt:     time.Now().UTC().Format(time.RFC3339),
				Tags:          tagMap,
				Source:        source,
				Files:         files,
				Components:    components,
			}

			switch purpose {
			case dataset.PurposePretrain:
				rec.Pretrain = &dataset.Pretrain{
					Tokenizer:       tokenizer,
					Format:          format,
					TotalTokens:     totalTokens,
					SequencePacking: sequencePacking,
				}
			case dataset.PurposeRL:
				rec.RL = &dataset.RL{
					Kind:        rlKind,
					PolicyRun:   rlPolicyRun,
					RewardModel: rlReward,
					Schema:      rlSchema,
				}
			case dataset.PurposeEval:
				rec.Eval = &dataset.Eval{
					Task:   evalTask,
					Split:  evalSplit,
					Metric: evalMetric,
				}
			}

			// Workspace mode: register the immutable record from inside the
			// cluster under the workspace's workload identity. The record is
			// built and validated locally, transported via a size-bounded
			// ConfigMap, and written by the hidden register worker using the
			// SDK-backed registry (idempotent for an identical digest; drift
			// fails). Local behavior below is unchanged when --workspace is unset.
			if workspace != "" {
				return runRegisterWorkspace(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
					rec, rf, workspace, workerImage, wait, dryRun, output)
			}

			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			written, err := reg.Register(cmd.Context(), rec)
			if err != nil {
				if dataset.IsExist(err) {
					return fmt.Errorf("%s@%s already exists; datasets are immutable (register a new version)", ref.Name, ref.Version)
				}
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), written)
			}
			return writeDatasetDetail(cmd.OutOrStdout(), written)
		},
	}
	cmd.Flags().StringVar(&purpose, "purpose", "", "pretrain|rl|eval (required)")
	cmd.Flags().StringVar(&from, "from", "", "bytes source to scan+hash: local dir or az://account/container/prefix")
	cmd.Flags().StringVar(&manifest, "manifest", "", "manifest json {source,files[]} to record without downloading bytes")
	cmd.Flags().StringVar(&assurance, "assurance", "", "verified|manifest-supplied|trusted (default: verified for --from, manifest-supplied for --manifest)")
	cmd.Flags().StringVar(&account, "account", "", "dataset storage account (record pointer)")
	cmd.Flags().StringVar(&container, "container", "", "dataset blob container (record pointer)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "blob prefix within the container (record pointer)")
	cmd.Flags().StringVar(&tokenizer, "tokenizer", "", "pretrain: tokenizer id (e.g. gpt2)")
	cmd.Flags().StringVar(&format, "format", dataset.FormatTokenizedBinUint16, "pretrain: shard format")
	cmd.Flags().BoolVar(&sequencePacking, "sequence-packing", false, "pretrain: shards use sequence packing")
	cmd.Flags().StringVar(&rlKind, "rl-kind", "", "rl: prompts|preferences|trajectories")
	cmd.Flags().StringVar(&rlPolicyRun, "rl-policy-run", "", "rl: policy run that produced the data")
	cmd.Flags().StringVar(&rlReward, "rl-reward-model", "", "rl: reward model reference")
	cmd.Flags().StringVar(&rlSchema, "rl-schema", "", "rl: record schema id")
	cmd.Flags().StringVar(&evalTask, "eval-task", "", "eval: task name")
	cmd.Flags().StringVar(&evalSplit, "eval-split", "", "eval: split name")
	cmd.Flags().StringVar(&evalMetric, "eval-metric", "", "eval: primary metric")
	cmd.Flags().StringVar(&srcKind, "source-kind", "", "provenance: source kind (e.g. huggingface)")
	cmd.Flags().StringVar(&srcRepo, "source-repo", "", "provenance: source repo")
	cmd.Flags().StringVar(&srcRevision, "source-revision", "", "provenance: source revision")
	cmd.Flags().StringVar(&srcConfig, "source-config", "", "provenance: source config")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag key=value (repeatable)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "register from inside the cluster under this TauWorkspace's workload identity")
	cmd.Flags().StringVar(&workerImage, "worker-image", "", "digest-pinned worker image (image@sha256:...) for --workspace mode")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the register Job to complete (required for --workspace)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "render manifests without applying (workspace mode)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "az://")
	return cmd
}

// runRegisterWorkspace transports the immutable record via a size-bounded
// ConfigMap and runs the hidden register worker Job under the workspace
// workload identity. The transient ConfigMap is always removed after a waited
// run. For safety, non-wait workspace registration is refused: without waiting
// we cannot guarantee cleanup of the ConfigMap that carries the record.
func runRegisterWorkspace(
	ctx context.Context, out, errOut io.Writer,
	rec dataset.Record, rf registryFlags,
	wsName, workerImage string, wait, dryRun bool, output string,
) error {
	if workerImage == "" {
		return fmt.Errorf("--worker-image is required for workspace register")
	}
	if err := validateDigestPinnedImage(workerImage); err != nil {
		return err
	}
	if !strings.HasPrefix(rf.registry, "az://") {
		return fmt.Errorf(
			"workspace register requires an az:// registry (--registry az://<account>/<container>); " +
				"the in-cluster worker writes the record via workload identity")
	}
	if !wait && !dryRun {
		return fmt.Errorf(
			"workspace register requires --wait so the transient record ConfigMap can be cleaned up deterministically")
	}

	// Canonicalize the record (schema/total/digest) so the transported bytes
	// match exactly what the worker will register, and size-check the payload.
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = dataset.SchemaVersion
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()
	recJSON, err := rec.Marshal()
	if err != nil {
		return fmt.Errorf("marshal record for ConfigMap: %w", err)
	}
	if len(recJSON) > configMapPayloadLimit {
		return fmt.Errorf(
			"dataset record is %d bytes which exceeds the ConfigMap transport limit (%d bytes); "+
				"split or compact the manifest, or use a reviewed durable record transport",
			len(recJSON), configMapPayloadLimit,
		)
	}

	ws, err := datasetFetchWorkspace(ctx, rf.kubeContext, tauworkspace.PlatformNamespace, wsName)
	if err != nil {
		return fmt.Errorf("fetch workspace %q: %w", wsName, err)
	}
	ident, err := validateWorkspaceForJob(ws)
	if err != nil {
		return err
	}

	jobName := datasetRunJobName("tau-ds-register", rec.Name, rec.Version)
	cmName := jobName
	labels := map[string]string{
		"app":                     "tau-dataset",
		workloadmeta.LabelDataset: sanitizeLabelValue(rec.Name),
	}
	cmManifest, err := renderRecordConfigMap(cmName, ident.Namespace, datasetRecordFileName, string(recJSON), labels)
	if err != nil {
		return fmt.Errorf("render record ConfigMap: %w", err)
	}
	jobManifest, err := renderDatasetWorkerJob(datasetWorkerJobSpec{
		JobName:     jobName,
		Namespace:   ident.Namespace,
		ServiceAcct: ident.ServiceAccountName,
		Image:       workerImage,
		DatasetName: rec.Name,
		Version:     rec.Version,
		Command:     []string{"tau", "data", "dataset", registerWorkerCmdName},
		FlagArgs: []string{
			"--registry", rf.registry,
			"--record-file", datasetRecordMountPath + "/" + datasetRecordFileName,
		},
		Labels: labels,
		ConfigMapMount: &datasetConfigMapMount{
			ConfigMapName: cmName,
			MountPath:     datasetRecordMountPath,
		},
	})
	if err != nil {
		return fmt.Errorf("render register Job: %w", err)
	}

	fmt.Fprintf(errOut, "workspace register: dataset %s@%s (digest %s)\n", rec.Name, rec.Version, rec.Digest)
	fmt.Fprintf(errOut, "  workspace: %s (namespace %s, sa %s)\n", wsName, ident.Namespace, ident.ServiceAccountName)
	fmt.Fprintf(errOut, "  worker:    %s\n", workerImage)

	if dryRun {
		fmt.Fprintf(errOut, "dry-run: rendered manifests (not applied):\n")
		writeManifests(out, cmManifest, jobManifest)
		return nil
	}

	runner := newDatasetKubeRunner(rf.kubeContext)
	// Always attempt to clean up the transient ConfigMap on the way out.
	defer func() {
		_ = deleteConfigMap(ctx, runner, ident.Namespace, cmName)
	}()

	if _, err := applyManifest(ctx, runner, cmManifest, ""); err != nil {
		return fmt.Errorf("apply record ConfigMap: %w", err)
	}
	if _, err := applyManifest(ctx, runner, jobManifest, ""); err != nil {
		return fmt.Errorf("apply register Job: %w", err)
	}
	fmt.Fprintf(errOut, "applied Job %s/%s\n", ident.Namespace, jobName)

	phase, err := waitForJob(ctx, runner, ident.Namespace, jobName, datasetJobDefaultWait)
	if err != nil {
		return err
	}
	logs, logErr := jobLogs(ctx, runner, ident.Namespace, jobName)
	if phase == "Failed" {
		if logErr == nil && strings.TrimSpace(logs) != "" {
			fmt.Fprintf(errOut, "worker logs:\n%s\n", logs)
		}
		return fmt.Errorf("register Job %s/%s failed", ident.Namespace, jobName)
	}

	raw, err := extractJSONObject(logs)
	if err != nil {
		return fmt.Errorf("read register result from worker logs: %w", err)
	}
	var result datasetRegisterResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse register result: %w", err)
	}
	if output == "json" {
		return writeJSON(out, result)
	}
	if result.Created {
		fmt.Fprintf(out, "registered %s@%s (digest %s)\n", result.Name, result.Version, result.Digest)
	} else {
		fmt.Fprintf(out, "%s@%s already registered with identical digest %s (idempotent no-op)\n", result.Name, result.Version, result.Digest)
	}
	return nil
}

// dataSourceForRegister selects the bytes source for register. A local dir is
// hashed in-process; an az URL is hashed via the Azure CLI with caller RBAC.
func dataSourceForRegister(from, account, container, prefix string) (dataSource, error) {
	if strings.HasPrefix(from, "az://") {
		a, c, p, err := parseAzURL(from)
		if err != nil {
			return nil, err
		}
		if c == "" {
			return nil, fmt.Errorf("--from az URL must be az://<account>/<container>[/<prefix>]")
		}
		return azDataSource{account: a, container: c, prefix: p}, nil
	}
	return localDataSource{root: from}, nil
}

// datasetManifest is the --manifest input: a caller-supplied file list whose
// per-file sha256 is already known (for example a Hugging Face dataset's LFS
// object ids), so a large external dataset can be cataloged without downloading
// its bytes. The recorded assurance is "manifest-supplied": the registry did
// not recompute the hashes itself.
type datasetManifest struct {
	Source     dataset.Source      `json:"source"`
	Components []dataset.Component `json:"components,omitempty"`
	Files      []manifestFile      `json:"files"`
}

type manifestFile struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	TokenCount int64  `json:"token_count,omitempty"`
	Source     string `json:"source,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Split      string `json:"split,omitempty"`
}

func loadDatasetManifest(p string) (datasetManifest, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return datasetManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m datasetManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return datasetManifest{}, fmt.Errorf("parse manifest %s: %w", p, err)
	}
	if len(m.Files) == 0 {
		return datasetManifest{}, fmt.Errorf("manifest %s has no files", p)
	}
	return m, nil
}
