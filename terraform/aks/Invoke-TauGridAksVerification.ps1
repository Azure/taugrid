# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Maintainer-only integration verification. This script provisions billable
# Azure resources and requires an Azure subscription with sufficient quota.

[CmdletBinding()]
param(
    [ValidatePattern("^[a-z0-9-]+$")]
    [string] $ResourceNamePrefix = "taugrid-verify",
    [ValidatePattern("^[a-z0-9-]*$")]
    [string] $RunId = "",
    [ValidatePattern("^[a-z0-9]+$")]
    [string] $Location = "westeurope",
    [ValidatePattern("^[A-Za-z0-9_.-]+$")]
    [string] $AdxSkuName = "Standard_D11_v2",
    [ValidateRange(1, 1000)]
    [int] $AdxSkuCapacity = 2,
    [ValidatePattern("^[A-Za-z0-9_.-]+$")]
    [string] $GpuVmSize = "Standard_NC40ads_H100_v5",
    [ValidateRange(1, 8)]
    [int] $GpuCountPerNode = 1,
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $GpuMonitoringSkuName = "h100-nvl-1g",
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $GpuFlavorName = "taugrid-h100",
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $GpuClass = "h100-95gb",
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $GpuSeries = "nc-h100-v5",
    [bool] $NormalizeGpuMig = $false,
    [string] $TauCommand = "tau",
    [switch] $EnableBootstrapWorkspace,
    [string] $BootstrapWorkspaceEntraGroupObjectId = "",
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $BootstrapWorkspaceName = "taugrid-default",
    [ValidatePattern("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
    [string] $WorkspaceNamespace = "taugrid-default",
    [ValidatePattern("^[0-9]+\.[0-9]+\.[0-9]+$")]
    [string] $TauGridVersion = "0.4.0",
    [ValidateRange(10, 60)]
    [int] $PortalWaitMinutes = 30
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

class TerminalWaitError : System.Exception {
    TerminalWaitError([string] $Message) : base($Message) {}
}

function Invoke-Native {
    param([string] $FilePath, [string[]] $Arguments = @())
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Command failed with exit code ${LASTEXITCODE}: $FilePath $($Arguments -join ' ')" }
}

function Wait-ForCondition {
    param([scriptblock] $Condition, [string] $Description, [datetime] $Deadline)
    $lastError = ""
    while ((Get-Date) -lt $Deadline) {
        try {
            $result = & $Condition
            if ($result) { return $result }
        } catch [TerminalWaitError] { throw } catch { $lastError = $_.Exception.Message }
        Start-Sleep -Seconds 15
    }
    throw "Timed out waiting for $Description. Last error: $lastError"
}

function Get-AvailableLoopbackPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return ([System.Net.IPEndPoint] $listener.LocalEndpoint).Port } finally { $listener.Stop() }
}

function Get-PortForwardExitDetails {
    param([System.Diagnostics.Process] $Process, [string] $ErrorLog)
    $details = if (Test-Path -LiteralPath $ErrorLog) { (Get-Content -LiteralPath $ErrorLog -Raw).Trim() } else { "" }
    if ([string]::IsNullOrWhiteSpace($details)) { $details = "No stderr was captured." }
    return "kubectl port-forward exited with code $($Process.ExitCode): $details"
}

function Start-PortalPortForward {
    param([string] $GeneratedPath, [string] $RunIdentifier, [int] $Attempt)
    $port = Get-AvailableLoopbackPort
    $stdout = Join-Path $GeneratedPath "kubectl-port-forward-$RunIdentifier-$Attempt.stdout.log"
    $stderr = Join-Path $GeneratedPath "kubectl-port-forward-$RunIdentifier-$Attempt.stderr.log"
    $arguments = @{ FilePath = "kubectl"; ArgumentList = @("-n", "tau-system", "port-forward", "service/tau-portal", "${port}:80"); PassThru = $true; RedirectStandardOutput = $stdout; RedirectStandardError = $stderr }
    if ($IsWindows) { $arguments["WindowStyle"] = "Hidden" }
    $process = Start-Process @arguments
    return [pscustomobject]@{
        Process = $process
        BaseUri = "http://127.0.0.1:$port"
        ErrorLog = $stderr
    }
}

function Ensure-PortalPortForward {
    param(
        [ref] $PortForward,
        [ref] $Attempt,
        [System.Collections.Generic.List[string]] $Diagnostics,
        [string] $GeneratedPath,
        [string] $RunIdentifier,
        [int] $MaximumRestarts = 5
    )
    if ($null -eq $PortForward.Value) {
        $PortForward.Value = Start-PortalPortForward -GeneratedPath $GeneratedPath -RunIdentifier $RunIdentifier -Attempt $Attempt.Value
        return
    }
    $process = $PortForward.Value.Process
    if (-not $process.HasExited) { return }

    $Diagnostics.Add((Get-PortForwardExitDetails -Process $process -ErrorLog $PortForward.Value.ErrorLog))
    if ($Attempt.Value -ge $MaximumRestarts) {
        throw [TerminalWaitError]::new("kubectl port-forward exited repeatedly. Diagnostics: $($Diagnostics -join [Environment]::NewLine)")
    }
    $Attempt.Value++
    $PortForward.Value = Start-PortalPortForward -GeneratedPath $GeneratedPath -RunIdentifier $RunIdentifier -Attempt $Attempt.Value
}

function Invoke-TerraformApplyWithAdxRecovery {
    param([string] $ModulePath, [string] $PlanFile, [string] $StateFile, [string] $VarsFile, [string] $ResourceGroup)

    & terraform "-chdir=$ModulePath" apply "-state=$StateFile" $PlanFile
    if ($LASTEXITCODE -eq 0) { return }

    $stateEntries = @(terraform "-chdir=$ModulePath" state list "-state=$StateFile")
    if ($LASTEXITCODE -ne 0 -or "azurerm_kusto_cluster.this[0]" -in $stateEntries) {
        throw "Terraform apply failed and there is no recoverable untracked ADX cluster."
    }
    $clusterIDs = @(az resource list --resource-group $ResourceGroup --resource-type "Microsoft.Kusto/clusters" --query "[].id" --output tsv)
    if ($LASTEXITCODE -ne 0 -or $clusterIDs.Count -ne 1) {
        throw "Terraform apply failed and did not leave exactly one recoverable ADX cluster in resource group '$ResourceGroup'."
    }
    $clusterID = $clusterIDs[0]
    $deadline = (Get-Date).AddMinutes(20)
    Wait-ForCondition {
        $provisioningState = az resource show --ids $clusterID --api-version "2024-04-13" --query "properties.provisioningState" --output tsv
        if ($LASTEXITCODE -ne 0) { throw "Unable to inspect ADX cluster '$clusterID'." }
        if ($provisioningState -eq "Failed" -or $provisioningState -eq "Canceled") { throw [TerminalWaitError]::new("ADX cluster '$clusterID' reached terminal state '$provisioningState'.") }
        $provisioningState -eq "Succeeded"
    } "ADX cluster provisioning after Terraform apply" $deadline
    Invoke-Native terraform @("-chdir=$ModulePath", "import", "-state=$StateFile", "-var-file=$VarsFile", "azurerm_kusto_cluster.this[0]", $clusterID)
    Invoke-Native terraform @("-chdir=$ModulePath", "apply", "-auto-approve", "-state=$StateFile", "-var-file=$VarsFile")
}

foreach ($name in @("az", "terraform", "kubectl", "helm", "pwsh")) {
    if ($null -eq (Get-Command $name -ErrorAction SilentlyContinue)) { throw "Required command '$name' was not found on PATH." }
}
$tau = Get-Command $TauCommand -ErrorAction SilentlyContinue
if ($null -eq $tau) { throw "Tau command '$TauCommand' was not found. Install the OS-specific tau release or pass -TauCommand." }
$expectedTauVersion = "v$TauGridVersion"
$tauVersion = (@(& $tau.Source version --short) -join [Environment]::NewLine).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Tau command '$($tau.Source)' could not report its version. Install TauGrid CLI $expectedTauVersion or pass a matching -TauCommand."
}
if ($tauVersion -ne $expectedTauVersion) {
    throw "Tau command '$($tau.Source)' is version '$tauVersion', but this deployment requires $expectedTauVersion to match TauGrid chart $TauGridVersion. Update Tau before provisioning Azure resources."
}
if ($EnableBootstrapWorkspace) {
    if ([string]::IsNullOrWhiteSpace($BootstrapWorkspaceEntraGroupObjectId)) {
        throw "BootstrapWorkspaceEntraGroupObjectId is required when EnableBootstrapWorkspace is set."
    }
    $bootstrapGroupId = [guid]::Empty
    if (-not [guid]::TryParse($BootstrapWorkspaceEntraGroupObjectId, [ref] $bootstrapGroupId)) {
        throw "BootstrapWorkspaceEntraGroupObjectId must be an Entra group object ID UUID."
    }
}
$tauDirectory = Split-Path -Parent $tau.Source
if ([string]::IsNullOrWhiteSpace($tauDirectory)) {
    throw "Tau command '$TauCommand' did not resolve to an executable file."
}

