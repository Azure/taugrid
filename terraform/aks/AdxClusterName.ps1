# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

function Get-TauGridAdxClusterName {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string] $SubscriptionId,
        [Parameter(Mandatory)]
        [string] $ResourceGroupName,
        [Parameter(Mandatory)]
        [string] $ClusterName
    )

    $input = "$SubscriptionId`:$ResourceGroupName`:$ClusterName"
    $hashBytes = [System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($input))
    $hash = [System.Convert]::ToHexString($hashBytes).ToLowerInvariant()
    return "taugrid$($hash.Substring(0, 13))"
}
