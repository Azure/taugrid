# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

. (Join-Path $PSScriptRoot "AdxClusterName.ps1")

$subscriptionId = "00000000-0000-0000-0000-000000000001"
$resourceGroupName = "taugrid-verify-test"
$clusterName = "taugrid-verify-test"
$actual = Get-TauGridAdxClusterName -SubscriptionId $subscriptionId -ResourceGroupName $resourceGroupName -ClusterName $clusterName
$terraformExpression = '"taugrid${substr(sha256(join(":", ["' + $subscriptionId + '", "' + $resourceGroupName + '", "' + $clusterName + '"])), 0, 13)}"'
$expected = @($terraformExpression | & terraform "-chdir=$PSScriptRoot" console -no-color)
if ($LASTEXITCODE -ne 0) {
    throw "Terraform could not evaluate the ADX cluster naming expression."
}
$expected = ($expected -join [Environment]::NewLine).Trim().Trim('"')
if ($actual -ne $expected) {
    throw "PowerShell ADX cluster name '$actual' does not match Terraform's '$expected'."
}

$verificationScript = [System.IO.File]::ReadAllText((Join-Path $PSScriptRoot "Invoke-TauGridAksVerification.ps1"))
$accountLookup = $verificationScript.IndexOf('$account = az account show --output json | ConvertFrom-Json', [System.StringComparison]::Ordinal)
$nameDerivation = $verificationScript.IndexOf('$adxClusterName = Get-TauGridAdxClusterName', [System.StringComparison]::Ordinal)
if ($accountLookup -lt 0 -or $nameDerivation -lt 0 -or $accountLookup -ge $nameDerivation) {
    throw "The verification runner must read the Azure account before deriving the ADX cluster name."
}

$differentResourceGroupName = "taugrid-verify-other"
$different = Get-TauGridAdxClusterName -SubscriptionId $subscriptionId -ResourceGroupName $differentResourceGroupName -ClusterName $differentResourceGroupName
if ($actual -eq $different) {
    throw "ADX cluster naming must distinguish independent resource groups."
}

$repositoryCommandOutput = @('local.helm_dcgm_repository_command' | & terraform "-chdir=$PSScriptRoot" console -no-color -var "subscription_id=$subscriptionId")
if ($LASTEXITCODE -ne 0) {
    throw "Terraform could not evaluate the DCGM Helm repository command."
}
$repositoryCommand = ($repositoryCommandOutput -join [Environment]::NewLine).Trim() | ConvertFrom-Json
$repositoryAdd = $repositoryCommand.IndexOf("helm repo add dcgm-exporter https://nvidia.github.io/dcgm-exporter/helm-charts --force-update", [System.StringComparison]::Ordinal)
$repositoryList = $repositoryCommand.IndexOf("helm repo list --output json", [System.StringComparison]::Ordinal)
$expectedRepository = $repositoryCommand.IndexOf("https://nvidia.github.io/dcgm-exporter/helm-charts", $repositoryList, [System.StringComparison]::Ordinal)
$chartVerification = $repositoryCommand.IndexOf("helm show chart dcgm-exporter/dcgm-exporter --version", [System.StringComparison]::Ordinal)
if ($repositoryAdd -lt 0 -or $repositoryList -lt $repositoryAdd -or $expectedRepository -lt $repositoryList -or $chartVerification -lt $repositoryList) {
    throw "The DCGM Helm repository command must verify the isolated repository state and the pinned chart version after adding the repository."
}

foreach ($helmEnvironmentVariable in @("HELM_CONFIG_HOME", "HELM_CACHE_HOME", "HELM_DATA_HOME")) {
    if ($repositoryCommand.IndexOf("`$env:$helmEnvironmentVariable", [System.StringComparison]::Ordinal) -lt 0) {
        throw "The DCGM Helm repository command must create the isolated $helmEnvironmentVariable directory."
    }
}

$mainTerraform = [System.IO.File]::ReadAllText((Join-Path $PSScriptRoot "main.tf"))
if ($mainTerraform -notmatch '(?s)resource "terraform_data" "install_taugrid".*?triggers_replace\s+=\s+\[.*?abspath\(local\.kubeconfig_path\).*?KUBECONFIG\s+=\s+abspath\(local\.kubeconfig_path\)') {
    throw "The TauGrid installer must receive and track an absolute kubeconfig path."
}

foreach ($helmEnvironmentVariable in @("HELM_CONFIG_HOME", "HELM_CACHE_HOME", "HELM_DATA_HOME")) {
    if ($mainTerraform -notmatch "(?m)^\s*$helmEnvironmentVariable\s+=\s+local\.") {
        throw "The DCGM Terraform local-exec environment must set $helmEnvironmentVariable."
    }
}

$functionsWaiter = [System.IO.File]::ReadAllText((Join-Path $PSScriptRoot "Wait-ForAdxFunctionsReady.ps1"))
if ($functionsWaiter.IndexOf('function Get-FunctionReconciliationStatus', [System.StringComparison]::Ordinal) -lt 0 -or $functionsWaiter.IndexOf('PSObject.Properties["status"]', [System.StringComparison]::Ordinal) -lt 0) {
    throw "The ADX Functions waiter must treat a missing status field as not ready instead of failing under StrictMode."
}
