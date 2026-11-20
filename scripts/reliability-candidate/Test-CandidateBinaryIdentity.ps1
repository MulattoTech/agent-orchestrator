#Requires -Version 7.4
# In-memory, deliberately non-runnable PE fixtures. No compiler, executable,
# filesystem write, account, network call or ACL mutation is used.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Candidate.Binary.ps1')

function Set-FixtureUInt16($Bytes, [int]$At, [uint16]$Value) { [BitConverter]::GetBytes($Value).CopyTo($Bytes, $At) }
function Set-FixtureUInt32($Bytes, [int]$At, [uint32]$Value) { [BitConverter]::GetBytes($Value).CopyTo($Bytes, $At) }
function Set-FixtureUInt64($Bytes, [int]$At, [uint64]$Value) { [BitConverter]::GetBytes($Value).CopyTo($Bytes, $At) }

function New-BinaryFixture {
    $bytes = [byte[]]::new(4096)
    $guard = 'example.invalid/guard.RequirePolicy'
    $commit = 'example.invalid/cli.Commit'
    # DOS header, PE signature and COFF header.
    Set-FixtureUInt16 $bytes 0 0x5a4d
    Set-FixtureUInt32 $bytes 0x3c 0x80
    Set-FixtureUInt32 $bytes 0x80 0x00004550
    Set-FixtureUInt16 $bytes 0x84 0x8664
    Set-FixtureUInt16 $bytes 0x86 2
    Set-FixtureUInt32 $bytes 0x8c 0x600
    Set-FixtureUInt32 $bytes 0x90 2
    Set-FixtureUInt16 $bytes 0x94 0xf0
    Set-FixtureUInt16 $bytes 0x96 0x22
    # PE32+ optional header. There is no entry point or code section.
    Set-FixtureUInt16 $bytes 0x98 0x20b
    Set-FixtureUInt64 $bytes 0xb0 0x140000000
    Set-FixtureUInt32 $bytes 0xb8 0x1000
    Set-FixtureUInt32 $bytes 0xbc 0x200
    Set-FixtureUInt32 $bytes 0xd0 0x3000
    Set-FixtureUInt32 $bytes 0xd4 0x200
    Set-FixtureUInt16 $bytes 0xdc 3
    Set-FixtureUInt32 $bytes 0x104 16
    # .data and .rdata have different virtual/raw sizes to catch padding/BSS reads.
    [Text.Encoding]::ASCII.GetBytes('.data').CopyTo($bytes, 0x188)
    Set-FixtureUInt32 $bytes 0x190 0x80
    Set-FixtureUInt32 $bytes 0x194 0x1000
    Set-FixtureUInt32 $bytes 0x198 0x200
    Set-FixtureUInt32 $bytes 0x19c 0x200
    Set-FixtureUInt32 $bytes 0x1ac 3221225536
    [Text.Encoding]::ASCII.GetBytes('.rdata').CopyTo($bytes, 0x1b0)
    Set-FixtureUInt32 $bytes 0x1b8 0x100
    Set-FixtureUInt32 $bytes 0x1bc 0x2000
    Set-FixtureUInt32 $bytes 0x1c0 0x200
    Set-FixtureUInt32 $bytes 0x1c4 0x400
    Set-FixtureUInt32 $bytes 0x1d4 1073741888
    # Two Go string descriptors: 64-bit pointer followed by 64-bit byte length.
    Set-FixtureUInt64 $bytes 0x200 0x140002000
    Set-FixtureUInt64 $bytes 0x208 1
    Set-FixtureUInt64 $bytes 0x210 0x140002020
    Set-FixtureUInt64 $bytes 0x218 40
    $bytes[0x400] = [byte][char]'1'
    [Text.Encoding]::ASCII.GetBytes(('a' * 40)).CopyTo($bytes, 0x420)
    # Standard 18-byte COFF records and their null-terminated long-name table.
    $table = 0x624
    $nameOffset = 4
    $number = 0
    foreach ($name in @($guard, $commit)) {
        $record = 0x600 + $number * 18
        Set-FixtureUInt32 $bytes ($record + 4) $nameOffset
        Set-FixtureUInt32 $bytes ($record + 8) ($number * 16)
        Set-FixtureUInt16 $bytes ($record + 12) 1
        $bytes[$record + 16] = 2
        $encoded = [Text.Encoding]::UTF8.GetBytes($name)
        $encoded.CopyTo($bytes, $table + $nameOffset)
        $nameOffset += $encoded.Length + 1
        $number++
    }
    Set-FixtureUInt32 $bytes $table $nameOffset
    return [pscustomobject]@{
        Bytes = $bytes; Guard = $guard; Commit = $commit
        Expected = [ordered]@{ $guard = '1'; $commit = ('a' * 40) }
    }
}

