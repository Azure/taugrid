// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
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
