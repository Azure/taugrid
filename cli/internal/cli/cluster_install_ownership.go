// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
	"github.com/spf13/cobra"
)

const (
	managedKueueRelease        = "aks-kueue"
	managedKueueNamespace      = "kueue-system"
	managedKubeRayRelease      = "kuberay"
	managedKubeRayNamespace    = "kuberay-system"
	supportedManagedKueueChart = "kueue-0.19."
	supportedManagedRayChart   = "kuberay-operator-1.7."
)

type ownedMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Generation  int64             `json:"generation"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

type ownedDeploymentList struct {
	Items []struct {
		Metadata ownedMetadata `json:"metadata"`
		Spec     struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			ReadyReplicas      int32 `json:"readyReplicas"`
			AvailableReplicas  int32 `json:"availableReplicas"`
			UpdatedReplicas    int32 `json:"updatedReplicas"`
		} `json:"status"`
	} `json:"items"`
}

var runOperatorOwnershipPreflight = operatorOwnershipPreflight

func installedOperatorMode(cmd *cobra.Command, kubeContext, release, namespace string) (string, error) {
	values, err := tauGridReleaseValues(cmd, kubeContext, release, namespace)
	if err != nil {
		return "", fmt.Errorf("read existing TauGrid operator ownership mode: %w", err)
	}
	return installedOperatorModeForValues(values)
}

func installedOperatorModeForValues(values []byte) (string, error) {
	var doc struct {
		Installation struct {
			Mode string `json:"mode"`
		} `json:"installation"`
		Components struct {
			Kueue struct {
				Enabled *bool `json:"enabled"`
			} `json:"kueue"`
			KubeRay struct {
				Enabled *bool `json:"enabled"`
			} `json:"kuberayOperator"`
		} `json:"components"`
	}
	if err := json.Unmarshal(values, &doc); err != nil {
		return "", fmt.Errorf("decode TauGrid operator ownership mode: %w", err)
	}
	if doc.Installation.Mode != "" {
		if doc.Installation.Mode != operatorModeManaged && doc.Installation.Mode != operatorModeHelm {
			return "", fmt.Errorf("release records unsupported installation.mode %q", doc.Installation.Mode)
		}
		if doc.Components.Kueue.Enabled == nil || doc.Components.KubeRay.Enabled == nil {
			return "", fmt.Errorf("release records installation.mode=%s without complete operator component ownership values", doc.Installation.Mode)
		}
		expectedEnabled := doc.Installation.Mode == operatorModeHelm
		if *doc.Components.Kueue.Enabled != expectedEnabled || *doc.Components.KubeRay.Enabled != expectedEnabled {
			return "", fmt.Errorf("release records installation.mode=%s with inconsistent Kueue or KubeRay component ownership", doc.Installation.Mode)
		}
		return doc.Installation.Mode, nil
	}
	kueueEnabled := doc.Components.Kueue.Enabled == nil || *doc.Components.Kueue.Enabled
	kubeRayEnabled := doc.Components.KubeRay.Enabled == nil || *doc.Components.KubeRay.Enabled
	if !kueueEnabled && !kubeRayEnabled {
		return operatorModeManaged, nil
	}
	if kueueEnabled != kubeRayEnabled {
		return "", fmt.Errorf("legacy release has a partial external-operator configuration; restore its original values before upgrading to an explicit ownership mode")
	}
	return operatorModeHelm, nil
}

func operatorOwnershipPreflight(ctx context.Context, runner installationcheck.Runner, spec clusterInstallSpec, releaseExists bool) error {
	components := []struct {
		name                string
		selector            string
		crd                 string
		managedRelease      string
		managedNamespace    string
		compatibleChartStem string
	}{
		{
			name:                "Kueue",
			selector:            "app.kubernetes.io/name=kueue",
			crd:                 "clusterqueues.kueue.x-k8s.io",
			managedRelease:      managedKueueRelease,
			managedNamespace:    managedKueueNamespace,
			compatibleChartStem: supportedManagedKueueChart,
		},
		{
			name:                "KubeRay",
			selector:            "app.kubernetes.io/name=kuberay-operator",
			crd:                 "rayclusters.ray.io",
			managedRelease:      managedKubeRayRelease,
			managedNamespace:    managedKubeRayNamespace,
			compatibleChartStem: supportedManagedRayChart,
		},
	}
	for _, component := range components {
		deployments, err := operatorDeployments(ctx, runner, component.selector)
		if err != nil {
			return fmt.Errorf("inspect %s controller ownership: %w", component.name, err)
		}
		crd, crdFound, err := operatorCRDOwnership(ctx, runner, component.crd)
		if err != nil {
			return fmt.Errorf("inspect %s CRD ownership: %w", component.name, err)
		}
		if spec.Mode == operatorModeManaged {
			if err := validateManagedOperator(component.name, deployments, crd, crdFound, component.managedRelease, component.managedNamespace, component.compatibleChartStem); err != nil {
				return err
			}
			continue
		}
		if err := validateHelmOperator(component.name, deployments, crd, crdFound, spec.Release, spec.Namespace, releaseExists); err != nil {
			return err
		}
	}
	return nil
}

func operatorDeployments(ctx context.Context, runner installationcheck.Runner, selector string) (ownedDeploymentList, error) {
	output, err := runner.Raw(ctx, []string{
		"get", "deployments", "--all-namespaces", "--selector", selector, "--output=json",
	}, nil)
	if err != nil {
		return ownedDeploymentList{}, err
	}
	var deployments ownedDeploymentList
	if err := json.Unmarshal([]byte(output), &deployments); err != nil {
		return ownedDeploymentList{}, fmt.Errorf("decode deployment list: %w", err)
	}
	return deployments, nil
}

func operatorCRDOwnership(ctx context.Context, runner installationcheck.Runner, name string) (ownedMetadata, bool, error) {
	output, err := runner.Raw(ctx, []string{
		"get", "customresourcedefinition", name, "--ignore-not-found", "--output=json",
	}, nil)
	if err != nil {
		return ownedMetadata{}, false, err
	}
	if strings.TrimSpace(output) == "" {
		return ownedMetadata{}, false, nil
	}
	var object struct {
		Metadata ownedMetadata `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(output), &object); err != nil {
		return ownedMetadata{}, false, fmt.Errorf("decode CRD %s: %w", name, err)
	}
	return object.Metadata, true, nil
}

