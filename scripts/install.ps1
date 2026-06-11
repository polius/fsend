<#
.SYNOPSIS
  fsend installer for Windows (PowerShell).

.DESCRIPTION
  Downloads the latest fsend release, verifies its SHA-256 checksum, and
  installs fsend.exe into a directory on your PATH. PowerShell 5.1+ (in-box
  on Windows 10/11) is the only requirement — no Git Bash needed.

  Quick install:
    irm https://getfsend.alzina.dev/install.ps1 | iex

  With options, set env vars before piping:
    $env:FSEND_VERSION='1.2.3'; irm https://getfsend.alzina.dev/install.ps1 | iex

  Or download and run with parameters:
    .\install.ps1 -Prefix C:\tools\bin -Version 1.2.3

.PARAMETER Prefix
  Install directory (default: %LOCALAPPDATA%\fsend\bin). Env: FSEND_PREFIX.

.PARAMETER Version
  Version to install, e.g. 1.2.3 or v1.2.3 (default: latest). Env: FSEND_VERSION.

.LINK
  https://github.com/polius/fsend/blob/main/scripts/install.ps1
#>
[CmdletBinding()]
param(
    [string]$Prefix  = $env:FSEND_PREFIX,
    [string]$Version = $env:FSEND_VERSION,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo   = 'polius/fsend'
$Binary = 'fsend.exe'

function Info($m) { Write-Host "› $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "✓ $m" -ForegroundColor Green }

# throw, not exit: the install one-liner runs via `irm ... | iex` *in the
# user's own session*, so `exit` would close their terminal. throw halts,
# shows the message in red, leaves an interactive window open, and still
# yields a non-zero exit code under `powershell -File install.ps1`.
function Err($m)  { Write-Host "✗ $m" -ForegroundColor Red; throw }

function Show-Usage {
    Write-Host @'
fsend installer (Windows / PowerShell)

Usage:
  irm https://getfsend.alzina.dev/install.ps1 | iex
  .\install.ps1 [-Prefix DIR] [-Version VERSION]

Parameters (or matching env var):
  -Prefix DIR        Install location      (default: %LOCALAPPDATA%\fsend\bin; env FSEND_PREFIX)
  -Version VERSION   Version to install    (default: latest; env FSEND_VERSION)
  -Help              Show this help and exit

Source: https://github.com/polius/fsend/blob/main/scripts/install.ps1
'@
}

if ($Help) { Show-Usage; return }

# Windows releases cover amd64/arm64/386 only. PROCESSOR_ARCHITECTURE reports
# the *process* arch, so a 32-bit PowerShell on 64-bit Windows would read x86 —
# PROCESSOR_ARCHITEW6432 carries the true machine arch in that case.
function Get-Arch {
    $a = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $a = $env:PROCESSOR_ARCHITEW6432 }
    switch ($a) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        'x86'   { '386' }
        default { Err "unsupported architecture: $a" }
    }
}

function Download($url, $out) {
    try {
        Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing
    } catch {
        Err "download failed: $url"
    }
}

# Windows PowerShell 5.1 defaults to TLS 1.0/1.1; GitHub requires 1.2+.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$arch = Get-Arch

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("fsend-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
    $checksums = Join-Path $tmp 'checksums.txt'

    if (-not $Version -or $Version -eq 'latest') {
        # Resolve "latest" through the release-asset redirect, not the GitHub
        # API (unauthenticated API is capped at 60/hr per IP). checksums.txt is
        # needed anyway, and the version is recovered from the archive names in
        # it — tags are always v-prefixed.
        Info 'looking up latest release...'
        Download "https://github.com/$Repo/releases/latest/download/checksums.txt" $checksums
        $line = Get-Content $checksums | Where-Object { $_ -match 'fsend_([^_]+)_' } | Select-Object -First 1
        if ($line -match 'fsend_([^_]+)_') { $vnum = $Matches[1] } else { Err 'could not resolve latest version' }
        $Version = "v$vnum"
    } else {
        # Accept "1.2.3" and "v1.2.3" alike: tags are v-prefixed, archive names are not.
        $vnum = $Version -replace '^v', ''
        $Version = "v$vnum"
        Info 'downloading checksums'
        Download "https://github.com/$Repo/releases/download/$Version/checksums.txt" $checksums
    }

    if (-not $Prefix) { $Prefix = Join-Path $env:LOCALAPPDATA 'fsend\bin' }
    Info "installing fsend $Version for windows-$arch into $Prefix"

    $archive = "fsend_${vnum}_windows_${arch}.zip"
    Info "downloading $archive"
    Download "https://github.com/$Repo/releases/download/$Version/$archive" (Join-Path $tmp $archive)

    Info 'verifying checksum'
    $row = Get-Content $checksums | Where-Object { $_ -match ("\s" + [regex]::Escape($archive) + "$") } | Select-Object -First 1
    if (-not $row) { Err "no checksum found for $archive" }
    $expected = (($row -split '\s+') | Where-Object { $_ })[0].ToLower()
    $actual   = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLower()
    if ($actual -ne $expected) { Err "checksum mismatch: expected $expected, got $actual" }
    Ok 'checksum verified'

    Info 'extracting'
    Expand-Archive -LiteralPath (Join-Path $tmp $archive) -DestinationPath $tmp -Force
    $src = Join-Path $tmp $Binary
    if (-not (Test-Path $src)) { Err "binary $Binary not found in archive" }

    New-Item -ItemType Directory -Force -Path $Prefix | Out-Null
    $dst = Join-Path $Prefix $Binary
    Move-Item -Force -Path $src -Destination $dst
    Ok "installed: $dst"

    # Persist Prefix on the user's PATH (no admin needed). SetEnvironmentVariable
    # with 'User' writes the registry; $env:PATH is patched so this session works
    # immediately without a restart.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if (($userPath -split ';') -notcontains $Prefix) {
        [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $Prefix).TrimStart(';'), 'User')
        $env:PATH = $env:PATH + ';' + $Prefix
        Ok "added $Prefix to your user PATH (open a new terminal for other apps to see it)"
    }

    $ver = (& $dst --version 2>$null | Select-Object -First 1)
    if ($ver) { Ok "verify: $ver" }

    Write-Host ''
    Write-Host 'Next: send a file with  fsend <path>'
    Write-Host '      see all options:  fsend --help'
}
finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}