$modulePath = $PSScriptRoot
$run = if ($RunId) { $RunId } else { "{0}-{1:x4}" -f (Get-Date -Format "yyyyMMdd"), (Get-Random -Minimum 0 -Maximum 65536) }
if ($run.Length -gt 13) { throw "RunId must be at most 13 characters." }
$resourceGroup = "$ResourceNamePrefix-$run"
if ($resourceGroup.Length -gt 54) { throw "The generated AKS name '$resourceGroup' exceeds 54 characters." }

$generated = Join-Path $modulePath "generated"
$deployment = Join-Path $generated "deployments"
$state = Join-Path $generated "state"
$plans = Join-Path $generated "plans"
$data = Join-Path $generated "terraform-data-$run"
New-Item -ItemType Directory -Force -Path $deployment, $state, $plans, $data | Out-Null
$varsFile = Join-Path $deployment "$run.tfvars"
$stateFile = Join-Path $state "$run.tfstate"
$planFile = Join-Path $plans "$run.tfplan"
$kubeconfig = Join-Path $generated "kubeconfig-$run"

$account = az account show --output json | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace($account.id) -or [string]::IsNullOrWhiteSpace($account.tenantId)) { throw "Run az login and az account set first." }
$adxCatalog = az rest --method get --url "https://management.azure.com/subscriptions/$($account.id)/providers/Microsoft.Kusto/skus?api-version=2024-04-13" --output json | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw "Unable to list ADX SKUs for location '$Location'." }
$availableAdxSkus = @($adxCatalog.value | Where-Object { $_.resourceType -eq "clusters" -and $_.locations -contains $Location } | ForEach-Object { $_.name })
if ($AdxSkuName -notin $availableAdxSkus) { throw "ADX SKU '$AdxSkuName' is not available in location '$Location'. Choose one reported by the Azure Resource Manager SKU catalog." }
$bootstrapWorkspaceTfvars = ""
if ($EnableBootstrapWorkspace) {
    $bootstrapWorkspaceTfvars = @"
bootstrap_workspace = {
  name                  = "$BootstrapWorkspaceName"
  entra_group_object_id = "$BootstrapWorkspaceEntraGroupObjectId"
}
"@
}
$tfvars = @"
subscription_id     = "$($account.id)"
tenant_id           = "$($account.tenantId)"
location            = "$Location"
adx_sku_name        = "$AdxSkuName"
adx_sku_capacity    = $AdxSkuCapacity
resource_group_name = "$resourceGroup"
cluster_name        = "$resourceGroup"
command_interpreter = ["pwsh", "-NoProfile", "-NonInteractive", "-Command"]
gpu_vm_size             = "$GpuVmSize"
gpu_node_count          = 1
gpu_count_per_node      = $GpuCountPerNode
gpu_monitoring_sku_name = "$GpuMonitoringSkuName"
gpu_flavor_name         = "$GpuFlavorName"
gpu_class               = "$GpuClass"
gpu_series              = "$GpuSeries"
gpu_stack_mode          = "self_managed"
normalize_gpu_mig       = $($NormalizeGpuMig.ToString().ToLowerInvariant())
enable_adx                = true
enable_lifecycle_recorder = true
workspace_namespace       = "$WorkspaceNamespace"
taugrid_version           = "$TauGridVersion"
$bootstrapWorkspaceTfvars
"@
Set-Content -LiteralPath $varsFile -Value $tfvars -Encoding utf8NoBOM

