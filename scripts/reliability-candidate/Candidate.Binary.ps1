# Read-only verification of the Go string globals in this Windows/amd64 recipe.
# PEReader parses PE headers; COFF records follow Microsoft's PE/COFF format:
# https://learn.microsoft.com/en-us/windows/win32/debug/pe-format
# No executable is loaded or started. Callers retain the existing path/ACL gates.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-CandidateGoStringValues {
    param([Parameter(Mandatory)][IO.Stream]$Stream,
        [Parameter(Mandatory)][Collections.IDictionary]$ExpectedValues)
    if (-not $Stream.CanRead -or -not $Stream.CanSeek -or $Stream.Length -gt 536870912 -or
        $ExpectedValues.Count -lt 1 -or $ExpectedValues.Count -gt 64) {
        throw 'AO_BINARY_FORMAT: expected a bounded, seekable PE image and explicit string identities.'
    }
    $utf8 = [Text.UTF8Encoding]::new($false, $true)
    $wanted = [Collections.Generic.Dictionary[string,string]]::new([StringComparer]::Ordinal)
    foreach ($entry in $ExpectedValues.GetEnumerator()) {
        if ($entry.Key -isnot [string] -or $entry.Value -isnot [string] -or
            [string]::IsNullOrEmpty($entry.Key) -or $entry.Key.Length -gt 1024 -or
            [string]::IsNullOrEmpty($entry.Value) -or $utf8.GetByteCount($entry.Value) -gt 4096) {
            throw 'AO_BINARY_VALUE: expected symbol names and bounded nonempty strings.'
        }
        $wanted.Add($entry.Key, $entry.Value)
    }
    function Read-BinaryRange([long]$Offset, [int]$Count) {
        if ($Offset -lt 0 -or $Count -lt 0 -or $Offset -gt $Stream.Length - $Count) {
            throw 'AO_BINARY_BOUNDS: read extends outside the image.'
        }
        $Stream.Position = $Offset
        $bytes = [byte[]]::new($Count)
        $Stream.ReadExactly($bytes)
        return ,$bytes
    }
    $pe = $null
    try {
        $Stream.Position = 0
        $pe = [Reflection.PortableExecutable.PEReader]::new($Stream, [Reflection.PortableExecutable.PEStreamOptions]::LeaveOpen)
        $headers = $pe.PEHeaders
        if ($headers -isnot [Reflection.PortableExecutable.PEHeaders]) { throw 'PE headers were not available.' }
    } catch { throw 'AO_BINARY_FORMAT: invalid or truncated PE headers.' }
    finally { if ($null -ne $pe) { $pe.Dispose() } }
    if ($null -eq $headers.PEHeader -or $headers.CoffHeader.Machine -ne [Reflection.PortableExecutable.Machine]::Amd64 -or
        $headers.PEHeader.Magic -ne [Reflection.PortableExecutable.PEMagic]::PE32Plus -or
        ($headers.CoffHeader.Characteristics -band [Reflection.PortableExecutable.Characteristics]::ExecutableImage) -eq 0) {
        throw 'AO_BINARY_FORMAT: only PE32+ AMD64 executable images are supported.'
    }
    $sections = @($headers.SectionHeaders)
    if ($sections.Count -lt 1 -or $sections.Count -gt 96) { throw 'AO_BINARY_FORMAT: invalid section count.' }
    foreach ($section in $sections) {
        if ($section.VirtualAddress -lt 0 -or $section.VirtualSize -lt 0 -or
            $section.PointerToRawData -lt 0 -or $section.SizeOfRawData -lt 0 -or
            [long]$section.PointerToRawData -gt $Stream.Length - [long]$section.SizeOfRawData) {
            throw 'AO_BINARY_BOUNDS: invalid section extent.'
        }
    }
    for ($i = 0; $i -lt $sections.Count; $i++) {
        for ($j = $i + 1; $j -lt $sections.Count; $j++) {
            $a = $sections[$i]; $b = $sections[$j]
            $virtualOverlap = $a.VirtualSize -gt 0 -and $b.VirtualSize -gt 0 -and
                [long]$a.VirtualAddress -lt [long]$b.VirtualAddress + $b.VirtualSize -and
                [long]$b.VirtualAddress -lt [long]$a.VirtualAddress + $a.VirtualSize
            $rawOverlap = $a.SizeOfRawData -gt 0 -and $b.SizeOfRawData -gt 0 -and
                [long]$a.PointerToRawData -lt [long]$b.PointerToRawData + $b.SizeOfRawData -and
                [long]$b.PointerToRawData -lt [long]$a.PointerToRawData + $a.SizeOfRawData
            if ($virtualOverlap -or $rawOverlap) { throw 'AO_BINARY_BOUNDS: overlapping sections are ambiguous.' }
        }
    }
    function Get-SectionOffset([int]$Index, [long]$Relative, [int]$Count) {
        if ($Index -lt 0 -or $Index -ge $sections.Count -or $Relative -lt 0) {
            throw 'AO_BINARY_BOUNDS: invalid symbol section or offset.'
        }
        $section = $sections[$Index]
        $backed = [Math]::Min([long]$section.VirtualSize, [long]$section.SizeOfRawData)
        if ($Relative -gt $backed - $Count -or
            ($section.SectionCharacteristics -band [Reflection.PortableExecutable.SectionCharacteristics]::ContainsInitializedData) -eq 0) {
            throw 'AO_BINARY_BOUNDS: string data is outside initialized, file-backed section bytes.'
        }
        return [long]$section.PointerToRawData + $Relative
    }
    $symbolOffset = [long]$headers.CoffHeader.PointerToSymbolTable
    $symbolCount = [long]$headers.CoffHeader.NumberOfSymbols
    if ($symbolOffset -le 0 -or $symbolCount -lt 1 -or $symbolCount -gt 1000000) {
        throw 'AO_BINARY_SYMBOL: required COFF symbols are missing, stripped, or oversized.'
    }
    $symbols = Read-BinaryRange $symbolOffset ([int]($symbolCount * 18))
    $stringOffset = $symbolOffset + $symbolCount * 18
    $sizeBytes = Read-BinaryRange $stringOffset 4
    $stringSize = [BitConverter]::ToUInt32($sizeBytes, 0)
    if ($stringSize -lt 4 -or $stringSize -gt 67108864) { throw 'AO_BINARY_BOUNDS: invalid COFF string table size.' }
    $strings = Read-BinaryRange $stringOffset ([int]$stringSize)
    $found = [Collections.Generic.Dictionary[string,object]]::new([StringComparer]::Ordinal)
    $decodedNameBytes = 0L
    for ($index = 0; $index -lt $symbolCount; $index++) {
        $at = $index * 18
        $aux = [int]$symbols[$at + 17]
        if ($index + $aux -ge $symbolCount) { throw 'AO_BINARY_BOUNDS: auxiliary symbol records are truncated.' }
        if ([BitConverter]::ToUInt32($symbols, $at) -eq 0) {
            $offset = [BitConverter]::ToUInt32($symbols, $at + 4)
            if ($offset -eq 0) { $name = '' }
            else {
                if ($offset -lt 4 -or $offset -ge $stringSize) { throw 'AO_BINARY_BOUNDS: invalid COFF name offset.' }
                $searchLength = [Math]::Min(4096, [int]$stringSize - [int]$offset)
                $end = [Array]::IndexOf($strings, [byte]0, [int]$offset, $searchLength)
                if ($end -lt 0) { throw 'AO_BINARY_BOUNDS: unterminated or oversized COFF name.' }
                $decodedNameBytes += [long]($end - $offset) + 1
                if ($decodedNameBytes -gt 67108864) { throw 'AO_BINARY_BOUNDS: cumulative COFF name budget exceeded.' }
                $name = $utf8.GetString($strings, [int]$offset, [int]($end - $offset))
            }
        } else {
            $length = 0
            while ($length -lt 8 -and $symbols[$at + $length] -ne 0) { $length++ }
            $decodedNameBytes += $length
            if ($decodedNameBytes -gt 67108864) { throw 'AO_BINARY_BOUNDS: cumulative COFF name budget exceeded.' }
            $name = $utf8.GetString($symbols, $at, $length)
        }
        if ($wanted.ContainsKey($name)) {
            if ($found.ContainsKey($name)) { throw "AO_BINARY_SYMBOL: duplicate required symbol: $name" }
            if ($aux -ne 0 -or [BitConverter]::ToUInt16($symbols, $at + 14) -ne 0 -or $symbols[$at + 16] -notin @(2, 3)) {
                throw "AO_BINARY_SYMBOL: required symbol is not a defined data variable: $name"
            }
            $sectionIndex = [int][BitConverter]::ToInt16($symbols, $at + 12) - 1
            $relative = [long][BitConverter]::ToUInt32($symbols, $at + 8)
            $headerOffset = Get-SectionOffset $sectionIndex $relative 16
            $header = Read-BinaryRange $headerOffset 16
            $pointer = [BitConverter]::ToUInt64($header, 0)
            $length = [BitConverter]::ToUInt64($header, 8)
            $expected = $wanted[$name]
            $expectedLength = $utf8.GetByteCount($expected)
            if ($length -ne $expectedLength) { throw "AO_BINARY_VALUE: initialized string length differs: $name" }
            $imageBase = $headers.PEHeader.ImageBase
            if ($imageBase -gt [long]::MaxValue -or $pointer -gt [long]::MaxValue -or $pointer -lt $imageBase) {
                throw 'AO_BINARY_BOUNDS: unsupported string address.'
            }
            $rva = [long]$pointer - [long]$imageBase
            $containingSections = @(for ($k = 0; $k -lt $sections.Count; $k++) {
                $section = $sections[$k]
                if ($rva -ge $section.VirtualAddress -and $rva -lt [long]$section.VirtualAddress + $section.VirtualSize) { $k }
            })
            if ($containingSections.Count -ne 1) { throw 'AO_BINARY_BOUNDS: string pointer does not identify one section.' }
            $dataOffset = Get-SectionOffset $containingSections[0] ($rva - $sections[$containingSections[0]].VirtualAddress) $expectedLength
            $actual = $utf8.GetString((Read-BinaryRange $dataOffset $expectedLength))
            if (-not [string]::Equals($actual, $expected, [StringComparison]::Ordinal)) {
                throw "AO_BINARY_VALUE: initialized string differs: $name"
            }
            $found.Add($name, [ordered]@{ symbol = $name; value = $actual; headerOffset = $headerOffset; valueOffset = $dataOffset })
        }
        # Auxiliary entries are not symbol definitions, even when their bytes resemble one.
        $index += $aux
    }
    foreach ($name in $wanted.Keys) {
        if (-not $found.ContainsKey($name)) { throw "AO_BINARY_SYMBOL: required symbol was not found: $name" }
    }
    return [ordered]@{ format = 'PE32+/amd64'; symbols = @($wanted.Keys | Sort-Object -CaseSensitive | ForEach-Object { $found[$_] }) }
}

