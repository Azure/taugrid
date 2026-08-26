---
title: Enable single-workspace researcher access
weight: 6
description: Replace shared administrator kubeconfigs with TauWorkspace RBAC and a fixed-scope Portal transport
url: "/docs/platform-admin-guide/single-workspace-researcher-access/"
aliases:
  - "/docs/tasks/platform/single-workspace-researcher-access/"
---

{{< maturity status="alpha" reviewed="2026-08-25" >}}

This procedure migrates one existing TauWorkspace from administrator credentials
to controller-owned `workspace-rbac`. It also supports a shared, fixed-scope
Portal reached through a local Kubernetes port-forward. The Portal has no login:
Kubernetes authenticates the tunnel, while every permitted tunnel reaches the
same server-side view.

This procedure does not create an identity, manage group membership, obtain an
AKS kubeconfig, or install an authenticated proxy. Each researcher must already
have a normal non-admin AKS user kubeconfig supplied through the platform's
existing process.

## Understand the three identities

1. The **platform administrator** keeps a separate operator or break-glass
   credential and changes TauWorkspace and Helm desired state.
2. The **researcher** authenticates to Kubernetes with a normal user kubeconfig.
   Kubernetes evaluates the User and Group strings in that credential against
   Tau's RoleBindings.
3. The **Portal ServiceAccount** reads backend data. It is not the connecting
   researcher and receives no caller identity through port-forward.

For managed-Entra AKS, the current TauWorkspace API records
`principalRef.provider: entra`, and the Group name must be the value AKS asserts
to Kubernetes. Tau does not call Microsoft Graph or provision that group.

## Back up the existing desired state

Keep the old administrator kubeconfigs available but inactive until both
researcher machines pass acceptance.

```bash
export TAU_SYSTEM_NAMESPACE=tau-platform
export TAU_WORKSPACE=default
export TAU_NAMESPACE=tau-default
export PORTAL_NAMESPACE=tau-portal-sdk
export PORTAL_SERVICE=taugrid-portal-sdk

kubectl -n "$TAU_SYSTEM_NAMESPACE" get \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" -o yaml \
  >workspace-before.yaml
kubectl get rolebinding,clusterrolebinding \
  -A -l "tau.azure.com/workspace=$TAU_WORKSPACE" -o yaml \
  >workspace-rbac-before.yaml
helm -n "$PORTAL_NAMESPACE" get values <portal-release> -a \
  >portal-values-before.yaml
helm -n "$PORTAL_NAMESPACE" history <portal-release>
```

Confirm that `$PORTAL_NAMESPACE` is dedicated to the Portal. A churn-safe
`kubectl port-forward service/...` requires Pod list/watch/get plus
`pods/portforward` create. Kubernetes RBAC cannot restrict those calls by Pod
label, so any unrelated Pod in that Namespace would also be reachable.

## Configure the fixed-scope Portal

Merge the following fields into the complete values file for the Helm release
that owns the existing Portal. Do not apply this fragment alone.

```yaml
portal:
  resourceName: taugrid-portal-sdk
  serviceName: taugrid-portal-sdk
  viewProfile: single-workspace
  workspace: default
  workloadNamespace: tau-default
  workspaceDirectory:
    enabled: false
  access:
    mode: cluster-internal
    externalURL: ""
  jobs:
    scopeMode: disabled
  kueueviz:
    enabled: false
  serviceAccount:
    create: true
    name: taugrid-portal-sdk
  rbac:
    create: true
  researcherPortForward:
    acknowledgeDedicatedNamespace: true
    group: <existing-kubernetes-group>
```

`single-workspace` locks request workspace and namespace parameters, exposes
only Experiments, Runs/job detail, and Ray, and prevents the broad Jobs, Cluster
Health, Cluster Nodes, Cost, Node Utilization, and KueueViz readers from
running. The Portal ServiceAccount receives a Role in `tau-default` instead of
the operator profile's ClusterRole.

The separate transport Role permits only:

- `get` on Service `taugrid-portal-sdk`;
- `get`, `list`, and `watch` on Pods in `tau-portal-sdk`;
- `create` on `pods/portforward` in `tau-portal-sdk`.

It does not grant logs, exec, attach, Secrets, ConfigMaps, mutations, or
cluster-scoped access. It does not provide per-user Portal authorization.

