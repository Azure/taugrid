# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $SubscriptionId,
    [Parameter(Mandatory)]
    [string] $ResourceGroup,
    [Parameter(Mandatory)]
    [string] $ClusterName,
    [Parameter(Mandatory)]
    [string] $Kubeconfig,
    [Parameter(Mandatory)]
    [string] $WorkspaceManifest,
    [Parameter(Mandatory)]
    [string] $WorkspaceName
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Invoke-Native {
    param([string] $FilePath, [string[]] $Arguments)

    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Command failed with exit code ${LASTEXITCODE}: $FilePath $($Arguments -join ' ')"
    }
}

Invoke-Native az @("aks", "get-credentials", "--admin", "--subscription", $SubscriptionId, "--resource-group", $ResourceGroup, "--name", $ClusterName, "--file", $Kubeconfig, "--overwrite-existing")
Invoke-Native kubectl @("apply", "--server-side", "--field-manager=taugrid-terraform", "-f", $WorkspaceManifest)

$workspaceResource = "workspace.tau.azure.com/$WorkspaceName"
$generation = kubectl get $workspaceResource --namespace tau-system --output=jsonpath='{.metadata.generation}'
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($generation)) {
    throw "Unable to read metadata.generation for $workspaceResource."
}

Invoke-Native kubectl @("wait", "--for=jsonpath={.status.observedGeneration}=$generation", $workspaceResource, "--namespace", "tau-system", "--timeout=10m")
Invoke-Native kubectl @("wait", "--for=jsonpath={.status.phase}=Ready", $workspaceResource, "--namespace", "tau-system", "--timeout=10m")
