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
$allowanceParameter = @($start.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -ceq 'EnableAllowanceObservation' })
if ($allowanceParameter.Count -ne 1 -or $null -ne $allowanceParameter[0].DefaultValue -or
    @($allowanceParameter[0].Attributes | Where-Object { $_.TypeName.FullName -ceq 'switch' }).Count -ne 1) {
    throw 'Allowance observation must be an explicit switch with no enabled default.'
}
$allowanceAssignments = @($start.FindAll({
    param($n)
    $n -is [Management.Automation.Language.AssignmentStatementAst] -and
        $n.Left.Extent.Text -ceq '$childEnv[''AO_ENABLE_ALLOWANCE_CHECK'']'
}, $true) | Sort-Object { $_.Extent.StartOffset })
if ($allowanceAssignments.Count -ne 2 -or $allowanceAssignments[0].Right.Extent.Text -cne "'0'" -or
    $allowanceAssignments[1].Right.Extent.Text -cne "'1'") {
    throw 'Allowance observation requires a literal disabled default and exactly one opt-in assignment.'
}
$enableIf = $allowanceAssignments[1].Parent
while ($null -ne $enableIf -and $enableIf -isnot [Management.Automation.Language.IfStatementAst]) { $enableIf = $enableIf.Parent }
if ($null -eq $enableIf -or $enableIf.Clauses.Count -ne 1 -or $null -ne $enableIf.ElseClause -or
    $enableIf.Clauses[0].Item1.Extent.Text -cne '$EnableAllowanceObservation' -or
    $enableIf.Clauses[0].Item2.Statements.Count -ne 1) {
    throw 'Only the explicit allowance-observation switch may enable the bridge; it must not change policy.'
}
foreach ($guard in @('if (-not $AuthorizePostRefreshValidation)', '2026-11-21T12:30:00Z',
    'Assert-CandidateBundle', 'Assert-CandidateOwnerPolicy', 'Assert-CandidateACLAncestors', 'Initialize-CandidatePrivateRoot')) {
    $at = $startText.IndexOf($guard)
    if ($at -lt 0 -or $at -gt $allowanceAssignments[1].Extent.StartOffset) {
        throw "Allowance opt-in precedes required gate: $guard"
    }
}
if ($startText -match '/allowance/verify' -or
    -not $startText.Contains('[bool]$state.allowanceCheckEnabled -ne [bool]$EnableAllowanceObservation') -or
    -not $startText.Contains('allowanceCheckEnabled = [bool]$EnableAllowanceObservation')) {
    throw 'Startup must observe the requested allowance switch without invoking the verification endpoint.'
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
    $policy.turn_budget -ne 2 -or $policy.reserve_turns -ne 1 -or
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
Write-Output "PASS: parsed $($asts.Count) PowerShell scripts; build is compile-only, fleet arguments are fixed, startup gates, default-off allowance opt-in and owned stop checks are present, template is structurally valid."
Write-Output 'Scope: AST/static validation only. No candidate executable, daemon, native account endpoint or model provider was invoked.'
