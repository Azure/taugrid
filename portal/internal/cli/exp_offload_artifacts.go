package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/portal/internal/artifactoffload"
	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func newExpOffloadArtifactsCmd(storePath *string) *cobra.Command {
	var opts artifactoffload.Options
	var output string
	var jsonOutput bool
	var objectStoreKind, objectRoot, objectBaseURI, accountURL string
	var watch bool
	var interval time.Duration
	var maxIterations int
	var completionFile string
	cmd := &cobra.Command{
		Use:   "artifacts",
		Short: "Upload indexed local artifacts to durable object storage",
		Long: `Upload indexed local artifacts to durable object storage.

This command is the rich-artifact companion to scalar metrics offload. It reads
artifact rows from expstore, uploads and verifies local/PVC artifact bytes,
writes a restartable JSON checkpoint, then commits durable references back to
the expstore index. Scalar metrics remain on the existing adx-mon path.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			objectStore, baseURI, err := openArtifactObjectStore(cmd.Context(), objectStoreKind, objectRoot, objectBaseURI, opts.Account, opts.Container, accountURL)
			if err != nil {
				return err
			}
			opts.ObjectStore = objectStore
			opts.ObjectBaseURI = baseURI
			if watch {
				results, err := runArtifactOffloadWatch(cmd.Context(), store, opts, interval, maxIterations, completionFile)
				if err != nil {
					return err
				}
				if out == "json" {
					return writeExpJSON(cmd.OutOrStdout(), results)
				}
				return writeArtifactOffloadWatchTable(cmd.OutOrStdout(), results)
			}
			result, err := artifactoffload.Run(cmd.Context(), store, opts)
			if out == "json" {
				if writeErr := writeExpJSON(cmd.OutOrStdout(), result); writeErr != nil {
					return writeErr
				}
			} else if writeErr := writeArtifactOffloadTable(cmd.OutOrStdout(), result); writeErr != nil {
				return writeErr
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opts.RunID, "run", "", "run id whose indexed artifacts should be uploaded (required)")
	cmd.Flags().StringVar(&opts.Out, "out", "", "spool/checkpoint output directory (required unless --checkpoint is set)")
	cmd.Flags().StringVar(&opts.Checkpoint, "checkpoint", "", "explicit artifact upload checkpoint file")
	cmd.Flags().StringVar(&objectStoreKind, "object-store", "file", "object store implementation: file|azblob")
	cmd.Flags().StringVar(&objectRoot, "object-root", "", "root directory for file object store (required for --object-store=file)")
	cmd.Flags().StringVar(&objectBaseURI, "object-base-uri", "", "durable URI prefix recorded in artifact references (default: file://<object-root>)")
	cmd.Flags().StringVar(&opts.Account, "account", "", "object-store account name recorded in durable references")
	cmd.Flags().StringVar(&opts.Container, "container", "", "object-store container name recorded in durable references")
	cmd.Flags().StringVar(&accountURL, "account-url", "", "azblob account URL (default: https://<account>.blob.core.windows.net)")
	cmd.Flags().Int64Var(&opts.MaxSizeBytes, "max-size-bytes", 0, "reject artifact payloads larger than this size; 0 disables the limit")
	cmd.Flags().BoolVar(&watch, "watch", false, "poll for newly indexed artifacts until interrupted, --max-iterations, or --completion-file")
	cmd.Flags().DurationVar(&interval, "interval", 1*time.Minute, "poll interval when --watch is set")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum watch iterations; 0 runs until interrupted or completion sentinel")
	cmd.Flags().StringVar(&completionFile, "completion-file", "", "sentinel file; when present, watch mode performs one final drain and exits")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func openArtifactObjectStore(ctx context.Context, kind, root, baseURI, account, container, accountURL string) (blobstore.Store, string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "file"
	}
	switch kind {
	case "file":
		fileStore, err := blobstore.NewFileStore(os.ExpandEnv(root))
		if err != nil {
			return nil, "", err
		}
		baseURI = strings.TrimSpace(os.ExpandEnv(baseURI))
		if baseURI == "" {
			baseURI = fileStore.BaseURI
		}
		return fileStore, strings.TrimRight(baseURI, "/"), nil
	case blobstore.AzureObjectStoreKind:
		azureStore, err := blobstore.NewAzureStore(ctx, blobstore.AzureOptions{
			Account:    os.ExpandEnv(account),
			Container:  os.ExpandEnv(container),
			AccountURL: os.ExpandEnv(accountURL),
			BaseURI:    os.ExpandEnv(baseURI),
		})
		if err != nil {
			return nil, "", err
		}
		return azureStore, azureStore.BaseURI, nil
	default:
		return nil, "", fmt.Errorf("--object-store must be file or azblob")
	}
}

func runArtifactOffloadWatch(ctx context.Context, store *expstore.Store, opts artifactoffload.Options, interval time.Duration, maxIterations int, completionFile string) ([]artifactoffload.Result, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("--interval must be positive")
	}
	if maxIterations < 0 {
		return nil, fmt.Errorf("--max-iterations must be non-negative")
	}
	var results []artifactoffload.Result
	for iteration := 0; ; iteration++ {
		result, err := artifactoffload.Run(ctx, store, opts)
		if err != nil && !artifactoffload.IsPartialFailure(err) {
			return results, err
		}
		completed, err := artifactCompletionExists(completionFile)
		if err != nil {
			return results, err
		}
		result.Completed = completed
		results = append(results, result)
		if completed || (maxIterations > 0 && iteration+1 >= maxIterations) {
			return results, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return results, ctx.Err()
		case <-timer.C:
		}
	}
}

func artifactCompletionExists(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func writeArtifactOffloadTable(w io.Writer, result artifactoffload.Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "RUN\tUPLOADED\tDEDUPED\tSKIPPED\tVERIFIED\tINDEXED\tFAILED\tCHECKPOINT\n")
	fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", result.RunID, result.Uploaded, result.Deduped, result.Skipped, result.Verified, result.Indexed, result.Failed, result.Checkpoint)
	return tw.Flush()
}

func writeArtifactOffloadWatchTable(w io.Writer, results []artifactoffload.Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ITERATION\tRUN\tUPLOADED\tDEDUPED\tSKIPPED\tVERIFIED\tINDEXED\tFAILED\tCOMPLETED\tCHECKPOINT\n")
	for i, result := range results {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%t\t%s\n", i+1, result.RunID, result.Uploaded, result.Deduped, result.Skipped, result.Verified, result.Indexed, result.Failed, result.Completed, result.Checkpoint)
	}
	return tw.Flush()
}

func newExpOffloadArtifactsAgentCmd() *cobra.Command {
	var name, namespace, image, pvc, mountPath, store, run, out, objectStoreKind, objectRoot, objectBaseURI, account, container, accountURL, interval, completionFile, serviceAccount string
	var cpuRequest, memoryRequest, cpuLimit, memoryLimit string
	var nodeSelectors []string
	var maxIterations int
	cmd := &cobra.Command{
		Use:   "artifacts-agent",
		Short: "Render a Kubernetes worker that uploads Tau artifacts to durable storage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if image == "" {
				return fmt.Errorf("--image is required: specify the container image containing the tau binary")
			}
			manifest, err := renderArtifactOffloadAgentManifest(artifactOffloadAgentManifestOptions{
				Name:           name,
				Namespace:      namespace,
				Image:          image,
				PVC:            pvc,
				MountPath:      mountPath,
				Store:          store,
				Run:            run,
				Out:            out,
				ObjectStore:    objectStoreKind,
				ObjectRoot:     objectRoot,
				ObjectBaseURI:  objectBaseURI,
				Account:        account,
				Container:      container,
				AccountURL:     accountURL,
				Interval:       interval,
				MaxIterations:  maxIterations,
				CompletionFile: completionFile,
				ServiceAccount: serviceAccount,
				CPURequest:     cpuRequest,
				MemoryRequest:  memoryRequest,
				CPULimit:       cpuLimit,
				MemoryLimit:    memoryLimit,
				NodeSelectors:  nodeSelectors,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), manifest)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "tau-artifacts-agent", "Deployment name")
	cmd.Flags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace")
	cmd.Flags().StringVar(&image, "image", "", "container image containing the tau binary (required)")
	cmd.Flags().StringVar(&pvc, "pvc", "blob-training", "PVC name that contains Tau/Stellar outputs")
	cmd.Flags().StringVar(&mountPath, "mount-path", "/data", "PVC mount path inside the artifacts agent")
	cmd.Flags().StringVar(&store, "store", "/data/tau-exp", "Tau expstore path inside the mounted PVC")
	cmd.Flags().StringVar(&run, "run", "", "run id whose artifacts should be uploaded (required)")
	cmd.Flags().StringVar(&out, "out", "/data/tau-artifacts-offload", "spool/checkpoint output root inside the mounted PVC")
	cmd.Flags().StringVar(&objectStoreKind, "object-store", "file", "object store implementation: file|azblob")
	cmd.Flags().StringVar(&objectRoot, "object-root", "/data/tau-object-store", "file object-store root inside the mounted PVC")
	cmd.Flags().StringVar(&objectBaseURI, "object-base-uri", "", "durable URI prefix recorded in artifact references")
	cmd.Flags().StringVar(&account, "account", "", "azblob storage account name")
	cmd.Flags().StringVar(&container, "container", "", "azblob container name")
	cmd.Flags().StringVar(&accountURL, "account-url", "", "azblob account URL")
	cmd.Flags().StringVar(&interval, "interval", "10s", "watch interval")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum watch iterations; 0 runs until completion sentinel or interruption")
	cmd.Flags().StringVar(&completionFile, "completion-file", "/data/tau-artifacts.done", "sentinel file that triggers one final drain and exit")
	cmd.Flags().StringVar(&serviceAccount, "service-account", "default", "service account for the artifacts agent")
	cmd.Flags().StringVar(&cpuRequest, "cpu-request", "100m", "CPU request for the artifacts agent container")
	cmd.Flags().StringVar(&memoryRequest, "memory-request", "256Mi", "memory request for the artifacts agent container")
	cmd.Flags().StringVar(&cpuLimit, "cpu-limit", "1", "CPU limit for the artifacts agent container")
	cmd.Flags().StringVar(&memoryLimit, "memory-limit", "1Gi", "memory limit for the artifacts agent container")
	cmd.Flags().StringArrayVar(&nodeSelectors, "node-selector", nil, "node selector key=value for the artifacts agent pod (repeatable)")
	return cmd
}

type artifactOffloadAgentManifestOptions struct {
	Name           string
	Namespace      string
	Image          string
	PVC            string
	MountPath      string
	Store          string
	Run            string
	Out            string
	ObjectStore    string
	ObjectRoot     string
	ObjectBaseURI  string
	Account        string
	Container      string
	AccountURL     string
	Interval       string
	MaxIterations  int
	CompletionFile string
	ServiceAccount string
	CPURequest     string
	MemoryRequest  string
	CPULimit       string
	MemoryLimit    string
	NodeSelectors  []string
}

func renderArtifactOffloadAgentManifest(opts artifactOffloadAgentManifestOptions) (string, error) {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Namespace = strings.TrimSpace(opts.Namespace)
	opts.Image = strings.TrimSpace(opts.Image)
	opts.PVC = strings.TrimSpace(opts.PVC)
	opts.MountPath = strings.TrimSpace(opts.MountPath)
	opts.Store = strings.TrimSpace(opts.Store)
	opts.Run = strings.TrimSpace(opts.Run)
	opts.Out = strings.TrimSpace(opts.Out)
	opts.ObjectStore = strings.TrimSpace(opts.ObjectStore)
	opts.ObjectRoot = strings.TrimSpace(opts.ObjectRoot)
	opts.ObjectBaseURI = strings.TrimSpace(opts.ObjectBaseURI)
	opts.Account = strings.TrimSpace(opts.Account)
	opts.Container = strings.TrimSpace(opts.Container)
	opts.AccountURL = strings.TrimSpace(opts.AccountURL)
	opts.ServiceAccount = strings.TrimSpace(opts.ServiceAccount)
	opts.CPURequest = strings.TrimSpace(opts.CPURequest)
	opts.MemoryRequest = strings.TrimSpace(opts.MemoryRequest)
	opts.CPULimit = strings.TrimSpace(opts.CPULimit)
	opts.MemoryLimit = strings.TrimSpace(opts.MemoryLimit)
	if opts.ObjectStore == "" {
		opts.ObjectStore = "file"
	}
	if opts.Name == "" || opts.Namespace == "" || opts.Image == "" || opts.PVC == "" || opts.MountPath == "" || opts.Store == "" || opts.Run == "" || opts.Out == "" {
		return "", fmt.Errorf("--name, --namespace, --image, --pvc, --mount-path, --store, --run, and --out are required")
	}
	switch opts.ObjectStore {
	case "file":
		if opts.ObjectRoot == "" {
			return "", fmt.Errorf("--object-root is required for --object-store=file")
		}
	case blobstore.AzureObjectStoreKind:
		if opts.Account == "" || opts.Container == "" {
			return "", fmt.Errorf("--account and --container are required for --object-store=azblob")
		}
	default:
		return "", fmt.Errorf("--object-store must be file or azblob")
	}
	if opts.Interval == "" {
		opts.Interval = "10s"
	}
	if _, err := time.ParseDuration(opts.Interval); err != nil {
		return "", fmt.Errorf("--interval: %w", err)
	}
	if opts.MaxIterations < 0 {
		return "", fmt.Errorf("--max-iterations must be non-negative")
	}
	if opts.ServiceAccount == "" {
		opts.ServiceAccount = "default"
	}
	if opts.CPURequest == "" || opts.MemoryRequest == "" || opts.CPULimit == "" || opts.MemoryLimit == "" {
		return "", fmt.Errorf("--cpu-request, --memory-request, --cpu-limit, and --memory-limit are required")
	}
	nodeSelector, err := parseAgentKeyValues(opts.NodeSelectors, "--node-selector")
	if err != nil {
		return "", err
	}
	args := []string{
		"experiment", "--store", opts.Store,
		"offload", "artifacts",
		"--watch",
		"--interval", opts.Interval,
		"--run", opts.Run,
		"--out", opts.Out,
		"--object-store", opts.ObjectStore,
	}
	if opts.ObjectStore == "file" {
		args = append(args, "--object-root", opts.ObjectRoot)
	}
	if opts.ObjectStore == blobstore.AzureObjectStoreKind {
		args = append(args, "--account", opts.Account, "--container", opts.Container)
		if opts.AccountURL != "" {
			args = append(args, "--account-url", opts.AccountURL)
		}
	}
	if opts.MaxIterations > 0 {
		args = append(args, "--max-iterations", fmt.Sprint(opts.MaxIterations))
	}
	if strings.TrimSpace(opts.CompletionFile) != "" {
		args = append(args, "--completion-file", strings.TrimSpace(opts.CompletionFile))
	}
	if opts.ObjectBaseURI != "" {
		args = append(args, "--object-base-uri", opts.ObjectBaseURI)
	}
	return renderOffloadAgentDeploymentManifest(offloadAgentDeploymentManifestOptions{
		Name:           opts.Name,
		Namespace:      opts.Namespace,
		AppName:        "tau-artifacts-agent",
		ServiceAccount: opts.ServiceAccount,
		ContainerName:  "artifacts-agent",
		Image:          opts.Image,
		Args:           args,
		CPURequest:     opts.CPURequest,
		MemoryRequest:  opts.MemoryRequest,
		CPULimit:       opts.CPULimit,
		MemoryLimit:    opts.MemoryLimit,
		VolumeName:     "tau-artifacts",
		MountPath:      opts.MountPath,
		PVC:            opts.PVC,
		NodeSelector:   nodeSelector,
	})
}