$previousData = $env:TF_DATA_DIR
$previousKubeconfig = $env:KUBECONFIG
$previousPath = $env:PATH
$env:TF_DATA_DIR = $data
$env:KUBECONFIG = $kubeconfig
$env:PATH = "$tauDirectory$([IO.Path]::PathSeparator)$previousPath"
$portForward = $null
$portForwardAttempt = 0
$portForwardDiagnostics = [System.Collections.Generic.List[string]]::new()
try {
    Invoke-Native terraform @("-chdir=$modulePath", "init", "-backend=false")
    Invoke-Native terraform @("-chdir=$modulePath", "plan", "-state=$stateFile", "-var-file=$varsFile", "-out=$planFile")
    Invoke-TerraformApplyWithAdxRecovery -ModulePath $modulePath -PlanFile $planFile -StateFile $stateFile -VarsFile $varsFile -ResourceGroup $resourceGroup
    Invoke-Native az @("aks", "get-credentials", "--admin", "--subscription", $account.id, "--resource-group", $resourceGroup, "--name", $resourceGroup, "--file", $kubeconfig, "--overwrite-existing")
    Invoke-Native $tau.Source @("cluster", "validate", "installation", "--timeout", "10m")
    Invoke-Native kubectl @("-n", "tau-system", "rollout", "status", "deployment/tau-portal", "--timeout=15m")

    $deadline = (Get-Date).AddMinutes($PortalWaitMinutes)
    Ensure-PortalPortForward -PortForward ([ref] $portForward) -Attempt ([ref] $portForwardAttempt) -Diagnostics $portForwardDiagnostics -GeneratedPath $generated -RunIdentifier $run
    Wait-ForCondition {
        Ensure-PortalPortForward -PortForward ([ref] $portForward) -Attempt ([ref] $portForwardAttempt) -Diagnostics $portForwardDiagnostics -GeneratedPath $generated -RunIdentifier $run
        (Invoke-WebRequest -Uri "$($portForward.BaseUri)/healthz" -TimeoutSec 10).StatusCode -eq 200
    } "Portal health endpoint" $deadline
    $nodes = Wait-ForCondition {
        Ensure-PortalPortForward -PortForward ([ref] $portForward) -Attempt ([ref] $portForwardAttempt) -Diagnostics $portForwardDiagnostics -GeneratedPath $generated -RunIdentifier $run
        Invoke-RestMethod -Uri "$($portForward.BaseUri)/api/portal/nodes" -TimeoutSec 30
    } "Portal Nodes API" $deadline
    if ([int]$nodes.totalGPUs -lt 1) { throw "Portal Nodes API did not report a GPU node." }
    Wait-ForCondition {
        Ensure-PortalPortForward -PortForward ([ref] $portForward) -Attempt ([ref] $portForwardAttempt) -Diagnostics $portForwardDiagnostics -GeneratedPath $generated -RunIdentifier $run
        $cluster = Invoke-RestMethod -Uri "$($portForward.BaseUri)/api/portal/cluster?window=30m" -TimeoutSec 30
        $cluster.state -eq "ready" -and [int]$cluster.totalGPUs -ge 1
    } "Portal ADX GPU telemetry query" $deadline
    if ($EnableBootstrapWorkspace) {
        Invoke-Native kubectl @("-n", "tau-system", "wait", "--for=jsonpath={.status.phase}=Ready", "workspace.tau.azure.com/$BootstrapWorkspaceName", "--timeout=10m")
        Wait-ForCondition {
            $tauCluster = kubectl get clusters.tau.azure.com cluster --output=json | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0) { throw "Unable to read TauCluster workload profile status." }
            $workloadProfile = @($tauCluster.status.workloadProfiles.profiles | Where-Object { $_.name -eq "azure.research.training.l" }) | Select-Object -First 1
            if ($null -eq $workloadProfile) { return $false }
            $readyCondition = @($workloadProfile.conditions | Where-Object { $_.type -eq "Ready" }) | Select-Object -First 1
            $null -ne $readyCondition -and $readyCondition.status -eq "True"
        } "azure.research.training.l workload profile readiness" $deadline
        $smokeContext = kubectl config current-context
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($smokeContext)) { throw "Unable to determine the administrator kubeconfig context for the smoke run." }
        $smokeName = "taugrid-verify-$run"
        $smokeDirectory = Join-Path $generated "smoke-$run"
        $smokeConfig = Join-Path $smokeDirectory "tau.yaml"
        $smokeEntrypoint = Join-Path $smokeDirectory "smoke.py"
        New-Item -ItemType Directory -Force -Path $smokeDirectory | Out-Null
        Set-Content -LiteralPath $smokeEntrypoint -Value "import ray`nray.init()`nprint('TauGrid ADX lifecycle verification')" -Encoding utf8NoBOM
        $smokeConfigContents = @"
