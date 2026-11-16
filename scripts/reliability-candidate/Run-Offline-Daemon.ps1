#Requires -Version 7.4
[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($args.Count -ne 0) { throw 'This runner accepts no arguments.' }
if (-not $IsWindows) { throw 'The isolated daemon fixture requires Windows.' }
$bootstrap = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'manifest.json') -Raw | ConvertFrom-Json -AsHashtable
$common = Join-Path $PSScriptRoot 'Candidate.Common.ps1'
if ((Get-FileHash -LiteralPath $common -Algorithm SHA256).Hash.ToLowerInvariant() -cne $bootstrap.files['Candidate.Common.ps1'].sha256) {
    throw 'Candidate helper script digest mismatch.'
}
. $common
$bundle = Assert-CandidateBundle $PSScriptRoot
$daemonExe = Join-Path $bundle.Root 'ao-offline-daemon.exe'

# This fixture never adopts an existing directory, changes an ancestor ACL,
# imports native credentials, or launches the ordinary AO.exe daemon command.
Assert-CandidateACLAncestors 'C:\'
$runId = [Guid]::NewGuid().ToString('N')
$fixtureRoot = Get-CandidateLocalPath (Join-Path 'C:\' "AO-MTechLabs-DaemonFixture-$runId")
if (Test-Path -LiteralPath $fixtureRoot) { throw 'Fixture directory already exists; it will not be adopted.' }
New-Item -ItemType Directory -Path $fixtureRoot -ErrorAction Stop | Out-Null
$null = Get-CandidateLocalPath $fixtureRoot
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
$acl = Get-Acl -LiteralPath $fixtureRoot -ErrorAction Stop
if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -cne $sid.Value) {
    throw 'New fixture directory has an unexpected owner; no ACL update will be attempted.'
}
$acl.SetAccessRuleProtection($true, $false)
foreach ($rule in @($acl.Access)) { $acl.RemoveAccessRuleSpecific($rule) }
foreach ($principal in @($sid.Value, 'S-1-5-18', 'S-1-5-32-544')) {
    $rule = [Security.AccessControl.FileSystemAccessRule]::new(
        [Security.Principal.SecurityIdentifier]::new($principal),
        [Security.AccessControl.FileSystemRights]::FullControl,
        [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
        [Security.AccessControl.PropagationFlags]::None,
        [Security.AccessControl.AccessControlType]::Allow)
    $acl.AddAccessRule($rule)
}
Set-Acl -LiteralPath $fixtureRoot -AclObject $acl -ErrorAction Stop
$null = Assert-CandidateACL $fixtureRoot -Private -CurrentOwner
Assert-CandidateACLAncestors $fixtureRoot
Write-CandidateJSONNew (Join-Path $fixtureRoot 'fixture-root.json') @{
    purpose = 'AO MTechLabs offline daemon fixture'; runId = $runId; ownerSID = $sid.Value
    sourceCommit = $bundle.Manifest.sourceCommit
    fixtureSHA256 = $bundle.Manifest.files['ao-offline-daemon.exe'].sha256
    createdAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
}
$resultDir = Join-Path $fixtureRoot 'results'
New-Item -ItemType Directory -Path $resultDir -ErrorAction Stop | Out-Null
$logPath = Join-Path $resultDir 'offline-daemon.log'
$proofPath = Join-Path $fixtureRoot 'fixture-proof.json'
$fixtureEnv = @{
    AO_DAEMON_FIXTURE_ROOT = $fixtureRoot
    AO_DAEMON_FIXTURE_SOURCE = $bundle.Manifest.sourceCommit
    AO_DAEMON_FIXTURE_EXE_SHA256 = $bundle.Manifest.files['ao-offline-daemon.exe'].sha256
}
$savedEnv = @{}
foreach ($item in Get-ChildItem Env:) {
    if ($item.Name -like 'AO_*' -or $item.Name -like 'CODEX_*' -or
        $item.Name -like 'OPENAI_*' -or $item.Name -like 'ANTHROPIC_*' -or
        $item.Name -like 'CLAUDE_*' -or $item.Name -like 'AZURE_OPENAI_*' -or
        $item.Name -like '*_API_KEY' -or $item.Name -like '*_TOKEN') {
        $savedEnv[$item.Name] = $item.Value
        [Environment]::SetEnvironmentVariable($item.Name, $null, 'Process')
    }
}
foreach ($name in $fixtureEnv.Keys) {
    if (-not $savedEnv.ContainsKey($name)) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    [Environment]::SetEnvironmentVariable($name, $fixtureEnv[$name], 'Process')
}
$started = [DateTimeOffset]::UtcNow
$testExit = 1
try {
    # The linked TestMain selects the sole parent. Only its nonce-bound child
    # can enter the child test; command-line test selection is not accepted.
    & $daemonExe 2>&1 | Tee-Object -FilePath $logPath
    $testExit = $LASTEXITCODE
} finally {
    foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process') }
}
$proofValidation = 'NOT_VALIDATED'
$proofHash = $null
try {
    if ($testExit -ne 0) { throw 'The fixed daemon fixture failed.' }
    if (-not (Test-Path -LiteralPath $proofPath -PathType Leaf)) { throw 'Fixture completion proof is missing; skipped tests are not a pass.' }
    $null = Get-CandidateLocalPath $proofPath
    $proof = Get-Content -LiteralPath $proofPath -Raw | ConvertFrom-Json -AsHashtable
    if ($proof.sourceCommit -cne $bundle.Manifest.sourceCommit -or
        $proof.fixtureSHA256 -cne $bundle.Manifest.files['ao-offline-daemon.exe'].sha256 -or
        $proof.runStarts -ne 2 -or $proof.forbiddenEffectCount -ne 0 -or $proof.realProviderCalls -ne 0 -or
        $proof.crashRecoveryVerified -isnot [bool] -or -not $proof.crashRecoveryVerified -or
        $proof.gracefulShutdownVerified -isnot [bool] -or -not $proof.gracefulShutdownVerified -or
        $proof.dirtyFilePreserved -isnot [bool] -or -not $proof.dirtyFilePreserved -or
        $proof.ownedChildCrash -isnot [bool] -or -not $proof.ownedChildCrash -or
        $proof.registeredSessions -ne 40 -or $proof.queuedRequestsPreserved -ne 38 -or
        $proof.hibernatedHeldSessions -ne 38 -or $proof.syntheticProviderWrites -ne 2) {
        throw 'Daemon completion proof did not satisfy the fixed source, recovery and zero-provider contract.'
    }
    foreach ($required in @('startupFailureCleanupVerified', 'uncertainDispatchQuarantined',
        'uncertainAdmissionPreserved', 'durableReportAttachmentPreserved', 'privateIPCBindingsVerified')) {
        if ($proof[$required] -isnot [bool] -or -not $proof[$required]) {
            throw "Daemon proof did not verify $required."
        }
    }
    if ($proof.allowedSyntheticEffectCounts -isnot [System.Collections.IDictionary] -or
        $proof.allowedSyntheticEffectCounts['chat.synthetic-provider-write'] -ne 2) {
        throw 'Daemon proof does not contain exactly two synthetic write callbacks.'
    }
    $ipcPrefixes = @{
        'ipc.browser-runtime.bound' = 'browser-runtime-browser'
        'ipc.browser-stream.bound' = 'browser-stream'
        'ipc.supervisor.bound' = 'supervise'
    }
    if ($proof.privateIPCAddresses -isnot [System.Collections.IDictionary] -or $proof.privateIPCAddresses.Count -ne 3) {
        throw 'Daemon proof must contain exactly three private IPC bindings.'
    }
    $authorityDigest = $null
    foreach ($key in $ipcPrefixes.Keys) {
        $address = $proof.privateIPCAddresses[$key]
        $pattern = '^\\\\\.\\pipe\\ao-' + $ipcPrefixes[$key] + '-v2-([0-9a-f]{64})$'
        if ($address -isnot [string] -or $address -cnotmatch $pattern) {
            throw "Daemon proof contains an invalid private IPC binding: $key."
        }
        $digest = [regex]::Match($address, $pattern).Groups[1].Value
        if ($null -eq $authorityDigest) { $authorityDigest = $digest }
        elseif ($authorityDigest -cne $digest) { throw 'Daemon IPC bindings do not share one qualified state authority.' }
    }
    $proofHash = (Get-FileHash -LiteralPath $proofPath -Algorithm SHA256).Hash.ToLowerInvariant()
    Copy-Item -LiteralPath $proofPath -Destination (Join-Path $resultDir 'fixture-proof.json') -ErrorAction Stop
    $proofValidation = 'PASS'
} catch {
    $testExit = 1
    $proofValidation = $_.Exception.Message
}
Write-CandidateJSONNew (Join-Path $resultDir 'result.json') @{
    sourceCommit = $bundle.Manifest.sourceCommit; manifestSHA256 = $bundle.ManifestSHA256
    binarySHA256 = $bundle.Manifest.files['ao-offline-daemon.exe'].sha256
    startedAtUtc = $started.ToString('o'); completedAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
    exitCode = $testExit; logSHA256 = (Get-FileHash -LiteralPath $logPath -Algorithm SHA256).Hash.ToLowerInvariant()
    proofSHA256 = $proofHash; proofValidation = $proofValidation
    scope = 'Compiled shared daemon lifecycle with complete test-only fake effects, private fixture state and ephemeral loopback HTTP; no ordinary AO.exe daemon or real provider startup'
}
Write-Output "Offline daemon result: $resultDir"
exit $testExit
