---
title: Tau Workspace Setup
linkTitle: Tau Workspace Setup
weight: 5
description: Create, validate, and hand off the single TauGrid v0 researcher workspace
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

This phase turns a validated TauGrid cluster into a workspace that a researcher can use. A platform operator creates and validates the workspace, then hands the researcher a repository containing a non-secret connection descriptor and Tau target configs.

At the end of this page:

- one `TauWorkspace` is `Ready`;
- its workload Namespace, LocalQueue, and researcher RBAC exist;
- any required PVC and workload identity have been validated separately;
- the repository contains `tau/workspace.connection.yaml`; and
- `tau run smoke` succeeds from a clean researcher checkout.

TauGrid v0 has one active workspace per cluster. Use a separate cluster, not a second workspace, when you need a concurrent isolation boundary.

| Task | Cluster information required |
|---|---|
| Create, inspect, and validate a Workspace | A working kubeconfig; current-context is enough |
| Run a workload through an existing kubeconfig | A working kubeconfig; Tau can discover the v0 workspace or accept `--workspace` |
| Generate a project scaffold without connection bootstrap | No cluster information |
| Generate `workspace.connection.yaml` for clean researcher bootstrap | AKS subscription, resource group, cluster, and managed-Entra tenant, written into the descriptor by the platform owner |

## What TauWorkspace creates

`tau workspace create` creates one `TauWorkspace` object in the selected TauGrid system namespace (`tau-system` by default). The Tau controller then reconciles the workspace-owned Kubernetes resources:

| Reconciled by TauWorkspace | Must already exist or be provisioned separately |
|---|---|
| Workload Namespace metadata and Pod Security labels | AKS and network access; managed Entra only for clean researcher bootstrap |
| Researcher RoleBindings and workspace reader access | Kueue, the baseline ClusterQueue, the shared researcher ClusterRoles, and the Tau controller |
| A LocalQueue in the workload Namespace | StorageClass, PVC, PV, CSI configuration, and cloud storage |
| An optional Azure Workload Identity ServiceAccount | Managed identity, federated credential, and Azure role assignments |
| Workspace defaults such as output root and priority | Project source, container image, data, secrets, and result semantics |

`TauWorkspace Ready` does **not** prove that a PVC is bound or writable, that an Azure federated credential works, or that the project image contains the researcher's code. Those are separate gates below.

## 1. Select the cluster and set the workspace parameters

For direct Kubernetes operations, Tau uses the current context from your normal kubeconfig when neither `TAU_CONTEXT` nor `--context` names another cluster. Start by making sure `kubectl` already points to the intended TauGrid cluster:

```bash
unset TAU_CONTEXT
kubectl config current-context
kubectl cluster-info

export TAU_WORKSPACE="taugrid-default"
export TAU_NAMESPACE="$TAU_WORKSPACE"
export TAU_SYSTEM_NAMESPACE="tau-system"
export TAU_QUEUE="jobqueue"

# Use the exact identity value that AKS presents to Kubernetes.
export TAU_PRINCIPAL="<entra-group-object-id>"
export TAU_SUBJECT_KIND="Group"
```

Expected: `kubectl config current-context` prints the intended cluster context, `kubectl cluster-info` reaches that cluster without an authentication or network error, and the shell variables are set without output.

The commands in steps 2 through 7 intentionally omit `--context`. Run them outside a research repository that already contains `tau/workspace.connection.yaml`, because descriptor-aware commands such as `workspace list`, `status`, and `check` intentionally prefer that repository connection. To pin a longer administrator session to the current cluster, use `export TAU_CONTEXT="$(kubectl config current-context)"`; this is an optional safety override, not required setup. For a different cluster, change kubeconfig current-context first or pass the same explicit context to both Tau and `kubectl`.

For an individual Entra user instead of a group, use the user's UPN and set:

```bash
export TAU_PRINCIPAL="<researcher-user-principal-name>"
export TAU_SUBJECT_KIND="User"
```

For a group, do not use a display name that AKS never places in the token claim. An explicit `--principal-name` is recommended for every real handoff. If it is omitted for the default Entra `Group` case, Tau deliberately creates an inert subject named after the workspace and warns that nobody has access yet.

The stock defaults are:

