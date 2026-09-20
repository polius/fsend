#!/usr/bin/env sh
# fsend installer — https://github.com/polius/fsend
#
# Downloads a release, verifies its SHA-256 checksum, and installs the
# binary. Per-user by design: refuses to run as root, never elevates,
# never asks for a password. Everything it does is readable
# top-to-bottom below.
#
#   curl -fsSL https://getfsend.alzina.dev | sh
#
set -eu

REPO="polius/fsend"
BINARY="fsend"
DOCS="https://github.com/${REPO}#readme"

FSEND_VERSION="${FSEND_VERSION:-latest}"
# FSEND_PREFIX is the documented name; PREFIX is kept because `fsend --update`
# sets it when re-running this installer pinned to the binary's directory.
PREFIX="${FSEND_PREFIX:-${PREFIX:-}}"
MODIFY_PATH=1
VERBOSE=0
# Test seam: point the installer at a local HTTP server to exercise the
# full download → verify → install path without a real release. Same-user
# trust, like rustup's RUSTUP_DIST_SERVER. A caller that redirects the
# release source also owns its transport (the HTTPS-only pin is dropped).
RELEASE_BASE="${FSEND_RELEASE_BASE_URL:-https://github.com/${REPO}/releases}"

if [ -n "$PREFIX" ]; then PREFIX_EXPLICIT=1; else PREFIX_EXPLICIT=0; fi

# Color only when stderr is a tty and NO_COLOR is unset/empty — the same
# auto-detection the fsend binary applies (https://no-color.org).
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
    esc="$(printf '\033')"
    C_RED="${esc}[31m" C_GRN="${esc}[32m" C_YLW="${esc}[33m" C_CYN="${esc}[36m" C_MUT="${esc}[2m" C_RST="${esc}[0m"
else
    C_RED='' C_GRN='' C_YLW='' C_CYN='' C_MUT='' C_RST=''
fi

err()  { printf '%s✗%s %s\n' "$C_RED" "$C_RST" "$*" >&2; exit 1; }
info() { printf '%s›%s %s\n' "$C_CYN" "$C_RST" "$*" >&2; }
warn() { printf '%s!%s %s\n' "$C_YLW" "$C_RST" "$*" >&2; }
ok()   { printf '%s✓%s %s\n' "$C_GRN" "$C_RST" "$*" >&2; }
mut()  { printf '%s%s%s\n' "$C_MUT" "$*" "$C_RST" >&2; }
vinfo() {
    if [ "$VERBOSE" = "1" ]; then
        info "$@"
    fi
}

usage() {
    cat <<'EOF'
fsend installer

Usage:
  curl -fsSL https://getfsend.alzina.dev | sh
  curl -fsSL https://getfsend.alzina.dev | sh -s -- [options]

Options:
  -p, --prefix DIR        Install location (default: auto-pick a writable dir)
  -v, --version VERSION   Version to install (default: latest)
  -n, --no-modify-path    Don't add the install dir to your shell config
  --verbose               Show the individual install steps
  -h, --help              Show this help and exit

Environment:
  FSEND_PREFIX, PREFIX    Same as -p/--prefix (the flag wins)
  FSEND_VERSION           Same as -v/--version (the flag wins)

Per-user install: the script refuses to run as root and never uses sudo.
More: https://github.com/polius/fsend#readme
EOF
}

need() {
    command -v "$1" >/dev/null 2>&1 || err "missing required command: $1"
}

detect_os() {
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$os" in
        linux)        echo "linux" ;;
        darwin)       echo "darwin" ;;
        freebsd)      echo "freebsd" ;;
        openbsd)      echo "openbsd" ;;
        msys*|mingw*|cygwin*) echo "windows" ;;
        *)            err "unsupported OS: $os" ;;
    esac
}

detect_arch() {
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)  echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        armv7*)        echo "armv7" ;;
        # 32-bit userland on a 64-bit ARM kernel: the armv7 binary is the
        # one that runs there. (aarch64 kernels reporting their native arch
        # despite a 32-bit userland, and Rosetta-translated x86_64 shells,
        # keep their native mapping.)
        armv8l|armv9l) echo "armv7" ;;
        armv6*)        echo "armv6" ;;
        riscv64)       echo "riscv64" ;;
        i386|i686)     echo "386" ;;
        *)             err "unsupported architecture: $arch" ;;
    esac
}

# Releases only cover a subset of the os × arch product (see the ignore
# list in .goreleaser.yml). Catch unbuilt combinations up front so the
# user sees "no prebuilt binary" instead of a mystifying download 404.
check_release_target() {
    # release-matrix:begin (synced with .goreleaser.yml; CI asserts equality)
    case "$1-$2" in
        linux-amd64|linux-arm64|linux-386|linux-armv7|linux-armv6|linux-riscv64) ;;
        darwin-amd64|darwin-arm64) ;;
        windows-amd64|windows-arm64|windows-386) ;;
        freebsd-amd64|freebsd-arm64) ;;
        openbsd-amd64|openbsd-arm64) ;;
        *) err "no prebuilt binary for $1/$2 — build from source: go install github.com/${REPO}/cmd/fsend@latest" ;;
    esac
    # release-matrix:end
}

