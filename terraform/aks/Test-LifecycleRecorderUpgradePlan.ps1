# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $PlanPath,
    [Parameter(Mandatory)]
    [ValidateNotNullOrEmpty()]
    [string] $ExpectedAssignmentId,
    [Parameter(Mandatory)]
    [guid] $ExpectedClientId,
    [Parameter(Mandatory)]
    [guid] $ExpectedTenantId
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Test-JsonEqual {
    param($Left, $Right)

    if ($null -eq $Left -or $null -eq $Right) { return $null -eq $Left -and $null -eq $Right }
    if ($Left -is [System.Collections.IDictionary] -and $Right -is [System.Collections.IDictionary]) {
        if ($Left.Count -ne $Right.Count) { return $false }
        foreach ($key in $Left.Keys) {
            if (@($Right.Keys) -cnotcontains $key -or -not (Test-JsonEqual $Left[$key] $Right[$key])) { return $false }
        }
        return $true
    }
    if ($Left -is [System.Collections.IList] -and $Right -is [System.Collections.IList]) {
        if ($Left.Count -ne $Right.Count) { return $false }
        for ($i = 0; $i -lt $Left.Count; $i++) {
            if (-not (Test-JsonEqual $Left[$i] $Right[$i])) { return $false }
        }
        return $true
    }
    return $Left.GetType() -eq $Right.GetType() -and $Left -ceq $Right
}

function Test-ContainsUnknown {
    param($Value)

    if ($Value -is [System.Collections.IDictionary]) {
        foreach ($item in $Value.Values) {
            if (Test-ContainsUnknown $item) { return $true }
        }
    } elseif ($Value -is [System.Collections.IList]) {
        foreach ($item in $Value) {
            if (Test-ContainsUnknown $item) { return $true }
        }
    } elseif ($Value -is [bool]) {
        return $Value
    }
    return $false
}

$rawPlan = & terraform show -json $PlanPath
if ($LASTEXITCODE -ne 0) {
    throw "Unable to read the saved Terraform plan. Do not apply it."
}
$plan = ConvertFrom-Json -InputObject ($rawPlan -join [Environment]::NewLine) -AsHashtable -Depth 100
if ($plan["format_version"] -notlike "1.*" -or $plan["errored"] -ne $false -or $plan["complete"] -ne $true) {
    throw "Expected a complete, non-errored Terraform plan in JSON format 1.x. Do not apply it."
}

$oldAddress = "azurerm_kusto_database_principal_assignment.lifecycle_recorder[0]"
$newAddress = "azapi_resource.lifecycle_recorder_principal_assignment[0]"
$changes = @($plan["resource_changes"] | Where-Object {
    $_["address"] -eq $newAddress -or $_["address"] -eq $oldAddress -or $_["previous_address"] -eq $oldAddress
})
if ($changes.Count -ne 1 -or $changes[0]["address"] -ne $newAddress -or $changes[0]["previous_address"] -ne $oldAddress) {
    throw "Expected exactly one AzureRM-to-AzAPI lifecycle recorder move. Missing or conflicting ownership; do not apply."
}
$change = $changes[0].change
if (@($change.actions).Count -ne 1 -or $change.actions[0] -notin @("no-op", "update")) {
    throw "The lifecycle recorder grant would be created, deleted, or replaced. Do not apply."
}
if (Test-ContainsUnknown $change["after_unknown"]) {
    throw "The grant has unknown planned values. A write-free migration cannot be established; do not apply."
}

$before = $change["before"]
$after = $change["after"]
if ($null -eq $before -or $null -eq $after) {
    throw "The plan is missing before/after grant values. Do not apply."
}
foreach ($key in @("id", "name", "parent_id")) {
    if ([string]::IsNullOrWhiteSpace($before[$key]) -or $before[$key] -cne $after[$key]) {
        throw "The grant's $key is missing or changed. Do not apply."
    }
}
if ($after.name -cne "taugrid-lifecycle-recorder-ingestor" -or
    $after.parent_id -notmatch "/providers/Microsoft\.Kusto/clusters/[^/]+/databases/Metrics$" -or
    $after.id -cne "$($after.parent_id)/principalAssignments/$($after.name)" -or
    $after["type"] -cnotmatch "^Microsoft\.Kusto/clusters/databases/principalAssignments@(2024-04-13|2025-02-14)$") {
    throw "The plan does not preserve the expected Metrics principal assignment. Do not apply."
}
if ($null -eq $before["body"] -or $null -eq $before.body["properties"] -or
    $null -eq $after["body"] -or $null -eq $after.body["properties"]) {
    throw "The plan is missing the refreshed grant body. Use a normal refreshed plan; do not apply."
}
foreach ($key in @("principalId", "tenantId", "principalType", "role")) {
    $oldValue = $before.body.properties[$key]
    $newValue = $after.body.properties[$key]
    if ([string]::IsNullOrWhiteSpace($oldValue) -or $oldValue -cne $newValue) {
        throw "The grant's $key is missing or changed. Do not apply."
    }
}
if ($after.body.properties.principalType -cne "App" -or $after.body.properties.role -cne "Ingestor") {
    throw "The recorder must retain its App/Ingestor grant. Do not apply."
}
if ($after.id -cne $ExpectedAssignmentId -or
    $after.body.properties.principalId -ine $ExpectedClientId.ToString() -or
    $after.body.properties.tenantId -ine $ExpectedTenantId.ToString()) {
    throw "The grant does not match the independently recorded pre-plan ARM ID, client ID, and tenant ID. Do not apply."
}

# In the locked AzAPI 2.12.0 model only these configured attributes are marked
# skip_on:"update". Other differences can cause a PUT even with the same grant.
$stateOnlyAttributes = @("retry", "timeouts")
$keys = @($before.Keys) + @($after.Keys) | Sort-Object -Unique
foreach ($key in $keys) {
    if ($key -notin $stateOnlyAttributes -and -not (Test-JsonEqual $before[$key] $after[$key])) {
        throw "Non-state-only attribute '$key' changes in the grant plan. An external write cannot be excluded; do not apply."
    }
}

Write-Host "The grant move has no resource changes or only AzAPI retry/timeout state changes. Review all other plan changes before an approved apply."