| Setting | Default |
|---|---|
| Workspace name | `taugrid-default` |
| TauGrid system Namespace | `tau-system` |
| Workload Namespace | Workspace name |
| LocalQueue and backing ClusterQueue | `jobqueue` |
| Authorization mode | `workspace-rbac` |
| Kubernetes subject name | `--principal-name` |
| Output root | `/data/projects/<workspace>/runs` |
| Priority | `normal` |
| Workload ServiceAccount without workload identity | Kubernetes `default` ServiceAccount |

`tau workspace create --system-namespace <name>` selects the namespace that stores the `TauWorkspace` object; omit it to use a discovered repository descriptor, then `TAU_SYSTEM_NAMESPACE`, then `tau-system`. `--namespace <name>` selects the separate researcher workload Namespace; omit it to use the workspace name. The deprecated `--platform-namespace` alias is accepted for existing automation but should not be used in new commands.

## 2. Verify the cluster is ready for a workspace

```bash
tau cluster validate installation
kubectl get clusterqueue.kueue.x-k8s.io "$TAU_QUEUE"
tau workspace list
```

Expected:

- installation validation exits `0`;
- ClusterQueue `jobqueue` exists; and
- a fresh cluster prints `no workspaces found`.

This basic gate uses only the working kubeconfig. It does not require the Azure subscription ID, tenant ID, AKS resource group, or AKS cluster name. Those values are introduced in step 8 only if the platform will generate a connection descriptor for clean researcher bootstrap.

If a workspace already exists, inspect it instead of creating another one:

```bash
tau workspace status <existing-workspace>
```

`tau workspace create` refuses a second workspace with different intent. This protects the v0 single-workspace contract.

## 3. Preview the TauWorkspace

Preview is read-only. It checks the existing ClusterQueue and prints the exact `TauWorkspace` manifest without writing it:

```bash
tau workspace create "$TAU_WORKSPACE" \
  --namespace "$TAU_NAMESPACE" \
  --queue "$TAU_QUEUE" \
  --principal-name "$TAU_PRINCIPAL" \
  --subject-kind "$TAU_SUBJECT_KIND" \
  | tee "/tmp/${TAU_WORKSPACE}-workspace.yaml"
```

Expected:

- the first comment says the preflight passed;
- the YAML contains `kind: TauWorkspace`;
- `authorization.mode` is `workspace-rbac`;
- the target Namespace and queue match the variables above;
- `kubernetesSubject` matches the intended `Group` or `User`; and
- no `TauWorkspace` is created.

Confirm that preview did not mutate the cluster:

```bash
kubectl get workspaces.tau.azure.com "$TAU_WORKSPACE" \
  -n "$TAU_SYSTEM_NAMESPACE"
```

Expected: `NotFound`. Any created object at this point means the command was run previously with `--apply` or the cluster was not fresh.

### Optional: configure workload identity

Only add these two flags when workload pods need Azure data-plane access:

```text
--service-account <workload-service-account> \
--workload-identity-client-id <managed-identity-client-id>
```

The flags are a pair: setting only one is rejected. They configure the Kubernetes ServiceAccount only. Create the Azure managed identity, federated credential, and Azure role assignments through the platform's Azure/IaC path.

## 4. Create the TauWorkspace

Run the same command with `--apply`:

```bash
tau workspace create "$TAU_WORKSPACE" \
  --namespace "$TAU_NAMESPACE" \
  --queue "$TAU_QUEUE" \
  --principal-name "$TAU_PRINCIPAL" \
  --subject-kind "$TAU_SUBJECT_KIND" \
  --apply
```

If workload identity is required, add both workload identity flags from the previous section to both the preview and apply commands.

Expected:

- the preflight reports that ClusterQueue `jobqueue` exists;
- the server-side dry run succeeds; and
- the output reports that the `TauWorkspace` was created.

The command creates only the `TauWorkspace`. It does not create an AKS cluster, Azure identity, federated credential, storage account, StorageClass, or PVC.

## 5. Wait for Ready and inspect every condition

```bash
kubectl wait \
  --for=jsonpath='{.status.phase}'=Ready \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" \
  -n "$TAU_SYSTEM_NAMESPACE" \
  --timeout=5m

tau workspace status "$TAU_WORKSPACE"
tau workspace check "$TAU_WORKSPACE"
```

Expected:

- `phase` is `Ready`;
- `namespace` is the resolved workload Namespace;
- `queue` and `clusterQ` are `jobqueue`;
- `RBACReady=True`;
- `QueueReady=True`;
- `DriftDetected` is not `True`; and
- `tau workspace check` exits `0` only after the controller has observed the current object generation.

`WorkloadIdentityReady=Unknown` with reason `NotConfigured` is expected when the workspace does not configure workload identity. It does not prevent the workspace from becoming `Ready`. If workload identity was requested, `WorkloadIdentityReady` must be `True` before handing off a workload that needs Azure data-plane access.

When the phase is `Degraded`, use the condition reason and message:

| Condition | What to fix |
|---|---|
| `RBACReady=False` | Correct the declared subject or remove a conflicting foreign RBAC object. |
| `QueueReady=False` | Restore the declared LocalQueue and its readable backing ClusterQueue. |
| `DriftDetected=True` | Repair the dependency or owned object named in the message and let the controller reconcile it. |
| `WorkloadIdentityReady=False` | Repair the requested ServiceAccount configuration; then separately verify Azure federation and authorization. |

There is no Tau readiness bypass flag.

## 6. Verify the derived Kubernetes resources

```bash
kubectl get namespace "$TAU_NAMESPACE" \
  --show-labels

kubectl get localqueue.kueue.x-k8s.io "$TAU_QUEUE" \
  -n "$TAU_NAMESPACE" \
  -o wide

kubectl get role,rolebinding \
  -n "$TAU_NAMESPACE"

kubectl get rolebinding tau-researcher-v1 \
  -n "$TAU_NAMESPACE" \
  -o yaml

kubectl get clusterrolebinding "tau-clusterqueue-reader-$TAU_WORKSPACE" \
  -o yaml

kubectl get role "tau-workspace-reader-$TAU_WORKSPACE" \
  -n "$TAU_SYSTEM_NAMESPACE" \
  -o yaml

kubectl get rolebinding "tau-workspace-reader-$TAU_WORKSPACE" \
  -n "$TAU_SYSTEM_NAMESPACE" \
  -o yaml

test "$(
  kubectl get namespace "$TAU_NAMESPACE" \
    -o jsonpath='{.metadata.labels.tau\.azure\.com/workspace}'
)" = "$TAU_WORKSPACE"

test "$(
  kubectl get localqueue.kueue.x-k8s.io "$TAU_QUEUE" \
    -n "$TAU_NAMESPACE" \
    -o jsonpath='{.spec.clusterQueue}'
)" = "$TAU_QUEUE"

test "$(
  kubectl get rolebinding tau-researcher-v1 \
    -n "$TAU_NAMESPACE" \
    -o jsonpath='{.subjects[0].kind}:{.subjects[0].name}'
)" = "${TAU_SUBJECT_KIND}:${TAU_PRINCIPAL}"

test "$(
  kubectl get clusterrolebinding "tau-clusterqueue-reader-$TAU_WORKSPACE" \
    -o jsonpath='{.subjects[0].kind}:{.subjects[0].name}'
)" = "${TAU_SUBJECT_KIND}:${TAU_PRINCIPAL}"

test "$(
  kubectl get rolebinding "tau-workspace-reader-$TAU_WORKSPACE" \
    -n "$TAU_SYSTEM_NAMESPACE" \
    -o jsonpath='{.subjects[0].kind}:{.subjects[0].name}'
)" = "${TAU_SUBJECT_KIND}:${TAU_PRINCIPAL}"
```

Expected:

- the Namespace is `Active` and has the Tau workspace, default LocalQueue, and Pod Security labels;
- LocalQueue `jobqueue` points to ClusterQueue `jobqueue`;
- RoleBinding `tau-researcher-v1` grants the exact researcher subject access in the workload Namespace;
- ClusterRoleBinding `tau-clusterqueue-reader-$TAU_WORKSPACE` grants that subject read access to the backing ClusterQueue;
- the matching Role and RoleBinding `tau-workspace-reader-$TAU_WORKSPACE` exist in `$TAU_SYSTEM_NAMESPACE` for the same subject; and
- all five shell assertions exit `0` without output.

If no workload identity was configured, workloads that do not override `serviceAccountName` use the Namespace's Kubernetes `default` ServiceAccount. The current `tau workspace status` output does not print that implicit value.

If workload identity was configured, inspect the reconciled ServiceAccount:

```bash
export TAU_WORKLOAD_SERVICE_ACCOUNT="<workload-service-account>"

kubectl get serviceaccount "$TAU_WORKLOAD_SERVICE_ACCOUNT" \
  -n "$TAU_NAMESPACE" \
  -o yaml
```

Expected: it has label `azure.workload.identity/use: "true"` and annotation `azure.workload.identity/client-id` with the requested client ID. This proves the Kubernetes part only; test the actual Azure service from a workload before claiming the cloud identity works.

## 7. Validate storage separately when the project uses `/data`

Skip this section when the initial targets do not mount a PVC.

TauWorkspace does not create or own storage. Provision the StorageClass, cloud storage, CSI configuration, and PVC through the platform's normal path. Then record the claim name and inspect it:

```bash
export TAU_DATA_PVC="<platform-pvc-name>"
export TAU_STORAGE_PROBE_IMAGE="mcr.microsoft.com/azurelinux/base/core@sha256:8bb51342bd5eba915990ab608f91060d502bb7891a2d3d909e0419b932533029"

export TAU_WORKLOAD_SERVICE_ACCOUNT="$(
  kubectl get workspaces.tau.azure.com "$TAU_WORKSPACE" \
    -n "$TAU_SYSTEM_NAMESPACE" \
    -o jsonpath='{.spec.workloadIdentity.serviceAccountName}'
)"
: "${TAU_WORKLOAD_SERVICE_ACCOUNT:=default}"

tau workspace check "$TAU_WORKSPACE" \
  --data-pvc "$TAU_DATA_PVC"

kubectl get pvc "$TAU_DATA_PVC" \
  -n "$TAU_NAMESPACE" \
  -o wide
```

Expected before a storage-backed handoff:

- the PVC exists in the resolved workload Namespace;
- its StorageClass and access mode match the workload contract; and
- it is `Bound`, or a `WaitForFirstConsumer` claim binds when the first test pod is scheduled.

`tau workspace check --data-pvc` warns about a missing or unbound claim but does not fail the workspace readiness gate, because storage is optional. Treat that warning as a handoff failure for any repository that sets `storage.data_pvc`.

`Bound` proves only that Kubernetes completed provisioning. Run the following restricted-policy-compatible writer and reader pods to prove mount, non-root write access, and persistence across pods:

```bash
kubectl delete pod tau-storage-writer tau-storage-reader \
  -n "$TAU_NAMESPACE" \
  --ignore-not-found \
  --wait=true

kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: tau-storage-writer
  namespace: ${TAU_NAMESPACE}
spec:
  restartPolicy: Never
  serviceAccountName: ${TAU_WORKLOAD_SERVICE_ACCOUNT}
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: writer
      image: ${TAU_STORAGE_PROBE_IMAGE}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -eu
          printf 'taugrid-storage-ok\n' > /data/taugrid-storage-probe.txt
          cat /data/taugrid-storage-probe.txt
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${TAU_DATA_PVC}
YAML

kubectl wait pod/tau-storage-writer \
  -n "$TAU_NAMESPACE" \
  --for=jsonpath='{.status.phase}'=Succeeded \
  --timeout=3m

kubectl logs tau-storage-writer \
  -n "$TAU_NAMESPACE"

PVC_PHASE="$(
  kubectl get pvc "$TAU_DATA_PVC" \
    -n "$TAU_NAMESPACE" \
    -o jsonpath='{.status.phase}'
)"
test "$PVC_PHASE" = "Bound"

kubectl delete pod tau-storage-writer \
  -n "$TAU_NAMESPACE" \
  --wait=true

kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: tau-storage-reader
  namespace: ${TAU_NAMESPACE}
spec:
  restartPolicy: Never
  serviceAccountName: ${TAU_WORKLOAD_SERVICE_ACCOUNT}
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: reader
      image: ${TAU_STORAGE_PROBE_IMAGE}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -eu
          test "\$(cat /data/taugrid-storage-probe.txt)" = "taugrid-storage-ok"
          cat /data/taugrid-storage-probe.txt
          rm /data/taugrid-storage-probe.txt
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${TAU_DATA_PVC}
YAML

kubectl wait pod/tau-storage-reader \
  -n "$TAU_NAMESPACE" \
  --for=jsonpath='{.status.phase}'=Succeeded \
  --timeout=3m

kubectl logs tau-storage-reader \
  -n "$TAU_NAMESPACE"

kubectl delete pod tau-storage-reader \
  -n "$TAU_NAMESPACE" \
  --wait=true
```

