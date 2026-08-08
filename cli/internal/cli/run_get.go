package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type runResultRef struct {
	Path          string
	PVC           string
	Publication   string
	PublicationID string
	BundleID      string
	ArtifactStore string
	// CheckpointArtifact is the storage.checkpoint value the run declared,
	// empty when it declared none. Presence is what lets an empty result
	// directory be reported as a missing promised artifact rather than as an
	// ordinary empty listing.
	CheckpointArtifact string
}

// cleanRelativeArtifact keeps --artifact inside the run's result directory.
func cleanRelativeArtifact(artifact string) (string, error) {
	if strings.HasPrefix(artifact, "/") {
		return "", fmt.Errorf("--artifact must be relative, got %s", artifact)
	}
	clean := path.Clean(artifact)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("--artifact must stay under the result directory, got %s", artifact)
	}
	return clean, nil
}

func newRunGetCmd() *cobra.Command {
	var (
		connection   runLifecycleConnectionFlags
		artifact     string
		pathOverride string
		pvcOverride  string
		destination  string
		output       string
	)
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Fetch the result file or directory recorded by storage.output",
		Long: `Fetch artifacts a run Job or RayJob wrote to its configured storage.output path.

Reads the ` + workloadmeta.AnnotationResultPath + ` and ` + workloadmeta.AnnotationResultPVC + ` annotations the
run recorded on the Job. If the path is a file, it's catted directly. If the
path is a directory, its recursive listing is printed; pass --artifact NAME to
fetch one file from it.

For runs submitted by a current Tau version, --destination downloads the complete
acknowledged bundle: staged terminal artifacts, immutable metrics histories and
offload metadata, plus the durable checkpoint tree when declared. Tau records the
non-secret Blob CSI account/container identity on new workloads (and falls back
to bound-PV discovery for legacy workloads), then reads through Azure RBAC with
DefaultAzureCredential. It never reads storage Secrets, account keys, or SAS
tokens and does not create a PVC-reader pod. The command fails closed if either
the staged publication marker or final bundle acknowledgement is absent, and it
never replaces existing destination files.
Override the recorded path/pvc with --path/--pvc.

Examples:
  tau run get swordfish-bench-001 -n ray
  tau run get swordfish-bench-001 -n ray --artifact profile/rank-0.summary.md
  tau run get swordfish-bench-001 -n ray --destination ./results
  tau run get my-job --path /data/my-job/results -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedContext, ns, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			switch output {
			case "table", "json", "raw":
			default:
				return fmt.Errorf("--output must be one of: table, json, raw")
			}

			ref := runResultRef{}
			if pathOverride == "" || pvcOverride == "" {
				ref, err = runResultRefFor(cmd.Context(), resolvedContext, ns, name)
				if err != nil {
					return err
				}
			}
			if pathOverride != "" {
				if ref.Path != "" && path.Clean(pathOverride) != path.Clean(ref.Path) {
					ref.BundleID = ""
					ref.Publication = ""
					ref.PublicationID = ""
				}
				ref.Path = pathOverride
			}
			if pvcOverride != "" {
				ref.PVC = pvcOverride
				ref.ArtifactStore = ""
			}
			if ref.Path == "" {
				return fmt.Errorf("run workload %s/%s has no %s; resubmit with storage.output, or pass --path", ns, name, workloadmeta.AnnotationResultPath)
			}
			if ref.PVC == "" {
				return fmt.Errorf("run workload %s/%s has no %s; pass --pvc to override", ns, name, workloadmeta.AnnotationResultPVC)
			}
			if destination != "" && artifact != "" {
				return fmt.Errorf("--destination and --artifact cannot be combined")
			}
			var blobVolume runBlobVolume
			if strings.TrimSpace(ref.ArtifactStore) != "" {
				blobVolume, err = parseRunBlobVolume(ref.ArtifactStore)
			} else {
				blobVolume, err = resolveRunBlobVolume(cmd.Context(), kube.New(resolvedContext), ns, ref.PVC)
			}
			if err != nil {
				return err
			}
			store, err := newAzureRunArtifactStore(blobVolume)
			if err != nil {
				return err
			}
			if ref.BundleID != "" || destination != "" {
				manifest, loadErr := artifactbundle.Load(cmd.Context(), store, ref.Path, ref.BundleID)
				if loadErr != nil {
					if destination != "" {
						return fmt.Errorf(
							"complete artifact bundle is unavailable: %w; this run may predate Tau's final bundle acknowledgement",
							loadErr,
						)
					}
					return loadErr
				}
				if manifest.ResultPVC != ref.PVC || path.Clean(manifest.ResultRoot) != path.Clean(ref.Path) {
					return fmt.Errorf("artifact bundle identity does not match workload result metadata")
				}
				if destination != "" {
					objects, err := artifactbundle.Enumerate(cmd.Context(), store, manifest)
					if err != nil {
						return err
					}
					files, err := artifactbundle.Download(cmd.Context(), store, manifest, objects, destination)
					if err != nil {
						return err
					}
					return writeRunBundleDownload(cmd, output, manifest, destination, files)
				}
				if artifact == "" {
					objects, err := artifactbundle.Enumerate(cmd.Context(), store, manifest)
					if err != nil {
						return err
					}
					entries := make([]string, 0, len(objects))
					for _, object := range objects {
						entries = append(entries, object.Name)
					}
					return writeRunGet(cmd, output, nil, entries, manifest.ResultRoot, manifest.ResultPVC, ref.CheckpointArtifact)
				}
			}

			resultPath := ref.Path
			if ref.Publication == artifactpublish.ModeStaged {
				if strings.TrimSpace(ref.PublicationID) == "" {
					return fmt.Errorf("staged artifact publication has no generation ID")
				}
				resultPath = path.Join(ref.Path, artifactpublish.GenerationsDir, ref.PublicationID)
				marker := path.Join(resultPath, artifactpublish.CompletionMarker)
				raw, err := readRunBlobPath(cmd.Context(), store, marker)
				if err != nil {
					return fmt.Errorf("staged artifacts are not completely published: %w", err)
				}
				if strings.TrimSpace(string(raw)) != "complete "+ref.PublicationID {
					return fmt.Errorf("staged artifact publication marker %s is invalid", marker)
				}
			}

			isDir := looksLikeDirectory(resultPath)
			// Single artifact path: treat as file fetch (joins onto the dir).
			if isDir && artifact != "" {
				cleanArtifact, err := cleanRelativeArtifact(artifact)
				if err != nil {
					return err
				}
				file := path.Join(resultPath, cleanArtifact)
				raw, err := readRunBlobPath(cmd.Context(), store, file)
				if err != nil {
					return err
				}
				return writeRunGet(cmd, output, raw, nil, file, ref.PVC, "")
			}
			if !isDir {
				raw, err := readRunBlobPath(cmd.Context(), store, resultPath)
				if err != nil {
					return err
				}
				return writeRunGet(cmd, output, raw, nil, resultPath, ref.PVC, "")
			}
			entries, err := listRunBlobPath(cmd.Context(), store, resultPath)
			if err != nil {
				return err
			}
			return writeRunGet(cmd, output, nil, entries, resultPath, ref.PVC, ref.CheckpointArtifact)
		},
	}
	connection.add(cmd)
	cmd.Flags().StringVar(&artifact, "artifact", "", "fetch this filename under the result directory")
	cmd.Flags().StringVar(&pathOverride, "path", "", "override the recorded result path")
	cmd.Flags().StringVar(&pvcOverride, "pvc", "", "override the recorded result PVC")
	cmd.Flags().StringVarP(&destination, "destination", "d", "", "download the complete acknowledged bundle into this directory")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json|raw")
	return cmd
}

// runResultRefFor reads result metadata from the user-owned Job or RayJob.
// After Kubernetes metadata is deleted, callers may still fetch durable output
// by supplying both --path and --pvc.
func runResultRefFor(ctx context.Context, kubeContext, namespace, name string) (runResultRef, error) {
	return runResultRefWithReader(ctx, kube.New(kubeContext), namespace, name)
}

type runResultReader interface {
	Raw(context.Context, []string, []byte) (string, error)
}

func runResultRefWithReader(ctx context.Context, reader runResultReader, namespace, name string) (runResultRef, error) {
	var failures []string
	type resolvedRef struct {
		resource string
		ref      runResultRef
	}
	var matches []resolvedRef
	for _, resource := range []string{"job", "rayjob.ray.io"} {
		out, err := reader.Raw(ctx, []string{"get", resource, name, "-n", namespace, "-o", "json", "--ignore-not-found"}, nil)
		if err != nil {
			if resource == "job" {
				return runResultRef{}, fmt.Errorf("get run Job %s/%s: %w", namespace, name, err)
			}
			if len(matches) == 1 && isUnknownResourceError(err) {
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", resource, err))
			return runResultRef{}, fmt.Errorf("resolve run workload %s/%s: %s", namespace, name, strings.Join(failures, "; "))
		}
		if strings.TrimSpace(out) == "" {
			failures = append(failures, resource+": not found")
			continue
		}
		ref, err := parseRunResultRef([]byte(out), resource)
		if err != nil {
			return runResultRef{}, err
		}
		matches = append(matches, resolvedRef{resource: resource, ref: ref})
	}
	if len(matches) == 1 {
		return matches[0].ref, nil
	}
	if len(matches) > 1 {
		return runResultRef{}, fmt.Errorf("run workload %s/%s is ambiguous: both Job and RayJob exist; delete or rename the stale resource", namespace, name)
	}
	return runResultRef{}, fmt.Errorf("get run workload %s/%s as Job or RayJob: %s; after workload deletion pass both --path and --pvc to read durable output", namespace, name, strings.Join(failures, "; "))
}

func parseRunResultRef(raw []byte, resource string) (runResultRef, error) {
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return runResultRef{}, fmt.Errorf("parse run %s json: %w", resource, err)
	}
	return runResultRef{
		Path:               obj.Metadata.Annotations[workloadmeta.AnnotationResultPath],
		PVC:                obj.Metadata.Annotations[workloadmeta.AnnotationResultPVC],
		Publication:        obj.Metadata.Annotations[workloadmeta.AnnotationArtifactPublication],
		PublicationID:      obj.Metadata.Annotations[workloadmeta.AnnotationArtifactPublicationID],
		BundleID:           obj.Metadata.Annotations[workloadmeta.AnnotationArtifactBundleID],
		ArtifactStore:      obj.Metadata.Annotations[workloadmeta.AnnotationArtifactStore],
		CheckpointArtifact: obj.Metadata.Annotations[workloadmeta.AnnotationCheckpointArtifact],
	}, nil
}

func readRunBlobPath(ctx context.Context, store artifactbundle.Store, absolutePath string) ([]byte, error) {
	key, err := artifactbundle.PVCRelativePath(absolutePath)
	if err != nil {
		return nil, err
	}
	raw, err := store.Read(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read durable artifact %s: %w", absolutePath, err)
	}
	return raw, nil
}

func listRunBlobPath(ctx context.Context, store artifactbundle.Store, absolutePath string) ([]string, error) {
	prefix, err := artifactbundle.PVCRelativePath(absolutePath)
	if err != nil {
		return nil, err
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("enumerate durable artifact directory %s: %w", absolutePath, err)
	}
	entries := make([]string, 0, len(objects))
	for _, object := range objects {
		entries = append(entries, strings.TrimPrefix(object.Name, prefix))
	}
	sort.Strings(entries)
	return entries, nil
}

func writeRunBundleDownload(
	cmd *cobra.Command,
	output string,
	manifest artifactbundle.Manifest,
	destination string,
	files []artifactbundle.DownloadedFile,
) error {
	var total int64
	for _, file := range files {
		total += file.Size
	}
	switch output {
	case "json":
		raw, err := json.MarshalIndent(map[string]any{
			"bundle_id":   manifest.BundleID,
			"destination": destination,
			"file_count":  len(files),
			"size_bytes":  total,
			"files":       files,
			"references":  manifest.References,
		}, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		_, err = cmd.OutOrStdout().Write(raw)
		return err
	case "raw":
		for _, file := range files {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), file.Path); err != nil {
				return err
			}
		}
		return nil
	default:
		_, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"Downloaded bundle %s: %d files, %d bytes\nDestination: %s\nLogs: %s\n",
			manifest.BundleID,
			len(files),
			total,
			destination,
			manifest.References.Logs,
		)
		return err
	}
}

// looksLikeDirectory is a heuristic: a path with no extension on its final
// segment is treated as a directory. Researchers can always force file or dir
// semantics with --path or --artifact.
func looksLikeDirectory(p string) bool {
	base := filepath.Base(filepath.Clean(p))
	if base == "" || base == "." || base == "/" {
		return true
	}
	if strings.HasSuffix(p, "/") {
		return true
	}
	return filepath.Ext(base) == ""
}

func writeRunGet(cmd *cobra.Command, output string, raw []byte, entries []string, path, pvc, checkpointArtifact string) error {
	w := cmd.OutOrStdout()
	switch output {
	case "raw":
		if raw != nil {
			_, err := w.Write(raw)
			return err
		}
		for _, e := range entries {
			if _, err := fmt.Fprintln(w, e); err != nil {
				return err
			}
		}
		return nil
	case "json":
		if raw != nil {
			_, err := w.Write(raw)
			return err
		}
		out := map[string]any{
			"path":    path,
			"pvc":     pvc,
			"entries": entries,
		}
		buf, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		buf = append(buf, '\n')
		_, err = w.Write(buf)
		return err
	default: // "table"
		if raw != nil {
			fmt.Fprintf(w, "PATH: %s\nPVC:  %s\n---\n", path, pvc)
			_, err := w.Write(raw)
			if err != nil {
				return err
			}
			if len(raw) > 0 && raw[len(raw)-1] != '\n' {
				fmt.Fprintln(w)
			}
			return nil
		}
		fmt.Fprintf(w, "PATH: %s (directory)\nPVC:  %s\n", path, pvc)
		if len(entries) == 0 {
			fmt.Fprintln(w, "(empty)")
			// A run that declared storage.checkpoint promised a servable
			// model, and nothing else in run get mentions that. The index lives
			// outside this path, so point at it rather than claim it is missing.
			if checkpointArtifact != "" {
				fmt.Fprintf(w, "storage.checkpoint %q was declared; the artifact index is written\n"+
					"outside this path and is what 'tau serve deploy --from-finetune' reads. If the\n"+
					"run log shows the artifact index step failing, the model is not resolvable by\n"+
					"run name.\n", checkpointArtifact)
			}
			return nil
		}
		fmt.Fprintln(w, "ENTRIES:")
		for _, e := range entries {
			fmt.Fprintf(w, "  %s\n", e)
		}
		fmt.Fprintf(w, "\nFetch one file with: tau run get <NAME> --artifact <NAME>\n")
		return nil
	}
}
