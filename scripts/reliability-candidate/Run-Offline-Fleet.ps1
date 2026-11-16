#Requires -Version 7.4
[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($args.Count -ne 0) { throw 'This runner accepts no arguments.' }
if (-not $IsWindows) { throw 'The compiled synthetic fleet requires Windows.' }
$bootstrap = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'manifest.json') -Raw | ConvertFrom-Json -AsHashtable
$common = Join-Path $PSScriptRoot 'Candidate.Common.ps1'
if ((Get-FileHash -LiteralPath $common -Algorithm SHA256).Hash.ToLowerInvariant() -cne $bootstrap.files['Candidate.Common.ps1'].sha256) {
    throw 'Candidate helper script digest mismatch.'
}
. $common
$bundle = Assert-CandidateBundle $PSScriptRoot
$fleetExe = Join-Path $bundle.Root 'ao-offline-fleet.exe'
$runId = [Guid]::NewGuid().ToString('N')
$logRoot = Get-CandidateLocalPath 'C:\AODev\validation'
$resultDir = Join-Path $logRoot "offline-$runId"
New-Item -ItemType Directory -Path $resultDir -ErrorAction Stop | Out-Null
$logPath = Join-Path $resultDir 'offline-fleet.log'
$savedEnv = @{}
# Only the type-bound compiled fake companion runs here. Isolate it from live AO
# session/account configuration; this does not alter the strict AO.exe binary.
foreach ($item in Get-ChildItem Env:) {
    if ($item.Name -like 'AO_*' -or $item.Name -in @('CODEX_HOME', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'CLAUDE_CONFIG_DIR')) {
        $savedEnv[$item.Name] = $item.Value
        [Environment]::SetEnvironmentVariable($item.Name, $null, 'Process')
    }
}
$started = [DateTimeOffset]::UtcNow
$testExit = 1
try {
    # No external test arguments: linked TestMain selects its fixed allowlist.
    & $fleetExe 2>&1 | Tee-Object -FilePath $logPath
    $testExit = $LASTEXITCODE
} finally {
    foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process') }
}
Write-CandidateJSONNew (Join-Path $resultDir 'result.json') @{
    sourceCommit = $bundle.Manifest.sourceCommit; manifestSHA256 = $bundle.ManifestSHA256
    binarySHA256 = $bundle.Manifest.files['ao-offline-fleet.exe'].sha256
    startedAtUtc = $started.ToString('o'); completedAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
    exitCode = $testExit; logSHA256 = (Get-FileHash -LiteralPath $logPath -Algorithm SHA256).Hash.ToLowerInvariant()
    scope = 'Offline compiled fake fleet; no AO daemon or model request'
}
Write-Output "Synthetic fleet result: $resultDir"
exit $testExit