Expected: both pods reach `Succeeded` and print exactly `taugrid-storage-ok`; the explicit phase assertion exits `0` with the PVC in `Bound`; and the reader proves that the marker survived deletion of the writer pod. This is the same two-pod acceptance pattern that passed in the previous AKS validation.

## 8. Generate the repository handoff

The handoff artifact is a research repository, not a kubeconfig. The Azure subscription, tenant, resource group, and cluster name were not needed for the kubeconfig-driven operations in steps 1 through 7. They are required together here so `init-repo` can generate `tau/workspace.connection.yaml` for a researcher who does not already have this cluster in kubeconfig.

```bash
export TAU_SUBSCRIPTION_ID="<azure-subscription-id>"
export TAU_AKS_RESOURCE_GROUP="<aks-resource-group>"
export TAU_AKS_CLUSTER="<aks-cluster-name>"

export TAU_AKS_RESOURCE_ID="$(
  az aks show \
    --subscription "$TAU_SUBSCRIPTION_ID" \
    --resource-group "$TAU_AKS_RESOURCE_GROUP" \
    --name "$TAU_AKS_CLUSTER" \
    --query id \
    --output tsv
)"

export TAU_TENANT_ID="$(
  az aks show \
    --ids "$TAU_AKS_RESOURCE_ID" \
    --query aadProfile.tenantID \
    --output tsv
)"

az aks show \
  --ids "$TAU_AKS_RESOURCE_ID" \
  --query '{managedEntra:aadProfile.managed,entraTenant:aadProfile.tenantID,localAccountsDisabled:disableLocalAccounts}' \
  --output yaml

export TAU_REPO_NAME="<research-repository-name>"
export TAU_PROJECT_IMAGE="<registry>/<repository>:<immutable-tag>"

tau workspace init-repo "$TAU_REPO_NAME" \
  --image "$TAU_PROJECT_IMAGE" \
  --workspace "$TAU_WORKSPACE" \
  --system-namespace "$TAU_SYSTEM_NAMESPACE" \
  --azure-subscription-id "$TAU_SUBSCRIPTION_ID" \
  --azure-tenant-id "$TAU_TENANT_ID" \
  --aks-resource-group "$TAU_AKS_RESOURCE_GROUP" \
  --aks-cluster "$TAU_AKS_CLUSTER"
```

Expected before generation: `TAU_AKS_RESOURCE_ID` identifies the intended AKS cluster, `TAU_TENANT_ID` is non-empty, `managedEntra` is `true`, `entraTenant` matches `TAU_TENANT_ID`, and `localAccountsDisabled` may remain `false`. Managed Entra supplies the normal human cluster-user authentication used in step 9; it does not require disabling AKS local accounts. An empty tenant or `managedEntra: null` means this cluster cannot complete the canonical `workspace-rbac` clean bootstrap until its human authentication path is enabled.

Expected: the new repository contains at least:

```text
tau/
  workspace.connection.yaml
  smoke.yaml
  train.yaml
  train-gpu.yaml
images/
  train.Dockerfile
scripts/
  configure.sh
```

`tau/workspace.connection.yaml` contains the AKS resource ID, tenant, context, TauGrid system namespace, workspace, minimum Tau version, and `workspace-rbac` requirement. It must not contain a kubeconfig, client secret, access token, or other credential.

If every researcher already receives a working kubeconfig through another process, you may omit all four Azure/AKS flags. `init-repo` still generates the Tau configs and project scaffold, but it does not create `tau/workspace.connection.yaml`; cluster-backed Tau commands then use the current kubeconfig, and you may pass `--workspace "$TAU_WORKSPACE"` when the workspace cannot be discovered. This manual kubeconfig path supports basic operation, but it is not the clean-machine repository bootstrap accepted in step 9.

The image passed to `init-repo` is written into the generated target configs. Before handoff, build and push the generated project image, then pin the final tag or digest back into the configs:

```bash
cd "$TAU_REPO_NAME"

docker build -f images/train.Dockerfile -t "$TAU_PROJECT_IMAGE" .
docker push "$TAU_PROJECT_IMAGE"
./scripts/configure.sh --image "$TAU_PROJECT_IMAGE"

tau run validate --config tau/smoke.yaml
tau run validate --config tau/train.yaml
```

