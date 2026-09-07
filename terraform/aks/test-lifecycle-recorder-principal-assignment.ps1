# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$mainPath = Join-Path $PSScriptRoot "main.tf"
$contents = Get-Content -LiteralPath $mainPath -Raw

$resourcePattern = '(?s)resource\s+"azapi_resource"\s+"lifecycle_recorder_principal_assignment"\s*\{(.*?)\n\}'
$match = [regex]::Match($contents, $resourcePattern)
if (-not $match.Success) {
    throw "Lifecycle recorder principal assignment must be managed by an azapi_resource."
}

$resource = $match.Groups[1].Value
$requiredPatterns = @(
    'type\s*=\s*"Microsoft\.Kusto/clusters/databases/principalAssignments@2024-04-13"',
    'parent_id\s*=\s*azurerm_kusto_database\.this\["Metrics"\]\.id',
    'principalId\s*=\s*azurerm_user_assigned_identity\.lifecycle_recorder\[0\]\.client_id',
    'principalType\s*=\s*"App"',
    'role\s*=\s*"Ingestor"',
    'tenantId\s*=\s*azurerm_user_assigned_identity\.lifecycle_recorder\[0\]\.tenant_id',
    'error_message_regex\s*=\s*\["\(\?i\)AAD principal was not found"\]',
    'interval_seconds\s*=\s*10',
    'max_interval_seconds\s*=\s*180',
    'create\s*=\s*"60m"'
)

foreach ($pattern in $requiredPatterns) {
    if (-not [regex]::IsMatch($resource, $pattern)) {
        throw "Lifecycle recorder principal assignment is missing required setting: $pattern"
    }
}

if ([regex]::IsMatch($contents, 'azurerm_kusto_database_principal_assignment"\s+"lifecycle_recorder"')) {
    throw "Lifecycle recorder principal assignment must not use the non-retrying AzureRM resource."
}