$results = [Collections.Generic.List[object]]::new()
function Test-BinaryCase([string]$Label, [scriptblock]$Mutate, [string]$ExpectedError = '') {
    $fixture = New-BinaryFixture
    $null = & $Mutate $fixture
    $stream = [IO.MemoryStream]::new($fixture.Bytes, $false)
    $observed = ''
    try {
        try {
            $proof = Assert-CandidateGoStringValues $stream $fixture.Expected
            if ($proof.symbols.Count -ne $fixture.Expected.Count) { throw 'Unexpected proof inventory.' }
        } catch { $observed = $_.Exception.Message }
        $passed = if ($ExpectedError) { $observed.StartsWith(($ExpectedError + ':'), [StringComparison]::Ordinal) } else { $observed -eq '' }
        if (-not $stream.CanRead) { $passed = $false; $observed = 'Verifier disposed its caller-owned stream.' }
        $results.Add([ordered]@{ name = $Label; passed = $passed; expected = $ExpectedError; observed = $observed })
    } finally { $stream.Dispose() }
}
Test-BinaryCase 'exact initialized values without linker metadata' { param($f) }
Test-BinaryCase 'static data symbol supported' { param($f) $f.Bytes[0x610] = 3 }
Test-BinaryCase 'short COFF name supported' {
    param($f)
    [Array]::Clear($f.Bytes, 0x600, 8)
    [Text.Encoding]::ASCII.GetBytes('guard').CopyTo($f.Bytes, 0x600)
    $f.Expected = [ordered]@{ guard = '1'; $f.Commit = ('a' * 40) }
}
Test-BinaryCase 'auxiliary records are skipped' {
    param($f)
    $primary = [byte[]]($f.Bytes[0x600..0x611])
    $namesLength = [BitConverter]::ToUInt32($f.Bytes, 0x624)
    $names = [byte[]]($f.Bytes[0x624..(0x624 + $namesLength - 1)])
    Set-FixtureUInt32 $f.Bytes 0x90 3
    $primary.CopyTo($f.Bytes, 0x624)
    $names.CopyTo($f.Bytes, 0x636)
    [Array]::Clear($f.Bytes, 0x600, 8)
    [Text.Encoding]::ASCII.GetBytes('ignored').CopyTo($f.Bytes, 0x600)
    $f.Bytes[0x611] = 1
    Set-FixtureUInt32 $f.Bytes 0x616 4294967295
    $f.Expected = [ordered]@{ $f.Guard = '1' }
}
Test-BinaryCase 'disabled policy rejected' { param($f) $f.Bytes[0x400] = [byte][char]'0' } 'AO_BINARY_VALUE'
Test-BinaryCase 'different source commit rejected' { param($f) $f.Bytes[0x420] = [byte][char]'b' } 'AO_BINARY_VALUE'
Test-BinaryCase 'empty initialized value rejected' { param($f) Set-FixtureUInt64 $f.Bytes 0x208 0 } 'AO_BINARY_VALUE'
Test-BinaryCase 'unmapped pointer rejected' { param($f) Set-FixtureUInt64 $f.Bytes 0x200 0x140009000 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'pointer below image base rejected' { param($f) Set-FixtureUInt64 $f.Bytes 0x200 1 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'virtual-only string bytes rejected' {
    param($f)
    Set-FixtureUInt32 $f.Bytes 0x1b8 0x400
    Set-FixtureUInt64 $f.Bytes 0x200 0x140002200
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'virtual-only descriptor bytes rejected' {
    param($f)
    Set-FixtureUInt32 $f.Bytes 0x190 0x400
    Set-FixtureUInt32 $f.Bytes 0x608 0x200
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'raw padding beyond virtual extent rejected' {
    param($f)
    Set-FixtureUInt64 $f.Bytes 0x200 0x140002100
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'descriptor crossing section extent rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x608 0x78 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'string crossing file-backed section extent rejected' {
    param($f)
    Set-FixtureUInt32 $f.Bytes 0x1b8 0x400
    Set-FixtureUInt64 $f.Bytes 0x200 0x1400021ff
    Set-FixtureUInt64 $f.Bytes 0x208 2
    $f.Expected[$f.Guard] = '11'
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'overlapping virtual sections rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x1bc 0x1000 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'overlapping raw sections rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x1c4 0x200 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'invalid symbol section rejected' { param($f) Set-FixtureUInt16 $f.Bytes 0x60c 3 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'truncated PE header rejected' { param($f) $f.Bytes = [byte[]]($f.Bytes[0..0x90]) } 'AO_BINARY_FORMAT'
Test-BinaryCase 'wrong target architecture rejected' { param($f) Set-FixtureUInt16 $f.Bytes 0x84 0xaa64 } 'AO_BINARY_FORMAT'
Test-BinaryCase 'stripped COFF symbols rejected' {
    param($f)
    Set-FixtureUInt32 $f.Bytes 0x8c 0
    Set-FixtureUInt32 $f.Bytes 0x90 0
} 'AO_BINARY_SYMBOL'
Test-BinaryCase 'truncated symbol table rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x90 1000 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'oversized symbol count rejected before allocation' { param($f) Set-FixtureUInt32 $f.Bytes 0x90 1000001 } 'AO_BINARY_SYMBOL'
Test-BinaryCase 'oversized name table rejected before allocation' { param($f) Set-FixtureUInt32 $f.Bytes 0x624 4294967295 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'invalid long-name offset rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x604 1 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'unterminated long name rejected' {
    param($f)
    Set-FixtureUInt32 $f.Bytes 0x624 (4 + [Text.Encoding]::UTF8.GetByteCount($f.Guard))
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'COFF name scan stops at supported bound' {
    param($f)
    $larger = [byte[]]::new(8192)
    $f.Bytes.CopyTo($larger, 0)
    $f.Bytes = $larger
    Set-FixtureUInt32 $f.Bytes 0x624 4101
    [Array]::Fill[byte]($f.Bytes, [byte][char]'a', 0x628, 4096)
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'repeated long names respect cumulative budget' {
    param($f)
    $count = 16385
    $table = 0x600 + $count * 18
    $larger = [byte[]]::new($table + 4100)
    [Array]::Copy($f.Bytes, $larger, 0x600)
    $f.Bytes = $larger
    Set-FixtureUInt32 $f.Bytes 0x90 $count
    for ($i = 0; $i -lt $count; $i++) { Set-FixtureUInt32 $f.Bytes (0x604 + $i * 18) 4 }
    Set-FixtureUInt32 $f.Bytes $table 4100
    [Array]::Fill[byte]($f.Bytes, [byte][char]'z', $table + 4, 4095)
} 'AO_BINARY_BOUNDS'
Test-BinaryCase 'duplicate required symbol rejected' { param($f) Set-FixtureUInt32 $f.Bytes 0x616 4 } 'AO_BINARY_SYMBOL'
Test-BinaryCase 'missing required symbol rejected' { param($f) $f.Expected['example.invalid/missing'] = '1' } 'AO_BINARY_SYMBOL'
Test-BinaryCase 'symbol matching is case sensitive' {
    param($f)
    $f.Expected = [ordered]@{ 'example.invalid/guard.requirepolicy' = '1' }
} 'AO_BINARY_SYMBOL'
Test-BinaryCase 'function symbol cannot impersonate data' { param($f) Set-FixtureUInt16 $f.Bytes 0x60e 0x20 } 'AO_BINARY_SYMBOL'
Test-BinaryCase 'truncated auxiliary records rejected' { param($f) $f.Bytes[0x611] = 2 } 'AO_BINARY_BOUNDS'
Test-BinaryCase 'required data with auxiliary records rejected' { param($f) $f.Bytes[0x611] = 1 } 'AO_BINARY_SYMBOL'
Test-BinaryCase 'required name inside auxiliary bytes is not proof' {
    param($f)
    [Array]::Clear($f.Bytes, 0x600, 8)
    [Text.Encoding]::ASCII.GetBytes('ignored').CopyTo($f.Bytes, 0x600)
    $f.Bytes[0x611] = 1
    $f.Expected = [ordered]@{ $f.Commit = ('a' * 40) }
} 'AO_BINARY_SYMBOL'

$failed = @($results | Where-Object { -not $_.passed })
$report = [ordered]@{ schema = 'ao-binary-identity-fixtures-v1'; total = $results.Count; passed = $results.Count - $failed.Count; failed = $failed.Count; cases = $results }
Write-Output ($report | ConvertTo-Json -Depth 6 -Compress)
if ($failed.Count -gt 0) { throw "Binary identity fixture failures: $($failed.Count)" }
Write-Output "PASS: $($results.Count) in-memory binary identity cases; no executable or external process was invoked."