func validateManagedOperator(name string, deployments ownedDeploymentList, crd ownedMetadata, crdFound bool, release, namespace, chartStem string) error {
	if len(deployments.Items) != 1 {
		return fmt.Errorf("managed mode requires exactly one ready AKS-managed %s controller, found %d; provision and verify the managed extension before installing TauGrid", name, len(deployments.Items))
	}
	deployment := deployments.Items[0]
	if err := requireHelmOwner(name+" controller", deployment.Metadata, release, namespace); err != nil {
		return fmt.Errorf("managed mode: %w", err)
	}
	chart := deployment.Metadata.Labels["helm.sh/chart"]
	if !strings.HasPrefix(chart, chartStem) {
		return fmt.Errorf("managed mode: %s controller chart %q is outside the supported compatibility line %q; upgrade the extension or use --mode helm", name, chart, chartStem+"x")
	}
	if deployment.Spec.Replicas < 1 ||
		deployment.Status.ObservedGeneration < deployment.Metadata.Generation ||
		deployment.Status.ReadyReplicas != deployment.Spec.Replicas ||
		deployment.Status.AvailableReplicas != deployment.Spec.Replicas ||
		deployment.Status.UpdatedReplicas != deployment.Spec.Replicas {
		return fmt.Errorf("managed mode: %s controller Deployment %s/%s is not ready", name, deployment.Metadata.Namespace, deployment.Metadata.Name)
	}
	if !crdFound {
		return fmt.Errorf("managed mode requires the %s API CRD; wait for the managed extension to finish provisioning", name)
	}
	if err := requireHelmOwner(name+" CRD", crd, release, namespace); err != nil {
		return fmt.Errorf("managed mode: %w", err)
	}
	return nil
}

func validateHelmOperator(name string, deployments ownedDeploymentList, crd ownedMetadata, crdFound bool, release, namespace string, releaseExists bool) error {
	for _, deployment := range deployments.Items {
		if helmOwnerMatches(deployment.Metadata, release, namespace) {
			continue
		}
		return fmt.Errorf("helm mode refuses foreign %s controller ownership at %s/%s; remove it through its current owner or use --mode managed only for verified AKS extensions", name, deployment.Metadata.Namespace, deployment.Metadata.Name)
	}
	if !crdFound || helmOwnerMatches(crd, release, namespace) {
		return nil
	}
	crdRelease := crd.Annotations["meta.helm.sh/release-name"]
	crdNamespace := crd.Annotations["meta.helm.sh/release-namespace"]
	if releaseExists && crdRelease == "" && crdNamespace == "" && len(deployments.Items) == 1 {
		return nil
	}
	return fmt.Errorf("helm mode refuses foreign %s CRD ownership on %s; do not use force-conflicts to transfer Helm ownership", name, crd.Name)
}

func requireHelmOwner(resource string, metadata ownedMetadata, release, namespace string) error {
	if helmOwnerMatches(metadata, release, namespace) {
		return nil
	}
	return fmt.Errorf(
		"%s %s/%s is owned by Helm release %q in namespace %q, expected %q in %q",
		resource,
		metadata.Namespace,
		metadata.Name,
		metadata.Annotations["meta.helm.sh/release-name"],
		metadata.Annotations["meta.helm.sh/release-namespace"],
		release,
		namespace,
	)
}

func helmOwnerMatches(metadata ownedMetadata, release, namespace string) bool {
	return metadata.Annotations["meta.helm.sh/release-name"] == release &&
		metadata.Annotations["meta.helm.sh/release-namespace"] == namespace
}
