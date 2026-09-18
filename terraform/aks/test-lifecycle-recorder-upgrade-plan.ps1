# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$parentId = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/taugrid-test/providers/Microsoft.Kusto/clusters/taugridtest/databases/Metrics"
$grant = @{
    id = "$parentId/principalAssignments/taugrid-lifecycle-recorder-ingestor"
    name = "taugrid-lifecycle-recorder-ingestor"
    parent_id = $parentId
    type = "Microsoft.Kusto/clusters/databases/principalAssignments@2024-04-13"
    body = @{
        properties = @{
            principalId = "00000000-0000-0000-0000-000000000002"
            tenantId = "00000000-0000-0000-0000-000000000003"
            principalType = "App"
            role = "Ingestor"
        }
    }
}
$fixture = @{
    format_version = "1.2"
    errored = $false
    complete = $true
    resource_changes = @(@{
        address = "azapi_resource.lifecycle_recorder_principal_assignment[0]"
        previous_address = "azurerm_kusto_database_principal_assignment.lifecycle_recorder[0]"
        change = @{
            actions = @("no-op")
            before = $grant
            after = $grant
            after_unknown = @{}
        }
    })
} | ConvertTo-Json -Depth 20

function Invoke-PlanCase {
    param([string] $Name, [scriptblock] $Modify, [string] $ExpectedError = "")

    $plan = ConvertFrom-Json -InputObject $fixture -AsHashtable -Depth 20
    & $Modify $plan
    $global:upgradePlanJson = $plan | ConvertTo-Json -Depth 20
    $global:LASTEXITCODE = 0
    function global:terraform {
        param([Parameter(ValueFromRemainingArguments)] [string[]] $Arguments)
        if (($Arguments -join " ") -ne "show -json fixture.tfplan") {
            throw "Unexpected Terraform command: $($Arguments -join ' ')"
        }
        $global:upgradePlanJson
    }
    try {
        $failure = ""
        try {
            & (Join-Path $PSScriptRoot "Test-LifecycleRecorderUpgradePlan.ps1") -PlanPath fixture.tfplan `
                -ExpectedAssignmentId $grant.id -ExpectedClientId $grant.body.properties.principalId -ExpectedTenantId $grant.body.properties.tenantId
        } catch {
            $failure = $_.Exception.Message
        }
        if ($ExpectedError -eq "") {
            if ($failure -ne "") { throw "${Name}: unexpected failure: $failure" }
        } elseif ($failure -notlike "*$ExpectedError*") {
            throw "${Name}: expected '$ExpectedError', got '$failure'"
        }
    } finally {
        Remove-Item Function:global:terraform
        Remove-Variable upgradePlanJson -Scope Global
    }
}

# These are checker unit tests, not evidence of a real provider migration plan.
Invoke-PlanCase "unchanged moved grant" {}
Invoke-PlanCase "unrelated changes still require review" {
    param($p)
    $p.resource_changes += @{ address = "unrelated.example"; change = @{ actions = @("update") } }
}
Invoke-PlanCase "destroy/create" { param($p) $p.resource_changes[0].change.actions = @("delete", "create") } "created, deleted, or replaced"
Invoke-PlanCase "state-only retry/timeout update" {
    param($p)
    $p.resource_changes[0].change.actions = @("update")
    $p.resource_changes[0].change.before.retry = $null
    $p.resource_changes[0].change.before.timeouts = $null
    $p.resource_changes[0].change.after.retry = @{ error_message_regex = @("(?i)AAD principal was not found"); interval_seconds = 10; max_interval_seconds = 180 }
    $p.resource_changes[0].change.after.timeouts = @{ create = "60m" }
}
Invoke-PlanCase "retained moved API version" {
    param($p)
    $p.resource_changes[0].change.before.type = "Microsoft.Kusto/clusters/databases/principalAssignments@2025-02-14"
    $p.resource_changes[0].change.after.type = $p.resource_changes[0].change.before.type
}
Invoke-PlanCase "non-state-only update" {
    param($p)
    $p.resource_changes[0].change.actions = @("update")
    $p.resource_changes[0].change.before.schema_validation_enabled = $true
    $p.resource_changes[0].change.after.schema_validation_enabled = $false
} "Non-state-only attribute"
Invoke-PlanCase "changed export settings" {
    param($p)
    $p.resource_changes[0].change.actions = @("update")
    $p.resource_changes[0].change.after.response_export_values = @("*")
} "Non-state-only attribute"
Invoke-PlanCase "extra body change" {
    param($p)
    $p.resource_changes[0].change.actions = @("update")
    $p.resource_changes[0].change.after.body.properties.tenantName = "different"
} "Non-state-only attribute"
Invoke-PlanCase "unknown computed output" { param($p) $p.resource_changes[0].change.after_unknown.output = $true } "unknown planned values"
Invoke-PlanCase "nested unknown" { param($p) $p.resource_changes[0].change.after_unknown.body = @{ properties = @{ principalId = $true } } } "unknown planned values"
Invoke-PlanCase "independent client-ID baseline" {
    param($p)
    $p.resource_changes[0].change.before.body.properties.principalId = "00000000-0000-0000-0000-000000000099"
    $p.resource_changes[0].change.after.body.properties.principalId = $p.resource_changes[0].change.before.body.properties.principalId
} "independently recorded"
Invoke-PlanCase "missing move" { param($p) $p.resource_changes[0].Remove("previous_address") } "exactly one"
Invoke-PlanCase "no grant" { param($p) $p.resource_changes = @() } "exactly one"
Invoke-PlanCase "conflicting old ownership" {
    param($p)
    $p.resource_changes += @{ address = $p.resource_changes[0].previous_address; change = @{ actions = @("delete") } }
} "exactly one"
Invoke-PlanCase "changed ARM ID" { param($p) $p.resource_changes[0].change.after.id += "-different" } "id is missing or changed"
Invoke-PlanCase "changed database" { param($p) $p.resource_changes[0].change.after.parent_id += "-different" } "parent_id is missing or changed"
Invoke-PlanCase "changed client ID" { param($p) $p.resource_changes[0].change.after.body.properties.principalId = "different" } "principalId is missing or changed"
Invoke-PlanCase "changed tenant" { param($p) $p.resource_changes[0].change.after.body.properties.tenantId = "different" } "tenantId is missing or changed"
Invoke-PlanCase "wrong role" {
    param($p)
    $p.resource_changes[0].change.before.body.properties.role = "Admin"
    $p.resource_changes[0].change.after.body.properties.role = "Admin"
} "App/Ingestor"
Invoke-PlanCase "wrong principal type" {
    param($p)
    $p.resource_changes[0].change.before.body.properties.principalType = "User"
    $p.resource_changes[0].change.after.body.properties.principalType = "User"
} "App/Ingestor"
Invoke-PlanCase "unrefreshed body" { param($p) $p.resource_changes[0].change.before.body = $null } "refreshed grant body"
Invoke-PlanCase "incomplete plan" { param($p) $p.complete = $false } "complete, non-errored"
Invoke-PlanCase "errored plan" { param($p) $p.errored = $true } "complete, non-errored"
Invoke-PlanCase "future plan format" { param($p) $p.format_version = "2.0" } "format 1.x"
Write-Host "Passed 24 lifecycle recorder upgrade-plan checker cases."
