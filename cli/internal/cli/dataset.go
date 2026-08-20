// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/fileutil"
)

func newDatasetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dataset",
		Short: "Register, discover, version, and reference training datasets",
		Long: `Manage the durable dataset registry for pre-training and RL datasets.

The registry is a control plane: small immutable JSON records describe what a
dataset is, where its bytes live (a dedicated dataset storage account, accessed
by workload identity — never a SAS URL), and how to verify and consume them.
Records are immutable once registered; only aliases move (compare-and-swap).

Read verbs (list/show/ref/alias get) read records in-cluster via a short-lived
helper pod that mounts the blob-training PVC, mirroring tau data model. Write/verify
verbs (register/verify) use the caller's Azure RBAC directly so datasets can be
ingested from a laptop or CI before any cluster exists.`,
	}
	cmd.AddCommand(
		newDatasetListCmd(),
		newDatasetShowCmd(),
		newDatasetRegisterCmd(),
		newDatasetRefCmd(),
		newDatasetAliasCmd(),
		newDatasetVerifyCmd(),
		newDatasetRemoveCmd(),
		newDatasetIngestCmd(),
		newDatasetStatusCmd(),
		newDatasetIngestWorkerCmd(),
		newDatasetRegisterWorkerCmd(),
		newDatasetStatusWorkerCmd(),
	)
	return cmd
}

// registryFlags are shared backend-selection flags. The default backend is the
// in-cluster helper-pod-over-blob-training PVC; --registry az://acct/container
// selects the no-cluster Azure-blob backend.
type registryFlags struct {
	registry        string
	namespace       string
	systemNamespace string
	kubeContext     string
	restore         func()
}

func (f *registryFlags) bind(cmd *cobra.Command, defaultRegistry string) {
	cmd.Flags().StringVar(&f.registry, "registry", defaultRegistry, "registry backend: pvc | az://<account>/<container> | file://<dir>")
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "", "namespace for the pvc backend (default: from the connected workspace)")
	cmd.Flags().StringVar(&f.systemNamespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().StringVar(&f.kubeContext, "context", defaultKubeContext(), kubeContextHelp())

	// The pvc backend reads the registry off the workload PVC, so it needs the
	// namespace the workspace actually submits into. Resolving in PreRunE keeps
	// every `tau data dataset` subcommand on the workspace-first path without
	// threading a *cobra.Command through registryClient and its ~14 callers,
	// and lets the kubeconfig swap live for the whole command rather than being
	// restored the moment the backend is constructed.
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if !cmd.Flags().Changed("system-namespace") {
			f.systemNamespace = systemNamespaceFromCommand(cmd)
		}
		if !f.usesPVCBackend() {
			return nil
		}
		kubeContext, namespace, restore, err := resolveWorkloadDataConnection(cmd, f.kubeContext, f.namespace)
		if err != nil {
			return err
		}
		f.kubeContext = kubeContext
		f.namespace = namespace
		f.restore = restore
		return nil
	}
	cmd.PostRun = func(*cobra.Command, []string) {
		if f.restore != nil {
			f.restore()
			f.restore = nil
		}
	}
}

func (f *registryFlags) usesPVCBackend() bool {
	return f.registry == "" || f.registry == "pvc"
}

func (f *registryFlags) registryClient() (*dataset.Registry, error) {
	backend, err := f.backend()
	if err != nil {
		return nil, err
	}
	return dataset.NewRegistry(backend, datasetRegistryPaths(), time.Now), nil
}

// inClusterRegistryClient builds a registry for the hidden in-cluster worker /
// register / status paths. It differs from registryClient only for az://
// registries: it selects the SDK-backed backend (azidentity + azblob) instead
// of the az-CLI backend, because the distroless Tau image has no `az` binary.
// pvc and file backends are identical to registryClient.
func (f *registryFlags) inClusterRegistryClient() (*dataset.Registry, error) {
	backend, err := f.inClusterBackend()
	if err != nil {
		return nil, err
	}
	return dataset.NewRegistry(backend, datasetRegistryPaths(), time.Now), nil
}

func (f *registryFlags) inClusterBackend() (dataset.Backend, error) {
	if strings.HasPrefix(f.registry, "az://") {
		account, container, _, err := parseAzURL(f.registry)
		if err != nil {
			return nil, err
		}
		if container == "" {
			return nil, fmt.Errorf("--registry az URL must be az://<account>/<container>")
		}
		return newSDKAzBackend(account, container)
	}
	return f.backend()
}

