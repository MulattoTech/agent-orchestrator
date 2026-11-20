#Requires -Version 7.4
# Synthetic descriptors only: no Windows filesystem ACLs or credentials are read or changed.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Candidate.Common.ps1')

$script:descriptorBytes = [byte[]]::new(0)
function Get-Acl {
    param([string]$LiteralPath)
    $fake = [pscustomobject]@{}
    $fake | Add-Member -MemberType ScriptMethod -Name GetSecurityDescriptorBinaryForm -Value {
        return $script:descriptorBytes
    }
    return $fake
}

function Test-SyntheticACL {
    param([string]$Label, [string]$Sddl, [string]$Expected, [switch]$Private, [switch]$CurrentOwner, [switch]$File)
    $descriptor = [Security.AccessControl.RawSecurityDescriptor]::new($Sddl)
    $script:descriptorBytes = [byte[]]::new($descriptor.BinaryLength)
    $descriptor.GetBinaryForm($script:descriptorBytes, 0)
    $observed = 'PASS'
    try {
        $null = Assert-CandidateACL -Path 'C:\SyntheticACLFixture' -Private:$Private -CurrentOwner:$CurrentOwner -File:$File
    } catch {
        if ($_.Exception.Message -match '^(AO_ACL_[A-Z_]+):') {
            $observed = $Matches[1]
        } else {
            throw "Unexpected error in $Label (type=$($_.Exception.GetType().Name))"
        }
    }
    if ($observed -cne $Expected) {
        throw "FAIL: $Label expected $Expected, observed $observed"
    }
    Write-Output "PASS: $Label => $observed"
}

Test-SyntheticACL 'trusted owner and allow' 'O:SYG:SYD:P(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'deny-only outsider cannot grant' 'O:SYG:SYD:P(D;;FA;;;WD)(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'outsider read on ancestor' 'O:SYG:SYD:P(A;;FR;;;WD)(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'outsider mutation on ancestor' 'O:SYG:SYD:P(A;;FA;;;WD)(A;;FA;;;SY)' 'AO_ACL_UNTRUSTED_ANCESTOR_MUTATION_ALLOW'
Test-SyntheticACL 'outsider read on private root' 'O:SYG:SYD:P(A;;FR;;;WD)(A;;FA;;;SY)' 'AO_ACL_UNTRUSTED_PRIVATE_ALLOW' -Private
Test-SyntheticACL 'untrusted owner' 'O:WDG:SYD:P(A;;FA;;;SY)' 'AO_ACL_OWNER_OR_DACL_UNVERIFIED'
Test-SyntheticACL 'unsupported callback allow is refused' 'O:SYG:SYD:P(XA;;FR;;;WD;(TRUE))(A;;FA;;;SY)' 'AO_ACL_UNSUPPORTED_ALLOW_ACE'
Test-SyntheticACL 'callback deny cannot grant' 'O:SYG:SYD:P(XD;;FA;;;WD;(TRUE))(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'inherit-only outsider allow ignored' 'O:SYG:SYD:P(A;IO;FA;;;WD)(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'inherited outsider allow on ancestor blocked' 'O:SYG:SYD:(A;ID;FA;;;WD)(A;;FA;;;SY)' 'AO_ACL_UNTRUSTED_ANCESTOR_MUTATION_ALLOW'
Test-SyntheticACL 'system owner fails current-owner gate' 'O:SYG:SYD:P(A;;FA;;;SY)' 'AO_ACL_OWNER_OR_DACL_UNVERIFIED' -CurrentOwner
$currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
Test-SyntheticACL 'current owner passes current-owner gate' "O:${currentSid}G:SYD:P(A;;FA;;;SY)" 'PASS' -CurrentOwner

Test-SyntheticACL 'directory create-only rights' 'O:SYG:SYD:P(A;;0x6;;;WD)(A;;FA;;;SY)' 'PASS'
Test-SyntheticACL 'outsider file read/execute' 'O:SYG:SYD:P(A;;FRFX;;;WD)(A;;FA;;;SY)' 'PASS' -File
foreach ($right in @(
    @{ Name = 'write data'; Mask = '0x2' }, @{ Name = 'append data'; Mask = '0x4' },
    @{ Name = 'write extended attributes'; Mask = '0x10' }, @{ Name = 'write attributes'; Mask = '0x100' },
    @{ Name = 'delete'; Mask = '0x10000' }, @{ Name = 'change DACL'; Mask = '0x40000' },
    @{ Name = 'change owner'; Mask = '0x80000' }, @{ Name = 'generic write'; Mask = '0x40000000' },
    @{ Name = 'generic all'; Mask = '0x10000000' })) {
    Test-SyntheticACL "outsider file $($right.Name)" "O:SYG:SYD:P(A;;$($right.Mask);;;WD)(A;;FA;;;SY)" 'AO_ACL_UNTRUSTED_FILE_MUTATION_ALLOW' -File
}
Test-SyntheticACL 'file deny-only outsider' 'O:SYG:SYD:P(D;;FA;;;WD)(A;;FA;;;SY)' 'PASS' -File
Test-SyntheticACL 'file inherit-only outsider write' 'O:SYG:SYD:P(A;IO;0x2;;;WD)(A;;FA;;;SY)' 'PASS' -File
Test-SyntheticACL 'file inherited outsider write' 'O:SYG:SYD:(A;ID;0x2;;;WD)(A;;FA;;;SY)' 'AO_ACL_UNTRUSTED_FILE_MUTATION_ALLOW' -File
Test-SyntheticACL 'file callback allow is refused' 'O:SYG:SYD:P(XA;;FR;;;WD;(TRUE))(A;;FA;;;SY)' 'AO_ACL_UNSUPPORTED_ALLOW_ACE' -File
Test-SyntheticACL 'file untrusted owner' 'O:WDG:SYD:P(A;;FA;;;SY)' 'AO_ACL_OWNER_OR_DACL_UNVERIFIED' -File
Test-SyntheticACL 'file null DACL' 'O:SYG:SYD:NO_ACCESS_CONTROL' 'AO_ACL_OWNER_OR_DACL_UNVERIFIED' -File
