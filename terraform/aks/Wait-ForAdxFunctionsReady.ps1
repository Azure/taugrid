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
    [string] $ChartPath,
    [Parameter(Mandatory)]
    [string] $BaseValuesFile,
    [Parameter(Mandatory)]
    [string] $EnvironmentValuesFile,
    [ValidateRange(1, 10)]
    [int] $MaximumAttempts = 6,
    [ValidateRange(60, 900)]
    [int] $FunctionWaitSeconds = 300
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

for ($attempt = 1; $attempt -le $MaximumAttempts; $attempt++) {
    Invoke-Native helm @("upgrade", "--install", "adx-mon", $ChartPath, "--namespace", "adx-mon", "--create-namespace", "--values", $BaseValuesFile, "--values", $EnvironmentValuesFile, "--wait", "--timeout", "30m")

    $deadline = (Get-Date).AddSeconds($FunctionWaitSeconds)
    $permanentFailures = @()
    while ((Get-Date) -lt $deadline) {
        $rawFunctions = kubectl get functions --namespace adx-mon --output=json
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to read adx-mon Function status."
        }
        $functions = @((ConvertFrom-Json -InputObject ($rawFunctions -join [Environment]::NewLine)).items)
        if ($functions.Count -gt 0) {
            $permanentFailures = @($functions | Where-Object { $_.status.status -eq "PermanentFailure" })
            if ($permanentFailures.Count -gt 0) {
                break
            }
            if (@($functions | Where-Object { $_.status.status -ne "Success" }).Count -eq 0) {
                Write-Host "All adx-mon Functions reached Success."
                exit 0
            }
        }
        Start-Sleep -Seconds 15
    }

    $failedNames = @($permanentFailures | ForEach-Object { $_.metadata.name }) -join ", "
    if ([string]::IsNullOrWhiteSpace($failedNames)) {
        Write-Warning "adx-mon Functions did not reach Success within $FunctionWaitSeconds seconds on attempt $attempt."
    } else {
        Write-Warning "adx-mon Functions reached PermanentFailure on attempt ${attempt}: $failedNames"
    }
    if ($attempt -eq $MaximumAttempts) {
        throw "adx-mon Functions did not reach Success after $MaximumAttempts attempts."
    }

    Invoke-Native kubectl @("delete", "functions", "--namespace", "adx-mon", "--all", "--ignore-not-found")
    Start-Sleep -Seconds (60 * $attempt)
}
