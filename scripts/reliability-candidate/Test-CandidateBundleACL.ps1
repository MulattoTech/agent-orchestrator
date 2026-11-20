#Requires -Version 7.4
# In-memory bundle and descriptors only. No candidate files, filesystem ACLs or credentials are used.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Candidate.Common.ps1')

$script:fixtureRoot = 'C:\AO-MTechLabs-Releases-20261116\ao-unofficial-20261119T000000Z-0123456789ab'
$script:fixtureHash = 'a' * 64
$script:fixtureDirectories = @('C:\', 'C:\AO-MTechLabs-Releases-20261116', $script:fixtureRoot)
$script:fixtureFiles = [ordered]@{}
foreach ($name in @('AO.exe', 'ao-offline-worker.exe', 'ao-offline-fleet.exe', 'ao-offline-conpty.exe',
    'ao-offline-daemon.exe', 'Candidate.Common.ps1', 'Run-Offline-Fleet.ps1', 'Run-Offline-ConPTY.ps1',
    'Run-Offline-Daemon.ps1', 'Start-PostRefresh-Validation.ps1', 'Stop-Candidate.ps1', 'policy.template.json', 'extra-payload.txt')) {
    $script:fixtureFiles[$name] = @{ sha256 = $script:fixtureHash; bytes = 1 }
}
$script:fixtureManifest = @{
    schemaVersion = 1; sourceCommit = '0' * 40; strictPolicy = $true; unsigned = $true
    offlineCompanion = @{ allowlistEnforced = $true }
    offlineConPTY = @{ allowlistEnforced = $true }
    offlineDaemon = @{ allowlistEnforced = $true; sourcePinned = '0' * 40 }
    files = $script:fixtureFiles
} | ConvertTo-Json -Depth 5
$script:fixturePaths = @('manifest.json', 'manifest.sha256') + @($script:fixtureFiles.Keys) |
    ForEach-Object { Join-Path $script:fixtureRoot $_ }
$script:aclOverrides = @{}
$script:reparsePaths = @()
$script:events = [Collections.Generic.List[string]]::new()
$script:failures = [Collections.Generic.List[string]]::new()

function Test-Path {
    [CmdletBinding()]
    param([string]$LiteralPath, [string]$PathType = 'Any')
    if ($PathType -eq 'Leaf') { return $LiteralPath -in $script:fixturePaths }
    if ($PathType -eq 'Container') { return $LiteralPath -in $script:fixtureDirectories }
    return $LiteralPath -in $script:fixturePaths -or $LiteralPath -in $script:fixtureDirectories
}
function Get-Item {
    [CmdletBinding()]
    param([string]$LiteralPath, [switch]$Force)
    if (-not (Test-Path -LiteralPath $LiteralPath)) { throw 'Unexpected synthetic path.' }
    $attributes = if ($LiteralPath -in $script:reparsePaths) { [IO.FileAttributes]::ReparsePoint } else { [IO.FileAttributes]::Normal }
    return [pscustomobject]@{ Attributes = $attributes; Length = 1 }
}
function Get-Acl {
    [CmdletBinding()]
    param([string]$LiteralPath)
    if (-not (Test-Path -LiteralPath $LiteralPath)) { throw 'Unexpected synthetic ACL path.' }
    $script:events.Add("ACL:$LiteralPath")
    $sddl = 'O:SYG:SYD:P(A;;FA;;;SY)'
    if ($script:aclOverrides.ContainsKey($LiteralPath)) { $sddl = $script:aclOverrides[$LiteralPath] }
    $descriptor = [Security.AccessControl.RawSecurityDescriptor]::new($sddl)
    $bytes = [byte[]]::new($descriptor.BinaryLength)
    $descriptor.GetBinaryForm($bytes, 0)
    $fake = [pscustomobject]@{ DescriptorBytes = $bytes }
    $fake | Add-Member -MemberType ScriptMethod -Name GetSecurityDescriptorBinaryForm -Value { return $this.DescriptorBytes }
    return $fake
}
function Get-Content {
    [CmdletBinding()]
    param([string]$LiteralPath, [switch]$Raw)
    $script:events.Add("READ:$LiteralPath")
    switch ([IO.Path]::GetFileName($LiteralPath)) {
        'manifest.json' { return $script:fixtureManifest }
        'manifest.sha256' { return $script:fixtureHash }
        default { throw 'Unexpected synthetic content read.' }
    }
}
function Get-FileHash {
    [CmdletBinding()]
    param([string]$LiteralPath, [string]$Algorithm)
    if ($LiteralPath -notin $script:fixturePaths -or $Algorithm -cne 'SHA256') { throw 'Unexpected synthetic hash request.' }
    $script:events.Add("HASH:$LiteralPath")
    return [pscustomobject]@{ Hash = $script:fixtureHash }
}

function Test-BundleACL {
    param([string]$Label, [string]$Target, [string]$Sddl, [string]$Expected, [switch]$Reparse)
    $script:aclOverrides = @{}
    $script:reparsePaths = @()
    $script:events.Clear()
    if ($Reparse) { $script:reparsePaths = @($Target) }
    elseif ($Target) { $script:aclOverrides[$Target] = $Sddl }
    $observed = 'PASS'
    try { $null = Assert-CandidateBundle $script:fixtureRoot }
    catch {
        if ($_.Exception.Message -match '^(AO_ACL_[A-Z_]+):') { $observed = $Matches[1] }
        elseif ($_.Exception.Message.StartsWith('Reparse-point path refused:')) { $observed = 'REPARSE_POINT' }
        else { throw "Unexpected error in ${Label}: $($_.Exception.Message)" }
    }
    if ($observed -cne $Expected) {
        $script:failures.Add("${Label}: expected $Expected, observed $observed")
        return
    }
    if ($Expected -ceq 'PASS') {
        foreach ($path in $script:fixturePaths) {
            $aclAt = $script:events.IndexOf("ACL:$path")
            $readAt = $script:events.IndexOf("READ:$path")
            $hashAt = $script:events.IndexOf("HASH:$path")
            if ($aclAt -lt 0 -or ($readAt -ge 0 -and $aclAt -gt $readAt) -or ($hashAt -ge 0 -and $aclAt -gt $hashAt)) {
                $script:failures.Add("${Label}: file was not ACL-verified before reading or hashing: $path")
                return
            }
        }
    } elseif ($script:events.Contains("READ:$Target") -or $script:events.Contains("HASH:$Target")) {
        $script:failures.Add("${Label}: rejected file was read or hashed before its ACL rejection")
        return
    }
    if ($Target -in $script:fixtureDirectories -and $Expected -cne 'PASS' -and
        @($script:events | Where-Object { $_ -like 'READ:*' -or $_ -like 'HASH:*' }).Count -ne 0) {
        $script:failures.Add("${Label}: bundle was read before its ancestry rejection")
        return
    }
    Write-Output "PASS: $Label => $observed"
}

$unsafeWrite = 'O:SYG:SYD:P(A;;0x2;;;WD)(A;;FA;;;SY)'
foreach ($path in $script:fixturePaths) {
    Test-BundleACL "writable $([IO.Path]::GetFileName($path)) rejected" $path $unsafeWrite 'AO_ACL_UNTRUSTED_FILE_MUTATION_ALLOW'
}
Test-BundleACL 'safe bundle' '' '' 'PASS'
Test-BundleACL 'readable executable remains valid' (Join-Path $script:fixtureRoot 'AO.exe') 'O:SYG:SYD:P(A;;FR;;;WD)(A;;FA;;;SY)' 'PASS'
foreach ($path in $script:fixtureDirectories) {
    Test-BundleACL "unsafe ancestry $path" $path 'O:SYG:SYD:P(A;;FA;;;WD)(A;;FA;;;SY)' 'AO_ACL_UNTRUSTED_ANCESTOR_MUTATION_ALLOW'
}
Test-BundleACL 'directory create-only rights remain valid' $script:fixtureDirectories[1] 'O:SYG:SYD:P(A;;0x6;;;WD)(A;;FA;;;SY)' 'PASS'
foreach ($name in @('manifest.json', 'manifest.sha256', 'AO.exe')) {
    Test-BundleACL "reparse $name" (Join-Path $script:fixtureRoot $name) '' 'REPARSE_POINT' -Reparse
}
if ($script:failures.Count -ne 0) { throw ($script:failures -join [Environment]::NewLine) }
Write-Output 'PASS: shared bundle verification rejects unsafe ancestry and every mutable bundle file before reading it.'
