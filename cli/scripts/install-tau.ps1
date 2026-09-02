# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [string] $Version = "@TAU_VERSION@",
    [string] $Repository = "Azure/taugrid",
    [string] $InstallDir = (Join-Path $env:LOCALAPPDATA "TauGrid\bin")
)

$ErrorActionPreference = "Stop"

if (-not $IsWindows) {
    throw "tau installer: Windows is required. Use install.sh on Linux or macOS."
}

$semverPattern = "^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|([0-9]*[A-Za-z-][0-9A-Za-z-]*))(\.((0|[1-9][0-9]*)|([0-9]*[A-Za-z-][0-9A-Za-z-]*)))*)?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$"
if ($Version -notmatch $semverPattern) {
    throw "tau installer: invalid release version '$Version'."
}
if ($Repository -notmatch "^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$") {
    throw "tau installer: repository must use the owner/repository form."
}
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    throw "tau installer: InstallDir must not be empty."
}

$asset = "tau-windows-amd64.exe"
$releaseUrl = "https://github.com/$Repository/releases/download/$Version"
$temporaryDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("tau-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporaryDirectory | Out-Null

try {
    $assetPath = Join-Path $temporaryDirectory $asset
    $checksumsPath = Join-Path $temporaryDirectory "SHA256SUMS"
    $licensePath = Join-Path $temporaryDirectory "LICENSE"
    Invoke-WebRequest -Uri "$releaseUrl/$asset" -OutFile $assetPath
    Invoke-WebRequest -Uri "$releaseUrl/SHA256SUMS" -OutFile $checksumsPath
    Invoke-WebRequest -Uri "$releaseUrl/LICENSE" -OutFile $licensePath

    function Get-ExpectedChecksum([string] $FileName) {
        $checksumPattern = "^(?<hash>[A-Fa-f0-9]{64})\s+\*?(?<name>\S+)$"
        $matches = @(Get-Content -LiteralPath $checksumsPath | ForEach-Object {
            $match = [regex]::Match($_, $checksumPattern)
            if ($match.Success -and $match.Groups["name"].Value -eq $FileName) {
                $match
            }
        })
        if ($matches.Count -ne 1) {
            throw "tau installer: SHA256SUMS has no unique checksum for $FileName."
        }
        return $matches[0].Groups["hash"].Value.ToLowerInvariant()
    }

    foreach ($file in @($asset, "LICENSE")) {
        $expectedChecksum = Get-ExpectedChecksum $file
        $actualChecksum = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $temporaryDirectory $file)).Hash.ToLowerInvariant()
        if ($actualChecksum -ne $expectedChecksum) {
            throw "tau installer: checksum verification failed for $file."
        }
    }

    $installedVersion = (& $assetPath version --short | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $installedVersion -ne $Version) {
        throw "tau installer: binary reports '$installedVersion', expected '$Version'."
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -LiteralPath $assetPath -Destination (Join-Path $InstallDir "tau.exe") -Force
    Copy-Item -LiteralPath $licensePath -Destination (Join-Path $InstallDir "LICENSE") -Force

    Write-Output "Installed tau $Version to $(Join-Path $InstallDir 'tau.exe')"
    Write-Output "Installed the MIT license to $(Join-Path $InstallDir 'LICENSE')"
    Write-Output "Add $InstallDir to PATH for the current PowerShell session:"
    Write-Output "  `$env:PATH = '$InstallDir;' + `$env:PATH"
    Write-Output "To add it to your user PATH for future terminals:"
    Write-Output "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';$InstallDir', 'User')"
} finally {
    Remove-Item -LiteralPath $temporaryDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
