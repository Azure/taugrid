// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSystemReaderLoggingPermissionUsesConfiguredNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	reconciler := &TauWorkspaceReconciler{Client: c, SystemNamespace: "custom-system"}
	if err := reconciler.reconcileSystemReaderRBAC(context.Background(), testWorkspace("research")); err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "custom-system", Name: workspaceReaderRBACName("research")}, &role); err != nil {
		t.Fatal(err)
	}
	want := rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"tau-log-connection"}, Verbs: []string{"get"},
	}
	if len(role.Rules) != 3 || !reflect.DeepEqual(role.Rules[2], want) {
		t.Fatalf("system reader must only grant named get access to logging metadata: %#v", role.Rules)
	}
}

func TestKustomizeResearcherRoleGrantsRayServicePermissions(t *testing.T) {
	role := kustomizeClusterRole(t, defaultRoleName)
	want := rbacv1.PolicyRule{
		APIGroups: []string{"ray.io"},
		Resources: []string{"rayjobs", "rayservices"},
		Verbs:     []string{"create", "get", "list", "watch", "delete", "patch", "update"},
	}
	for _, rule := range role.Rules {
		if reflect.DeepEqual(rule, want) {
			return
		}
	}
	t.Fatalf("researcher role must grant RayService lifecycle permissions: %#v", role.Rules)
}

func TestKustomizeControllerRoleMatchesTopologyAndGPUFlavorPermissions(t *testing.T) {
	role := kustomizeClusterRole(t, "tau-core-controller")
	for _, want := range []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"nodes/status"},
			Verbs:     []string{"get", "patch"},
		},
		{
			APIGroups: []string{"kueue.x-k8s.io"},
			Resources: []string{"clusterqueues"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{"kueue.x-k8s.io"},
			Resources: []string{"resourceflavors"},
			Verbs:     []string{"get", "list", "watch", "create"},
		},
	} {
		found := false
		for _, rule := range role.Rules {
			if reflect.DeepEqual(rule, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Kustomize controller role is missing topology/GPU discovery rule %#v: %#v", want, role.Rules)
		}
	}
}

func TestKustomizeControllerRoleAllowsLegacyRoleDeletionWithoutMutation(t *testing.T) {
	role := kustomizeClusterRole(t, "tau-core-controller")
	want := rbacv1.PolicyRule{
		APIGroups: []string{rbacv1.GroupName},
		Resources: []string{"roles"},
		Verbs:     []string{"get", "list", "watch", "delete"},
	}
	for _, rule := range role.Rules {
		if reflect.DeepEqual(rule, want) {
			return
		}
	}
	t.Fatalf("Kustomize controller role must permit identity-checked Role deletion without Role mutation: %#v", role.Rules)
}

func TestKustomizeControllerRoleAllowsReadOnlySourceJobDiscovery(t *testing.T) {
	role := kustomizeClusterRole(t, "tau-core-controller")
	want := rbacv1.PolicyRule{
		APIGroups: []string{"batch"},
		Resources: []string{"jobs"},
		Verbs:     []string{"get", "list", "watch"},
	}
	for _, rule := range role.Rules {
		if reflect.DeepEqual(rule, want) {
			return
		}
	}
	t.Fatalf("controller role must allow read-only source Job discovery: %#v", role.Rules)
}

func TestRetainedNamespaceRestoresOnlyCurrentAuthorizedResearcherBinding(t *testing.T) {
	for _, scenario := range []string{"group", "service-account", "cluster-wide", "missing-subject", "missing-principal", "abandoned-namespace", "foreign-namespace"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			workspace := testWorkspace("research")
			workspace.Status.Target.ResolvedNamespace = "retained"
			namespace := testNamespace("retained")
			namespace.Labels = workspaceLabels(workspace.Name)
			namespace.Annotations[annotationOwnerUID] = string(workspace.UID)
			wantBinding := scenario == "group" || scenario == "service-account"
			switch scenario {
			case "service-account":
				workspace.Spec.KubernetesSubject = &tauv1alpha1.KubernetesSubject{Kind: rbacv1.ServiceAccountKind, Name: "current-researcher"}
			case "cluster-wide":
				workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
			case "missing-subject":
				workspace.Spec.KubernetesSubject = nil
			case "missing-principal":
				workspace.Spec.PrincipalRef = nil
			case "abandoned-namespace":
				workspace.Status.Target.ResolvedNamespace = "another-namespace"
			case "foreign-namespace":
				namespace.Annotations[annotationOwnerUID] = "another-workspace-uid"
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(namespace).Build()
			if err := newTestWorkspaceReconciler(c).reconcileOwnedNamespaceResearcherAccess(ctx, workspace, "new-target"); err != nil {
				t.Fatal(err)
			}
			var binding rbacv1.RoleBinding
			err := c.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: defaultRoleName}, &binding)
			if !wantBinding {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("unexpected researcher access was created: %v, %#v", err, binding)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantSubjects := []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, namespace.Name)}
			if !reflect.DeepEqual(binding.Subjects, wantSubjects) ||
				binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}) ||
				!ownedByWorkspace(binding.Labels, workspace.Name) ||
				binding.Annotations[annotationOwnerUID] != string(workspace.UID) {
				t.Fatalf("restored researcher access does not match current workspace: %#v", binding)
			}
		})
	}
}

func kustomizeClusterRole(t *testing.T, name string) rbacv1.ClusterRole {
	t.Helper()
	file, err := os.Open("../../../../charts/tau-core-controller/kustomize/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		var role rbacv1.ClusterRole
		if err := decoder.Decode(&role); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if role.Kind != "ClusterRole" || role.Name != name {
			continue
		}
		return role
	}
	t.Fatalf("ClusterRole %q is missing from Kustomize RBAC", name)
	return rbacv1.ClusterRole{}
}
