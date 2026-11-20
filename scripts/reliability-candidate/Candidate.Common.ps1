# This file defines helpers only. Callers require PowerShell 7.4 or newer.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-CandidateLocalPath {
    param([Parameter(Mandatory)][string]$Path)
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or $Path.StartsWith('\\') -or $Path -notmatch '^[A-Za-z]:\\') {
        throw 'An absolute local Windows drive path is required.'
    }
    $full = [IO.Path]::GetFullPath($Path)
    if ($full.Length -gt [IO.Path]::GetPathRoot($full).Length) { $full = $full.TrimEnd('\') }
    $check = $full
    while ($check) {
        if (Test-Path -LiteralPath $check) {
            $item = Get-Item -LiteralPath $check -Force
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "Reparse-point path refused: $check"
            }
        }
        $parent = [IO.Path]::GetDirectoryName($check)
        if ($parent -eq $check) { break }
        $check = $parent
    }
    return $full
}

function Assert-CandidateBundle {
    param([Parameter(Mandatory)][string]$Root)
    $rootPath = Get-CandidateLocalPath $Root
    if (-not $rootPath.StartsWith('C:\AO-MTechLabs-Releases-20261116\', [StringComparison]::OrdinalIgnoreCase) -or
        [IO.Path]::GetFileName($rootPath) -notmatch '^ao-unofficial-\d{8}T\d{6}Z-[0-9a-f]{12}$') {
        throw 'Candidate must be a timestamped commit directory under C:\AO-MTechLabs-Releases-20261116.'
    }
    # Gate bundle validation and every runner's executable dispatch.
    Assert-CandidateACLAncestors $rootPath
    $manifestPath = Get-CandidateLocalPath (Join-Path $rootPath 'manifest.json')
    $digestPath = Get-CandidateLocalPath (Join-Path $rootPath 'manifest.sha256')
    $null = Assert-CandidateACL $manifestPath -File
    $null = Assert-CandidateACL $digestPath -File
    $expected = (Get-Content -LiteralPath $digestPath -Raw).Trim()
    if ($expected -notmatch '^[0-9a-f]{64}$' -or
        (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant() -cne $expected) {
        throw 'Candidate manifest digest mismatch.'
    }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json -AsHashtable
    if ($manifest.schemaVersion -ne 1 -or $manifest.sourceCommit -notmatch '^[0-9a-f]{40}$' -or
        $manifest.strictPolicy -ne $true -or $manifest.offlineCompanion.allowlistEnforced -ne $true -or
        $manifest.offlineConPTY.allowlistEnforced -ne $true -or
        $manifest.offlineDaemon.allowlistEnforced -ne $true -or $manifest.unsigned -ne $true -or
        $manifest.offlineDaemon.sourcePinned -cne $manifest.sourceCommit) {
        throw 'Candidate manifest is missing strict build identity.'
    }
    foreach ($required in @('AO.exe', 'ao-offline-worker.exe', 'ao-offline-fleet.exe', 'ao-offline-conpty.exe', 'ao-offline-daemon.exe', 'Candidate.Common.ps1',
        'Run-Offline-Fleet.ps1', 'Run-Offline-ConPTY.ps1', 'Run-Offline-Daemon.ps1', 'Start-PostRefresh-Validation.ps1', 'Stop-Candidate.ps1', 'policy.template.json')) {
        if (-not $manifest.files.ContainsKey($required)) { throw "Missing candidate file record: $required" }
    }
    foreach ($name in $manifest.files.Keys) {
        if ($name -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Manifest contains a nonlocal filename.' }
        $file = Get-CandidateLocalPath (Join-Path $rootPath $name)
        $null = Assert-CandidateACL $file -File
        $record = $manifest.files[$name]
        if ($record.sha256 -notmatch '^[0-9a-f]{64}$' -or -not (Test-Path -LiteralPath $file -PathType Leaf) -or
            (Get-Item -LiteralPath $file).Length -ne $record.bytes -or
            (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -cne $record.sha256) {
            throw "Candidate file digest/size mismatch: $name"
        }
    }
    return @{ Root = $rootPath; Manifest = $manifest; ManifestSHA256 = $expected }
}

function Assert-CandidateOwnerPolicy {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$VerifiedNativeAccountSHA256)
    $policyPath = Get-CandidateLocalPath $Path
    if ($VerifiedNativeAccountSHA256 -notmatch '^[0-9a-fA-F]{64}$') { throw 'An explicitly verified native account fingerprint is required.' }
    $p = Get-Content -LiteralPath $policyPath -Raw | ConvertFrom-Json -AsHashtable
    if ($p.version -ne 1 -or [string]::IsNullOrWhiteSpace($p.account_key) -or
        [string]::IsNullOrWhiteSpace($p.authorization_epoch)) { throw 'Owner policy identity is incomplete.' }
    if ($p.native_account_sha256 -notmatch '^[0-9a-fA-F]{64}$' -or
        $p.native_account_sha256.ToLowerInvariant() -cne $VerifiedNativeAccountSHA256.ToLowerInvariant()) {
        throw 'Policy fingerprint is blank or differs from the explicitly verified native account.'
    }
    if ($p.emergency_stop -isnot [bool] -or -not $p.emergency_stop -or
        $p.pause -isnot [bool] -or -not $p.pause) { throw 'Initial policy requires emergency_stop=true and pause=true.' }
    if ($p.maximum_effort -cne 'medium' -or $p.maximum_inference -ne 1 -or
        $p.maximum_per_project -ne 1 -or $p.maximum_reviewers -ne 1) {
        throw 'Initial validation requires Medium maximum and inference/project/reviewer caps of one.'
    }
    if ($p.turn_budget -lt 2 -or $p.reserve_turns -lt 1 -or ($p.turn_budget - $p.reserve_turns) -ne 1) {
        throw 'Initial validation must reserve all but one outer turn; this is not a billable-call ceiling.'
    }
    if ($p.ContainsKey('exceptions') -and $p.exceptions.Count -ne 0) { throw 'Initial validation permits no effort exceptions.' }
    $authority = Get-CandidateLocalPath $p.authority_path
    if (-not $authority.Equals('C:\AO-MTechLabs-Private-20261116\authority\codex-owner-inference.sqlite', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Candidate versions must share the stable account authority path from the template.'
    }
    foreach ($role in @('orchestrator', 'implementation', 'complex', 'reviewer', 'lightweight')) {
        if (-not $p.roles.ContainsKey($role)) { throw "Missing role: $role" }
        $r = $p.roles[$role]
        if ([string]::IsNullOrWhiteSpace($r.default_model) -or $r.default_model -cnotin $r.models) {
            throw "Role has no explicit allowed default: $role"
        }
        foreach ($model in $r.models) {
            if ([string]::IsNullOrWhiteSpace($model) -or $model -match '(?i)luna') {
                throw 'Luna remains excluded pending fitness evidence and an explicit owner policy revision.'
            }
        }
    }
    return @{ Path = $policyPath; Policy = $p; SHA256 = (Get-FileHash -LiteralPath $policyPath -Algorithm SHA256).Hash.ToLowerInvariant() }
}

function Assert-CandidateACL {
    param([Parameter(Mandatory)][string]$Path, [switch]$Private, [switch]$CurrentOwner, [switch]$File)
    $currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $trusted = @($currentSid, 'S-1-5-18', 'S-1-5-32-544',
        'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $raw = [Security.AccessControl.RawSecurityDescriptor]::new($acl.GetSecurityDescriptorBinaryForm(), 0)
    if ($null -eq $raw.Owner -or $raw.Owner.Value -notin $trusted -or
        ($CurrentOwner -and $raw.Owner.Value -cne $currentSid) -or $null -eq $raw.DiscretionaryAcl) {
        throw "AO_ACL_OWNER_OR_DACL_UNVERIFIED: $Path"
    }
    # Matches codexWindowsAncestorMutationMask: generic write/all, DELETE,
    # DELETE_CHILD, WRITE_DAC, WRITE_OWNER. Specific create-file/subdirectory
    # rights on a drive ancestor are not treated as delete/replace authority.
    $ancestorMutation = [long]0x500D0040
    # File write/append and write-EA/attributes can change a verified payload
    # without replacing its directory entry. Keep directory create-only rights
    # permitted: the same 0x2/0x4 bits mean add-file/add-subdirectory there.
    $mutationMask = if ($File) { $ancestorMutation -bor 0x116L } else { $ancestorMutation }
    foreach ($ace in $raw.DiscretionaryAcl) {
        if (($ace.AceFlags -band [Security.AccessControl.AceFlags]::InheritOnly) -ne 0) { continue }
        $kind = [int]$ace.AceType
        if ($kind -in @(1, 6, 10, 12)) { continue } # deny ACEs cannot grant access
        if ($kind -ne 0 -or $ace -isnot [Security.AccessControl.CommonAce]) {
            throw "AO_ACL_UNSUPPORTED_ALLOW_ACE: $Path"
        }
        $sid = $ace.SecurityIdentifier.Value
        $mask = ([long]$ace.AccessMask) -band 0xFFFFFFFFL
        if ($sid -notin $trusted -and (($Private -and $mask -ne 0) -or (($mask -band $mutationMask) -ne 0))) {
            $reason = if ($Private) { 'AO_ACL_UNTRUSTED_PRIVATE_ALLOW' }
                elseif ($File) { 'AO_ACL_UNTRUSTED_FILE_MUTATION_ALLOW' }
                else { 'AO_ACL_UNTRUSTED_ANCESTOR_MUTATION_ALLOW' }
            throw "${reason}: $Path (no permission changes made)"
        }
    }
    return $acl
}

function Assert-CandidateACLAncestors {
    param([Parameter(Mandatory)][string]$Path)
    $check = Get-CandidateLocalPath $Path
    while ($check) {
        if (Test-Path -LiteralPath $check) { $null = Assert-CandidateACL $check }
        $parent = [IO.Path]::GetDirectoryName($check)
        if ($parent -eq $check) { break }
        $check = $parent
    }
}

function Initialize-CandidateReleaseRoot {
    $root = Get-CandidateLocalPath 'C:\AO-MTechLabs-Releases-20261116'
    Assert-CandidateACLAncestors 'C:\'
    if (Test-Path -LiteralPath $root) {
        $acl = Assert-CandidateACL $root -Private -CurrentOwner
        if (-not $acl.AreAccessRulesProtected) {
            throw 'Existing candidate release root is not private and protected; no ACL changes were made.'
        }
        Assert-CandidateACLAncestors $root
        return $root
    }
    New-Item -ItemType Directory -Path $root -ErrorAction Stop | Out-Null
    $acl = Get-Acl -LiteralPath $root -ErrorAction Stop
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -cne $sid.Value) {
        throw 'New candidate release root owner differs; no ACL change was attempted.'
    }
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.Access)) { $acl.RemoveAccessRuleSpecific($rule) }
    foreach ($principal in @($sid.Value, 'S-1-5-18', 'S-1-5-32-544')) {
        $entry = [Security.AccessControl.FileSystemAccessRule]::new(
            [Security.Principal.SecurityIdentifier]::new($principal),
            [Security.AccessControl.FileSystemRights]::FullControl,
            [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
            [Security.AccessControl.PropagationFlags]::None,
            [Security.AccessControl.AccessControlType]::Allow)
        $acl.AddAccessRule($entry)
    }
    Set-Acl -LiteralPath $root -AclObject $acl -ErrorAction Stop
    $verified = Assert-CandidateACL $root -Private -CurrentOwner
    if (-not $verified.AreAccessRulesProtected) { throw 'Release root DACL protection failed.' }
    Assert-CandidateACLAncestors $root
    return $root
}

function Initialize-CandidatePrivateRoot {
    param([Parameter(Mandatory)]$Owner)
    $root = 'C:\AO-MTechLabs-Private-20261116'
    $marker = Join-Path $root 'root-identity.json'
    $policyPath = Join-Path $root 'owner-policy.json'
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    Assert-CandidateACLAncestors 'C:\'
    if (Test-Path -LiteralPath $root) {
        $null = Get-CandidateLocalPath $root
        $acl = Assert-CandidateACL $root -Private -CurrentOwner
        if (-not $acl.AreAccessRulesProtected -or -not (Test-Path -LiteralPath $marker -PathType Leaf)) {
            throw 'Existing private-root candidate was not created by this procedure; it will not be modified.'
        }
        $prior = Get-Content -LiteralPath $marker -Raw | ConvertFrom-Json -AsHashtable
        if ($prior.purpose -cne 'AO MTechLabs isolated reliability authority' -or $prior.ownerSID -cne $sid.Value -or
            $prior.accountKey -cne $Owner.Policy.account_key -or
            $prior.nativeAccountSHA256 -cne $Owner.Policy.native_account_sha256.ToLowerInvariant() -or
            -not $Owner.Path.Equals($policyPath, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'Existing private root/account does not match; use its preserved owner policy and do not reset its authority.'
        }
        Assert-CandidateACLAncestors $policyPath
        $null = Assert-CandidateACL $policyPath -Private -CurrentOwner
    } else {
        # New-Item has no -Force: an existing/racing directory is never adopted.
        New-Item -ItemType Directory -Path $root -ErrorAction Stop | Out-Null
        $null = Get-CandidateLocalPath $root
        $acl = Get-Acl -LiteralPath $root -ErrorAction Stop
        if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -cne $sid.Value) {
            throw 'New private directory owner changed; no ACL update will be attempted.'
        }
        # Modify only the new directory's DACL. Do not set owner/SACL, elevate,
        # repair ancestors, or touch an existing profile/native credential ACL.
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
        Set-Acl -LiteralPath $root -AclObject $acl -ErrorAction Stop
        $null = Assert-CandidateACL $root -Private -CurrentOwner
        Assert-CandidateACLAncestors $root
        # This copies only the already validated owner policy, never auth files.
        Write-CandidateJSONNew $policyPath $Owner.Policy
        Write-CandidateJSONNew $marker @{
            purpose = 'AO MTechLabs isolated reliability authority'; ownerSID = $sid.Value
            accountKey = $Owner.Policy.account_key; nativeAccountSHA256 = $Owner.Policy.native_account_sha256.ToLowerInvariant()
            createdAtUtc = [DateTimeOffset]::UtcNow.ToString('o')
        }
    }
    $privateOwner = Assert-CandidateOwnerPolicy $policyPath $Owner.Policy.native_account_sha256
    return @{ Root = $root; Owner = $privateOwner }
}

function Write-CandidateJSONNew {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)]$Value)
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes(($Value | ConvertTo-Json -Depth 20) + [Environment]::NewLine)
    $stream = [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::Read)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
}

