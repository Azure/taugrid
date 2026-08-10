# Azure Pipelines

TauGrid keeps the two Kind end-to-end suites in separate pipelines so changes
run only the suites whose dependencies were touched.

| Pipeline | YAML path | Repository entry point |
| --- | --- | --- |
| Tau core E2E | `.ado/tau-core-e2e.yml` | `make -C controllers/tau-core test-kind-e2e` |
| Tau CLI E2E | `.ado/tau-cli-e2e.yml` | `make -C cli test-kind-e2e` |
| Tau AKS E2E | `.ado/tau-aks-e2e.yml` | Ephemeral ACR + CPU AKS, then Tau core validation and Tau CLI smoke |

Create each pipeline from the `Azure/taugrid` GitHub repository by selecting
**Existing Azure Pipelines YAML file** and the corresponding path above. The
definitions trigger for pull requests targeting `main` and pushes to `main`,
with dependency-aware path filters.

Both pipelines use a Microsoft-hosted Ubuntu agent. The test steps require no
Azure credentials or service connections beyond repository access. Go,
kubectl, Helm, and Kind are pinned in the YAML; the tests create disposable
local Kind clusters through the Docker engine already available on the hosted
agent.

## Subscription-backed AKS E2E

`.ado/tau-aks-e2e.yml` is manual-only. It targets the
`aks-ai-runtime-longhaul-gpu` ADO Environment, sharing its approval and
exclusive-lock checks with related long-haul validation. The pipeline uses the
`aks ai runtime - corp` workload-identity Azure Resource Manager service
connection and creates these resources in `westus3`:

- one owned resource group;
- one Basic ACR containing the controller built from the checked-out commit;
- one three-node, CPU-only AKS cluster using `Standard_D4_v5`.

The service connection must be scoped to the intended subscription and allowed
to create and delete those resources. It must also be allowed to create the
`AcrPull` role assignment used by `az aks create --attach-acr`; use an approved
OIDC or managed-identity service connection rather than a client secret.

The deployment validates the checked-out Tau core controller image, creates a
workspace, and runs the Tau CLI onboarding smoke against the AKS API. Its final
step runs under `always()` and waits for the owned resource group to disappear.
The ownership tag is `taugrid-e2e=true`, and resource names include the ADO
build ID for audit and emergency cleanup.
