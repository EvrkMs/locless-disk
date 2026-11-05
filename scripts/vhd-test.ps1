# Runs the real clone and deploy code on two virtual disks and compares the result.
# It needs administrator rights and Go. Only VHD files it creates are touched: the
# Go test refuses any disk that is not a VHD.
#
#   powershell -ExecutionPolicy Bypass -File scripts\vhd-test.ps1 -Work C:\Temp\vhd
param(
    [Parameter(Mandatory)][string]$Work,
    [string]$SourceLetter = 'Q',
    [string]$TargetLetter = 'R',
    [string]$EfiLetter = 'P',
    [int]$TargetSize = 2048,
    [int]$SourceData = 0,  # size of the source NTFS partition in MB, 0 fills the disk
    [ValidateSet('gpt', 'mbr', 'raw')][string]$TargetStyle = 'gpt'  # what the target looks like before the deploy
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
foreach ($l in $SourceLetter, $TargetLetter, $EfiLetter) {
    if (Test-Path "${l}:\") { throw "Drive $l is in use, pass other letters" }
}
New-Item -ItemType Directory -Force $Work | Out-Null
$src = Join-Path $Work 'src.vhdx'
$dst = Join-Path $Work 'dst.vhdx'

function Invoke-Diskpart([string[]]$Lines) {
    $file = Join-Path $Work 'diskpart.txt'
    Set-Content $file $Lines -Encoding ascii
    $out = diskpart /s $file
    if ($LASTEXITCODE -ne 0) { throw "diskpart failed:`n$($out -join "`n")" }
}
function Get-DiskNumbers {
    @((Get-CimInstance Win32_DiskDrive).Index)
}
# New-Vhd runs diskpart and returns the number of the disk that appeared.
function New-Vhd([string[]]$Lines) {
    $before = Get-DiskNumbers
    Invoke-Diskpart $Lines
    for ($i = 0; $i -lt 40; $i++) {
        $new = @(Get-DiskNumbers | Where-Object { $before -notcontains $_ })
        if ($new.Count -eq 1) { return $new[0] }
        Start-Sleep -Milliseconds 250
    }
    throw 'The virtual disk did not appear'
}
function Assert-Volume([string]$Letter, [string]$Format, [string]$Label) {
    $d = New-Object IO.DriveInfo $Letter
    for ($i = 0; $i -lt 30 -and -not $d.IsReady; $i++) {  # mounting takes a moment
        Start-Sleep -Milliseconds 500
        $d = New-Object IO.DriveInfo $Letter
    }
    if ($d.DriveFormat -ne $Format -or $d.VolumeLabel -ne $Label) {
        throw "Volume ${Letter}: $($d.DriveFormat) '$($d.VolumeLabel)', want $Format '$Label'"
    }
    Write-Host "  ${Letter}: is $Format '$Label'"
}

try {
    # The source: EFI, MSR, NTFS, as Windows installs them.
    $srcDisk = New-Vhd @(
        "create vdisk file=`"$src`" maximum=1024 type=expandable", 'attach vdisk', 'convert gpt',
        'create partition efi size=100', 'format fs=fat32 quick label=SRCEFI',
        'create partition msr size=16',
        $(if ($SourceData) { "create partition primary size=$SourceData" } else { 'create partition primary' }),
        'format fs=ntfs quick label=SRCDATA', "assign letter=$SourceLetter"
    )
    # The target: another layout, full of old data that must not survive, or a disk
    # from another PC, or a new one with no table at all.
    $create = @("create vdisk file=`"$dst`" maximum=$TargetSize type=expandable", 'attach vdisk')
    $dstDisk = New-Vhd ($create + $(switch ($TargetStyle) {
        'gpt' {
            'convert gpt', 'create partition efi size=100', 'format fs=fat32 quick label=OLDEFI',
            'create partition msr size=16',
            'create partition primary size=300', 'format fs=ntfs quick label=OLDDATA', "assign letter=$TargetLetter"
        }
        'mbr' {
            'convert mbr', 'create partition primary size=300', 'format fs=ntfs quick label=OLDDATA', "assign letter=$TargetLetter"
        }
    }))

    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    $bytes = New-Object byte[] (32MB)
    $rng.GetBytes($bytes)
    if ($TargetStyle -ne 'raw') { [IO.File]::WriteAllBytes("${TargetLetter}:\old.bin", $bytes) }  # stale data on the target
    $rng.GetBytes($bytes)
    [IO.File]::WriteAllBytes("${SourceLetter}:\random.bin", $bytes)
    [IO.File]::WriteAllBytes("${SourceLetter}:\zeros.bin", (New-Object byte[] (16MB)))  # used, all zeros
    New-Item -ItemType Directory "${SourceLetter}:\папка" | Out-Null
    [IO.File]::WriteAllText("${SourceLetter}:\папка\файл.txt", 'locless')
    Write-Host "Source is disk $srcDisk, target is disk $dstDisk"
    # A clone reads the disk itself, past the cache of this running Windows. In
    # WinPE the source system is off, here the volume has to be flushed by hand.
    fsutil volume dismount "${SourceLetter}:" | Out-Null

    $env:LOCLESS_VHD_SRC = $srcDisk
    $env:LOCLESS_VHD_DST = $dstDisk
    Push-Location $repo
    go test -count=1 ./agent -run VHDCloneAndDeploy -v -timeout 300s
    $code = $LASTEXITCODE
    Pop-Location
    if ($code -ne 0) { throw 'The Go test failed' }

    # Windows must mount what was written. The Go test already compared the tables.
    Start-Sleep -Seconds 3
    Invoke-Diskpart @(
        "select disk $dstDisk", 'rescan',
        'select partition 4', "assign letter=$TargetLetter noerr",
        'select partition 2', "assign letter=$EfiLetter noerr"
    )
    Start-Sleep -Seconds 2
    Assert-Volume $TargetLetter 'NTFS' 'SRCDATA'
    $total = (New-Object IO.DriveInfo $TargetLetter).TotalSize
    Write-Host "  volume is $([math]::Round($total / 1MB)) MB on a $TargetSize MB disk"
    if ($total -lt ($TargetSize - 200) * 1MB) { throw 'The file system was not stretched over the disk' }
    # The Go test compared the EFI partition byte for byte. Windows mounts this
    # hidden partition only sometimes, so a failure here is just a note.
    try { Assert-Volume $EfiLetter 'FAT32' 'SRCEFI' } catch { Write-Warning "EFI partition not mounted: $_" }
    # A repair rewrites the table of the source, and Windows may drop its letter.
    Invoke-Diskpart @("select disk $srcDisk", 'rescan', 'select partition 4', "assign letter=$SourceLetter noerr")
    Start-Sleep -Seconds 2
    Assert-Volume $SourceLetter 'NTFS' 'SRCDATA'
    if ($SourceData) {
        $grown = [math]::Round((New-Object IO.DriveInfo $SourceLetter).TotalSize / 1MB)
        Write-Host "  repaired source volume is $grown MB, it was $SourceData MB"
        if ($grown -lt $SourceData + 120) { throw 'The repair did not stretch the source volume' }
    }
    foreach ($name in 'random.bin', 'zeros.bin', 'папка\файл.txt') {
        $a = (Get-FileHash -LiteralPath "${SourceLetter}:\$name").Hash
        $b = (Get-FileHash -LiteralPath "${TargetLetter}:\$name").Hash
        if ($a -ne $b) { throw "$name differs" }
        Write-Host "  $name matches"
    }
    if (Test-Path "${TargetLetter}:\old.bin") { throw 'A file of the old disk survived' }

    foreach ($l in $TargetLetter, $SourceLetter) {
        $dirty = fsutil dirty query "${l}:"
        if ($dirty -notmatch 'NOT Dirty|не явля') { throw "${l}: is marked dirty: $dirty" }
        Write-Host "  ${l}: is clean"
    }
    try {
        foreach ($l in $TargetLetter, $SourceLetter) {
            $check = chkdsk "${l}:"
            if ($LASTEXITCODE -ne 0) { throw "chkdsk found problems on ${l}:`n$($check -join "`n")" }
            Write-Host "  chkdsk found no problems on ${l}:"
        }
    }
    catch [System.Management.Automation.ApplicationFailedException] {
        Write-Warning 'chkdsk is not allowed to run on this PC, skipped'
    }
    Write-Host 'PASS'
}
finally {
    Remove-Item Env:LOCLESS_VHD_SRC, Env:LOCLESS_VHD_DST -ErrorAction SilentlyContinue
    foreach ($l in $SourceLetter, $TargetLetter, $EfiLetter) {
        if (Test-Path "${l}:\") { fsutil volume dismount "${l}:" | Out-Null }
    }
    foreach ($file in $src, $dst) {
        if (Test-Path $file) {
            try { Invoke-Diskpart @("select vdisk file=`"$file`"", 'detach vdisk noerr') } catch { Write-Warning "detach ${file}: $_" }
            Start-Sleep -Seconds 1
            Remove-Item $file -ErrorAction SilentlyContinue
        }
    }
}
