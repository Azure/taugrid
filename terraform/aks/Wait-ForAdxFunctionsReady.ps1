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

function Get-FunctionReconciliationState {
    param([object] $Function)

    $metadata = $Function.PSObject.Properties["metadata"].Value
    $status = $Function.PSObject.Properties["status"].Value
    if ($null -eq $metadata -or $null -eq $status) {
        return [pscustomobject]@{
            Name = ""
            Generation = ""
            ObservedGeneration = ""
            Status = ""
            Error = ""
        }
    }
    return [pscustomobject]@{
        Name = Get-FunctionProperty -Function $metadata -PropertyName "name"
        Generation = Get-FunctionProperty -Function $metadata -PropertyName "generation"
        ObservedGeneration = Get-FunctionProperty -Function $status -PropertyName "observedGeneration"
        Status = Get-FunctionProperty -Function $status -PropertyName "status"
        Error = Get-FunctionProperty -Function $status -PropertyName "error"
    }
}

function Test-RetryableAdxThrottle {
    param([string] $ErrorMessage)

    return $ErrorMessage -match "(?i)throttl|requestratelimitpolicy|toomanyrequests"
}

function Format-FunctionDiagnostic {
    param([object] $FunctionState)

    return "$($FunctionState.Name) (generation=$($FunctionState.Generation), observedGeneration=$($FunctionState.ObservedGeneration), status=$($FunctionState.Status), error=$($FunctionState.Error))"
}

function Get-FunctionProperty {
    param([object] $Function, [string] $PropertyName)

    $property = $Function.PSObject.Properties[$PropertyName]
    if ($null -eq $property -or $null -eq $property.Value) {
        return ""
    }
    return [string] $property.Value
}

Invoke-Native az @("aks", "get-credentials", "--admin", "--subscription", $SubscriptionId, "--resource-group", $ResourceGroup, "--name", $ClusterName, "--file", $Kubeconfig, "--overwrite-existing")

for ($attempt = 1; $attempt -le $MaximumAttempts; $attempt++) {
    Invoke-Native helm @("upgrade", "--install", "adx-mon", $ChartPath, "--namespace", "adx-mon", "--create-namespace", "--values", $BaseValuesFile, "--values", $EnvironmentValuesFile, "--wait", "--timeout", "30m")

    $deadline = (Get-Date).AddSeconds($FunctionWaitSeconds)
    $retryableFailureNames = @()
    while ((Get-Date) -lt $deadline) {
        $rawFunctions = kubectl get functions --namespace adx-mon --output=json
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to read adx-mon Function status."
        }
        $functions = @((ConvertFrom-Json -InputObject ($rawFunctions -join [Environment]::NewLine)).items)
        if ($functions.Count -gt 0) {
            $functionStates = @($functions | ForEach-Object { Get-FunctionReconciliationState -Function $_ })
            $currentGenerationFailures = @($functionStates | Where-Object {
                $_.Generation -eq $_.ObservedGeneration -and $_.Status -eq "PermanentFailure"
            })
            $terminalFailures = @($currentGenerationFailures | Where-Object { -not (Test-RetryableAdxThrottle -ErrorMessage $_.Error) })
            if ($terminalFailures.Count -gt 0) {
                $diagnostics = @($terminalFailures | ForEach-Object { Format-FunctionDiagnostic -FunctionState $_ }) -join "; "
                throw "adx-mon Function reconciliation reached a terminal failure: $diagnostics"
            }
            $retryableFailureNames = @($currentGenerationFailures | Where-Object {
                Test-RetryableAdxThrottle -ErrorMessage $_.Error
            } | ForEach-Object { $_.Name })
            if ($retryableFailureNames.Count -gt 0) {
                break
            }
            if (@($functionStates | Where-Object {
                $_.Generation -ne $_.ObservedGeneration -or $_.Status -ne "Success"
            }).Count -eq 0) {
                Write-Host "All adx-mon Functions reached Success."
                exit 0
            }
        }
        Start-Sleep -Seconds 15
    }

    if ($retryableFailureNames.Count -eq 0) {
        Write-Warning "adx-mon Functions did not reach Success within $FunctionWaitSeconds seconds on attempt $attempt."
        throw "adx-mon Functions did not reach Success within $FunctionWaitSeconds seconds."
    } else {
        Write-Warning "adx-mon Functions reached retryable ADX throttling on attempt ${attempt}: $($retryableFailureNames -join ', ')"
    }
    if ($attempt -eq $MaximumAttempts) {
        throw "adx-mon Functions did not recover from ADX throttling after $MaximumAttempts attempts."
    }

    $deleteArguments = @("delete", "functions", "--namespace", "adx-mon") + $retryableFailureNames + @("--ignore-not-found")
    Invoke-Native kubectl $deleteArguments
    Start-Sleep -Seconds (60 * $attempt)
}
