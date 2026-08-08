package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type runResultRef struct {
	Path          string
	PVC           string
	Publication   string
	PublicationID string
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
		output       string
	)
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Fetch the result file or directory recorded by storage.output",
		Long: `Fetch artifacts a run Job wrote to its configured storage.output path.

Reads the ` + workloadmeta.AnnotationResultPath + ` and ` + workloadmeta.AnnotationResultPVC + ` annotations the
run recorded on the Job. If the path is a file, it's catted directly. If the
path is a directory, its recursive listing is printed; pass --artifact NAME to
fetch one file from it. Object-backed mounts are allowed to settle before Tau
accepts a zero-entry listing, so a populated directory is not reported as empty
during the BlobFuse mount-time list suppression window.
Override the recorded path/pvc with --path/--pvc. For an externally submitted
or already deleted workload with no Tau result annotations, pass both flags.

Examples:
  tau run get swordfish-bench-001 -n ray
  tau run get swordfish-bench-001 -n ray --artifact profile/rank-0.summary.md
  tau run get my-job --path /data/my-job/results --pvc research-data -o json`,
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
				ref.Path = pathOverride
			}
			if pvcOverride != "" {
				ref.PVC = pvcOverride
			}
			if ref.Path == "" {
				return fmt.Errorf("run workload %s/%s has no %s; resubmit with storage.output, or pass both --path and --pvc for an external or deleted workload", ns, name, workloadmeta.AnnotationResultPath)
			}
			if ref.PVC == "" {
				return fmt.Errorf("run workload %s/%s has no %s; pass both --path and --pvc for an external or deleted workload", ns, name, workloadmeta.AnnotationResultPVC)
			}
			resultPath := ref.Path
			if ref.Publication == artifactpublish.ModeStaged {
				if strings.TrimSpace(ref.PublicationID) == "" {
					return fmt.Errorf("staged artifact publication has no generation ID")
				}
				resultPath = path.Join(ref.Path, artifactpublish.GenerationsDir, ref.PublicationID)
				marker := path.Join(resultPath, artifactpublish.CompletionMarker)
				raw, err := fetchPVCFile(cmd.Context(), resolvedContext, ns, name, ref.PVC, marker)
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
				raw, err := fetchPVCFile(cmd.Context(), resolvedContext, ns, name, ref.PVC, file)
				if err != nil {
					return err
				}
				return writeRunGet(cmd, output, raw, nil, file, ref.PVC, "")
			}
			if !isDir {
				raw, err := fetchPVCFile(cmd.Context(), resolvedContext, ns, name, ref.PVC, resultPath)
				if err != nil {
					return err
				}
				return writeRunGet(cmd, output, raw, nil, resultPath, ref.PVC, "")
			}
			entries, err := fetchPVCListRecursive(cmd.Context(), resolvedContext, ns, name, ref.PVC, resultPath)
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
		CheckpointArtifact: obj.Metadata.Annotations[workloadmeta.AnnotationCheckpointArtifact],
	}, nil
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
