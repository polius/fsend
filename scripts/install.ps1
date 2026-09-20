<#
.SYNOPSIS
  fsend installer for Windows (PowerShell).

.DESCRIPTION
  Downloads the latest fsend release, verifies its SHA-256 checksum, and
  installs fsend.exe into a directory on your PATH. PowerShell 5.1+ (in-box
  on Windows 10/11) is the only requirement - no Git Bash needed, and no
  admin rights: everything is per-user.

  Quick install:
    irm https://getfsend.alzina.dev/windows | iex

  With options, set env vars before piping:
    $env:FSEND_VERSION='1.2.3'; irm https://getfsend.alzina.dev/windows | iex

  Or download and run with parameters:
    .\install.ps1 -Prefix C:\tools\bin -Version 1.2.3 -NoModifyPath

.PARAMETER Prefix
  Install directory (default: %LOCALAPPDATA%\Programs\fsend). Env: FSEND_PREFIX.

.PARAMETER Version
  Version to install, e.g. 1.2.3 or v1.2.3 (default: latest). Env: FSEND_VERSION.

.PARAMETER NoModifyPath
  Don't add the install dir to your user PATH.

.LINK
  https://github.com/polius/fsend/blob/main/scripts/install.ps1
#>
[CmdletBinding()]
param(
    [string]$Prefix  = $env:FSEND_PREFIX,
    [string]$Version = $env:FSEND_VERSION,
    [switch]$NoModifyPath,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo   = 'polius/fsend'
$Binary = 'fsend.exe'

# Test seam: point the installer at a local HTTP server to exercise the
# full download -> verify -> install path without a real release.
$ReleaseBase = if ($env:FSEND_RELEASE_BASE_URL) {
    $env:FSEND_RELEASE_BASE_URL
} else {
    "https://github.com/$Repo/releases"
}

# PowerShell 5.1 renders Invoke-WebRequest progress byte-by-byte, which
# makes downloads orders of magnitude slower - silence it. curl.exe (the
# in-box fast path, see Download) draws its own progress bar.
$ProgressPreference = 'SilentlyContinue'
# Windows PowerShell 5.1 defaults to TLS 1.0/1.1; GitHub requires 1.2+.
# OR-ing preserves TLS 1.3 on hosts that already enable it.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# Unicode markers on PowerShell 7+; ASCII-only on 5.1, which mangles
# Unicode on both delivery paths (`irm` decodes the charset-less release
# asset as ISO-8859-1; a BOM-less .ps1 file parses as ANSI).
$mark = if ($PSVersionTable.PSEdition -eq 'Core') {
    @{ info = '›'; ok = '✓'; warn = '!'; err = '✗' }
} else {
    @{ info = '>'; ok = 'OK'; warn = '!'; err = 'x' }
}

function Info($m) { Write-Host "$($mark.info) $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "$($mark.ok) $m" -ForegroundColor Green }
function Warn($m) { Write-Host "$($mark.warn) $m" -ForegroundColor Yellow }
function Mut($m)  { Write-Host $m -ForegroundColor DarkGray }

# throw, not exit: the install one-liner runs via `irm ... | iex` *in the
# user's own session*, so `exit` would close their terminal. throw halts,
# shows the message in red, leaves an interactive window open, and still
# yields a non-zero exit code under `powershell -File install.ps1`.
function Err($m) { Write-Host "$($mark.err) $m" -ForegroundColor Red; throw $m }

function Show-Usage {
    Write-Host @'
fsend installer (Windows / PowerShell)

Usage:
  irm https://getfsend.alzina.dev/windows | iex
  .\install.ps1 [-Prefix DIR] [-Version VERSION] [-NoModifyPath] [-Help]

Parameters (or matching env var):
  -Prefix DIR        Install location    (default: %LOCALAPPDATA%\Programs\fsend; env FSEND_PREFIX)
  -Version VERSION   Version to install  (default: latest; env FSEND_VERSION)
  -NoModifyPath      Don't add the install dir to your user PATH
  -Help              Show this help and exit

Per-user install: no admin rights needed.
More: https://github.com/polius/fsend#readme
'@
}

if ($Help) { Show-Usage; return }

# Broadcast WM_SETTINGCHANGE so Explorer hands a raw registry PATH write to
# the processes it launches (SetEnvironmentVariable does this; raw writes don't).
function Broadcast-EnvChange {
    if (-not ('Win32.EnvBroadcast' -as [type])) {
        Add-Type -Namespace Win32 -Name EnvBroadcast -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    }
    $res = [UIntPtr]::Zero
    [Win32.EnvBroadcast]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$res) | Out-Null
}

# Windows releases cover amd64/arm64/386 only. PROCESSOR_ARCHITECTURE reports
# the *process* arch, so a 32-bit PowerShell on 64-bit Windows would read x86 -
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
    # curl.exe ships in-box since Windows 10 1803 and shows a progress bar;
    # Invoke-WebRequest is the fallback for stripped-down hosts.
    $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
    if ($curl) {
        $flags = if ([Console]::IsErrorRedirected) { '-fsSL' } else { '-fS#L' }
        & $curl.Source $flags --proto '=https' --tlsv1.2 -o $out $url
        if ($LASTEXITCODE -ne 0) { Err "download failed: $url" }
        return
    }
    try {
        Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing
    } catch {
        Err "download failed: $url"
    }
}

