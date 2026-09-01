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

$differentResourceGroupName = "taugrid-verify-other"
$different = Get-TauGridAdxClusterName -SubscriptionId $subscriptionId -ResourceGroupName $differentResourceGroupName -ClusterName $differentResourceGroupName
if ($actual -eq $different) {
    throw "ADX cluster naming must distinguish independent resource groups."
}
