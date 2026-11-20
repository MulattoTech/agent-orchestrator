#Requires -Version 7.4
[CmdletBinding()]
param(
    [switch]$AuthorizePostRefreshValidation,
    [switch]$EnableAllowanceObservation,
    [Parameter(Mandatory)][string]$OwnerPolicyPath,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-fA-F]{64}$')][string]$VerifiedNativeAccountSHA256,
    [Parameter(Mandatory)][string]$NativeHome
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $AuthorizePostRefreshValidation) { throw 'Explicit -AuthorizePostRefreshValidation is required; this script starts an isolated daemon.' }
if ([DateTimeOffset]::UtcNow -lt [DateTimeOffset]::Parse('2026-11-21T12:30:00Z')) {
    throw 'Post-refresh startup is locked until 2026-11-21T12:30:00Z. This time gate is not proof of available quota.'
}
if (-not $IsWindows) { throw 'This candidate requires Windows.' }
$bootstrap = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'manifest.json') -Raw | ConvertFrom-Json -AsHashtable
$common = Join-Path $PSScriptRoot 'Candidate.Common.ps1'
if ((Get-FileHash -LiteralPath $common -Algorithm SHA256).Hash.ToLowerInvariant() -cne $bootstrap.files['Candidate.Common.ps1'].sha256) {
    throw 'Candidate helper script digest mismatch.'
}
. $common
$bundle = Assert-CandidateBundle $PSScriptRoot
# File hashes alone cannot protect against replacement in a writable parent
# between verification and CreateProcess. Refuse an unsafe release ancestry.
Assert-CandidateACLAncestors $bundle.Root
$owner = Assert-CandidateOwnerPolicy $OwnerPolicyPath $VerifiedNativeAccountSHA256
$nativeRoot = Get-CandidateLocalPath $NativeHome
if (-not (Test-Path -LiteralPath $nativeRoot -PathType Container)) { throw 'The explicitly selected native account home must already exist.' }
Assert-CandidateACLAncestors $nativeRoot
$null = Assert-CandidateACL $nativeRoot -CurrentOwner
$nativeAuth = Join-Path $nativeRoot 'auth.json'
if (-not (Test-Path -LiteralPath $nativeAuth -PathType Leaf)) { throw 'Existing native authentication is not available; this script never logs in or creates credentials.' }
$null = Get-CandidateLocalPath $nativeAuth
# Conservatively require a private existing credential ACL. Do not trigger a
# native credential permission repair or copy/read the credential contents.
$null = Assert-CandidateACL $nativeAuth -Private -CurrentOwner
$private = Initialize-CandidatePrivateRoot $owner
$owner = $private.Owner
$port = 39431
if (@([Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners() | Where-Object Port -eq $port).Count -ne 0) {
    throw 'Port 39431 is already occupied. No existing daemon will be stopped or replaced.'
}
$runId = 'reliability-' + [Guid]::NewGuid().ToString('N')
$validationRoot = Get-CandidateLocalPath (Join-Path $private.Root 'validation')
$dataRoot = Join-Path $validationRoot $runId
if (Test-Path -LiteralPath $dataRoot) { throw 'New validation data root unexpectedly exists.' }
New-Item -ItemType Directory -Path $dataRoot -ErrorAction Stop | Out-Null
Assert-CandidateACLAncestors $dataRoot
$null = Assert-CandidateACL $dataRoot -Private -CurrentOwner
$runFile = Join-Path $dataRoot 'running.json'
$recordFile = Join-Path $dataRoot 'candidate-run.json'
$aoExe = Join-Path $bundle.Root 'AO.exe'
$stdout = Join-Path $dataRoot 'daemon.stdout.log'
$stderr = Join-Path $dataRoot 'daemon.stderr.log'
$childEnv = @{}
# Remove inherited AO session/supervisor/browser settings in the child only.
foreach ($item in Get-ChildItem Env:) {
    if ($item.Name -like 'AO_*') { $childEnv[$item.Name] = $null }
}
$childEnv['AO_DATA_DIR'] = $dataRoot
$childEnv['AO_RUN_FILE'] = $runFile
$childEnv['AO_PORT'] = '39431'
$childEnv['AO_APP_RUN_ID'] = $runId
# The script owns this isolated launch through the existing app-run shutdown
# contract. It opens no Electron app or supervisor named-pipe connection.
$childEnv['AO_OWNER'] = 'app'
$childEnv['AO_AGENT'] = 'codex'
$childEnv['AO_INFERENCE_POLICY'] = $owner.Path
$childEnv['AO_REQUIRE_INFERENCE_POLICY'] = '1'
$childEnv['AO_ENABLE_ALLOWANCE_CHECK'] = '0'
# This opt-in only exposes the separate non-model observation action. All
# post-refresh, owner-policy and storage gates above still run first.
if ($EnableAllowanceObservation) {
    $childEnv['AO_ENABLE_ALLOWANCE_CHECK'] = '1'
}
$childEnv['AO_FAKE_HARNESS'] = '0'
$childEnv['AO_FLEET_PRESET'] = '40'
$childEnv['AO_MAX_WORKER_SESSIONS'] = '2'
$childEnv['AO_MAX_PROJECT_WORKER_SESSIONS'] = '2'
$childEnv['AO_FLEET_AUTOSCALE'] = '0'
$childEnv['AO_HARNESS_UPDATE_CHECKS'] = 'off'
$childEnv['AO_UPDATE_WHILE_RUNNING'] = 'off'
$childEnv['AO_UPDATE_WHEN_IDLE'] = 'off'
$childEnv['AO_VERSION_REFRESH'] = 'off'
$childEnv['AO_TUI_VERSION_REFRESH'] = 'off'
$childEnv['AO_TRACKER_INTAKE'] = 'off'
$childEnv['AO_CLOUD_OFFERING'] = 'off'
$childEnv['AO_TELEMETRY_EVENTS'] = 'off'
$childEnv['AO_TELEMETRY_METRICS'] = 'off'
$childEnv['AO_TELEMETRY_REMOTE'] = 'off'
$childEnv['AO_SHUTDOWN_TIMEOUT'] = '5s'
$childEnv['AO_INSTALLER_SHUTDOWN_TIMEOUT'] = '5s'
$childEnv['CODEX_HOME'] = $nativeRoot
$childEnv['GOMAXPROCS'] = '2'
Write-CandidateJSONNew (Join-Path $dataRoot 'launch-intent.json') @{
    runId = $runId; sourceCommit = $bundle.Manifest.sourceCommit; manifestSHA256 = $bundle.ManifestSHA256
    dataRoot = $dataRoot; runFile = $runFile; port = $port; ownerPolicyPath = $owner.Path
    policySHA256AtLaunch = $owner.SHA256; nativeAccountSHA256 = $VerifiedNativeAccountSHA256.ToLowerInvariant()
    requestedAtUtc = [DateTimeOffset]::UtcNow.ToString('o'); allowanceCheckEnabled = [bool]$EnableAllowanceObservation
}
# Deliberately use "daemon"; "start" opens/downloads the desktop installation.
$process = Start-Process -FilePath $aoExe -ArgumentList @('daemon') -WorkingDirectory $dataRoot -Environment $childEnv -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
$record = @{
    schemaVersion = 1; runId = $runId; processId = $process.Id
    processStartTicksUtc = $process.StartTime.ToUniversalTime().Ticks
    processStartedAtUtc = $process.StartTime.ToUniversalTime().ToString('o')
    executablePath = $aoExe; executableSHA256 = $bundle.Manifest.files['AO.exe'].sha256
    sourceCommit = $bundle.Manifest.sourceCommit; manifestSHA256 = $bundle.ManifestSHA256
    dataRoot = $dataRoot; runFile = $runFile; port = $port; ownerPolicyPath = $owner.Path
    policySHA256AtLaunch = $owner.SHA256; stdout = $stdout; stderr = $stderr
}
Write-CandidateJSONNew $recordFile $record
$deadline = [DateTimeOffset]::UtcNow.AddSeconds(45)
$ready = $false
$lastError = 'readiness not observed'
while ([DateTimeOffset]::UtcNow -lt $deadline) {
    $process.Refresh()
    if ($process.HasExited) { throw "Candidate exited before readiness. Preserve and inspect $dataRoot" }
    if (Test-Path -LiteralPath $runFile) {
        try {
            $null = Get-CandidateOwnedRun $record
            $state = Invoke-RestMethod -Uri 'http://127.0.0.1:39431/internal/reliability' -Method Get -TimeoutSec 3 -NoProxy
            if (-not $state.inference.emergency_stop -or -not $state.inference.paused -or
                $state.inference.inference_cap -ne 1 -or
                [bool]$state.allowanceCheckEnabled -ne [bool]$EnableAllowanceObservation) {
                throw 'Initial emergency stop, pause, inference cap or allowance gate differs from the required state.'
            }
            $ready = $true
            break
        } catch { $lastError = $_.Exception.Message }
    }
    Start-Sleep -Milliseconds 250
}
if (-not $ready) { throw "Readiness not verified: $lastError. Data and process are retained; use the owned stop script if ownership checks succeed. Run record: $recordFile" }
Write-CandidateJSONNew (Join-Path $dataRoot 'startup-observation.json') @{
    observedAtUtc = [DateTimeOffset]::UtcNow.ToString('o'); runId = $runId
    emergencyStop = $true; paused = $true; inferenceCap = 1; allowanceCheckEnabled = [bool]$EnableAllowanceObservation
    modelValidation = 'NOT_PERFORMED'; nativeInterruption = 'NOT_QUALIFIED'
}
Write-Output "Isolated candidate is ready and stopped for inference: http://127.0.0.1:39431/internal/reliability/dashboard"
Write-Output "Run record: $recordFile"
if ($EnableAllowanceObservation) {
    Write-Output 'Non-model allowance observation is enabled for a separate explicit operator action. Startup did not invoke it.'
} else {
    Write-Output 'Non-model allowance observation remains disabled by default.'
}
Write-Output 'No allowance verification or model turn was requested. Keep emergency stop and pause enabled until the separate controlled qualification.'
