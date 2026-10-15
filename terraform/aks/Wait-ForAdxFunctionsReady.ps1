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
    [ValidateSet("true", "false")]
    [string] $TypedExperimentTelemetryEnabled = "false",
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

    $metadataProperty = $Function.PSObject.Properties["metadata"]
    $statusProperty = $Function.PSObject.Properties["status"]
    $metadata = if ($null -eq $metadataProperty) { $null } else { $metadataProperty.Value }
    $status = if ($null -eq $statusProperty) { $null } else { $statusProperty.Value }
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

function Get-ManagementCommandReconciliationState {
    param([object] $ManagementCommand)

    $metadataProperty = $ManagementCommand.PSObject.Properties["metadata"]
    $statusProperty = $ManagementCommand.PSObject.Properties["status"]
    $metadata = if ($null -eq $metadataProperty) { $null } else { $metadataProperty.Value }
    $status = if ($null -eq $statusProperty) { $null } else { $statusProperty.Value }
    $conditionsProperty = if ($null -eq $status) { $null } else { $status.PSObject.Properties["conditions"] }
    $conditions = if ($null -eq $conditionsProperty) { @() } else { @($conditionsProperty.Value) }
    $condition = @($conditions | Where-Object {
        $_.type -eq "managementcommand.adx-mon.azure.com"
    }) | Select-Object -First 1
    return [pscustomobject]@{
        Name = Get-FunctionProperty -Function $metadata -PropertyName "name"
        Generation = Get-FunctionProperty -Function $metadata -PropertyName "generation"
        ObservedGeneration = Get-FunctionProperty -Function $condition -PropertyName "observedGeneration"
        Status = Get-FunctionProperty -Function $condition -PropertyName "status"
        Error = Get-FunctionProperty -Function $condition -PropertyName "message"
    }
}

function Test-RetryableAdxThrottle {
    param([string] $ErrorMessage)

    return $ErrorMessage -match "(?i)throttl|requestratelimitpolicy|toomanyrequests"
}

function Test-RetryableAdxDependency {
    param([string] $ErrorMessage)

    return $ErrorMessage -match "(?i)failed to resolve (table|materialized-view)|table .* (does not exist|was not found)|materialized[- ]view .* (does not exist|was not found)"
}

function Format-FunctionDiagnostic {
    param([object] $FunctionState)

    return "$($FunctionState.Name) (generation=$($FunctionState.Generation), observedGeneration=$($FunctionState.ObservedGeneration), status=$($FunctionState.Status), error=$($FunctionState.Error))"
}

function Get-FunctionProperty {
    param([object] $Function, [string] $PropertyName)

    if ($null -eq $Function) {
        return ""
    }
    $property = $Function.PSObject.Properties[$PropertyName]
    if ($null -eq $property -or $null -eq $property.Value) {
        return ""
    }
    return [string] $property.Value
}

Invoke-Native az @("aks", "get-credentials", "--admin", "--subscription", $SubscriptionId, "--resource-group", $ResourceGroup, "--name", $ClusterName, "--file", $Kubeconfig, "--overwrite-existing")

if ($TypedExperimentTelemetryEnabled -eq "true") {
    $lifecycleDeadline = (Get-Date).AddSeconds($FunctionWaitSeconds)
    while ((Get-Date) -lt $lifecycleDeadline) {
        $rawLifecycleCommand = kubectl get managementcommand taugrid-lifecycle-schema --namespace adx-mon --output=json
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to read TauExpRunLifecycle schema status."
        }
        $lifecycleCommand = ConvertFrom-Json -InputObject ($rawLifecycleCommand -join [Environment]::NewLine)
        $lifecycleState = Get-ManagementCommandReconciliationState -ManagementCommand $lifecycleCommand
        if ($lifecycleState.Generation -eq $lifecycleState.ObservedGeneration -and $lifecycleState.Status -eq "False") {
            throw "TauExpRunLifecycle schema reconciliation failed: $(Format-FunctionDiagnostic -FunctionState $lifecycleState)"
        }
        if ($lifecycleState.Generation -eq $lifecycleState.ObservedGeneration -and $lifecycleState.Status -eq "True") {
            break
        }
        Start-Sleep -Seconds 15
    }
    if ($lifecycleState.Generation -ne $lifecycleState.ObservedGeneration -or $lifecycleState.Status -ne "True") {
        throw "TauExpRunLifecycle schema did not reach Success within $FunctionWaitSeconds seconds."
    }
}

$requiredManagementCommandNames = @(
    "adx-mon-typed-metric-events-v1",
    "adx-mon-typed-metric-events-v1-retention",
    "adx-mon-experiment-catalog-v1",
    "adx-mon-experiment-catalog-v1-retention"
)
$requiredTypedFunctionNames = @(
    "adx-mon-tau-exp-metric-event-rows",
    "adx-mon-tau-exp-series-catalog-rows",
    "adx-mon-tau-exp-run-catalog-rows"
)