default_prefix() {
    case "$1" in
        windows) echo "${HOME:-/usr/local}/bin" ;;
        *)
            if [ -w "/usr/local/bin" ]; then
                echo "/usr/local/bin"
            else
                echo "${HOME:-/tmp}/.local/bin"
            fi
            ;;
    esac
}

download() {
    url="$1"
    out="$2"
    # HTTPS-only, except through the test seam (FSEND_RELEASE_BASE_URL).
    if [ -n "${FSEND_RELEASE_BASE_URL:-}" ]; then
        pin=""
    else
        pin="--proto =https"
    fi
    if command -v curl >/dev/null 2>&1; then
        # Progress bar only on a tty (-s hides it, so swap the flag set).
        # shellcheck disable=SC2086  # $pin is an intentional word split
        if [ -t 2 ]; then
            curl $pin -fS#L --tlsv1.2 -o "$out" "$url" || err "download failed: $url"
        else
            curl $pin -fsSL --tlsv1.2 -o "$out" "$url" || err "download failed: $url"
        fi
    elif command -v wget >/dev/null 2>&1; then
        # shellcheck disable=SC2046 disable=SC2086  # $pin/$(...) split intentionally
        wget $(wget_flags) $pin -O "$out" "$url" || err "download failed: $url"
    else
        err "need curl or wget to download fsend"
    fi
}

# Flags for the wget flavor in use: an explicit TLS floor when supported
# (GNU wget; busybox aborts on the unknown flag but still validates
# certificates), and progress that mirrors the curl behavior above.
wget_flags() {
    if wget --help 2>&1 | grep -q -- --secure-protocol; then printf '%s ' --secure-protocol=TLSv1_2; fi
    if [ -t 2 ]; then
        if wget --help 2>&1 | grep -q -- --show-progress; then printf '%s ' --show-progress; fi
    else
        printf '%s ' -q
    fi
}

# winpath converts an MSYS/Cygwin path to a Windows path for native
# tools (System32 tar.exe, PowerShell), which can't resolve /tmp/...
winpath() {
    cygpath -w "$1" 2>/dev/null || printf '%s' "$1"
}

# extract_zip unpacks a release zip with whatever the host actually has.
# Git Bash — the shell the README points Windows users at — ships
# neither unzip nor bsdtar in its own /usr/bin (its tar is GNU tar,
# which cannot read zip), so the System32 bsdtar (Windows 10+) and
# PowerShell fallbacks are the paths that fire there.
extract_zip() {
    zip="$1"
    dest="$2"
    if command -v unzip >/dev/null 2>&1; then
        unzip -q "$zip" -d "$dest"
        return
    fi
    if command -v tar >/dev/null 2>&1 && tar --version 2>/dev/null | grep -q bsdtar; then
        tar -xf "$zip" -C "$dest"
        return
    fi
    systar="${SYSTEMROOT:-C:\\Windows}/System32/tar.exe"
    if [ -x "$systar" ]; then
        "$systar" -xf "$(winpath "$zip")" -C "$(winpath "$dest")"
        return
    fi
    if command -v powershell.exe >/dev/null 2>&1; then
        powershell.exe -NoProfile -NonInteractive -Command \
            "Expand-Archive -LiteralPath '$(winpath "$zip")' -DestinationPath '$(winpath "$dest")' -Force"
        return
    fi
    err "no zip extractor found (need unzip, bsdtar, or PowerShell)"
}

# The checksum catches corruption and truncation. checksums.txt and the
# archive both come from the same HTTPS host, so this is an integrity
# check, not a guarantee against a tampered release — that is GitHub's
# side of the trust model (see docs/security.md).
verify_checksum() {
    _archive="$1"
    _sums="$2"
    _expected="$(grep " $(basename "$_archive")$" "$_sums" | awk '{print $1}')"
    [ -n "$_expected" ] || err "no checksum found for $(basename "$_archive")"

    if command -v sha256sum >/dev/null 2>&1; then
        _actual="$(sha256sum "$_archive" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        _actual="$(shasum -a 256 "$_archive" | awk '{print $1}')"
    elif command -v sha256 >/dev/null 2>&1; then
        _actual="$(sha256 -q "$_archive")"
    elif command -v openssl >/dev/null 2>&1; then
        _actual="$(openssl dgst -sha256 "$_archive" | awk '{print $NF}')"
    else
        err "no sha256 tool available (need sha256sum, shasum, sha256, or openssl)"
    fi

    [ "$_actual" = "$_expected" ] \
        || err "checksum mismatch: expected $_expected, got $_actual"
}

