#Requires -Version 7.4
[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$asts = @{}
$sources = @{}
foreach ($file in Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1' -File) {
    $tokens = $null
    $parseErrors = $null
    $ast = [Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -ne 0) { throw "PowerShell syntax failure in $($file.Name): $parseErrors" }
    $asts[$file.Name] = $ast
    $sources[$file.Name] = Get-Content -LiteralPath $file.FullName -Raw
    $commands = $ast.FindAll({ param($n) $n -is [Management.Automation.Language.CommandAst] }, $true)
    foreach ($command in $commands) {
        if ($command.GetCommandName() -in @('Stop-Process', 'taskkill', 'taskkill.exe', 'Invoke-Expression', 'iex', 'Remove-Item')) {
            throw "Destructive/dynamic command is outside this candidate contract: $($file.Name)"
        }
    }
}
$build = $asts['Build-Candidate.ps1']
$goCalls = @($build.FindAll({
    param($n)
    $n -is [Management.Automation.Language.CommandAst] -and $n.CommandElements[0].Extent.Text -eq '$goTool'
}, $true))
if ($goCalls.Count -ne 4) { throw 'Build must invoke only Go version, two builds, and compile-only go test.' }
foreach ($call in $goCalls) {
    $parts = @($call.CommandElements | ForEach-Object { $_.Extent.Text })
    if ($parts[1] -notin @('version', 'build', 'test') -or ($parts[1] -eq 'test' -and '-c' -notin $parts)) {
        throw 'Build would execute a test or unqualified Go subcommand.'
    }
}
if ($sources['Build-Candidate.ps1'] -notmatch 'inferenceguard\.RequirePolicy=1' -or
    $sources['Build-Candidate.ps1'] -notmatch 'cli\.Version=' -or
    $sources['Build-Candidate.ps1'] -notmatch 'cli\.Commit=' -or
    $sources['Build-Candidate.ps1'] -notmatch 'cli\.Date=' -or
    $sources['Build-Candidate.ps1'] -notmatch 'admissionOfflineCompanion=1' -or
    $sources['Build-Candidate.ps1'] -notmatch 'admissionPackagedHelperSHA256=') {
    throw 'Strict or companion build identity is missing.'
}
$buildCommands = @($build.FindAll({ param($n) $n -is [Management.Automation.Language.CommandAst] }, $true) | ForEach-Object { $_.GetCommandName() })
if (@($buildCommands | Where-Object { $_ -in @('Start-Process', 'Invoke-RestMethod', 'Invoke-WebRequest') }).Count -ne 0) {
    throw 'Build contains a daemon/network launch command.'
}
$run = $asts['Run-Offline-Fleet.ps1']
$fleetCalls = @($run.FindAll({
    param($n)
    $n -is [Management.Automation.Language.CommandAst] -and $n.CommandElements[0].Extent.Text -eq '$fleetExe'
}, $true))
if ($fleetCalls.Count -ne 1 -or $fleetCalls[0].CommandElements.Count -ne 1 -or $run.ParamBlock.Parameters.Count -ne 0) {
    throw 'Offline fleet must execute its compiled allowlist with no external arguments.'
}
if ($sources['Run-Offline-Fleet.ps1'].IndexOf('Assert-CandidateBundle') -gt $fleetCalls[0].Extent.StartOffset) {
    throw 'Offline executable is used before hash verification.'
}
$start = $asts['Start-PostRefresh-Validation.ps1']
$starts = @($start.FindAll({ param($n) $n -is [Management.Automation.Language.CommandAst] -and $n.GetCommandName() -eq 'Start-Process' }, $true))
if ($starts.Count -ne 1) { throw 'Post-refresh startup must launch exactly one candidate process.' }
$startText = $sources['Start-PostRefresh-Validation.ps1']
foreach ($guard in @('if (-not $AuthorizePostRefreshValidation)', '2026-11-21T12:30:00Z', 'Assert-CandidateBundle', 'Assert-CandidateOwnerPolicy', 'Assert-CandidateACLAncestors', 'Initialize-CandidatePrivateRoot', 'New-Item -ItemType Directory')) {
    $at = $startText.IndexOf($guard)
    if ($at -lt 0 -or $at -gt $starts[0].Extent.StartOffset) { throw "Missing pre-launch gate: $guard" }
}
foreach ($required in @("['AO_ENABLE_ALLOWANCE_CHECK'] = '0'", "['AO_REQUIRE_INFERENCE_POLICY'] = '1'",
    "['AO_PORT'] = '39431'", "['AO_OWNER'] = 'app'", "-ArgumentList @('daemon')")) {
    if (-not $startText.Contains($required)) { throw "Missing fixed startup boundary: $required" }
}
$startHTTP = @($start.FindAll({ param($n) $n -is [Management.Automation.Language.CommandAst] -and $n.GetCommandName() -in @('Invoke-RestMethod', 'Invoke-WebRequest') }, $true))
foreach ($call in $startHTTP) {
    if ($call.Extent.Text -notmatch '-Method Get') { throw 'Startup must not mutate HTTP state or request a model turn.' }
}
$stopText = $sources['Stop-Candidate.ps1']
if ($stopText -match '-Method Put' -or $stopText -match '/internal/reliability/inference') {
    throw 'Stop must not mutate an endpoint without the server-side run-ID fence.'
}
foreach ($required in @('Get-CandidateOwnedProcess', 'Get-CandidateOwnedRun', 'stopAgents = $true', 'appRunId = $record.runId',
    "'http://127.0.0.1:39431/shutdown'", 'StatusCode -ne 202', 'daemonExitObserved')) {
    if (-not $stopText.Contains($required)) { throw "Missing owned graceful stop boundary: $required" }
}
$commonText = $sources['Candidate.Common.ps1']
foreach ($required in @('processStartTicksUtc', 'executableSHA256', 'Get-NetTCPConnection', 'OwningProcess',
    '$running.appRunId -cne $Record.runId', 'ReparsePoint', 'manifest.sha256', 'GetPathRoot', '0x500D0040', 'Get-Acl', 'SetAccessRuleProtection')) {
    if (-not $commonText.Contains($required)) { throw "Missing ownership/hash validation: $required" }
}
if ($commonText -match '\.SetOwner\(' -or $commonText -match '\.SetAudit' -or $commonText -match 'RunAs') {
    throw 'Candidate must not change owner/SACL or elevate.'
}
$aclWrites = @($asts['Candidate.Common.ps1'].FindAll({
    param($n)
    $n -is [Management.Automation.Language.CommandAst] -and $n.GetCommandName() -eq 'Set-Acl'
}, $true))
if ($aclWrites.Count -ne 1 -or $aclWrites[0].Extent.Text -notmatch '-LiteralPath \$root -AclObject \$acl') {
    throw 'Only the freshly created candidate root DACL may be modified.'
}
$policy = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'policy.template.json') -Raw | ConvertFrom-Json -AsHashtable
if ($policy.native_account_sha256 -ne '' -or -not $policy.emergency_stop -or -not $policy.pause -or
    $policy.maximum_effort -cne 'medium' -or $policy.maximum_inference -ne 1 -or
    ($policy.turn_budget - $policy.reserve_turns) -ne 1 -or
    $policy.authority_path -cne 'C:\AO-MTechLabs-Private-20261116\authority\codex-owner-inference.sqlite') {
    throw 'Initial policy template is not fail-closed and globally shared.'
}
foreach ($role in @('orchestrator', 'implementation', 'complex', 'reviewer', 'lightweight')) {
    if (-not $policy.roles.ContainsKey($role) -or $policy.roles[$role].default_model -cnotin $policy.roles[$role].models) {
        throw "Invalid role policy: $role"
    }
    foreach ($model in $policy.roles[$role].models) {
        if ($model -match '(?i)luna') { throw 'Template prematurely permits Luna.' }
    }
}
Write-Output "PASS: parsed $($asts.Count) PowerShell scripts; build is compile-only, fleet arguments are fixed, startup gates and owned stop checks are present, template is structurally valid."
Write-Output 'Scope: AST/static validation only. No candidate executable, daemon, native account endpoint or model provider was invoked.'
