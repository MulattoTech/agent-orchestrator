#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-fA-F]{40}$')][string]$SourceCommit,
    [string]$RepositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..')),
    [string]$GoExecutable = 'C:\Tools\Dev\AO-3921-20261114\toolchains\go\bin\go.exe'
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'This candidate recipe requires Windows.' }
. (Join-Path $PSScriptRoot 'Candidate.Common.ps1')
$repo = Get-CandidateLocalPath $RepositoryRoot
$goTool = Get-CandidateLocalPath $GoExecutable
if (-not (Test-Path -LiteralPath $goTool -PathType Leaf)) { throw 'The approved local Go toolchain is missing.' }
$sourceCommit = $SourceCommit.ToLowerInvariant()
$git = (Get-Command git.exe -ErrorAction Stop).Source
$initialLocation = Get-Location
$envNames = @('GOTOOLCHAIN', 'GOMAXPROCS', 'GOPROXY', 'GOSUMDB', 'GOWORK', 'GOENV', 'GOFLAGS', 'CGO_ENABLED', 'GOOS', 'GOARCH')
$savedEnv = @{}
foreach ($name in $envNames) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
try {
    Set-Location $repo
    $actualCommit = (& $git rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or $actualCommit -cne $sourceCommit) { throw 'HEAD does not match the explicitly selected source commit.' }
    $dirty = @(& $git status --porcelain --untracked-files=all)
    if ($LASTEXITCODE -ne 0 -or $dirty.Count -ne 0) { throw 'Build requires the complete source and packaging scripts committed in a clean worktree.' }
    $packageSource = Get-CandidateLocalPath (Join-Path $repo 'scripts\reliability-candidate')
    if (-not $packageSource.Equals((Get-CandidateLocalPath $PSScriptRoot), [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Run the packaging recipe from the selected committed source checkout.'
    }
    $builtAt = [DateTimeOffset]::UtcNow
    $stamp = $builtAt.ToString('yyyyMMddTHHmmssZ')
    $candidateName = "ao-unofficial-$stamp-$($sourceCommit.Substring(0,12))"
    $releaseParent = Get-CandidateLocalPath 'C:\AODev\releases'
    $releaseDir = Join-Path $releaseParent $candidateName
    if (Test-Path -LiteralPath $releaseDir) { throw 'Release directory already exists; existing artifacts will not be overwritten.' }
    New-Item -ItemType Directory -Path $releaseDir -ErrorAction Stop | Out-Null
    $buildLog = Join-Path $releaseDir 'build.log'
    $env:GOTOOLCHAIN = 'local'
    $env:GOMAXPROCS = '2'
    $env:GOPROXY = 'off'
    $env:GOSUMDB = 'off'
    $env:GOWORK = 'off'
    $env:GOENV = 'off'
    $env:GOFLAGS = ''
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $goVersion = (& $goTool version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Local Go toolchain could not report its version.' }
    Set-Location (Join-Path $repo 'backend')
    $version = "0.0.0-mtechlabs.$($builtAt.ToString('yyyyMMddHHmmss')).$($sourceCommit.Substring(0,12))"
    $date = $builtAt.ToString('yyyy-MM-ddTHH:mm:ssZ')
    $module = 'github.com/aoagents/agent-orchestrator/backend/internal'
    $strictFlags = "-X $module/inferenceguard.RequirePolicy=1 -X $module/cli.Version=$version -X $module/cli.Commit=$sourceCommit -X $module/cli.Date=$date"
    & $goTool build -p 1 -trimpath -ldflags $strictFlags -o (Join-Path $releaseDir 'AO.exe') ./cmd/ao 2>&1 | Tee-Object -FilePath $buildLog
    if ($LASTEXITCODE -ne 0) { throw 'Strict AO compilation failed; incomplete artifacts remain for inspection.' }
    & $goTool build -p 1 -trimpath -o (Join-Path $releaseDir 'ao-offline-worker.exe') ./internal/session_manager/testdata/admission-worker 2>&1 | Tee-Object -FilePath $buildLog -Append
    if ($LASTEXITCODE -ne 0) { throw 'Standard-library fake worker compilation failed.' }
    $helperHash = (Get-FileHash -LiteralPath (Join-Path $releaseDir 'ao-offline-worker.exe') -Algorithm SHA256).Hash.ToLowerInvariant()
    $companionFlags = "-X $module/session_manager.admissionPackagedHelperSHA256=$helperHash -X $module/session_manager.admissionOfflineCompanion=1"
    # -c compiles the package tests without executing them or launching providers.
    & $goTool test -p 1 -c -trimpath -ldflags $companionFlags -o (Join-Path $releaseDir 'ao-offline-fleet.exe') ./internal/session_manager 2>&1 | Tee-Object -FilePath $buildLog -Append
    if ($LASTEXITCODE -ne 0) { throw 'Offline allowlist companion compilation failed.' }
    $conptyFlags = "-X $module/adapters/runtime/conpty.OfflineCandidateConPTYFixture=1"
    & $goTool test -p 1 -c -trimpath -ldflags $conptyFlags -o (Join-Path $releaseDir 'ao-offline-conpty.exe') ./internal/adapters/runtime/conpty 2>&1 | Tee-Object -FilePath $buildLog -Append
    if ($LASTEXITCODE -ne 0) { throw 'Offline candidate ConPTY fixture compilation failed.' }
    $mainTest = Get-Content -LiteralPath (Join-Path $repo 'backend\internal\session_manager\main_test.go') -Raw
    $match = [regex]::Match($mainTest, '(?m)^const admissionOfflineTestAllowlist = "([^"]+)"')
    if (-not $match.Success -or $mainTest -notmatch 'admissionOfflineCompanion == "1"') {
        throw 'Compiled offline allowlist source could not be identified.'
    }
    [IO.File]::WriteAllText((Join-Path $releaseDir 'offline-test-allowlist.txt'), $match.Groups[1].Value + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
    foreach ($name in @('Candidate.Common.ps1', 'Build-Candidate.ps1', 'Run-Offline-Fleet.ps1', 'Run-Offline-ConPTY.ps1',
        'Start-PostRefresh-Validation.ps1', 'Stop-Candidate.ps1', 'Test-CandidateScripts.ps1', 'policy.template.json', 'README.md')) {
        Copy-Item -LiteralPath (Join-Path $packageSource $name) -Destination (Join-Path $releaseDir $name) -ErrorAction Stop
    }
    Set-Location $repo
    $afterCommit = (& $git rev-parse HEAD).Trim()
    $afterDirty = @(& $git status --porcelain --untracked-files=all)
    if ($LASTEXITCODE -ne 0 -or $afterCommit -cne $sourceCommit -or $afterDirty.Count -ne 0) {
        throw 'Source changed during compilation; no candidate manifest will be issued.'
    }
    $files = [ordered]@{}
    foreach ($file in Get-ChildItem -LiteralPath $releaseDir -File | Sort-Object Name) {
        $files[$file.Name] = @{ sha256 = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant(); bytes = $file.Length }
    }
    $manifest = [ordered]@{
        schemaVersion = 1; candidate = $candidateName; sourceCommit = $sourceCommit; sourceClean = $true
        builtAtUtc = $date; version = $version; toolchain = $goVersion; target = 'windows/amd64'; strictPolicy = $true
        strictLinkerFlags = $strictFlags
        offlineCompanion = @{
            allowlistEnforced = $true; allowlist = $match.Groups[1].Value; helperSHA256 = $helperHash
            maxNativeChildren = 40; childMemoryBudgetBytes = 805306368
            scope = 'Compiled fake-runtime/stdlib-worker tests; not the AO daemon or ConPTY/provider qualification'
        }
        offlineConPTY = @{
            allowlistEnforced = $true; parentTest = '^TestActualCandidateConPTYMechanics$'
            internalOwnerTest = '^TestCandidateConPTYOwnerProcess$'; linkerFlags = $conptyFlags
            scope = 'Actual candidate pty-host plus fixed stdlib helper; no normal daemon/account/provider startup'
        }
        validation = @{ syntheticFleet = 'NOT_RUN_BY_BUILD'; candidateConPTY = 'NOT_RUN_BY_BUILD'; daemonStartup = 'NOT_RUN'; liveProvider = 'NOT_RUN' }
        files = $files
    }
    Write-CandidateJSONNew (Join-Path $releaseDir 'manifest.json') $manifest
    $manifestHash = (Get-FileHash -LiteralPath (Join-Path $releaseDir 'manifest.json') -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $releaseDir 'manifest.sha256'), $manifestHash + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
    Write-Output "Candidate compiled without execution: $releaseDir"
    Write-Output "Manifest SHA256: $manifestHash"
} finally {
    Set-Location $initialLocation
    foreach ($name in $envNames) { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process') }
}