func (f *registryFlags) backend() (dataset.Backend, error) {
	switch {
	case f.usesPVCBackend():
		return newPVCBackend(f.kubeContext, f.namespace), nil
	case strings.HasPrefix(f.registry, "az://"):
		account, container, _, err := parseAzURL(f.registry)
		if err != nil {
			return nil, err
		}
		if container == "" {
			return nil, fmt.Errorf("--registry az URL must be az://<account>/<container>")
		}
		return newAzBackend(account, container), nil
	case strings.HasPrefix(f.registry, "file://"):
		dir := strings.TrimPrefix(f.registry, "file://")
		if dir == "" {
			return nil, fmt.Errorf("--registry file URL must be file://<dir>")
		}
		return newFileBackend(dir), nil
	default:
		return nil, fmt.Errorf("--registry must be 'pvc', 'az://<account>/<container>', or 'file://<dir>'")
	}
}

// parseAzURL parses az://<account>/<container>[/<prefix>].
func parseAzURL(u string) (account, container, prefix string, err error) {
	rest := strings.TrimPrefix(u, "az://")
	if rest == u {
		return "", "", "", fmt.Errorf("not an az URL: %q (want az://<account>/<container>[/<prefix>])", u)
	}
	parts := strings.SplitN(rest, "/", 3)
	account = parts[0]
	if account == "" {
		return "", "", "", fmt.Errorf("az URL %q is missing an account", u)
	}
	if len(parts) >= 2 {
		container = parts[1]
	}
	if len(parts) == 3 {
		prefix = strings.TrimSuffix(parts[2], "/")
	}
	return account, container, prefix, nil
}

