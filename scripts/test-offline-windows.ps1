#Requires -Version 7.0
<#
Runs only the committed finite Windows source-test inventory.
Hosted use adds -DownloadModules; tests always run with module fetching disabled.
Local use supplies the pinned Go executable and existing GoCache/ModuleCache.
ExpectedHead must be the exact clean checkout. EvidenceDirectory must be new and outside it.
#>
param(
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$ExpectedHead,
    [Parameter(Mandatory)][string]$EvidenceDirectory,
    [string]$GoExecutable = 'go.exe',
    [string]$GoCache = '',
    [string]$ModuleCache = '',
    [switch]$DownloadModules
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $IsWindows) { throw 'This finite suite requires Windows' }
$Repo = [IO.Path]::GetFullPath((Split-Path $PSScriptRoot -Parent))
$Go = (Get-Command -Name $GoExecutable -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$Git = (Get-Command -Name 'git.exe' -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$GoRoot = Split-Path (Split-Path $Go -Parent) -Parent
$Utf8 = [Text.UTF8Encoding]::new($false)
if (-not [IO.Path]::IsPathFullyQualified($EvidenceDirectory)) { throw 'EvidenceDirectory must be absolute' }
$RunRoot = [IO.Path]::GetFullPath($EvidenceDirectory)
if ($RunRoot.Equals($Repo,[StringComparison]::OrdinalIgnoreCase) -or $RunRoot.StartsWith($Repo+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)) { throw 'Evidence must be outside the checkout' }
if (Test-Path -LiteralPath $RunRoot) { throw 'Refusing to overwrite existing evidence' }
[void][IO.Directory]::CreateDirectory($RunRoot)
# Fixtures and caches are siblings of the artifact directory, never uploaded.
$StateRoot = Join-Path (Split-Path $RunRoot -Parent) ('offline-windows-state-'+[guid]::NewGuid().ToString('N'))
$GuardRoot = Join-Path $StateRoot 'ambient-fixtures'
$TempRoot = Join-Path $StateRoot 'tmp'
$TelemetryRoot = Join-Path $StateRoot 'toolchain-telemetry'
$TelemetryMode = Join-Path $TelemetryRoot 'mode'
$EmptyGitConfig = Join-Path $StateRoot 'empty.gitconfig'
$SelectionPath = Join-Path $PSScriptRoot 'offline-windows-tests.json'
$SelectionSHA256 = ''
$Phases = [Collections.Generic.List[object]]::new()
$SourceChecks = [Collections.Generic.List[object]]::new()
$Failures = [Collections.Generic.List[string]]::new()
$SourcePaths = @()
$SourceBefore = @()
$GuardBefore = ''
$Common = @{}
$GitReady = $false
$Started = [DateTime]::UtcNow.ToString('o')

function Write-Json([string]$Path, $Value) {
    [IO.File]::WriteAllText($Path,(ConvertTo-Json -InputObject $Value -Depth 20),$Utf8)
}
function Invoke-Captured([string]$Executable, [string[]]$CommandArgs, [hashtable]$ChildEnv, [int]$LimitSeconds) {
    $psi = [Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $Executable
    $psi.WorkingDirectory = Join-Path $Repo 'backend'
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = $Utf8
    $psi.StandardErrorEncoding = $Utf8
    foreach ($arg in $CommandArgs) { $psi.ArgumentList.Add($arg) }
    # No inherited GitHub token, provider key, AO session, tool override or profile.
    $psi.Environment.Clear()
    foreach ($entry in $ChildEnv.GetEnumerator()) { $psi.Environment[$entry.Key] = [string]$entry.Value }
    $p = [Diagnostics.Process]::new()
    $p.StartInfo = $psi
    $record = [ordered]@{pid=$null; exit_code=-1; timed_out=$false; elapsed_seconds=0; stdout=''; stderr=''; error=''}
    $watch = [Diagnostics.Stopwatch]::StartNew()
    $outTask = $null
    $errTask = $null
    try {
        if (-not $p.Start()) { throw 'Child did not start' }
        $record.pid = $p.Id
        $outTask = $p.StandardOutput.ReadToEndAsync()
        $errTask = $p.StandardError.ReadToEndAsync()
        if (-not $p.WaitForExit($LimitSeconds * 1000)) {
            $record.timed_out = $true
            $p.Kill($true) # Only this owned Go/Git child tree, never process names.
            if (-not $p.WaitForExit(10000)) { throw 'Owned child did not exit after watchdog kill' }
        }
        $record.exit_code = $p.ExitCode
        if (-not [Threading.Tasks.Task]::WaitAll([Threading.Tasks.Task[]]@($outTask,$errTask),10000)) { throw 'Redirected streams did not close' }
    } catch {
        $record.error = [string]$_.Exception.Message
    } finally {
        if ($null -ne $record.pid -and -not $p.HasExited) {
            try { $p.Kill($true); [void]$p.WaitForExit(10000) } catch { $record.error += '; cleanup: '+[string]$_.Exception.Message }
        }
        if ($null -ne $outTask -and $outTask.IsCompletedSuccessfully) { $record.stdout = [string]$outTask.Result }
        if ($null -ne $errTask -and $errTask.IsCompletedSuccessfully) { $record.stderr = [string]$errTask.Result }
        $watch.Stop()
        $record.elapsed_seconds = [Math]::Round($watch.Elapsed.TotalSeconds,3)
        $p.Dispose()
    }
    return [pscustomobject]$record
}
function Read-Git([string[]]$CommandArgs) {
    $r = Invoke-Captured $Git (@('--no-optional-locks','-c','core.fsmonitor=false','-c','core.untrackedCache=false','-C',$Repo)+$CommandArgs) $Common 30
    if ($r.exit_code -ne 0 -or $r.timed_out -or $r.error) { throw ('Git source guard failed: '+$r.error+' '+$r.stderr) }
    return $r.stdout.TrimEnd([char[]]@(13,10))
}
function Assert-Source([string]$Stage) {
    $head = Read-Git @('rev-parse','HEAD')
    $physical = Read-Git @('rev-parse','--show-toplevel')
    $status = Read-Git @('status','--porcelain=v1','--untracked-files=all')
    $SourceChecks.Add([ordered]@{stage=$Stage; head=$head; physical_root=$physical; status=$status; checked_utc=[DateTime]::UtcNow.ToString('o')})
    if ($head -cne $ExpectedHead -or [IO.Path]::GetFullPath($physical) -ine $Repo -or $status.Length -ne 0) { throw "Source identity or clean-tree guard failed at $Stage" }
}
function Get-SourceRecords {
    @(foreach ($relative in $SourcePaths) {
        $file = Get-Item -LiteralPath (Join-Path $Repo $relative)
        if ($file.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Unexpected source reparse entry: $relative" }
        [ordered]@{path=$relative; bytes=$file.Length; sha256=(Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash}
    })
}
function Get-FixtureRecord {
    $rows = @(Get-ChildItem -LiteralPath $GuardRoot -Recurse -Force | Sort-Object FullName | ForEach-Object {
        if ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Unexpected link in owned outer profile' }
        [ordered]@{path=$_.FullName.Substring($GuardRoot.Length+1); directory=$_.PSIsContainer; sha256=if($_.PSIsContainer){''}else{(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash}}
    })
    return ConvertTo-Json -InputObject $rows -Depth 4 -Compress
}
function Assert-Fixtures {
    if ((Get-FixtureRecord) -cne $GuardBefore) { throw 'Owned outer-profile fixtures changed' }
    if ([IO.File]::ReadAllText($TelemetryMode) -cne 'off' -or @(Get-ChildItem -LiteralPath $TelemetryRoot -Recurse -Force).Count -ne 1) { throw 'Owned telemetry-off guard changed' }
}
function Test-Exact([object[]]$Expected, [object[]]$Actual) {
    return $Expected.Count -eq $Actual.Count -and ((@($Expected | Sort-Object -CaseSensitive)-join [char]10) -ceq (@($Actual | Sort-Object -CaseSensitive)-join [char]10))
}
function Get-Coverage([string]$Stdout, $Definition) {
    $events = @($Stdout -split "\r?\n" | Where-Object { $_ -ne '' } | ForEach-Object { ConvertFrom-Json -InputObject $_ -AsHashtable })
    $terminals = @($events | Where-Object { $_.ContainsKey('Test') -and $_.Action -in @('pass','fail','skip') })
    $ids = @($terminals | ForEach-Object { $_.Package+'::'+$_.Test })
    $runIds = @($events | Where-Object { $_.ContainsKey('Test') -and $_.Action -eq 'run' } | ForEach-Object { $_.Package+'::'+$_.Test })
    $packages = @($events | Where-Object { -not $_.ContainsKey('Test') -and $_.Action -in @('pass','fail','skip') })
    $bad = @($events | Where-Object { $_.Action -in @('fail','skip') })
    $top = @($terminals | Where-Object { $_.Test -notmatch '/' }).Count
    $nested = @($terminals | Where-Object { $_.Test -match '/' }).Count
    $leaf = @($terminals | Where-Object {
        $package = $_.Package; $test = $_.Test
        @($terminals | Where-Object { $_.Package -ceq $package -and $_.Test.StartsWith($test+'/',[StringComparison]::Ordinal) }).Count -eq 0
    }).Count
    $valid = (Test-Exact $Definition.test_ids $ids) -and (Test-Exact $Definition.test_ids $runIds) -and (Test-Exact $Definition.package_ids @($packages | ForEach-Object { $_.Package })) -and $bad.Count -eq 0 -and $top -eq $Definition.expected_top -and $nested -eq $Definition.expected_nested -and $leaf -eq $Definition.expected_leaf
    return [ordered]@{valid=$valid; top=$top; nested=$nested; leaf=$leaf; run_ids=$runIds; terminal_ids=$ids; non_pass_events=$bad; package_results=$packages}
}
function Invoke-GoPhase([string]$Name, [string[]]$CommandArgs, [hashtable]$ChildEnv, [int]$LimitSeconds, $Definition=$null) {
    Assert-Source ($Name+'-before')
    Assert-Fixtures
    $dir = Join-Path $RunRoot $Name
    [void][IO.Directory]::CreateDirectory($dir)
    Write-Json (Join-Path $dir 'invocation.json') ([ordered]@{executable=$Go; arguments=$CommandArgs; cwd=(Join-Path $Repo 'backend'); outer_timeout_seconds=$LimitSeconds; environment=$ChildEnv; started_utc=[DateTime]::UtcNow.ToString('o')})
    [Console]::WriteLine("Starting $Name (owned child limit $LimitSeconds seconds)")
    $r = Invoke-Captured $Go $CommandArgs $ChildEnv $LimitSeconds
    [IO.File]::WriteAllText((Join-Path $dir 'stdout.log'),$r.stdout,$Utf8)
    [IO.File]::WriteAllText((Join-Path $dir 'stderr.log'),$r.stderr,$Utf8)
    $phase = [ordered]@{name=$Name; result='FAIL'; pid=$r.pid; exit_code=$r.exit_code; timed_out=$r.timed_out; elapsed_seconds=$r.elapsed_seconds; error=$r.error; stdout_sha256=(Get-FileHash -LiteralPath (Join-Path $dir 'stdout.log')).Hash; stderr_sha256=(Get-FileHash -LiteralPath (Join-Path $dir 'stderr.log')).Hash; evidence=$dir}
    $Phases.Add($phase)
    try {
        Assert-Source ($Name+'-after')
        Assert-Fixtures
        if ($null -ne $Definition) { $phase.coverage = Get-Coverage $r.stdout $Definition }
        if ($r.exit_code -ne 0 -or $r.timed_out -or $r.error) { throw "Go child failed at $Name" }
        if ($null -ne $Definition -and -not $phase.coverage.valid) { throw "Exact test coverage failed at $Name" }
        $phase.result = 'PASS'
    } finally {
        Write-Json (Join-Path $dir 'result.json') $phase
    }
    [Console]::WriteLine("Completed $Name ($($r.elapsed_seconds)s)")
    return $r.stdout
}

try {
    foreach ($path in @($StateRoot,$TelemetryRoot,$TempRoot)) { [void][IO.Directory]::CreateDirectory($path) }
    [IO.File]::WriteAllText($TelemetryMode,'off',$Utf8) # Before every Go call, including version discovery.
    [IO.File]::WriteAllText($EmptyGitConfig,'',$Utf8)
    foreach ($name in @('profile','code','share','ao','appdata','localappdata','config','cache','data','state')) { [void][IO.Directory]::CreateDirectory((Join-Path $GuardRoot $name)) }
    $profile = Join-Path $GuardRoot 'profile'
    foreach ($dir in @((Join-Path $GuardRoot 'code'),(Join-Path $GuardRoot 'share'),(Join-Path $profile '.kimi'),(Join-Path $profile '.kimi-code'))) {
        [void][IO.Directory]::CreateDirectory($dir)
        [IO.File]::WriteAllText((Join-Path $dir 'config.toml'),'[malformed synthetic outer-profile sentinel',$Utf8)
    }
    $GuardBefore = Get-FixtureRecord
    [IO.File]::WriteAllText((Join-Path $RunRoot 'outer-profile-before.json'),$GuardBefore,$Utf8)
    if ([string]::IsNullOrEmpty($GoCache)) { $GoCache = Join-Path $StateRoot 'go-build' }
    if ([string]::IsNullOrEmpty($ModuleCache)) { $ModuleCache = Join-Path $StateRoot 'go-mod' }
    foreach ($path in @($GoCache,$ModuleCache)) {
        if (-not [IO.Path]::IsPathFullyQualified($path) -or [IO.Path]::GetFullPath($path).Equals($Repo,[StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFullPath($path).StartsWith($Repo+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)) { throw 'Caches must be absolute and outside the checkout' }
        [void][IO.Directory]::CreateDirectory($path)
    }
    $systemRoot = [Environment]::GetEnvironmentVariable('SystemRoot','Process')
    if ([string]::IsNullOrEmpty($systemRoot) -or -not [IO.Path]::IsPathFullyQualified($systemRoot)) { throw 'Trusted Windows SystemRoot unavailable' }
    $system32 = Join-Path $systemRoot 'System32'
    $Common = @{
        SystemRoot=$systemRoot; WINDIR=$systemRoot; ComSpec=(Join-Path $system32 'cmd.exe'); PATH=((Split-Path $Go -Parent)+';'+$system32+';'+$systemRoot); PATHEXT='.EXE';
        GOROOT=$GoRoot; GOPATH=(Join-Path $StateRoot 'go-path'); GOCACHE=$GoCache; GOMODCACHE=$ModuleCache; GOTOOLCHAIN='local'; GOPROXY='off'; GOSUMDB='off'; GOPRIVATE=''; GONOPROXY='none'; GONOSUMDB='none'; GOVCS='*:off'; GO111MODULE='on'; GOENV='off'; GOWORK='off'; GOFLAGS=''; GOAUTH='off'; GOMAXPROCS='2'; CGO_ENABLED='0'; GOOS='windows'; GOARCH='amd64'; TEST_TELEMETRY_DIR=$TelemetryRoot;
        GIT_CONFIG_NOSYSTEM='1'; GIT_CONFIG_GLOBAL=$EmptyGitConfig; GIT_TERMINAL_PROMPT='0'; GCM_INTERACTIVE='Never';
        HOME=$profile; USERPROFILE=$profile; HOMEDRIVE=[IO.Path]::GetPathRoot($profile).TrimEnd('\'); HOMEPATH=$profile.Substring([IO.Path]::GetPathRoot($profile).Length-1);
        APPDATA=(Join-Path $GuardRoot 'appdata'); LOCALAPPDATA=(Join-Path $GuardRoot 'localappdata'); XDG_CONFIG_HOME=(Join-Path $GuardRoot 'config'); XDG_CACHE_HOME=(Join-Path $GuardRoot 'cache'); XDG_DATA_HOME=(Join-Path $GuardRoot 'data'); XDG_STATE_HOME=(Join-Path $GuardRoot 'state');
        KIMI_CODE_HOME=(Join-Path $GuardRoot 'code'); KIMI_SHARE_DIR=(Join-Path $GuardRoot 'share'); AO_DATA_DIR=(Join-Path $GuardRoot 'ao'); TEMP=$TempRoot; TMP=$TempRoot; GOTMPDIR=$TempRoot;
        POWERSHELL_TELEMETRY_OPTOUT='1'; POWERSHELL_UPDATECHECK='Off'; PSModuleAnalysisCachePath='NUL'; DOTNET_MultiCoreJitNoProfileGather='1'
    }
    $GitReady = $true
    Assert-Source 'initial'
    if ([IO.File]::ReadAllText((Join-Path $Repo 'backend/go.mod')) -notmatch '(?m)^go 1\.27\.4\r?$') { throw 'go.mod must retain the reviewed Go 1.27.4 directive' }
    $selection = Get-Content -Raw -LiteralPath $SelectionPath | ConvertFrom-Json -AsHashtable
    $SelectionSHA256 = (Get-FileHash -LiteralPath $SelectionPath).Hash
    if ($selection.schema -cne 'ao-offline-windows-selection-v1' -or $selection.go_version -cne '1.27.4' -or $selection.runs.Count -ne 6 -or $selection.expected_total.top -ne 47 -or $selection.expected_total.nested -ne 102 -or $selection.expected_total.leaf -ne 134) { throw 'Unexpected finite selection totals' }
    $safeTop = @(
        'TestAdmissionNativeWorkerSnapshotSelection',
        'TestInferenceGuardChildCompletionCannotReleaseRootLease',
        'TestInferenceGuardCompletionBeforeAcknowledgementSettlesOnlyExactTurn',
        'TestInferenceGuardInternalRPCErrorKeepsUncertainLease',
        'TestInferenceGuardReconnectAtCapacityRecoversKnownTurn',
        'TestInferenceGuardReconnectCapacityExceptionStaysBounded',
        'TestInferenceGuardStaleRootStartCannotBindPendingRequest',
        'TestKimiCopyFallbackRejectsNonFileCredentialTarget',
        'TestKimiMixedCopyRetriesAfterPartialFailure',
        'TestAvailablePressureIsUsedShare',
        'TestCPUPercentIsRateBetweenSnapshots',
        'TestCPUPercentUsesProcessCreationIdentity',
        'TestParseCPUTicksExcludesGuestTime',
        'TestParseCPUTicksSkipsIdleAndIOWait',
        'TestParseDropsZombies',
        'TestParseDropsZombiesWithLongCommands',
        'TestParseHandlesRowsPastTheDefaultScannerLimit',
        'TestParseKeepsFullCommandLineAndCapsIt',
        'TestParseLoadavgDividesByItsOwnScale',
        'TestParsePSISome10',
        'TestParseReadsOptionalCPUTime',
        'TestParseRejectsMalformedRow',
        'TestParseSwapUsageReadsTotalAndUsedFromXswUsage',
        'TestParseVMStatComputesAvailableAsTotalLessMemoryUsed',
        'TestParseVMStatDistinguishesSwapFromOrdinaryPaging',
        'TestParseVMStatRejectsOutputWithNoPageSize',
        'TestTreeCountsSharedDescendantsOnce',
        'TestTreeMissingRootIsEmpty',
        'TestTreeSumsDescendantsOnly',
        'TestTreeUsesKnownProcessCreationOrder',
        'TestConflictingThreadOwnershipCannotBeReintroducedByThirdBinding',
        'TestNativeResponseCountersIgnoreCumulativeAndBindExactContext',
        'TestObserverChargesMalformedAndOversizedReadsWithoutAdvancingCheckpoint',
        'TestObserverChargesPartialRecordsToSweepBudgetAndDoesNotStarveFinalFile',
        'TestObserverFortyThreadsPartialAppendRestartAndForeignFile',
        'TestObserverPreservesCheckpointOnOrdinaryAppend',
        'TestObserverRejectsMismatchedNativeIdentityAndCancellation',
        'TestObserverReplaysObservableFileRewrite',
        'TestReceiptBeforeAcknowledgementRetriesWithoutReplayingFile',
        'TestUnknownUsageDoesNotBecomeZero',
        'TestWindowsSnapshotRejectsUncertainHandoff',
        'TestWindowsSnapshotEnumerationFailuresRemainUnknown',
        'TestObserverAuthenticatesPrefixBeforeCachedState',
        'TestObserverPrefixVerificationMakesBoundedProgress',
        'TestWindowsHandleCountRequiresSnapshotIdentity',
        'TestObserverFutureDatedUnboundReceiptDoesNotPersist',
        'TestParserPendingReceiptExpiryWindow'
    )
    $selectedTop = @()
    $expectedPackages = @('./internal/adapters/agent/kimi','./internal/adapters/chatdriver/codexappserver','./internal/session_manager','./internal/procmem','./internal/nativereceipts','./internal/fleetpressure')
    if (-not (Test-Exact $expectedPackages @($selection.runs | ForEach-Object { $_.packages }))) { throw 'Unexpected test packages' }
    foreach ($definition in $selection.runs) {
        if ($definition.packages.Count -ne 1 -or $definition.package_ids.Count -ne 1 -or $definition.package_ids[0] -cne ('github.com/aoagents/agent-orchestrator/backend/'+$definition.packages[0].Substring(2))) { throw 'Package identity mismatch' }
        $prefix = $definition.package_ids[0]+'::'
        $tops = @()
        foreach ($id in $definition.test_ids) {
            if (-not $id.StartsWith($prefix,[StringComparison]::Ordinal)) { throw 'Unexpected test package identity' }
            $test = $id.Substring($prefix.Length)
            if ($test -cnotmatch '^Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_=-]+)*$') { throw 'Unexpected test identity syntax' }
            if ($test -notmatch '/') { $tops += $test }
        }
        if ($definition.test_ids.Count -ne @($definition.test_ids | Select-Object -Unique).Count -or $definition.regex -cne ('^('+(@($tops | Sort-Object -CaseSensitive)-join '|')+')$')) { throw 'Unbounded or duplicate test selection' }
        $selectedTop += $tops
    }
    if (-not (Test-Exact $safeTop $selectedTop)) { throw 'The selected top-level tests require a separate safety review' }
    $tracked = Read-Git @('ls-files','-z','--','backend','.github/workflows/offline-windows.yml','scripts/test-offline-windows.ps1','scripts/offline-windows-tests.json')
    $SourcePaths = @($tracked.Split([char]0,[StringSplitOptions]::RemoveEmptyEntries) | Sort-Object)
    $SourceBefore = @(Get-SourceRecords)
    Write-Json (Join-Path $RunRoot 'source-before.json') $SourceBefore
    [IO.File]::Copy($SelectionPath,(Join-Path $RunRoot 'selection.json'),$false)
    [IO.File]::Copy($PSCommandPath,(Join-Path $RunRoot 'runner.ps1'),$false)
    [IO.File]::Copy((Join-Path $Repo '.github/workflows/offline-windows.yml'),(Join-Path $RunRoot 'workflow.yml'),$false)
    $versionJson = Invoke-GoPhase 'toolchain' @('env','-json','GOVERSION','GOOS','GOARCH','GOCACHE','GOMODCACHE','GOROOT') $Common 30
    $version = ConvertFrom-Json -InputObject $versionJson -AsHashtable
    if ($version.GOVERSION -cne 'go1.27.4' -or $version.GOOS -cne 'windows' -or $version.GOARCH -cne 'amd64' -or $version.GOROOT -ine $GoRoot) { throw 'Unexpected pinned Go toolchain' }
    if ($DownloadModules) {
        $bootstrap = $Common.Clone()
        $bootstrap.GOPROXY = 'https://proxy.golang.org'
        $bootstrap.GOSUMDB = 'sum.golang.org'
        [void](Invoke-GoPhase 'module-download' @('mod','download') $bootstrap 180)
    }
    [void](Invoke-GoPhase 'module-verify' @('mod','verify') $Common 60)
    foreach ($definition in $selection.runs) {
        $arguments = @('test','-json','-mod=readonly','-buildvcs=false','-p=1','-parallel=1','-count=1','-timeout=3m','-run',$definition.regex)+@($definition.packages)
        [void](Invoke-GoPhase $definition.name $arguments $Common 360 $definition)
    }
} catch {
    $Failures.Add([string]$_.Exception.Message)
} finally {
    if ($GitReady) { try { Assert-Source 'final' } catch { $Failures.Add([string]$_.Exception.Message) } }
    if ($SourceBefore.Count -gt 0) {
        try {
            $after = @(Get-SourceRecords)
            Write-Json (Join-Path $RunRoot 'source-after.json') $after
            if ((ConvertTo-Json -InputObject $SourceBefore -Depth 4 -Compress) -cne (ConvertTo-Json -InputObject $after -Depth 4 -Compress)) { throw 'Source file hashes changed' }
        } catch { $Failures.Add([string]$_.Exception.Message) }
    }
    if ($GuardBefore.Length -gt 0) {
        try { Assert-Fixtures; [IO.File]::WriteAllText((Join-Path $RunRoot 'outer-profile-after.json'),(Get-FixtureRecord),$Utf8) } catch { $Failures.Add([string]$_.Exception.Message) }
    }
    Write-Json (Join-Path $RunRoot 'source-checks.json') @($SourceChecks.ToArray())
    $expectedPhases = if ($DownloadModules) { 9 } else { 8 }
    $pass = $Failures.Count -eq 0 -and $Phases.Count -eq $expectedPhases -and @($Phases | Where-Object { $_.result -ne 'PASS' }).Count -eq 0
    $result = [ordered]@{
        schema='ao-offline-windows-source-validation-v1'; result=if($pass){'PASS'}else{'FAIL'}; expected_head=$ExpectedHead; repository=$Repo; evidence=$RunRoot; fixture_root=$StateRoot;
        started_utc=$Started; finished_utc=[DateTime]::UtcNow.ToString('o'); selection_sha256=$SelectionSHA256; runner_sha256=(Get-FileHash -LiteralPath $PSCommandPath).Hash;
        expected_coverage=[ordered]@{top=47;nested=102;leaf=134}; dependency_bootstrap_enabled=[bool]$DownloadModules; phases=@($Phases.ToArray()); failures=@($Failures.ToArray());
        scope='Finite synthetic source tests only: Kimi file copy/retry, in-memory bound-turn reconnect, pure process/worker metrics, injected Windows fleet handle-identity checks, and receipt recovery/expiry in temporary files with memory sinks. No native worker fixture, AO, ConPTY, daemon, provider/authentication or real-profile qualification.'
    }
    Write-Json (Join-Path $RunRoot 'result.json') $result
    [Console]::WriteLine((ConvertTo-Json -InputObject ([ordered]@{result=$result.result; expected_head=$ExpectedHead; evidence=$RunRoot; phases=$Phases.Count; failures=@($Failures.ToArray())}) -Depth 4))
}
if (-not $pass) { exit 1 }
