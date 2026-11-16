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
    param([string]$Label, [string]$Sddl, [string]$Expected, [switch]$Private, [switch]$CurrentOwner)
    $descriptor = [Security.AccessControl.RawSecurityDescriptor]::new($Sddl)
    $script:descriptorBytes = [byte[]]::new($descriptor.BinaryLength)
    $descriptor.GetBinaryForm($script:descriptorBytes, 0)
    $observed = 'PASS'
    try {
        $null = Assert-CandidateACL -Path 'C:\SyntheticACLFixture' -Private:$Private -CurrentOwner:$CurrentOwner
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