ensure_prefix() {
    [ -d "$PREFIX" ] && return 0
    mkdir -p "$PREFIX" 2>/dev/null \
        || err "cannot create $PREFIX — pick a writable location with -p/--prefix"
}

install_binary() {
    src="$1"
    dst="$PREFIX/$(basename "$src")"
    if [ ! -w "$PREFIX" ]; then
        err "$PREFIX is not writable — pick a writable location with -p/--prefix"
    fi
    mv "$src" "$dst"
    chmod 755 "$dst"
}

# shellcheck disable=SC2016  # the literal $PATH is the point
print_path_hint() {
    case "$(basename "${SHELL:-sh}")" in
        fish) printf '    fish_add_path %s\n' "$PREFIX" >&2 ;;
        *)
            printf '    export PATH="%s:$PATH"\n' "$PREFIX" >&2
            ;;
    esac
}

# Prepend PREFIX to the user's PATH by appending one line to their shell
# config (opencode/rustup style). Idempotent; opt out with --no-modify-path.
# Returns 0 when the PATH is (or was made) fine, 1 when manual action is
# needed — a hint is printed in that case, so main stays quiet.
configure_path() {
    [ "$MODIFY_PATH" = "1" ] || return 1
    case ":${PATH}:" in *":$PREFIX:"*) return 0 ;; esac
    if [ -z "${HOME:-}" ]; then
        warn "HOME is not set — add $PREFIX to your PATH manually:"
        print_path_hint
        return 0
    fi

    xdg="${XDG_CONFIG_HOME:-$HOME/.config}"
    case "$(basename "${SHELL:-sh}")" in
        fish)
            line="fish_add_path $PREFIX"
            rc="$xdg/fish/config.fish"
            ;;
        zsh)
            line="export PATH=\"$PREFIX:\$PATH\""
            zd="${ZDOTDIR:-$HOME}"
            rc="$zd/.zshrc"
            [ -f "$rc" ] || rc="$zd/.zshenv"
            [ -f "$rc" ] || rc="$zd/.zshrc"
            ;;
        bash)
            line="export PATH=\"$PREFIX:\$PATH\""
            if [ "$(uname -s)" = "Darwin" ]; then
                rc="$HOME/.bash_profile"
                [ -f "$rc" ] || rc="$HOME/.bashrc"
                [ -f "$rc" ] || rc="$HOME/.bash_profile"
            else
                rc="$HOME/.bashrc"
                [ -f "$rc" ] || rc="$HOME/.bash_profile"
                [ -f "$rc" ] || rc="$HOME/.bashrc"
            fi
            ;;
        *)
            line="export PATH=\"$PREFIX:\$PATH\""
            rc="$HOME/.profile"
            ;;
    esac

    # Already referenced in the rc file (maybe added by hand) — leave it alone.
    if [ -f "$rc" ] && grep -qF -- "$PREFIX" "$rc"; then
        return 0
    fi

    mkdir -p "${rc%/*}" 2>/dev/null || true
    if [ -w "${rc%/*}" ] && { [ ! -e "$rc" ] || [ -w "$rc" ]; }; then
        printf '\n# fsend\n%s\n' "$line" >> "$rc"
        ok "PATH updated in $rc — open a new shell"
    else
        warn "could not update PATH — add $PREFIX manually:"
        print_path_hint
        return 1
    fi
}

