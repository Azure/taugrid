// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
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
		if role.Kind != "ClusterRole" || role.Name != defaultRoleName {
			continue
		}
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
	t.Fatalf("ClusterRole %q is missing from Kustomize RBAC", defaultRoleName)
}
