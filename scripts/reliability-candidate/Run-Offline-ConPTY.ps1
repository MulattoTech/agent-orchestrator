#Requires -Version 7.4
[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($args.Count -ne 0) { throw 'This runner accepts no arguments.' }
if (-not $IsWindows) { throw 'The compiled candidate ConPTY fixture requires Windows.' }
$bootstrap = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'manifest.json') -Raw | ConvertFrom-Json -AsHashtable
$common = Join-Path $PSScriptRoot 'Candidate.Common.ps1'
if ((Get-FileHash -LiteralPath $common -Algorithm SHA256).Hash.ToLowerInvariant() -cne $bootstrap.files['Candidate.Common.ps1'].sha256) {
    throw 'Candidate helper script digest mismatch.'
}
. $common
$bundle = Assert-CandidateBundle $PSScriptRoot
$conptyExe = Join-Path $bundle.Root 'ao-offline-conpty.exe'
$runId = [Guid]::NewGuid().ToString('N')
$logRoot = Get-CandidateLocalPath 'C:\AODev\validation'
$resultDir = Join-Path $logRoot "offline-conpty-$runId"
New-Item -ItemType Directory -Path $resultDir -ErrorAction Stop | Out-Null
$logPath = Join-Path $resultDir 'offline-conpty.log'
$fixtureEnv = @{
    AO_CONPTY_CANDIDATE_EXE = Join-Path $bundle.Root 'AO.exe'
    AO_CONPTY_CANDIDATE_SHA256 = $bundle.Manifest.files['AO.exe'].sha256
    AO_CONPTY_CANDIDATE_SOURCE = $bundle.Manifest.sourceCommit
    AO_CONPTY_HELPER_EXE = Join-Path $bundle.Root 'ao-offline-worker.exe'
    AO_CONPTY_HELPER_SHA256 = $bundle.Manifest.files['ao-offline-worker.exe'].sha256
}
$savedEnv = @{}
# The fixture starts only its exact candidate pty-host and fixed helper. Its
# children receive a separate small environment whitelist in the Go fixture.
foreach ($item in Get-ChildItem Env:) {
    if ($item.Name -like 'AO_*' -or $item.Name -like 'CODEX_*' -or
        $item.Name -like 'OPENAI_*' -or $item.Name -like 'ANTHROPIC_*' -or
        $item.Name -like 'CLAUDE_*' -or $item.Name -like 'AZURE_OPENAI_*' -or
        $item.Name -in @('GOOGLE_API_KEY', 'GEMINI_API_KEY', 'DEEPSEEK_API_KEY')) {
        $savedEnv[$item.Name] = $item.Value
        [Environment]::SetEnvironmentVariable($item.Name, $null, 'Process')
    }
}
foreach ($name in $fixtureEnv.Keys) {
    if (-not $savedEnv.ContainsKey($name)) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    [Environment]::SetEnvironmentVariable($name, $fixtureEnv[$name], 'Process')
}
# OWNER is private to a fixture-created child; callers cannot select that test.
[Environment]::SetEnvironmentVariable('AO_CONPTY_FIXTURE_OWNER', $null, 'Process')
$started = [DateTimeOffset]::UtcNow
$testExit = 1
try {
    # No external arguments: linked TestMain selects only the parent fixture.
    & $conptyExe 2>&1 | Tee-Object -FilePath $logPath
    $testExit = $LASTEXITCODE
} finally {
    foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process') }
}
Write-CandidateJSONNew (Join-Path $resultDir 'result.json') @{
    sourceCommit = $bundle.Manifest.sourceCommit; manifestSHA256 = $bundle.ManifestSHA256
    binarySHA256 = $bundle.Manifest.files['ao-offline-conpty.exe'].sha256
    candidateSHA256 = $bundle.Manifest.files['AO.exe'].sha256
    helperSHA256 = $bundle.Manifest.files['ao-offline-worker.exe'].sha256
    startedAtUtc = $started.ToString('o'); completedAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
    exitCode = $testExit; logSHA256 = (Get-FileHash -LiteralPath $logPath -Algorithm SHA256).Hash.ToLowerInvariant()
    scope = 'Actual candidate pty-host/ConPTY with fixed stdlib helper only; no normal daemon, account or provider startup'
}
Write-Output "Offline candidate ConPTY result: $resultDir"
exit $testExit