main() {
    need id uname mkdir rm awk sed grep basename head tr

    # Per-user by design. Running a network script as root is exactly how a
    # compromised mirror becomes a compromised machine, and a root install
    # has no single-user PATH story. Containers/CI should fetch the release
    # tarball directly instead.
    if [ "$(id -u)" = "0" ]; then
        err "refusing to run as root — fsend installs per-user, without sudo.
  run as your normal user, or download a release archive by hand:
  https://github.com/${REPO}/releases"
    fi

    os="$(detect_os)"
    arch="$(detect_arch)"
    check_release_target "$os" "$arch"

    if [ "$PREFIX_EXPLICIT" = "0" ]; then
        PREFIX="$(default_prefix "$os")"
    fi

    # Upgrade awareness: say what's already installed before touching it.
    if _prev="$(command -v "$BINARY" 2>/dev/null)"; then
        _cur="$("$_prev" --version 2>/dev/null | head -n1 || true)"
        [ -n "$_cur" ] && mut "currently installed: $_cur"
    fi

    tmp="$(mktemp -d 2>/dev/null || mktemp -d -t fsend.XXXXXXXX)"
    cleanup() { rm -rf "$tmp"; }
    trap cleanup EXIT
    trap 'cleanup; exit 130' INT
    trap 'cleanup; exit 143' TERM
    trap 'cleanup; exit 129' HUP

    version="$FSEND_VERSION"
    if [ "$version" = "latest" ]; then
        # Resolve "latest" through the plain release-asset redirect, NOT
        # the GitHub API: unauthenticated API calls are capped at 60/hr
        # per IP, which fails installs from shared egress IPs (offices,
        # CI, universities). checksums.txt is needed anyway; the version
        # is recovered from the archive names inside it, and the tag is
        # rebuilt as "v<version>" (Go module tags are always v-prefixed).
        vinfo "resolving the latest release..."
        download "${RELEASE_BASE}/latest/download/checksums.txt" "$tmp/checksums.txt"
        vnum="$(sed -n 's/.*[[:space:]]fsend_\([^_]*\)_.*/\1/p' "$tmp/checksums.txt" | head -n1)"
        [ -n "$vnum" ] || err "could not resolve the latest version"
        version="v${vnum}"
    else
        # Accept "-v 1.2.3" and "-v v1.2.3" alike: release tags are
        # always v-prefixed, archive names never are.
        vnum="${version#v}"
        version="v${vnum}"
        vinfo "downloading checksums"
        download "${RELEASE_BASE}/download/${version}/checksums.txt" "$tmp/checksums.txt"
    fi

    info "installing fsend $version (${os}-${arch})"

    case "$os" in
        windows) ext="zip";    bin_file="${BINARY}.exe" ;;
        *)       ext="tar.gz"; bin_file="${BINARY}"     ;;
    esac
    archive="fsend_${vnum}_${os}_${arch}.${ext}"

    vinfo "downloading $archive"
    download "${RELEASE_BASE}/download/${version}/${archive}" "$tmp/$archive"

    # The checksum catches corruption and truncation. checksums.txt and the
    # archive both come from the same HTTPS host, so this is an integrity
    # check, not a guarantee against a tampered release — that is GitHub's
    # side of the trust model (see docs/security.md).
    vinfo "verifying checksum"
    verify_checksum "$tmp/$archive" "$tmp/checksums.txt"
    ok "verified"

    vinfo "extracting"
    case "$ext" in
        tar.gz) need tar; tar -xzf "$tmp/$archive" -C "$tmp" ;;
        zip)    extract_zip "$tmp/$archive" "$tmp" ;;
    esac
    [ -f "$tmp/$bin_file" ] || err "binary $bin_file not found in archive"

    ensure_prefix
    install_binary "$tmp/$bin_file"

    configure_path || {
        if [ "$MODIFY_PATH" = "0" ]; then
            warn "PATH untouched (--no-modify-path) — add $PREFIX manually:"
            print_path_hint
        fi
    }

    # GitHub Actions: expose the install dir to later steps of the workflow.
    if [ "${GITHUB_ACTIONS:-}" = "true" ] && [ -n "${GITHUB_PATH:-}" ]; then
        printf '%s\n' "$PREFIX" >> "$GITHUB_PATH"
        ok "added $PREFIX to \$GITHUB_PATH"
    fi

    # Quiet on success, loud on failure: the outro below is the install
    # confirmation; this only speaks up if the fresh binary is broken.
    _ver="$("$PREFIX/$bin_file" --version 2>/dev/null | head -n1 || true)"
    [ -n "$_ver" ] || warn "the installed binary did not respond to --version"

    found="$(command -v "$BINARY" 2>/dev/null || true)"
    if [ -n "$found" ] && [ "$found" != "$PREFIX/$bin_file" ]; then
        # A stale install earlier on PATH shadows the fresh one in *this*
        # shell; the configure_path line above fixes it after a restart.
        warn "$found shadows the new binary — open a new shell, or run:"
        printf '    %s\n' "$PREFIX/$bin_file" >&2
    fi

    printf '\n' >&2
    mut "fsend $version installed → $PREFIX/$bin_file"
    printf 'fsend <path>    %ssend a file%s\n' "$C_MUT" "$C_RST" >&2
    printf 'fsend --help    %sall options%s\n' "$C_MUT" "$C_RST" >&2
    mut "docs: $DOCS"
}

while [ $# -gt 0 ]; do
    case "$1" in
        -p|--prefix)
            if [ $# -lt 2 ] || [ -z "$2" ]; then
                err "option $1 requires a directory argument (use -h for help)"
            fi
            PREFIX="$2" PREFIX_EXPLICIT=1
            shift 2 ;;
        -v|--version)
            if [ $# -lt 2 ] || [ -z "$2" ]; then
                err "option $1 requires a version argument (use -h for help)"
            fi
            FSEND_VERSION="$2"
            shift 2 ;;
        -n|--no-modify-path) MODIFY_PATH=0; shift ;;
        --verbose) VERBOSE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) err "unexpected argument: $1 (use -h for help)" ;;
    esac
done

main
