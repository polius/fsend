#!/usr/bin/env pwsh
# End-to-end smoke test for scripts/install.ps1: builds a fake release
# tree, serves it over local HTTP, and asserts the installer's behavior
# (flags, checksum verification, install locations, PATH handling,
# backup reaping). Mirror of scripts/smoke-install.sh.
#
# The installer runs against the local server via FSEND_RELEASE_BASE_URL,
# so no network and no real release is used. On Windows the installer is
# driven through the in-box PowerShell 5.1 (the documented install path);
# elsewhere pwsh. Non-Windows hosts simulate the Windows environment,
# since the installer targets Windows only.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$installer = Join-Path $PSScriptRoot 'install.ps1'
$ver = '9.9.9'
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("fsend-ps-smoke-" + [guid]::NewGuid().ToString('N'))
$onWindows = [System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT
$passed = 0
$failed = 0

function Check($desc, $ok) {
    if ($ok) { Write-Host "ok   $desc"; $script:passed++ }
    else { Write-Host "FAIL $desc"; $script:failed++ }
}

# In-box PowerShell 5.1 on Windows, pwsh elsewhere.
$psCmd = Get-Command powershell -ErrorAction SilentlyContinue
$ps = if ($psCmd) { $psCmd.Source } else { (Get-Command pwsh).Source }

# The installer reads Windows-only env vars; simulate them off-Windows.
if (-not $env:PROCESSOR_ARCHITECTURE) {
    $osArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    $env:PROCESSOR_ARCHITECTURE = if ($osArch -eq 'Arm64') { 'ARM64' } else { 'AMD64' }
}
$archToken = $env:PROCESSOR_ARCHITECTURE.ToLower()
$archive = "fsend_${ver}_windows_${archToken}.zip"

# Fixture release: stub binary zipped up, checksummed, plus the layout the
# "latest" redirect expects.
New-Item -ItemType Directory -Force -Path "$work/fixture/download/v$ver", "$work/fixture/latest/download" | Out-Null
$stub = Join-Path $work 'fixture/fsend.exe'
if ($onWindows) { Copy-Item (Get-Command hostname.exe).Source $stub }
else { Copy-Item '/bin/hostname' $stub -ErrorAction SilentlyContinue }
if (-not (Test-Path $stub)) { Copy-Item '/usr/bin/true' $stub }
Compress-Archive -Path $stub -DestinationPath "$work/fixture/download/v$ver/$archive"
$hash = (Get-FileHash -Algorithm SHA256 -Path "$work/fixture/download/v$ver/$archive").Hash.ToLower()
Set-Content -Path "$work/fixture/download/v$ver/checksums.txt" -Value "$hash  $archive" -NoNewline
Copy-Item "$work/fixture/download/v$ver/checksums.txt" "$work/fixture/latest/download/checksums.txt"

# Local release server.
$python = Get-Command python3 -ErrorAction SilentlyContinue
if (-not $python) { $python = Get-Command python }
$port = 18901
$serverArgs = @('-m', 'http.server', $port, '--bind', '127.0.0.1', '--directory', "$work/fixture")
# -WindowStyle is Windows-only in Start-Process.
$server = if ($onWindows) {
    Start-Process $python.Source -ArgumentList $serverArgs -PassThru -WindowStyle Hidden
} else {
    Start-Process $python.Source -ArgumentList $serverArgs -PassThru
}

$env:FSEND_RELEASE_BASE_URL = "http://127.0.0.1:$port"
$env:FSEND_PREFIX = $null
for ($i = 0; $i -lt 20; $i++) {
    try {
        Invoke-WebRequest "$($env:FSEND_RELEASE_BASE_URL)/latest/download/checksums.txt" -UseBasicParsing | Out-Null
        break
    } catch { Start-Sleep -Milliseconds 300 }
}

function Invoke-Installer([string[]]$installerArgs) {
    & $ps -NoProfile -File $installer @installerArgs 2>&1
}

try {
    # 1. -Help exits 0.
    $out = Invoke-Installer @('-Help')
    Check "-Help exits 0" ($LASTEXITCODE -eq 0)

    # 2. pinned install into an explicit prefix, rc/PATH untouched.
    $p1 = Join-Path $work 'bin1'
    $out = Invoke-Installer @('-Version', $ver, '-Prefix', $p1, '-NoModifyPath')
    Check "pinned install exits 0" ($LASTEXITCODE -eq 0)
    Check "binary installed" (Test-Path (Join-Path $p1 'fsend.exe'))
    Check "checksum verified line" ($null -ne ($out | Where-Object { $_ -like '*checksum verified*' }))
    # The outro prints last: its presence proves no mid-install abort was
    # swallowed (a statement-terminating error would exit 0 but half-done).
    Check "outro printed" ($null -ne ($out | Where-Object { $_ -like "*fsend v$ver installed*" }))

    # 3. default prefix without %LOCALAPPDATA% errors cleanly.
    $savedLad = $env:LOCALAPPDATA
    Remove-Item Env:LOCALAPPDATA -ErrorAction SilentlyContinue
    $out = Invoke-Installer @('-Version', $ver, '-NoModifyPath')
    $env:LOCALAPPDATA = $savedLad
    Check "missing LOCALAPPDATA errors cleanly" (($LASTEXITCODE -ne 0) -and
        ($null -ne ($out | Where-Object { $_ -like '*LOCALAPPDATA*' })))

    # 4. default prefix install; user PATH (Windows only) and $GITHUB_PATH.
    $env:LOCALAPPDATA = Join-Path $work 'lad'
    New-Item -ItemType Directory -Force -Path $env:LOCALAPPDATA | Out-Null
    $ghFile = Join-Path $work 'gh_path.txt'
    $env:GITHUB_ACTIONS = 'true'
    $env:GITHUB_PATH = $ghFile
    $defaultArgs = @('-Version', $ver)
    if (-not $onWindows) { $defaultArgs += '-NoModifyPath' }  # registry is Windows-only
    $out = Invoke-Installer $defaultArgs
    $env:GITHUB_ACTIONS = $null
    $env:GITHUB_PATH = $null
    Check "default-prefix install exits 0" ($LASTEXITCODE -eq 0)
    Check "binary at %LOCALAPPDATA%\Programs\fsend" (Test-Path "$env:LOCALAPPDATA/Programs/fsend/fsend.exe")
    Check "`$GITHUB_PATH populated" ($null -ne (Get-Content $ghFile | Where-Object { $_ -like '*Programs/fsend*' }))
    if ($onWindows) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        Check "user PATH updated" ($userPath -like '*Programs\fsend*')
    }

    # 5. a previous install's .old backup is reaped on success.
    $p2 = Join-Path $work 'bin2'
    New-Item -ItemType Directory -Force -Path $p2 | Out-Null
    Copy-Item $stub (Join-Path $p2 'fsend.exe')
    Copy-Item $stub (Join-Path $p2 'fsend.exe.old')
    $out = Invoke-Installer @('-Version', $ver, '-Prefix', $p2, '-NoModifyPath')
    Check "reinstall exits 0" ($LASTEXITCODE -eq 0)
    Check ".old backup reaped" (-not (Test-Path (Join-Path $p2 'fsend.exe.old')))
    Check "replaced binary present" (Test-Path (Join-Path $p2 'fsend.exe'))
    Check "reinstall outro printed" ($null -ne ($out | Where-Object { $_ -like "*fsend v$ver installed*" }))

    # 6. a corrupted archive is rejected by the checksum. Runs last: it
    # poisons the shared fixture archive.
    $bytes = [System.IO.File]::ReadAllBytes("$work/fixture/download/v$ver/$archive")
    $bytes[50] = $bytes[50] -bxor 0xFF
    [System.IO.File]::WriteAllBytes("$work/fixture/download/v$ver/$archive", $bytes)
    $out = Invoke-Installer @('-Version', $ver, '-Prefix', (Join-Path $work 'bin3'), '-NoModifyPath')
    Check "corrupt archive rejected" (($LASTEXITCODE -ne 0) -and
        ($null -ne ($out | Where-Object { $_ -like '*checksum mismatch*' })))
}
finally {
    if ($server -and -not $server.HasExited) { Stop-Process -Id $server.Id -Force }
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

if ($failed -gt 0) { exit 1 }
Write-Host "all $passed PS smoke scenario(s) passed"