for ($attempt = 1; $attempt -le $MaximumAttempts; $attempt++) {
    Invoke-Native helm @("upgrade", "--install", "adx-mon", $ChartPath, "--namespace", "adx-mon", "--create-namespace", "--values", $BaseValuesFile, "--values", $EnvironmentValuesFile, "--wait", "--timeout", "30m")

    $deadline = (Get-Date).AddSeconds($FunctionWaitSeconds)
    $retryableFunctionNames = @()
    $retryableManagementCommandNames = @()
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
            $retryableFunctionNames = @($currentGenerationFailures | Where-Object {
                Test-RetryableAdxThrottle -ErrorMessage $_.Error
            } | ForEach-Object { $_.Name })
        }

        $rawManagementCommands = kubectl get managementcommands --namespace adx-mon --output=json
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to read adx-mon ManagementCommand status."
        }
        $managementCommands = @((ConvertFrom-Json -InputObject ($rawManagementCommands -join [Environment]::NewLine)).items | Where-Object {
            $_.metadata.name -in $requiredManagementCommandNames
        })
        $managementCommandStates = @($managementCommands | ForEach-Object {
            Get-ManagementCommandReconciliationState -ManagementCommand $_
        })
        $currentManagementCommandFailures = @($managementCommandStates | Where-Object {
            $_.Generation -eq $_.ObservedGeneration -and $_.Status -eq "False"
        })
        $terminalManagementCommandFailures = @($currentManagementCommandFailures | Where-Object {
            -not (Test-RetryableAdxThrottle -ErrorMessage $_.Error) -and
            -not (Test-RetryableAdxDependency -ErrorMessage $_.Error)
        })
        if ($terminalManagementCommandFailures.Count -gt 0) {
            $diagnostics = @($terminalManagementCommandFailures | ForEach-Object {
                Format-FunctionDiagnostic -FunctionState $_
            }) -join "; "
            throw "adx-mon ManagementCommand reconciliation reached a terminal failure: $diagnostics"
        }
        $retryableManagementCommandNames = @($currentManagementCommandFailures | Where-Object {
            Test-RetryableAdxThrottle -ErrorMessage $_.Error
        } | ForEach-Object { $_.Name })
        if ($retryableFunctionNames.Count -gt 0 -or $retryableManagementCommandNames.Count -gt 0) {
            break
        }

        $functionsReady = $functions.Count -gt 0 -and @($functionStates | Where-Object {
            $_.Generation -ne $_.ObservedGeneration -or $_.Status -ne "Success"
        }).Count -eq 0
        if ($TypedExperimentTelemetryEnabled -eq "true") {
            $typedFunctionStates = @($functionStates | Where-Object {
                $_.Name -in $requiredTypedFunctionNames
            })
            $functionsReady = $functionsReady -and $typedFunctionStates.Count -eq $requiredTypedFunctionNames.Count
        }
        $managementCommandsReady = (
            $TypedExperimentTelemetryEnabled -ne "true" -and $managementCommandStates.Count -eq 0
        ) -or (
            $managementCommandStates.Count -eq $requiredManagementCommandNames.Count -and
            @($managementCommandStates | Where-Object {
                $_.Generation -ne $_.ObservedGeneration -or $_.Status -ne "True"
            }).Count -eq 0
        )
        if ($functionsReady -and $managementCommandsReady) {
            Write-Host "All adx-mon Functions and typed experiment ManagementCommands reached Success."
            return
        }
        Start-Sleep -Seconds 15
    }

    if ($retryableFunctionNames.Count -eq 0 -and $retryableManagementCommandNames.Count -eq 0) {
        Write-Warning "adx-mon Functions and typed experiment ManagementCommands did not reach Success within $FunctionWaitSeconds seconds on attempt $attempt."
        throw "adx-mon Functions and typed experiment ManagementCommands did not reach Success within $FunctionWaitSeconds seconds."
    } else {
        Write-Warning "adx-mon resources reached retryable ADX throttling on attempt ${attempt}: functions=$($retryableFunctionNames -join ', '); managementCommands=$($retryableManagementCommandNames -join ', ')"
    }
    if ($attempt -eq $MaximumAttempts) {
        throw "adx-mon resources did not recover from ADX throttling after $MaximumAttempts attempts."
    }

    if ($retryableFunctionNames.Count -gt 0) {
        $deleteArguments = @("delete", "functions", "--namespace", "adx-mon") + $retryableFunctionNames + @("--ignore-not-found")
        Invoke-Native kubectl $deleteArguments
    }
    if ($retryableManagementCommandNames.Count -gt 0) {
        $deleteArguments = @("delete", "managementcommands", "--namespace", "adx-mon") + $retryableManagementCommandNames + @("--ignore-not-found")
        Invoke-Native kubectl $deleteArguments
    }
    Start-Sleep -Seconds (60 * $attempt)
}
