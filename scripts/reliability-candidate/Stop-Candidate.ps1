#Requires -Version 7.4
[CmdletBinding()]
param([Parameter(Mandatory)][string]$RunRecord)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'This ownership check requires Windows.' }
$bootstrap = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'manifest.json') -Raw | ConvertFrom-Json -AsHashtable
$common = Join-Path $PSScriptRoot 'Candidate.Common.ps1'
if ((Get-FileHash -LiteralPath $common -Algorithm SHA256).Hash.ToLowerInvariant() -cne $bootstrap.files['Candidate.Common.ps1'].sha256) {
    throw 'Candidate helper script digest mismatch.'
}
. $common
$bundle = Assert-CandidateBundle $PSScriptRoot
$recordPath = Get-CandidateLocalPath $RunRecord
$record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json -AsHashtable
$expectedData = Join-Path 'C:\AO-MTechLabs-Private-20261116\validation' $record.runId
if (-not $recordPath.Equals((Join-Path $expectedData 'candidate-run.json'), [StringComparison]::OrdinalIgnoreCase) -or
    -not $record.dataRoot.Equals($expectedData, [StringComparison]::OrdinalIgnoreCase) -or
    -not $record.runFile.Equals((Join-Path $expectedData 'running.json'), [StringComparison]::OrdinalIgnoreCase) -or
    -not $record.executablePath.Equals((Join-Path $bundle.Root 'AO.exe'), [StringComparison]::OrdinalIgnoreCase) -or
    $record.executableSHA256 -cne $bundle.Manifest.files['AO.exe'].sha256 -or
    $record.manifestSHA256 -cne $bundle.ManifestSHA256 -or $record.sourceCommit -cne $bundle.Manifest.sourceCommit) {
    throw 'Run record does not belong to this isolated candidate.'
}
$process = Get-CandidateOwnedProcess $record
$null = Get-CandidateOwnedRun $record
# Only the existing server-fenced shutdown mutates state. An unfenced control
# PUT could affect a successor if the port were reused between requests.
$null = Get-CandidateOwnedRun $record
$body = @{ stopAgents = $true; appRunId = $record.runId } | ConvertTo-Json -Compress
$response = Invoke-WebRequest -Uri 'http://127.0.0.1:39431/shutdown' -Method Post -ContentType 'application/json' -Body $body -TimeoutSec 5 -NoProxy
$ack = $response.Content | ConvertFrom-Json -AsHashtable
if ($response.StatusCode -ne 202 -or $ack.status -ne 'shutting_down' -or
    $ack.service -ne 'agent-orchestrator-daemon' -or $ack.pid -ne $record.processId) {
    throw 'Owned shutdown was not acknowledged. State/logs are retained; no process kill was attempted.'
}
$deadline = [DateTimeOffset]::UtcNow.AddSeconds(60)
while ([DateTimeOffset]::UtcNow -lt $deadline) {
    $process.Refresh()
    if ($process.HasExited) { break }
    Start-Sleep -Milliseconds 250
}
$process.Refresh()
$result = @{
    runId = $record.runId; processId = $record.processId; observedAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
    shutdownAccepted = $true; daemonExitObserved = $process.HasExited; stopControl = 'run-scoped shutdown only'
    ownedAgentsStopRequested = $true; ownedAgentsStopConfirmed = 'UNKNOWN: inspect retained daemon cleanup evidence'
    destructiveCleanup = $false
}
Write-CandidateJSONNew (Join-Path $record.dataRoot ('stop-result-' + [Guid]::NewGuid().ToString('N') + '.json')) $result
if (-not $process.HasExited) { throw 'Graceful shutdown exceeded 60 seconds. No kill fallback was used; retain the candidate process, state and logs for inspection.' }
Write-Output "Owned daemon exit observed. State and logs preserved: $($record.dataRoot)"
Write-Output 'The server accepted owned-agent shutdown; individual native child completion still requires the retained cleanup evidence.'
