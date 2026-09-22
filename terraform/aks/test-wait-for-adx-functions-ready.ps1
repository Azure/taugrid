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
        [string] $ExpectedDelete = "",
        [string[]] $ManagementCommandResponses = @('{"items":[]}'),
        [string[]] $LifecycleSchemaResponses = @(""),
        [string] $TypedExperimentTelemetryEnabled = "false",
        [bool] $FailOnPendingSleep = $false
    )

    $global:waiterResponseIndex = 0
    $global:waiterResponses = $FunctionResponses
    $global:managementCommandResponseIndex = 0
    $global:lifecycleSchemaResponseIndex = 0
    $global:waiterDeletes = [System.Collections.Generic.List[string]]::new()
    $global:LASTEXITCODE = 0

    function global:az { }
    function global:helm { }
    function global:Start-Sleep {
        param([int] $Seconds)
        if ($global:failOnPendingSleep) {
            throw "resource remained pending"
        }
        Microsoft.PowerShell.Utility\Start-Sleep -Milliseconds 1
    }
    function global:kubectl {
        param([Parameter(ValueFromRemainingArguments)] [string[]] $Arguments)

        if ($Arguments[0] -eq "get") {
            if ($Arguments[1] -eq "managementcommand") {
                $index = [Math]::Min($global:lifecycleSchemaResponseIndex, $global:lifecycleSchemaResponses.Count - 1)
                $global:lifecycleSchemaResponseIndex++
                Write-Output $global:lifecycleSchemaResponses[$index]
                return
            }
            if ($Arguments[1] -eq "managementcommands") {
                $index = [Math]::Min($global:managementCommandResponseIndex, $global:managementCommandResponses.Count - 1)
                $global:managementCommandResponseIndex++
                Write-Output $global:managementCommandResponses[$index]
                return
            }
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
        $global:managementCommandResponses = $ManagementCommandResponses
        $global:lifecycleSchemaResponses = $LifecycleSchemaResponses
        $global:failOnPendingSleep = $FailOnPendingSleep
        $failed = $false
        $failureMessage = ""
        try {
            & $scriptPath -SubscriptionId "subscription" -ResourceGroup "resource-group" -ClusterName "cluster" -Kubeconfig "kubeconfig" -ChartPath "chart" -BaseValuesFile "base-values" -EnvironmentValuesFile "environment-values" -TypedExperimentTelemetryEnabled $TypedExperimentTelemetryEnabled -MaximumAttempts 2 -FunctionWaitSeconds 60
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
        Remove-Variable waiterResponseIndex, waiterResponses, waiterDeletes, managementCommandResponseIndex, managementCommandResponses, lifecycleSchemaResponseIndex, lifecycleSchemaResponses, failOnPendingSleep -Scope Global -ErrorAction SilentlyContinue
    }
}

$success = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"Success","error":""}}]}'
$typedFunctionsSuccess = '{"items":[{"metadata":{"name":"adx-mon-tau-exp-metric-event-rows","generation":1},"status":{"observedGeneration":1,"status":"Success","error":""}},{"metadata":{"name":"adx-mon-tau-exp-series-catalog-rows","generation":1},"status":{"observedGeneration":1,"status":"Success","error":""}},{"metadata":{"name":"adx-mon-tau-exp-run-catalog-rows","generation":1},"status":{"observedGeneration":1,"status":"Success","error":""}}]}'
$missingStatus = '{"items":[{}]}'
$staleSuccess = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":1,"status":"Success","error":""}}]}'
$terminalFailure = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"PermanentFailure","error":"invalid KQL"}}]}'
$throttleFailure = '{"items":[{"metadata":{"name":"metrics","generation":2},"status":{"observedGeneration":2,"status":"PermanentFailure","error":"RequestRateLimitPolicy throttling"}}]}'
$lifecycleSuccess = '{"metadata":{"name":"taugrid-lifecycle-schema","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"True","message":""}]}}'
$typedCommandsSuccess = '{"items":[{"metadata":{"name":"adx-mon-typed-metric-events-v1","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"True","message":""}]}},{"metadata":{"name":"adx-mon-typed-metric-events-v1-retention","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"True","message":""}]}},{"metadata":{"name":"adx-mon-experiment-catalog-v1","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"True","message":""}]}},{"metadata":{"name":"adx-mon-experiment-catalog-v1-retention","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"True","message":""}]}}]}'
$typedCommandFailure = '{"items":[{"metadata":{"name":"adx-mon-experiment-catalog-v1","generation":1},"status":{"conditions":[{"type":"managementcommand.adx-mon.azure.com","observedGeneration":1,"status":"False","message":"invalid KQL"}]}}]}'
$lifecycleMissingStatus = '{"metadata":{"name":"taugrid-lifecycle-schema","generation":1}}'

Invoke-WaiterCase -Name "current success" -FunctionResponses @($success) -ExpectFailure $false
Invoke-WaiterCase -Name "typed resources success" -FunctionResponses @($typedFunctionsSuccess) -ExpectFailure $false -ManagementCommandResponses @($typedCommandsSuccess) -LifecycleSchemaResponses @($lifecycleMissingStatus, $lifecycleSuccess) -TypedExperimentTelemetryEnabled "true"
Invoke-WaiterCase -Name "missing typed resources" -FunctionResponses @($success) -ExpectFailure $true -LifecycleSchemaResponses @($lifecycleSuccess) -TypedExperimentTelemetryEnabled "true" -FailOnPendingSleep $true
Invoke-WaiterCase -Name "missing Function status" -FunctionResponses @($missingStatus, $success) -ExpectFailure $false
Invoke-WaiterCase -Name "stale success" -FunctionResponses @($staleSuccess, $success) -ExpectFailure $false
Invoke-WaiterCase -Name "terminal failure" -FunctionResponses @($terminalFailure) -ExpectFailure $true
Invoke-WaiterCase -Name "retryable throttling" -FunctionResponses @($throttleFailure, $success) -ExpectFailure $false -ExpectedDelete "delete functions --namespace adx-mon metrics --ignore-not-found"
Invoke-WaiterCase -Name "typed ManagementCommand failure" -FunctionResponses @($typedFunctionsSuccess) -ExpectFailure $true -ManagementCommandResponses @($typedCommandFailure) -LifecycleSchemaResponses @($lifecycleSuccess) -TypedExperimentTelemetryEnabled "true"
