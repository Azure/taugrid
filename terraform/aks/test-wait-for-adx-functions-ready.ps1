# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$scriptPath = Join-Path $PSScriptRoot "Wait-ForAdxFunctionsReady.ps1"
$helmPath = (Get-Command helm -CommandType Application).Source
$cases = Get-Content -Raw (Join-Path $PSScriptRoot "adx-function-waiter-cases.json") | ConvertFrom-Json -AsHashtable
$failures = 0
$count = 0

foreach ($case in $cases) {
    if ($env:ADX_WAITER_CASE_FILTER -and -not $case.name.Contains($env:ADX_WAITER_CASE_FILTER)) { continue }
    $count++
    $release = if ($case["release"]) { $case["release"] } else { "adx-mon" }
    $namespace = if ($case["namespace"]) { $case["namespace"] } else { "adx-mon" }
    $values = if ($case["values"]) { $case["values"] | ConvertTo-Json -Depth 30 -Compress } else { "{}" }
    $global:waiterRender = $values | & $helmPath template $release "$PSScriptRoot/../../charts/adx-mon" --namespace $namespace --values "$PSScriptRoot/../../charts/adx-mon/values-ai-runtime.yaml" --values -
    if ($LASTEXITCODE -ne 0) { throw "Unable to render test chart." }
    $documents = $global:waiterRender | yq eval-all -o=json -I=0 '[.]' -
    if ($LASTEXITCODE -ne 0) { throw "Unable to parse test chart." }
    $global:waiterObjects = $documents | jq -c --arg release $release --arg namespace $namespace '
        [.[] | select(.kind == "Function") |
          .metadata += {uid: ("uid-" + .metadata.name), resourceVersion: "10", generation: 2,
            annotations: {"meta.helm.sh/release-name": $release, "meta.helm.sh/release-namespace": $namespace}} |
          .status = {observedGeneration: 2, status: "Success", error: ""}]'
    if ($LASTEXITCODE -ne 0) { throw "Unable to construct test objects." }
    $expectedCount = if ($case.ContainsKey("expectedCount")) { $case.expectedCount } else { 9 }
    if (@($global:waiterObjects | ConvertFrom-Json).Count -ne $expectedCount) { throw "Unexpected real chart Function count for $($case.name)." }
    if ($case["duplicateRender"]) {
        $global:waiterRender = @($global:waiterRender) + "---" + @($global:waiterObjects | jq '.[0]')
    }
    $global:waiterCase = $case
    $global:waiterRelease = $release
    $global:waiterNamespace = $namespace
    $global:waiterReads = 0
    $global:waiterUpgrades = 0
    $global:waiterDeletes = [System.Collections.Generic.List[object]]::new()
    $global:waiterMockErrors = [System.Collections.Generic.List[string]]::new()
    $global:waiterPhase = "preflight"
    $global:waiterLastObjects = "[]"
    $global:waiterTime = [datetime] "2026-01-01T00:00:00Z"
    $global:LASTEXITCODE = 0
    function global:az {
        $global:LASTEXITCODE = 0
        if ($global:waiterCase["failure"] -eq "az") { $global:LASTEXITCODE = 1; "mock az failure" }
    }
    function global:helm {
        $Arguments = @($args)
        $global:LASTEXITCODE = 0
        if ($Arguments[0] -eq "template") {
            if (($Arguments -join " ") -ne "template $global:waiterRelease chart --namespace $global:waiterNamespace --values base-values --values environment-values") {
                $global:waiterMockErrors.Add("Mismatched render arguments."); $global:LASTEXITCODE = 1; return
            }
            if ($global:waiterCase["failure"] -eq "render") { $global:LASTEXITCODE = 1; return "mock render failure" }
            if ($global:waiterCase.ContainsKey("render")) { return $global:waiterCase.render }
            return $global:waiterRender
        }
        if ($Arguments[0] -ne "upgrade") { throw "Unexpected helm command." }
        if (($Arguments -join " ") -ne "upgrade --install $global:waiterRelease chart --namespace $global:waiterNamespace --values base-values --values environment-values --reset-values --kubeconfig kubeconfig --create-namespace --wait --timeout 30m") {
            $global:waiterMockErrors.Add("Mismatched upgrade arguments."); $global:LASTEXITCODE = 1; return
        }
        $global:waiterUpgrades++
        $global:waiterPhase = "state"
        if ($global:waiterCase["failure"] -eq "upgrade") { $global:LASTEXITCODE = 1; return "mock upgrade failure" }
    }
    function global:Get-Date { return $global:waiterTime }
    function global:Start-Sleep {
        param([int] $Seconds)
        $global:waiterTime = $global:waiterTime.AddSeconds($Seconds)
        if ($Seconds -ge 60) { $global:waiterPhase = "preflight" }
    }
    function global:kubectl {
        $Arguments = @($args)
        $global:LASTEXITCODE = 0
        if ($Arguments[0] -eq "get") {
            if (($Arguments -join " ") -notlike "*--kubeconfig kubeconfig --request-timeout=30s*" -or
                $Arguments -notcontains "functions.adx-mon.azure.com") {
                $global:waiterMockErrors.Add("Unsafe read arguments."); $global:LASTEXITCODE = 1; return
            }
            if ($global:waiterPhase -eq "preflight") {
                if ($global:waiterCase["failure"] -eq "preflight") { $global:LASTEXITCODE = 1; return "mock preflight failure" }
                $filter = if ($global:waiterCase["preflight"]) { $global:waiterCase["preflight"] } else { "." }
                if ($global:waiterUpgrades -gt 0 -and $global:waiterCase["preflightAfterRetry"]) { $filter = $global:waiterCase.preflightAfterRetry }
            } else {
                $index = [Math]::Min($global:waiterReads, $global:waiterCase.responses.Count - 1)
                $global:waiterReads++
                if ($global:waiterCase["failure"] -eq "read") { $global:LASTEXITCODE = 1; return "mock read failure" }
                if ($global:waiterCase.ContainsKey("rawResponse")) { return $global:waiterCase.rawResponse }
                $filter = $global:waiterCase.responses[$index]
            }
            $response = $global:waiterObjects | jq -c $filter
            if ($LASTEXITCODE -ne 0) { throw "Invalid test filter." }
            $global:waiterLastObjects = $response
            $listFilter = if ($global:waiterPhase -eq "state" -and $global:waiterCase["listFilter"]) { $global:waiterCase.listFilter } else { "." }
            return $response | jq -c '{apiVersion:"adx-mon.azure.com/v1",kind:"FunctionList",metadata:{},items:.}' | jq -c $listFilter
        }
        if ($Arguments[0] -eq "delete") {
            $json = $input | Out-String
            $body = $json | ConvertFrom-Json
            $global:waiterDeletes.Add(@{command = $Arguments -join " "; body = $body})
            $target = @($global:waiterLastObjects | ConvertFrom-Json | Where-Object {
                "/apis/adx-mon.azure.com/v1/namespaces/$($_.metadata.namespace)/functions/$($_.metadata.name)" -eq $Arguments[2]
            })
            if (($Arguments -join " ") -notlike "delete --raw * --filename - --kubeconfig kubeconfig --request-timeout=30s" -or
                $target.Count -ne 1 -or $body.kind -ne "DeleteOptions" -or $body.apiVersion -ne "v1" -or
                $body.preconditions.uid -ne $target[0].metadata.uid -or $body.preconditions.resourceVersion -ne $target[0].metadata.resourceVersion -or
                $target[0].status.status -ne "PermanentFailure") {
                $global:waiterMockErrors.Add("Invalid conditional-delete target/body."); $global:LASTEXITCODE = 1; return
            }
            switch ($global:waiterCase["failure"]) {
                "delete" { $global:LASTEXITCODE = 1; return "mock delete failure" }
                "conflict" { $global:LASTEXITCODE = 1; return "Error from server (Conflict): precondition failed" }
                "notfound" { $global:LASTEXITCODE = 1; return "Error from server (NotFound): Function disappeared" }
            }
            return
        }
        throw "Unexpected kubectl command."
    }
    try {
        $result = 0
        $output = @()
        try {
            $allowNoFunctions = [bool] $case["allowNoFunctions"]
            $output = @(& $scriptPath -SubscriptionId subscription -ResourceGroup resource-group -ClusterName cluster -Kubeconfig kubeconfig -ChartPath chart -BaseValuesFile base-values -EnvironmentValuesFile environment-values -ReleaseName $release -ReleaseNamespace $namespace -AllowNoFunctions:$allowNoFunctions -MaximumAttempts 6 -FunctionWaitSeconds 60 *>&1)
        } catch {
            $result = 1
            $output += $_.Exception.Message
        }
        $actual = "exit=$result reads=$global:waiterReads upgrades=$global:waiterUpgrades deletes=$($global:waiterDeletes.Count)"
        if ($result -ne $case.exit -or $global:waiterReads -ne $case.reads -or
            $global:waiterUpgrades -ne $case.upgrades -or $global:waiterDeletes.Count -ne $case.deletes -or
            -not ($output -join "`n").Contains($case.diagnostic) -or $global:waiterMockErrors.Count -gt 0) {
            Write-Host "FAIL PowerShell: $($case.name): $actual`n$($output -join "`n")`n$($global:waiterMockErrors -join "`n")"
            $failures++
        } else {
            Write-Host "PASS PowerShell: $($case.name): $actual"
        }
    } finally {
        foreach ($command in @("az", "helm", "kubectl", "Get-Date", "Start-Sleep")) {
            Remove-Item "Function:global:$command"
        }
        Remove-Variable waiterRender, waiterObjects, waiterCase, waiterReads, waiterUpgrades, waiterDeletes, waiterTime, waiterPhase, waiterLastObjects, waiterMockErrors, waiterRelease, waiterNamespace -Scope Global
    }
}
Write-Host "PowerShell: $count cases, $failures failures."
if ($count -eq 0 -or $failures -ne 0) { exit 1 }
python3 "$PSScriptRoot/test-adx-function-delete.py"
if ($LASTEXITCODE -ne 0) { throw "kubectl conditional-delete transport tests failed." }