func newDatasetListCmd() *cobra.Command {
	var rf registryFlags
	var purpose, output string
	var tags []string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered datasets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			if purpose != "" && !validPurpose(purpose) {
				return fmt.Errorf("--purpose must be one of: pretrain, rl, eval")
			}
			tagFilters, err := parseTagPairs(tags)
			if err != nil {
				return err
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			records, warnings, err := reg.List(cmd.Context(), purpose, tagFilters)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", w)
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeDatasetTable(cmd.OutOrStdout(), records)
		},
	}
	cmd.Flags().StringVar(&purpose, "purpose", "", "filter by purpose: pretrain|rl|eval")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "filter by tag key=value (repeatable)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetShowCmd() *cobra.Command {
	var rf registryFlags
	var output string
	cmd := &cobra.Command{
		Use:   "show NAME[@VERSION|@ALIAS]",
		Short: "Show a dataset record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			rec, err := resolveRecord(cmd.Context(), reg, args[0])
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeDatasetDetail(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetRefCmd() *cobra.Command {
	var rf registryFlags
	var output, stagedRoot, baseURL, envPrefix string
	cmd := &cobra.Command{
		Use:   "ref NAME[@VERSION|@ALIAS]",
		Short: "Resolve a dataset to a launch-time reference",
		Long: `Resolve a dataset reference (pinning an alias to a concrete version) and emit
a resolved reference for a consumer to stage and verify its shards.

The reference records name@version+digest as run provenance and exposes three
consumption modes: durable_mount (blobfuse, small/eval only), hot_cache (Lustre),
and node_local_stage (per-rank download+verify to NVMe). node_local_stage is the
recommended mode for pretraining; the registry is a control plane and never
forces a shared FUSE mount as the 16-GPU read path.

  -o json (default): the full resolved reference.
  -o env:           FineWeb-compatible ${PREFIX}_URIS/_SHA256S/_TOKEN_COUNTS env
                    lines. Requires --staged-root (file:// URIs) or --base-url,
                    since the dataset bytes are private (no SAS URLs are minted).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "json" && output != "env" {
				return fmt.Errorf("--output must be one of: json, env")
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			rec, err := reg.Resolve(cmd.Context(), ref)
			if err != nil {
				return err
			}
			resolved := dataset.BuildReference(rec, dataset.ReferenceOptions{
				DurableDatasetsDir: storage.DurableDatasetsDir,
				HotDatasetsDir:     storage.HotDatasetsDir,
				ManifestPath:       storage.DatasetRegistryRecordFile(rec.Name, rec.Version),
			})
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), resolved)
			}
			return writeRefEnv(cmd.OutOrStdout(), resolved, envPrefix, stagedRoot, baseURL)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "json", "json|env")
	cmd.Flags().StringVar(&stagedRoot, "staged-root", "", "local directory where shards are staged; emits file:// URIs for -o env")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "base URL prefix for shard URIs (-o env), e.g. an authenticated proxy")
	cmd.Flags().StringVar(&envPrefix, "env-prefix", "FINEWEB_DATASET", "env var name prefix for -o env output")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage dataset aliases (movable pointers to pinned versions)",
	}
	cmd.AddCommand(newDatasetAliasSetCmd(), newDatasetAliasGetCmd())
	return cmd
}

func newDatasetAliasSetCmd() *cobra.Command {
	var rf registryFlags
	var output, expect string
	var expectAbsent bool
	cmd := &cobra.Command{
		Use:   "set NAME ALIAS VERSION",
		Short: "Point an alias at a version (compare-and-swap)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			rec, err := reg.SetAlias(cmd.Context(), args[0], args[1], args[2], dataset.SetAliasOptions{
				Expect:       expect,
				ExpectAbsent: expectAbsent,
			})
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s:%s -> %s\n", rec.Name, rec.Alias, rec.Version)
			return err
		},
	}
	cmd.Flags().StringVar(&expect, "expect", "", "compare-and-swap: require the alias to currently point at this version")
	cmd.Flags().BoolVar(&expectAbsent, "expect-absent", false, "compare-and-swap: require the alias to not yet exist")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetAliasGetCmd() *cobra.Command {
	var rf registryFlags
	var output string
	cmd := &cobra.Command{
		Use:   "get NAME ALIAS",
		Short: "Read an alias pointer",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "ref" && output != "json" {
				return fmt.Errorf("--output must be one of: ref, json")
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			rec, err := reg.GetAlias(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s@%s\n", rec.Name, rec.Version)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "ref", "ref|json")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetVerifyCmd() *cobra.Command {
	var rf registryFlags
	var from, output string
	cmd := &cobra.Command{
		Use:   "verify NAME@VERSION",
		Short: "Re-hash dataset bytes and check them against the record",
		Long: `Re-scan the dataset bytes and confirm every file's sha256 and the content
digest still match the immutable record. Bytes are read either from --from
(a local directory or az://account/container/prefix) or, by default, from the
account/container/prefix recorded in the dataset record, using the caller's
Azure RBAC.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			if ref.Version == "" {
				return fmt.Errorf("verify requires NAME@VERSION (an exact version, not an alias)")
			}
			rec, err := reg.Get(cmd.Context(), ref.Name, ref.Version)
			if err != nil {
				return err
			}
			src, err := dataSourceFor(from, rec)
			if err != nil {
				return err
			}
			result, err := verifyRecord(cmd.Context(), rec, src)
			if err != nil {
				return err
			}
			if output == "json" {
				if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else {
				printVerifyResult(cmd.OutOrStdout(), result)
			}
			if !result.OK {
				return fmt.Errorf("verification FAILED for %s@%s", rec.Name, rec.Version)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "bytes source: local dir or az://account/container/prefix (default: record pointer)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	rf.bind(cmd, "pvc")
	return cmd
}

func newDatasetRemoveCmd() *cobra.Command {
	var rf registryFlags
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm NAME@VERSION",
		Aliases: []string{"remove"},
		Short:   "Remove a dataset version (blocked while any alias points at it)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := dataset.ParseRef(args[0])
			if err != nil {
				return err
			}
			if ref.Version == "" {
				return fmt.Errorf("rm requires NAME@VERSION (an exact version, not an alias)")
			}
			if !yes {
				return fmt.Errorf("refusing to remove %s@%s without --yes", ref.Name, ref.Version)
			}
			reg, err := rf.registryClient()
			if err != nil {
				return err
			}
			if err := reg.Remove(cmd.Context(), ref.Name, ref.Version); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s@%s\n", ref.Name, ref.Version)
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm removal")
	rf.bind(cmd, "pvc")
	return cmd
}

// resolveRecord resolves NAME, NAME@VERSION, or NAME@ALIAS to a record.
func resolveRecord(ctx context.Context, reg *dataset.Registry, arg string) (dataset.Record, error) {
	ref, err := dataset.ParseRef(arg)
	if err != nil {
		return dataset.Record{}, err
	}
	return reg.Resolve(ctx, ref)
}

func validPurpose(p string) bool {
	return p == dataset.PurposePretrain || p == dataset.PurposeRL || p == dataset.PurposeEval
}

func parseTagPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--tag %q must be key=value", p)
		}
		out[k] = v
	}
	return out, nil
}

func writeJSON(w io.Writer, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(raw))
	return err
}

func writeDatasetTable(w io.Writer, records []dataset.Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tPURPOSE\tFILES\tSIZE\tASSURANCE\tDIGEST\tCREATED")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			r.Name, r.Version, r.Purpose, len(r.Files), humanBytes(r.TotalBytes),
			r.Assurance, shortDigest(r.Digest), r.CreatedAt)
	}
	return tw.Flush()
}

