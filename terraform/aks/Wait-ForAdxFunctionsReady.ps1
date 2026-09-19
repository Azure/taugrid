# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.3
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $SubscriptionId,
    [Parameter(Mandatory)] [string] $ResourceGroup,
    [Parameter(Mandatory)] [string] $ClusterName,
    [Parameter(Mandatory)] [string] $Kubeconfig,
    [Parameter(Mandatory)] [string] $ChartPath,
    [Parameter(Mandatory)] [string] $BaseValuesFile,
    [Parameter(Mandatory)] [string] $EnvironmentValuesFile,
    [string] $ReleaseName = "adx-mon",
    [string] $ReleaseNamespace = "adx-mon",
    [ValidateRange(1, 10)] [int] $MaximumAttempts = 6,
    [ValidateRange(60, 900)] [int] $FunctionWaitSeconds = 300,
    [switch] $AllowNoFunctions
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$PSNativeCommandUseErrorActionPreference = $false
$policy = Join-Path $PSScriptRoot "adx-function-state.jq"
$expected = "[]"

function Invoke-Native {
    param([string] $FilePath, [string[]] $Arguments)
    $result = & $FilePath @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Command failed with exit code ${LASTEXITCODE}: $FilePath $($Arguments -join ' '): $($result -join [Environment]::NewLine)"
    }
    return $result
}

function Convert-FunctionSource {
    param([string] $Json, [string] $Mode, [string] $Namespace = "")
    $result = $Json | jq -cse --arg mode $Mode --arg release $ReleaseName --arg releaseNamespace $ReleaseNamespace --arg namespace $Namespace --argjson expected $expected --from-file $policy 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Invalid Function source: $($result -join [Environment]::NewLine)"
    }
    return $result -join [Environment]::NewLine
}

function Get-FunctionStates {
    param([string] $Mode)
    foreach ($namespace in $namespaces) {
        $raw = Invoke-Native kubectl @("get", "functions.adx-mon.azure.com", "--namespace", $namespace, "--output=json", "--kubeconfig", $Kubeconfig, "--request-timeout=30s")
        $states = Convert-FunctionSource -Json ($raw -join [Environment]::NewLine) -Mode $Mode -Namespace $namespace
        $states | ConvertFrom-Json
    }
}

foreach ($tool in @("az", "helm", "kubectl", "jq", "yq")) {
    Get-Command $tool -ErrorAction Stop | Out-Null
}
$yqVersion = Invoke-Native yq @("--version")
if ("$yqVersion" -notlike "*github.com/mikefarah/yq/*version v4.*") {
    throw "The Function waiter requires Mike Farah yq v4."
}

$chartArguments = @($ReleaseName, $ChartPath, "--namespace", $ReleaseNamespace, "--values", $BaseValuesFile, "--values", $EnvironmentValuesFile)
# The render and every upgrade use identical ordered values, without live Helm storage reads.
$rendered = Invoke-Native helm (@("template") + $chartArguments)
$documents = $rendered | yq eval-all -o=json -I=0 '[.]' - 2>&1
if ($LASTEXITCODE -ne 0) { throw "Unable to parse rendered chart: $($documents -join [Environment]::NewLine)" }
$expected = Convert-FunctionSource -Json ($documents -join [Environment]::NewLine) -Mode expected
$requiredFunctions = @($expected | ConvertFrom-Json)
if ($requiredFunctions.Count -eq 0 -and -not $AllowNoFunctions) {
    throw "No required Functions rendered. Review the chart/values; use -AllowNoFunctions only for an intentionally Function-free installation."
}
$namespaces = @($requiredFunctions | ForEach-Object { $_.namespace } | Sort-Object -Unique)

Invoke-Native az @("aks", "get-credentials", "--admin", "--subscription", $SubscriptionId, "--resource-group", $ResourceGroup, "--name", $ClusterName, "--file", $Kubeconfig, "--overwrite-existing") | Write-Host

for ($attempt = 1; $attempt -le $MaximumAttempts; $attempt++) {
    # Preflight also fences a recreated or re-owned name after a conditional-delete conflict.
    Get-FunctionStates -Mode preflight | Out-Null
    Invoke-Native helm (@("upgrade", "--install") + $chartArguments + @("--reset-values", "--kubeconfig", $Kubeconfig, "--create-namespace", "--wait", "--timeout", "30m")) | Write-Host
    if ($requiredFunctions.Count -eq 0) {
        Write-Host "No Functions rendered; Function readiness was explicitly disabled."
        return
    }

    $deadline = (Get-Date).AddSeconds($FunctionWaitSeconds)
    $retryableFailures = @()
    $states = @()
    while ((Get-Date) -lt $deadline) {
        $states = @(Get-FunctionStates -Mode state)
        $terminalFailures = @($states | Where-Object { $_.phase -eq "Terminal" })
        if ($terminalFailures.Count -gt 0) {
            throw "adx-mon Function reconciliation reached a terminal failure: $($terminalFailures.diagnostic -join '; ')"
        }
        $retryableFailures = @($states | Where-Object { $_.phase -eq "Throttle" })
        if ($retryableFailures.Count -gt 0) { break }
        if ($states.Count -gt 0 -and @($states | Where-Object { $_.phase -ne "Success" }).Count -eq 0) {
            Write-Host "All required adx-mon Functions reached Success."
            return
        }
        Start-Sleep -Seconds 15
    }

    if ($retryableFailures.Count -eq 0) {
        $pending = @($states | Where-Object { $_.phase -ne "Success" } | ForEach-Object { $_.diagnostic })
        throw "adx-mon Functions did not reach Success within $FunctionWaitSeconds seconds on attempt ${attempt}: $($pending -join '; ')"
    }
    Write-Warning "adx-mon Functions reached retryable ADX throttling on attempt ${attempt}."
    if ($attempt -eq $MaximumAttempts) {
        throw "adx-mon Functions did not recover from ADX throttling after $MaximumAttempts attempts."
    }

    foreach ($failure in $retryableFailures) {
        $path = "/apis/adx-mon.azure.com/v1/namespaces/$($failure.namespace)/functions/$($failure.name)"
        $options = @{apiVersion = "v1"; kind = "DeleteOptions"; preconditions = @{uid = $failure.uid; resourceVersion = $failure.resourceVersion}} | ConvertTo-Json -Compress
        # Unlike a name-only delete, Kubernetes enforces both incarnation and update preconditions.
        $deleteOutput = $options | kubectl delete --raw $path --filename - --kubeconfig $Kubeconfig --request-timeout=30s 2>&1
        if ($LASTEXITCODE -eq 0) {
            $deleteOutput | Write-Host
        } elseif (($deleteOutput -join "`n") -cmatch "Error from server \((Conflict|NotFound)\):") {
            Write-Warning "Function changed before conditional deletion; re-observing within the retry budget: $($deleteOutput -join ' ')"
            break
        } else {
            throw "Conditional Function deletion failed: $($deleteOutput -join [Environment]::NewLine)"
        }
    }
    Start-Sleep -Seconds (60 * $attempt)
}