Render and inspect the complete release before upgrading. After the upgrade,
verify the Portal rollout, health, namespaced backend Role, and transport Role
with the administrator credential.

## Switch the existing TauWorkspace

Patch the same object; do not create a second workspace.

```bash
kubectl -n "$TAU_SYSTEM_NAMESPACE" patch \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" --type=merge -p '{
    "spec": {
      "authorization": {"mode": "workspace-rbac"},
      "principalRef": {
        "provider": "entra",
        "name": "<existing-kubernetes-group>"
      },
      "kubernetesSubject": {
        "kind": "Group",
        "name": "<existing-kubernetes-group>"
      },
      "role": "tau-researcher-v1"
    }
  }'

tau workspace status "$TAU_WORKSPACE"
kubectl -n "$TAU_NAMESPACE" get rolebinding tau-researcher-v1 -o yaml
kubectl -n "$TAU_SYSTEM_NAMESPACE" get \
  role,rolebinding "tau-workspace-reader-$TAU_WORKSPACE"
kubectl get clusterrolebinding \
  "tau-clusterqueue-reader-$TAU_WORKSPACE"
```

Wait for `Ready`, `RBACReady=True`, and the current observed generation before
testing as a researcher.

## Accept on every researcher machine

Run these checks independently on each machine with only its normal user
kubeconfig:

```bash
kubectl auth can-i '*' '*' --all-namespaces
kubectl auth can-i create jobs.batch -n "$TAU_NAMESPACE"
kubectl auth can-i get \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" \
  -n "$TAU_SYSTEM_NAMESPACE"
kubectl auth can-i list workspaces.tau.azure.com \
  -n "$TAU_SYSTEM_NAMESPACE"
kubectl auth can-i get secrets -n "$PORTAL_NAMESPACE"
kubectl auth can-i create pods/exec -n "$PORTAL_NAMESPACE"

tau workspace status "$TAU_WORKSPACE"
kubectl -n "$PORTAL_NAMESPACE" port-forward \
  "service/$PORTAL_SERVICE" 18080:80
```

Expected:

- cluster-wide authorization is `no`;
- workload creation and the named TauWorkspace read are `yes`;
- workspace listing, Portal Secrets, and Portal exec are `no`;
- Tau reports the workspace Ready;
- `curl -fsS http://127.0.0.1:18080/healthz` succeeds.

Also run the bounded Tau smoke workload and confirm that conflicting workspace,
namespace, and cluster query parameters do not return broader Portal data.
Run simultaneous tunnels from both machines. Finally, roll out the Portal once
and rerun the same Service port-forward command after its Pod name changes; no
RBAC edit should be required.

Only after both machines pass should administrator kubeconfigs be removed from
the researcher machines.

## Roll back safely

Portal transport and workspace workload access have different owners and must be
rolled back separately.

1. Clear `portal.researcherPortForward.group` and Helm-upgrade or roll back
   the Portal release. Confirm researcher port-forward is denied.
2. Restore the prior Portal values if reverting the fixed-scope profile.
3. With the retained operator credential, restore cluster-wide workspace mode.
   A merge patch removes the subject fields whether or not a partial forward
   migration created them:

```bash
kubectl -n "$TAU_SYSTEM_NAMESPACE" patch \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" --type=merge -p='{
    "spec": {
      "authorization": {"mode": "cluster-wide"},
      "principalRef": null,
      "kubernetesSubject": null,
      "role": null
    }
  }'
```

Wait for `RBACReady=True` with reason `ExistingClusterAuthorization`, then
confirm the controller-owned researcher RoleBinding, system reader
Role/RoleBinding, and ClusterQueue reader ClusterRoleBinding are absent.

Do not delete the TauWorkspace or workload Namespace as a rollback shortcut.

## Security boundary

`tau-researcher-v1` is bound only in `tau-default`, but it intentionally permits
workload management and ConfigMap and Secret read/write in that Namespace. The
named system and cluster readers do not permit listing every workspace,
ClusterQueue, or TauCluster.

The Portal profile and namespaced backend Role prevent accidental broader
reads; they are not multi-tenant application authorization. Use
workspace-directory behind a trusted authenticated proxy for a future per-user
Portal, not this port-forward model.