func writeDatasetDetail(w io.Writer, r dataset.Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", r.Name)
	fmt.Fprintf(tw, "Version:\t%s\n", r.Version)
	fmt.Fprintf(tw, "Purpose:\t%s\n", r.Purpose)
	fmt.Fprintf(tw, "Assurance:\t%s\n", r.Assurance)
	fmt.Fprintf(tw, "Digest:\t%s\n", r.Digest)
	fmt.Fprintf(tw, "Files:\t%d\n", len(r.Files))
	if len(r.Components) > 0 {
		fmt.Fprintf(tw, "Components:\t%d\n", len(r.Components))
	}
	fmt.Fprintf(tw, "Total bytes:\t%s (%d)\n", humanBytes(r.TotalBytes), r.TotalBytes)
	if r.Account != "" {
		fmt.Fprintf(tw, "Location:\taz://%s/%s/%s\n", r.Account, r.Container, r.Prefix)
	}
	if r.Source.Repo != "" || r.Source.Kind != "" {
		fmt.Fprintf(tw, "Source:\t%s %s %s\n", r.Source.Kind, r.Source.Repo, r.Source.Revision)
	}
	if r.CreatedAt != "" {
		fmt.Fprintf(tw, "Created:\t%s\n", r.CreatedAt)
	}
	switch {
	case r.Pretrain != nil:
		fmt.Fprintf(tw, "Pretrain:\ttokenizer=%s format=%s total_tokens=%d packing=%t\n",
			r.Pretrain.Tokenizer, r.Pretrain.Format, r.Pretrain.TotalTokens, r.Pretrain.SequencePacking)
	case r.RL != nil:
		fmt.Fprintf(tw, "RL:\tkind=%s policy_run=%s reward_model=%s\n",
			r.RL.Kind, r.RL.PolicyRun, r.RL.RewardModel)
	case r.Eval != nil:
		fmt.Fprintf(tw, "Eval:\ttask=%s split=%s metric=%s\n", r.Eval.Task, r.Eval.Split, r.Eval.Metric)
	}
	if len(r.Tags) > 0 {
		keys := make([]string, 0, len(r.Tags))
		for k := range r.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%s=%s", k, r.Tags[k])
		}
		fmt.Fprintf(tw, "Tags:\t%s\n", b.String())
	}
	return tw.Flush()
}

// writeRefEnv emits FineWeb-compatible env lines. The shard URIs come from
// --staged-root (file:// URIs for already-staged shards, preserving the
// existing urllib loader contract) or --base-url. Private blob bytes are never
// emitted as SAS URLs.
func writeRefEnv(w io.Writer, ref dataset.ResolvedReference, prefix, stagedRoot, baseURL string) error {
	if prefix == "" {
		prefix = "FINEWEB_DATASET"
	}
	files := ref.Modes.NodeLocalStage.Files
	if len(files) == 0 {
		return fmt.Errorf("dataset %s@%s has no files to reference", ref.Name, ref.Version)
	}
	if stagedRoot == "" && baseURL == "" {
		return fmt.Errorf("-o env requires --staged-root <dir> or --base-url <url>: dataset bytes are private and the registry never mints SAS URLs")
	}
	uris := make([]string, 0, len(files))
	shas := make([]string, 0, len(files))
	toks := make([]string, 0, len(files))
	for _, f := range files {
		var uri string
		switch {
		case stagedRoot != "":
			abs, err := filepath.Abs(filepath.Join(stagedRoot, filepath.FromSlash(f.Path)))
			if err != nil {
				return err
			}
			uri = "file://" + filepath.ToSlash(abs)
		default:
			uri = strings.TrimSuffix(baseURL, "/") + "/" + f.Path
		}
		// The FineWeb loader requires a real per-shard token count to size its
		// sampling; a record registered without verified token accounting would
		// otherwise silently emit zeros and corrupt training.
		if f.TokenCount <= 0 {
			return fmt.Errorf("-o env requires a positive token_count for every shard, but %s has %d; re-register the dataset as a verified %s pretraining dataset", f.Path, f.TokenCount, dataset.FormatTokenizedBinUint16)
		}
		uris = append(uris, uri)
		shas = append(shas, f.SHA256)
		toks = append(toks, fmt.Sprintf("%d", f.TokenCount))
	}
	fmt.Fprintf(w, "%s_URIS=%s\n", prefix, strings.Join(uris, ","))
	fmt.Fprintf(w, "%s_SHA256S=%s\n", prefix, strings.Join(shas, ","))
	fmt.Fprintf(w, "%s_TOKEN_COUNTS=%s\n", prefix, strings.Join(toks, ","))
	return nil
}

