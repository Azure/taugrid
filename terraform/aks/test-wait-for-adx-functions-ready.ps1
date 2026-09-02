# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$scriptPath = Join-Path $PSScriptRoot "Wait-ForAdxFunctionsReady.ps1"

function Invoke-WaiterCase {
    param(
        [string] $Name,
        [string[]] $FunctionResponses,
        [bool] $ExpectFailure,
        [string] $ExpectedDelete = ""
    )

    $global:waiterResponseIndex = 0
    $global:waiterResponses = $FunctionResponses
    $global:waiterDeletes = [System.Collections.Generic.List[string]]::new()
    $global:LASTEXITCODE = 0

    function global:az { }
    function global:helm { }
    function global:Start-Sleep {
        param([int] $Seconds)
        Microsoft.PowerShell.Utility\Start-Sleep -Milliseconds 1
    }
    function global:kubectl {
        param([Parameter(ValueFromRemainingArguments)] [string[]] $Arguments)

        if ($Arguments[0] -eq "get") {
            $index = [Math]::Min($global:waiterResponseIndex, $global:waiterResponses.Count - 1)
            $global:waiterResponseIndex++
            Write-Output $global:waiterResponses[$index]
            return
        }
        if ($Arguments[0] -eq "delete") {
            $global:waiterDeletes.Add(($Arguments -join " "))
        }
    }

    try {
        $failed = $false
        $failureMessage = ""
        try {
            & $scriptPath -SubscriptionId "subscription" -ResourceGroup "resource-group" -ClusterName "cluster" -Kubeconfig "kubeconfig" -ChartPath "chart" -BaseValuesFile "base-values" -EnvironmentValuesFile "environment-values" -MaximumAttempts 2 -FunctionWaitSeconds 60
        } catch {
            $failed = $true
            $failureMessage = $_.Exception.Message
        }
        if ($failed -ne $ExpectFailure) {
            throw "Waiter failure result did not match the expected result for ${Name}: $failureMessage"
        }
        if ($ExpectedDelete -ne "" -and $ExpectedDelete -notin $global:waiterDeletes) {
            throw "Waiter did not delete the expected retryable Function."
        }
    } finally {
        Remove-Item Function:global:az -ErrorAction SilentlyContinue
        Remove-Item Function:global:helm -ErrorAction SilentlyContinue
        Remove-Item Function:global:kubectl -ErrorAction SilentlyContinue
        Remove-Item Function:global:Start-Sleep -ErrorAction SilentlyContinue
        Remove-Variable waiterResponseIndex, waiterResponses, waiterDeletes -Scope Global -ErrorAction SilentlyContinue
    }
}

$success = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"Success","error":""}}]}'
$missingStatus = '{"items":[{}]}'
$staleSuccess = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":1,"status":"Success","error":""}}]}'
$terminalFailure = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"PermanentFailure","error":"invalid KQL"}}]}'
$throttleFailure = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"PermanentFailure","error":"RequestRateLimitPolicy throttling"}}]}'

Invoke-WaiterCase -Name "current success" -FunctionResponses @($success) -ExpectFailure $false
Invoke-WaiterCase -Name "missing Function status" -FunctionResponses @($missingStatus, $success) -ExpectFailure $false
Invoke-WaiterCase -Name "stale success" -FunctionResponses @($staleSuccess, $success) -ExpectFailure $false
Invoke-WaiterCase -Name "terminal failure" -FunctionResponses @($terminalFailure) -ExpectFailure $true
Invoke-WaiterCase -Name "retryable throttling" -FunctionResponses @($throttleFailure, $success) -ExpectFailure $false -ExpectedDelete "delete functions --namespace adx-mon metrics --ignore-not-found"