function Get-CandidateOwnedProcess {
    param([Parameter(Mandatory)]$Record)
    if ($Record.port -ne 39431 -or $Record.runId -notmatch '^reliability-[0-9a-f]{32}$' -or $Record.processId -lt 1) {
        throw 'Run record has an invalid process identity.'
    }
    $process = Get-Process -Id $Record.processId -ErrorAction Stop
    $actual = Get-CandidateLocalPath $process.Path
    $expected = Get-CandidateLocalPath $Record.executablePath
    if (-not $actual.Equals($expected, [StringComparison]::OrdinalIgnoreCase) -or
        $process.StartTime.ToUniversalTime().Ticks -ne [long]$Record.processStartTicksUtc) {
        throw 'PID reuse or executable identity mismatch; no action was taken.'
    }
    if ((Get-FileHash -LiteralPath $expected -Algorithm SHA256).Hash.ToLowerInvariant() -cne $Record.executableSHA256) {
        throw 'Running candidate executable digest changed; no action was taken.'
    }
    return $process
}

function Get-CandidateOwnedRun {
    param([Parameter(Mandatory)]$Record)
    $process = Get-CandidateOwnedProcess $Record
    $running = Get-Content -LiteralPath $Record.runFile -Raw | ConvertFrom-Json -AsHashtable
    if ($running.pid -ne $Record.processId -or $running.port -ne 39431 -or $running.appRunId -cne $Record.runId) {
        throw 'Run-file PID, port or run ID mismatch; no action was taken.'
    }
    $readyAt = ([DateTimeOffset]$running.startedAt).UtcDateTime
    if ($readyAt -lt $process.StartTime.ToUniversalTime().AddSeconds(-1) -or $readyAt -gt [DateTime]::UtcNow.AddSeconds(1)) {
        throw 'Run-file start time is inconsistent with the process.'
    }
    $listeners = @(Get-NetTCPConnection -State Listen -LocalPort 39431 -ErrorAction Stop)
    if ($listeners.Count -eq 0) { throw 'Candidate has no owned listener.' }
    foreach ($listener in $listeners) {
        if ($listener.OwningProcess -ne $Record.processId -or $listener.LocalAddress -notin @('127.0.0.1', '::1')) {
            throw 'Port is owned by a different process or is not loopback; no action was taken.'
        }
    }
    $health = Invoke-RestMethod -Uri 'http://127.0.0.1:39431/healthz' -Method Get -TimeoutSec 3 -NoProxy
    if ($health.status -ne 'ok' -or $health.service -ne 'agent-orchestrator-daemon' -or
        $health.pid -ne $Record.processId -or
        -not (Get-CandidateLocalPath $health.executablePath).Equals($Record.executablePath, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Loopback health identity does not match this candidate.'
    }
    $again = Get-Content -LiteralPath $Record.runFile -Raw | ConvertFrom-Json -AsHashtable
    if ($again.pid -ne $Record.processId -or $again.port -ne 39431 -or $again.appRunId -cne $Record.runId) {
        throw 'Candidate ownership changed while checking health.'
    }
    return $health
}