func shortDigest(d string) string {
	return fileutil.ShortDigest(strings.TrimPrefix(d, "sha256:"), 12)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// dataSourceFor selects the bytes source for register/verify.
func dataSourceFor(from string, rec dataset.Record) (dataSource, error) {
	if from == "" {
		if rec.Account == "" || rec.Container == "" {
			return nil, fmt.Errorf("--from is required: the record has no account/container to read from")
		}
		return azDataSource{account: rec.Account, container: rec.Container, prefix: rec.Prefix}, nil
	}
	if strings.HasPrefix(from, "az://") {
		account, container, prefix, err := parseAzURL(from)
		if err != nil {
			return nil, err
		}
		if container == "" {
			return nil, fmt.Errorf("--from az URL must be az://<account>/<container>[/<prefix>]")
		}
		return azDataSource{account: account, container: container, prefix: prefix}, nil
	}
	info, err := os.Stat(from)
	if err != nil {
		return nil, fmt.Errorf("--from %q: %w", from, err)
	}
	_ = info
	return localDataSource{root: from}, nil
}

// verifyResult reports a verification outcome.
type verifyResult struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	OK           bool     `json:"ok"`
	RecordDigest string   `json:"record_digest"`
	ActualDigest string   `json:"actual_digest"`
	Mismatches   []string `json:"mismatches,omitempty"`
}

func verifyRecord(ctx context.Context, rec dataset.Record, src dataSource) (verifyResult, error) {
	scanned, err := src.scan(ctx)
	if err != nil {
		return verifyResult{}, err
	}
	have := make(map[string]hashedFile, len(scanned))
	for _, f := range scanned {
		have[f.Path] = f
	}
	res := verifyResult{Name: rec.Name, Version: rec.Version, RecordDigest: rec.Digest, OK: true}
	rebuilt := dataset.Record{Files: make([]dataset.File, 0, len(rec.Files))}
	expected := make(map[string]bool, len(rec.Files))
	for _, want := range rec.Files {
		expected[want.Path] = true
		got, ok := have[want.Path]
		if !ok {
			res.OK = false
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: missing from source", want.Path))
			continue
		}
		if !strings.EqualFold(got.SHA256, want.SHA256) {
			res.OK = false
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: sha256 %s != %s", want.Path, got.SHA256, want.SHA256))
		}
		if got.Bytes != want.Bytes {
			res.OK = false
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: bytes %d != %d", want.Path, got.Bytes, want.Bytes))
		}
		rebuilt.Files = append(rebuilt.Files, dataset.File{Path: got.Path, SHA256: got.SHA256})
	}
	// Extra files under the source that the immutable record does not list mean
	// the byte source drifted after registration; fail closed.
	extra := make([]string, 0)
	for p := range have {
		if !expected[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	for _, p := range extra {
		res.OK = false
		res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: extra file in source (not in record)", p))
	}
	res.ActualDigest = rebuilt.ComputeDigest()
	if res.ActualDigest != rec.Digest {
		res.OK = false
	}
	return res, nil
}

func printVerifyResult(w io.Writer, r verifyResult) {
	status := "OK"
	if !r.OK {
		status = "FAILED"
	}
	fmt.Fprintf(w, "%s@%s: %s\n", r.Name, r.Version, status)
	fmt.Fprintf(w, "  record digest: %s\n", r.RecordDigest)
	fmt.Fprintf(w, "  actual digest: %s\n", r.ActualDigest)
	for _, m := range r.Mismatches {
		fmt.Fprintf(w, "  - %s\n", m)
	}
}
