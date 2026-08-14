// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type exactJobManifestIdentity struct {
	Name         string
	Namespace    string
	SubmissionID string
}

type exactJobManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

func exactManifestDigest(manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func serverDryRunOutputFormat(dryRun, manifestOut string) string {
	if dryRun == "server" && strings.TrimSpace(manifestOut) != "" {
		return "yaml"
	}
	return ""
}

func validateExactJobManifest(manifest []byte, expectedDigest, expectedName, expectedNamespace string) (exactJobManifestIdentity, error) {
	expectedDigest = strings.TrimSpace(expectedDigest)
	if len(expectedDigest) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(expectedDigest, "sha256:") ||
		expectedDigest != strings.ToLower(expectedDigest) {
		return exactJobManifestIdentity{}, fmt.Errorf("--digest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(expectedDigest, "sha256:")); err != nil {
		return exactJobManifestIdentity{}, fmt.Errorf("--digest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	actualDigest := exactManifestDigest(manifest)
	if actualDigest != expectedDigest {
		return exactJobManifestIdentity{}, fmt.Errorf("manifest digest mismatch: got %s, want %s", actualDigest, expectedDigest)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(manifest))
	var object exactJobManifest
	if err := decoder.Decode(&object); err != nil {
		return exactJobManifestIdentity{}, fmt.Errorf("decode exact Job manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return exactJobManifestIdentity{}, fmt.Errorf("exact manifest must contain one YAML document")
		}
		return exactJobManifestIdentity{}, fmt.Errorf("decode trailing manifest content: %w", err)
	}
	if object.APIVersion != "batch/v1" || object.Kind != "Job" {
		return exactJobManifestIdentity{}, fmt.Errorf("exact manifest must be one batch/v1 Job, got %s %s", object.APIVersion, object.Kind)
	}
	if object.Metadata.Name != expectedName || object.Metadata.Namespace != expectedNamespace {
		return exactJobManifestIdentity{}, fmt.Errorf(
			"manifest identity mismatch: got %s/%s, want %s/%s",
			object.Metadata.Namespace,
			object.Metadata.Name,
			expectedNamespace,
			expectedName,
		)
	}
	if object.Metadata.Labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue {
		return exactJobManifestIdentity{}, fmt.Errorf("manifest is not a Tau-managed direct Job")
	}
	submissionID := strings.TrimSpace(object.Metadata.Annotations[workloadmeta.AnnotationSubmissionID])
	if submissionID == "" {
		return exactJobManifestIdentity{}, fmt.Errorf("manifest is missing Tau submission identity")
	}
	return exactJobManifestIdentity{
		Name:         object.Metadata.Name,
		Namespace:    object.Metadata.Namespace,
		SubmissionID: submissionID,
	}, nil
}

func submitExactJobManifest(ctx context.Context, runner kubeRawRunner, manifest []byte, expectedDigest, expectedName, expectedNamespace string) (runSubmissionResult, error) {
	identity, err := validateExactJobManifest(manifest, expectedDigest, expectedName, expectedNamespace)
	if err != nil {
		return runSubmissionResult{}, err
	}
	return submitRunWorkload(ctx, runner, runSubmission{
		Resource:     "job",
		Name:         identity.Name,
		Namespace:    identity.Namespace,
		SubmissionID: identity.SubmissionID,
		Manifest:     manifest,
	})
}

func newRunSubmitManifestCmd() *cobra.Command {
	var manifestPath, digest, name, namespace, kubeContext string
	cmd := &cobra.Command{
		Use:   "submit-manifest",
		Short: "Submit an exact direct-Job manifest after server validation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(manifestPath) == "" ||
				strings.TrimSpace(digest) == "" ||
				strings.TrimSpace(name) == "" ||
				strings.TrimSpace(namespace) == "" {
				return fmt.Errorf("--manifest, --digest, --name, and --namespace are required")
			}
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				return fmt.Errorf("read exact Job manifest: %w", err)
			}
			result, err := submitExactJobManifest(
				cmd.Context(),
				kube.New(kubeContext),
				manifest,
				digest,
				name,
				namespace,
			)
			fmt.Fprint(cmd.OutOrStdout(), result.Output)
			return err
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path written by tau run --dry-run=server --manifest-out")
	cmd.Flags().StringVar(&digest, "digest", "", "expected sha256 digest printed by the server dry-run")
	cmd.Flags().StringVar(&name, "name", "", "expected fixed Job name")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "expected fixed Job namespace")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}