Expected: both config validations exit `0`. A public base image that does not contain this generated repository is not a valid project image, even if the image itself can be pulled.

For a private AKS API, edit the descriptor before committing it so `network.privateCluster` is `true` and `network.instructions` tells the researcher how to connect to the required VPN or private network.

Inspect the descriptor offline:

```bash
tau workspace connection inspect --output yaml

grep -Fx "workspace: $TAU_WORKSPACE" tau/workspace.connection.yaml
grep -Fxi "  resourceID: $TAU_AKS_RESOURCE_ID" tau/workspace.connection.yaml
grep -Fx "  systemNamespace: $TAU_SYSTEM_NAMESPACE" tau/workspace.connection.yaml
grep -Fxi "  tenantID: $TAU_TENANT_ID" tau/workspace.connection.yaml
grep -Fx "  mode: workspace-rbac" tau/workspace.connection.yaml
grep -Fx "  requiredRole: tau-researcher-v1" tau/workspace.connection.yaml

if grep -Ein 'client.?secret|access.?token|password|kubeconfig:' tau/workspace.connection.yaml; then
  echo "connection descriptor contains forbidden secret material" >&2
  exit 1
fi
```

Expected: inspection prints the intended workspace, AKS cluster/context, tenant, descriptor path, digest, `workspace-rbac` mode, and required role without contacting the cluster; each exact-field check prints one matching line; and the secret scan prints nothing and exits `0`.

Commit the non-secret descriptor, target configs, source, Dockerfile, and build instructions to the research repository. Do not commit `.env`, kubeconfigs, tokens, or registry credentials.

## 9. Prove the researcher handoff from a clean checkout

The final acceptance must use the researcher identity, not the platform operator's already-configured administrator kubeconfig.

The researcher needs:

- the released `tau` CLI;
- an Azure sign-in that can obtain normal AKS cluster-user credentials;
- `kubelogin` for the managed-Entra kubeconfig; and
- VPN/private-network access when the descriptor requires it.

Managed Entra can remain enabled while AKS local accounts are also retained. Tau's canonical researcher bootstrap requires the normal managed-Entra cluster-user path; it does not require disabling local accounts.

From a clean clone:

```bash
unset TAU_CONTEXT

git clone <research-repository-url>
cd <research-repository>

tau workspace connection inspect
tau run smoke
```

Do not add `--context`, `--workspace`, or `--namespace`, and make sure the `TAU_CONTEXT` environment variable is unset for this first canonical run. Without explicit routing overrides, Tau discovers `tau/workspace.connection.yaml`, obtains the normal AKS user credentials, writes an isolated mode-`0600` kubeconfig outside the repository, verifies the live workspace contract, and pins the connection. A flag or environment variable that explicitly names a context selects an already-configured kubeconfig path instead of this clean-machine bootstrap.

Expected:

- descriptor inspection succeeds offline;
- the first live command authenticates to the exact AKS resource and tenant;
- the workspace UID, generation, Namespace, LocalQueue, ServiceAccount, and authorization contract are verified; and
- the bounded smoke workload is admitted, scheduled, and completes.

`tau run smoke` proves workspace readiness, queue admission, scheduling, ServiceAccount selection, and container execution. It does not mount the project PVC, pull the project image, test a GPU, or access an Azure data service. Continue with the [researcher quickstart](quickstart/) to validate the project image and checked-in targets.

## Workspace setup completion checklist

- [ ] `tau cluster validate installation` exits `0`.
- [ ] The intended ClusterQueue exists.
- [ ] The workspace preview matches the intended Namespace, queue, and subject.
- [ ] `tau workspace check` exits `0` for the current generation.
- [ ] `RBACReady=True`, `QueueReady=True`, and no drift is reported.
- [ ] Workload identity is either intentionally absent or separately validated.
- [ ] Any required PVC is provisioned and write-tested separately.
- [ ] The checked-in connection descriptor contains no secrets.
- [ ] The project image contains the project code and is pullable by AKS.
- [ ] `tau run smoke` succeeds from a clean researcher checkout without `--context` and with `TAU_CONTEXT` unset.

For detailed recovery from a `Degraded` workspace, see the [TauWorkspace reference](../../reference/workspace/). For an existing Namespace and LocalQueue that must be adopted rather than created, use the platform [workspace enablement task](../../tasks/platform/enable-workspace/).
