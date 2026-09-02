# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"

$scriptDirectory = Split-Path -Parent $PSCommandPath
$repositoryRoot = Resolve-Path (Join-Path $scriptDirectory "..\\..")
$temporaryDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("tau-installer-test-" + [Guid]::NewGuid().ToString("N"))
$releaseDirectory = Join-Path $temporaryDirectory "release"
$installDirectory = Join-Path $temporaryDirectory "install"
$badInstallDirectory = Join-Path $temporaryDirectory "bad-install"

New-Item -ItemType Directory -Path $releaseDirectory | Out-Null

function global:Invoke-WebRequest {
    param(
        [string] $Uri,
        [string] $OutFile
    )

    Copy-Item -LiteralPath (Join-Path $global:TauInstallerTestReleaseDirectory (Split-Path -Leaf $Uri)) -Destination $OutFile -Force
}

try {
    Push-Location (Join-Path $repositoryRoot "cli")
    try {
        $env:CGO_ENABLED = "0"
        go build -trimpath -mod=readonly -buildvcs=false `
            -ldflags "-X github.com/Azure/taugrid/core/version.Version=v1.2.3" `
            -o (Join-Path $releaseDirectory "tau-windows-amd64.exe") ./cmd/tau
        if ($LASTEXITCODE -ne 0) {
            throw "unable to build test tau executable."
        }
    } finally {
        Pop-Location
    }

    Copy-Item -LiteralPath (Join-Path $repositoryRoot "LICENSE") -Destination (Join-Path $releaseDirectory "LICENSE")
    $checksums = foreach ($file in @("tau-windows-amd64.exe", "LICENSE")) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $releaseDirectory $file)).Hash.ToLowerInvariant()
        "$hash  $file"
    }
    Set-Content -LiteralPath (Join-Path $releaseDirectory "SHA256SUMS") -Value ($checksums -join [Environment]::NewLine) -NoNewline

    $stampedInstaller = Join-Path $temporaryDirectory "install.ps1"
    (Get-Content -LiteralPath (Join-Path $scriptDirectory "install-tau.ps1") -Raw).Replace("@TAU_VERSION@", "v1.2.3") |
        Set-Content -LiteralPath $stampedInstaller -NoNewline
    $global:TauInstallerTestReleaseDirectory = $releaseDirectory

    & $stampedInstaller -InstallDir $installDirectory
    if (-not (Test-Path -LiteralPath (Join-Path $installDirectory "tau.exe"))) {
        throw "installer did not create tau.exe."
    }
    $installedVersion = (& (Join-Path $installDirectory "tau.exe") version --short | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $installedVersion -ne "v1.2.3") {
        throw "installed tau.exe did not report v1.2.3."
    }

    Set-Content -LiteralPath (Join-Path $releaseDirectory "SHA256SUMS") -Value ("{0}  tau-windows-amd64.exe" -f ("0" * 64)) -NoNewline
    $acceptedBadChecksum = $true
    try {
        & $stampedInstaller -InstallDir $badInstallDirectory 2>$null
    } catch {
        $acceptedBadChecksum = $false
    }
    if ($acceptedBadChecksum) {
        throw "installer accepted a mismatched checksum."
    }
    if (Test-Path -LiteralPath (Join-Path $badInstallDirectory "tau.exe")) {
        throw "installer copied tau.exe after checksum validation failed."
    }

    Copy-Item -LiteralPath $env:ComSpec -Destination (Join-Path $releaseDirectory "tau-windows-amd64.exe") -Force
    $mismatchChecksums = foreach ($file in @("tau-windows-amd64.exe", "LICENSE")) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $releaseDirectory $file)).Hash.ToLowerInvariant()
        "$hash  $file"
    }
    Set-Content -LiteralPath (Join-Path $releaseDirectory "SHA256SUMS") -Value ($mismatchChecksums -join [Environment]::NewLine) -NoNewline
    $acceptedBadVersion = $true
    try {
        & $stampedInstaller -InstallDir $badInstallDirectory 2>$null
    } catch {
        $acceptedBadVersion = $false
    }
    if ($acceptedBadVersion) {
        throw "installer accepted a binary with the wrong version."
    }

    Write-Output "PowerShell tau installer tests passed"
} finally {
    Remove-Item function:global:Invoke-WebRequest -ErrorAction SilentlyContinue
    Remove-Variable -Name TauInstallerTestReleaseDirectory -Scope Global -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $temporaryDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
