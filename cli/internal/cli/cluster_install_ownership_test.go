// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type ownershipRunner map[string]string

func (r ownershipRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	output, ok := r[strings.Join(args, " ")]
	if !ok {
		return "", fmt.Errorf("unexpected kubectl args: %s", strings.Join(args, " "))
	}
	return output, nil
}

func managedOwnershipRunner() ownershipRunner {
	return ownershipRunner{
		"get deployments --all-namespaces --selector app.kubernetes.io/name=kueue --output=json": `{
			"items":[{
				"metadata":{
					"name":"kueue-controller-manager",
					"namespace":"kueue-system",
					"labels":{"helm.sh/chart":"kueue-0.19.10-aks.0.9.5"},
					"annotations":{
						"meta.helm.sh/release-name":"aks-kueue",
						"meta.helm.sh/release-namespace":"kueue-system"
					}
				},
				"spec":{"replicas":1},
				"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}
			}]
		}`,
		"get customresourcedefinition clusterqueues.kueue.x-k8s.io --ignore-not-found --output=json": `{
			"metadata":{
				"name":"clusterqueues.kueue.x-k8s.io",
				"annotations":{
					"meta.helm.sh/release-name":"aks-kueue",
					"meta.helm.sh/release-namespace":"kueue-system"
				}
			}
		}`,
		"get deployments --all-namespaces --selector app.kubernetes.io/name=kuberay-operator --output=json": `{
			"items":[{
				"metadata":{
					"name":"kuberay-operator",
					"namespace":"kuberay-system",
					"labels":{"helm.sh/chart":"kuberay-operator-1.7.0-aks.0.1.3"},
					"annotations":{
						"meta.helm.sh/release-name":"kuberay",
						"meta.helm.sh/release-namespace":"kuberay-system"
					}
				},
				"spec":{"replicas":1},
				"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}
			}]
		}`,
		"get customresourcedefinition rayclusters.ray.io --ignore-not-found --output=json": `{
			"metadata":{
				"name":"rayclusters.ray.io",
				"annotations":{
					"meta.helm.sh/release-name":"kuberay",
					"meta.helm.sh/release-namespace":"kuberay-system"
				}
			}
		}`,
	}
}

func TestManagedOwnershipPreflightAcceptsCompatibleExtensions(t *testing.T) {
	spec := clusterInstallSpec{
		Mode:      operatorModeManaged,
		Release:   defaultTauGridRelease,
		Namespace: defaultTauGridNamespace,
	}
	if err := operatorOwnershipPreflight(context.Background(), managedOwnershipRunner(), spec, false); err != nil {
		t.Fatalf("managed preflight failed: %v", err)
	}
}

func TestManagedOwnershipPreflightRejectsUnsupportedVersion(t *testing.T) {
	runner := managedOwnershipRunner()
	runner["get deployments --all-namespaces --selector app.kubernetes.io/name=kueue --output=json"] =
		strings.ReplaceAll(runner["get deployments --all-namespaces --selector app.kubernetes.io/name=kueue --output=json"], "kueue-0.19.10-aks.0.9.5", "kueue-0.20.0")
	spec := clusterInstallSpec{
		Mode:      operatorModeManaged,
		Release:   defaultTauGridRelease,
		Namespace: defaultTauGridNamespace,
	}
	err := operatorOwnershipPreflight(context.Background(), runner, spec, false)
	if err == nil || !strings.Contains(err.Error(), "outside the supported compatibility line") {
		t.Fatalf("unsupported managed version error = %v", err)
	}
}

func TestHelmOwnershipPreflightRejectsForeignCRDs(t *testing.T) {
	runner := managedOwnershipRunner()
	spec := clusterInstallSpec{
		Mode:      operatorModeHelm,
		Release:   defaultTauGridRelease,
		Namespace: defaultTauGridNamespace,
	}
	err := operatorOwnershipPreflight(context.Background(), runner, spec, false)
	if err == nil || !strings.Contains(err.Error(), "refuses foreign Kueue controller ownership") {
		t.Fatalf("foreign ownership error = %v", err)
	}
}

func TestHelmOwnershipPreflightAcceptsReleaseOwnedUpgrade(t *testing.T) {
	runner := managedOwnershipRunner()
	for key, value := range runner {
		value = strings.ReplaceAll(value, `"namespace":"kueue-system"`, `"namespace":"tau-system"`)
		value = strings.ReplaceAll(value, `"namespace":"kuberay-system"`, `"namespace":"tau-system"`)
		value = strings.ReplaceAll(value, `"meta.helm.sh/release-name":"aks-kueue"`, `"meta.helm.sh/release-name":"taugrid"`)
		value = strings.ReplaceAll(value, `"meta.helm.sh/release-name":"kuberay"`, `"meta.helm.sh/release-name":"taugrid"`)
		value = strings.ReplaceAll(value, `"meta.helm.sh/release-namespace":"kueue-system"`, `"meta.helm.sh/release-namespace":"tau-system"`)
		value = strings.ReplaceAll(value, `"meta.helm.sh/release-namespace":"kuberay-system"`, `"meta.helm.sh/release-namespace":"tau-system"`)
		runner[key] = value
	}
	spec := clusterInstallSpec{
		Mode:      operatorModeHelm,
		Release:   defaultTauGridRelease,
		Namespace: defaultTauGridNamespace,
	}
	if err := operatorOwnershipPreflight(context.Background(), runner, spec, true); err != nil {
		t.Fatalf("Helm-owned upgrade preflight failed: %v", err)
	}
}

func TestInstalledOperatorModeMigrationContract(t *testing.T) {
	for name, tc := range map[string]struct {
		values string
		want   string
		err    string
	}{
		"legacy umbrella is Helm-owned": {
			values: `{}`,
			want:   operatorModeHelm,
		},
		"managed marker": {
			values: `{"installation":{"mode":"managed"},"components":{"kueue":{"enabled":false},"kuberayOperator":{"enabled":false}}}`,
			want:   operatorModeManaged,
		},
		"legacy external operators migrate to managed": {
			values: `{"components":{"kueue":{"enabled":false},"kuberayOperator":{"enabled":false}}}`,
			want:   operatorModeManaged,
		},
		"partial legacy external ownership is rejected": {
			values: `{"components":{"kueue":{"enabled":false},"kuberayOperator":{"enabled":true}}}`,
			err:    "partial external-operator configuration",
		},
		"managed marker cannot hide Helm-owned operators": {
			values: `{"installation":{"mode":"managed"},"components":{"kueue":{"enabled":true},"kuberayOperator":{"enabled":false}}}`,
			err:    "inconsistent Kueue or KubeRay component ownership",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := installedOperatorModeForValues([]byte(tc.values))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("mode = %q, err = %v, want %q", got, err, tc.want)
			}
		})
	}
}