function Get-CandidateBinaryStringProof {
    param([Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][Collections.IDictionary]$ExpectedValues)
    $localPath = Get-CandidateLocalPath $Path
    $stream = [IO.File]::Open($localPath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        $proof = Assert-CandidateGoStringValues $stream $ExpectedValues
        $stream.Position = 0
        $hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($stream)).ToLowerInvariant()
        return [ordered]@{ file = [IO.Path]::GetFileName($localPath); sha256 = $hash; bytes = $stream.Length; format = $proof.format; symbols = $proof.symbols }
    } finally { $stream.Dispose() }
}

function Assert-CandidateBuildIdentity {
    param([Parameter(Mandatory)][string]$Root,
        [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$SourceCommit,
        [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{64}$')][string]$HelperSHA256,
        [Parameter(Mandatory)][string]$Version,
        [Parameter(Mandatory)][string]$BuiltAtUtc)
    $module = 'github.com/aoagents/agent-orchestrator/backend/internal'
    $expected = [ordered]@{
        'AO.exe' = [ordered]@{
            "$module/inferenceguard.RequirePolicy" = '1'
            "$module/cli.Commit" = $SourceCommit
            "$module/cli.Version" = $Version
            "$module/cli.Date" = $BuiltAtUtc
        }
        'ao-offline-fleet.exe' = [ordered]@{
            "$module/session_manager.admissionOfflineCompanion" = '1'
            "$module/session_manager.admissionPackagedHelperSHA256" = $HelperSHA256
        }
        'ao-offline-conpty.exe' = [ordered]@{ "$module/adapters/runtime/conpty.OfflineCandidateConPTYFixture" = '1' }
        'ao-offline-daemon.exe' = [ordered]@{
            "$module/daemon.OfflineDaemonFixture" = '1'
            "$module/daemon.OfflineDaemonFixtureSource" = $SourceCommit
            "$module/inferenceguard.RequirePolicy" = '1'
        }
    }
    $proofs = @(foreach ($entry in $expected.GetEnumerator()) {
        Get-CandidateBinaryStringProof (Join-Path $Root $entry.Key) $entry.Value
    })
    return [ordered]@{ schemaVersion = 1; method = 'Read-only PE/COFF initialized Go strings'; sourceCommit = $SourceCommit; files = $proofs }
}