$arch = Get-Arch

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("fsend-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
    # Upgrade awareness: say what's already installed before touching it.
    # Same PS 5.1 pitfall as the other native calls below: redirected native
    # stderr under EAP=Stop aborts a good run, so relax EAP around them.
    # try/catch on top keeps a broken binary from aborting the installer.
    $existing = Get-Command fsend -ErrorAction SilentlyContinue
    if ($existing) {
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $cur = $null
        try { $cur = (& $existing.Source --version 2>$null | Select-Object -First 1) } catch { $cur = $null }
        $ErrorActionPreference = $prevEAP
        if ($cur) { Mut "currently installed: $cur ($($existing.Source))" }
    }

    $checksums = Join-Path $tmp 'checksums.txt'

    if (-not $Version -or $Version -eq 'latest') {
        # Resolve "latest" through the release-asset redirect, not the GitHub
        # API (unauthenticated API is capped at 60/hr per IP). checksums.txt is
        # needed anyway, and the version is recovered from the archive names in
        # it - tags are always v-prefixed.
        Info 'looking up the latest release...'
        Download "$ReleaseBase/latest/download/checksums.txt" $checksums
        $line = Get-Content $checksums | Where-Object { $_ -match 'fsend_([^_]+)_' } | Select-Object -First 1
        if ($line -match 'fsend_([^_]+)_') { $vnum = $Matches[1] } else { Err 'could not resolve the latest version' }
        $Version = "v$vnum"
    } else {
        # Accept "1.2.3" and "v1.2.3" alike: tags are v-prefixed, archive names are not.
        $vnum = $Version -replace '^v', ''
        $Version = "v$vnum"
        Info 'downloading checksums'
        Download "$ReleaseBase/download/$Version/checksums.txt" $checksums
    }

    # %LOCALAPPDATA%\Programs (per-user, no admin) - deliberately NOT under
    # %LOCALAPPDATA%\fsend, which is the config dir: `fsend --uninstall`
    # RemoveAll's the config dir, and Windows can't delete a running .exe
    # nested inside it ("Access is denied"). Keep the two trees separate.
    if (-not $Prefix) {
        if (-not $env:LOCALAPPDATA) { Err '%LOCALAPPDATA% is not set - pass -Prefix explicitly' }
        $Prefix = Join-Path $env:LOCALAPPDATA 'Programs\fsend'
    }
    Info "installing fsend $Version for windows-$arch into $Prefix"

    $archive = "fsend_${vnum}_windows_${arch}.zip"
    Info "downloading $archive"
    Download "$ReleaseBase/download/$Version/$archive" (Join-Path $tmp $archive)

    # The checksum catches corruption and truncation. checksums.txt and the
    # archive both come from the same HTTPS host, so this is an integrity
    # check, not a guarantee against a tampered release - that is GitHub's
    # side of the trust model (see docs/security.md).
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
    $oldDst = Join-Path $Prefix ($Binary + '.old')
    try {
        if (Test-Path -LiteralPath $dst) {
            # A running .exe can't be overwritten but can be renamed; moving it
            # aside keeps the no-binary window to two adjacent moves. `fsend
            # --update` restores from .old if this script dies in between.
            Move-Item -Force -Path $dst -Destination $oldDst
        }
        Move-Item -Force -Path $src -Destination $dst
    } catch {
        if (-not (Test-Path -LiteralPath $dst) -and (Test-Path -LiteralPath $oldDst)) {
            Move-Item -Force -Path $oldDst -Destination $dst
        }
        Err "could not install into $Prefix : $($_.Exception.Message)"
    }
    Ok "installed: $dst"

    # Persist Prefix on the user's PATH (no admin needed). The registry round-trip
    # preserves REG_EXPAND_SZ (SetEnvironmentVariable flattens it) + broadcasts.
    if (-not $NoModifyPath) {
        $envKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        try {
            $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
            $userPath = ''
            if ($null -ne $envKey.GetValue('Path', $null)) {
                $kind = $envKey.GetValueKind('Path')
                $userPath = [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            }
            # TrimEnd: `...\fsend` and `...\fsend\` are the same dir to Windows.
            $entries = ($userPath -split ';') | Where-Object { $_ } | ForEach-Object { $_.TrimEnd('\') }
            if ($entries -notcontains $Prefix.TrimEnd('\')) {
                $envKey.SetValue('Path', ($userPath.TrimEnd(';') + ';' + $Prefix).TrimStart(';'), $kind)
                Broadcast-EnvChange
                $env:PATH = $env:PATH + ';' + $Prefix
                Ok "added $Prefix to your user PATH (open a new terminal for other apps to see it)"
            }
        } finally {
            $envKey.Close()
        }
    }

    # GitHub Actions: expose the install dir to later steps of the workflow.
    if ($env:GITHUB_ACTIONS -eq 'true' -and $env:GITHUB_PATH) {
        Add-Content -Path $env:GITHUB_PATH -Value $Prefix
        Ok "added $Prefix to `$GITHUB_PATH"
    }

    # Best-effort smoke check that the fresh binary runs. EAP=Continue covers
    # the PS 5.1 native-stderr abort; try/catch covers a binary that cannot
    # execute at all — neither may abort the tail of the install (backup
    # reaping, outro). A statement-terminating error here would otherwise
    # unwind the try block and exit 0 with the work silently half-done.
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $ver = $null
    try { $ver = (& $dst --version 2>$null | Select-Object -First 1) } catch { $ver = $null }
    $ErrorActionPreference = $prevEAP
    if ($ver) { Ok "verify: $ver" }

    # Reap the backup from a previous install. During `fsend --update` it is
    # the running image (locked) and skips silently; the updater reaps it.
    Remove-Item -LiteralPath $oldDst -Force -ErrorAction SilentlyContinue

    Write-Host ''
    Mut "fsend $Version installed -> $dst"
    Write-Host 'Next: send a file with  fsend <path>'
    Write-Host '      see all options:  fsend --help'
    Mut "docs: https://github.com/$Repo#readme"
}
finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}