schema_version: 1
name: $smokeName
engine: rayjob
run:
  entrypoint: smoke.py
compute:
  workers: 1
  gpus_per_worker: 1
  head_cpu_request: "1"
  head_memory_request: "2Gi"
  head_cpu_limit: "1"
  head_memory_limit: "2Gi"
  worker_cpu_request: "1"
  worker_memory_request: "2Gi"
  worker_cpu_limit: "1"
  worker_memory_limit: "2Gi"
policy:
  profile: azure.research.training.l
  team: research
  lane: training
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
"@
        Set-Content -LiteralPath $smokeConfig -Value $smokeConfigContents -Encoding utf8NoBOM
        Invoke-Native $tau.Source @("run", "--config", $smokeConfig, "--workspace", $BootstrapWorkspaceName, "--namespace", $WorkspaceNamespace, "--context", $smokeContext, "--system-namespace", "tau-system")
        $smokeResourceUID = Wait-ForCondition {
            $resourceUID = kubectl -n $WorkspaceNamespace get rayjob $smokeName --output=jsonpath='{.metadata.uid}'
            if ($LASTEXITCODE -ne 0) { throw "Unable to read smoke RayJob '$smokeName'." }
            if ([string]::IsNullOrWhiteSpace($resourceUID)) { return $false }
            $resourceUID
        } "smoke RayJob creation" $deadline
        Wait-ForCondition {
            $rayJob = kubectl -n $WorkspaceNamespace get rayjob $smokeName --output=json | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0) { throw "Unable to read smoke RayJob '$smokeName' status." }
            $jobStatus = "$($rayJob.status.jobStatus)"
            $deploymentStatus = "$($rayJob.status.jobDeploymentStatus)"
            if ($jobStatus -in @("FAILED", "CANCELED") -or $deploymentStatus -eq "Failed") {
                throw [TerminalWaitError]::new("Smoke RayJob '$smokeName' reached a terminal failure (jobStatus='$jobStatus', jobDeploymentStatus='$deploymentStatus').")
            }
            $jobStatus -eq "SUCCEEDED"
        } "smoke RayJob completion" $deadline
        Wait-ForCondition {
            Ensure-PortalPortForward -PortForward ([ref] $portForward) -Attempt ([ref] $portForwardAttempt) -Diagnostics $portForwardDiagnostics -GeneratedPath $generated -RunIdentifier $run
            $history = Invoke-RestMethod -Uri "$($portForward.BaseUri)/api/portal/ray/history/${smokeResourceUID}?workspace=$BootstrapWorkspaceName" -TimeoutSec 30
            $history.state -eq "ready" -and @($history.events | Where-Object { $_.resourceUid -eq $smokeResourceUID }).Count -ge 1
        } "Portal lifecycle history from ADX" $deadline
    }
    Write-Host "TauGrid deployment and Portal ADX verification succeeded."
    Write-Host "Destroy: `$env:TF_DATA_DIR = '$data'; terraform -chdir='$modulePath' init -backend=false; terraform -chdir='$modulePath' destroy -state='$stateFile' -var-file='$varsFile'"
} finally {
    if ($null -ne $portForward -and -not $portForward.Process.HasExited) { Stop-Process -Id $portForward.Process.Id -Force }
    $env:TF_DATA_DIR = $previousData
    $env:KUBECONFIG = $previousKubeconfig
    $env:PATH = $previousPath
}
