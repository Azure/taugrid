// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

func upgradeTauGridCRDs(cmd *cobra.Command, runner installationcheck.Runner, spec clusterInstallSpec) error {
	var rendered bytes.Buffer
	if err := runClusterInstallHelm(cmd, spec, &rendered, clusterInstallRenderArgs(spec)); err != nil {
		return fmt.Errorf("render TauGrid CRDs before upgrade: %w", err)
	}
	crds, err := tauGridCRDManifest(rendered.Bytes())
	if err != nil {
		return err
	}
	if len(crds) == 0 {
		return nil
	}
	args := []string{"apply", "--field-manager=taugrid-crds", "-f", "-"}
	if _, err := runner.Raw(cmd.Context(), append(append([]string(nil), args...), "--dry-run=server"), crds); err != nil {
		return fmt.Errorf("validate TauGrid CRD upgrade: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Updating TauGrid CRDs from the selected chart; existing custom resources are retained.")
	out, err := runner.Raw(cmd.Context(), args, crds)
	if out != "" {
		fmt.Fprint(cmd.OutOrStdout(), out)
	}
	if err != nil {
		return fmt.Errorf("update TauGrid CRDs before Helm upgrade: %w", err)
	}
	if _, err := runner.Raw(cmd.Context(), []string{
		"wait", "--for=condition=Established", "--timeout=" + spec.Timeout, "-f", "-",
	}, crds); err != nil {
		return fmt.Errorf("wait for upgraded TauGrid CRDs: %w", err)
	}
	return nil
}

func tauGridCRDManifest(rendered []byte) ([]byte, error) {
	var manifest bytes.Buffer
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	encoder := yaml.NewEncoder(&manifest)
	defer encoder.Close()
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		// Helm dependency-update progress can precede the first YAML document.
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("parse rendered TauGrid CRDs: %w", err)
		}
		if object["apiVersion"] != "apiextensions.k8s.io/v1" || object["kind"] != "CustomResourceDefinition" {
			continue
		}
		spec, _ := object["spec"].(map[string]any)
		if spec["group"] != "tau.azure.com" {
			continue
		}
		if err := encoder.Encode(object); err != nil {
			return nil, fmt.Errorf("encode TauGrid CRD: %w", err)
		}
	}
	return manifest.Bytes(), nil
}
