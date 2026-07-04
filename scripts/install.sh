#!/usr/bin/env sh
set -eu

REPO="polius/fsend"
BINARY="fsend"
FSEND_VERSION="${FSEND_VERSION:-latest}"
PREFIX="${PREFIX:-}"

# Keyless-cosign identity of the release workflow. checksums.txt is signed
# in CI (see .goreleaser.yml); these pin who is allowed to have signed it.
COSIGN_IDENTITY_REGEXP="^https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/v.*$"
COSIGN_ISSUER="https://token.actions.githubusercontent.com"

if [ -n "$PREFIX" ]; then
    PREFIX_EXPLICIT=1
else
    PREFIX_EXPLICIT=0
fi

# Color only when stderr is a tty and NO_COLOR is unset/empty — the same
# auto-detection the fsend binary applies (https://no-color.org).
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
    esc="$(printf '\033')"
    C_RED="${esc}[31m" C_GRN="${esc}[32m" C_YLW="${esc}[33m" C_CYN="${esc}[36m" C_RST="${esc}[0m"
else
    C_RED='' C_GRN='' C_YLW='' C_CYN='' C_RST=''
fi

err()  { printf '%s✗%s %s\n' "$C_RED" "$C_RST" "$*" >&2; exit 1; }
info() { printf '%s›%s %s\n' "$C_CYN" "$C_RST" "$*" >&2; }
warn() { printf '%s!%s %s\n' "$C_YLW" "$C_RST" "$*" >&2; }
ok()   { printf '%s✓%s %s\n' "$C_GRN" "$C_RST" "$*" >&2; }

usage() {
    cat <<'EOF'
fsend installer

Usage:
  curl -fsSL https://getfsend.alzina.dev | sh
  curl -fsSL https://getfsend.alzina.dev | sh -s -- [-p DIR] [-v VERSION]

Flags:
  -p DIR        Install location (default: auto-pick a writable dir)
  -v VERSION    Version to install (default: latest)
  -h            Show this help and exit

Environment variables:
  PREFIX, FSEND_VERSION      Same as -p / -v (the flag wins)
  FSEND_REQUIRE_SIGNATURE=1  Refuse to install unless cosign verifies the release signature

Source: https://github.com/polius/fsend/blob/main/scripts/install.sh
EOF
}

need() {
    command -v "$1" >/dev/null 2>&1 || err "missing required command: $1"
}

run_elevated() {
    if [ "$(id -u 2>/dev/null || echo 1000)" = "0" ]; then
        "$@"
        return $?
    fi
    if command -v sudo >/dev/null 2>&1; then
        sudo "$@"
        return $?
    fi
    if command -v doas >/dev/null 2>&1; then
        doas "$@"
        return $?
    fi
    return 127
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
    case "$1-$2" in
        linux-amd64|linux-arm64|linux-386|linux-armv7|linux-armv6|linux-riscv64) ;;
        darwin-amd64|darwin-arm64) ;;
        windows-amd64|windows-arm64|windows-386) ;;
        freebsd-amd64|freebsd-arm64) ;;
        openbsd-amd64|openbsd-arm64) ;;
        *) err "no prebuilt binary for $1/$2 — build from source: go install github.com/${REPO}/cmd/fsend@latest" ;;
    esac
}

default_prefix() {
    pf_os="$1"
    case "$pf_os" in
        windows)
            echo "${HOME:-/usr/local}/bin"
            ;;
        *)
            if [ "$(id -u 2>/dev/null || echo 1000)" = "0" ] \
               || [ -w "/usr/local/bin" ]; then
                echo "/usr/local/bin"
            else
                echo "${HOME:-/tmp}/.local/bin"
            fi
            ;;
    esac
}

# GNU wget supports an explicit TLS floor; busybox wget (Alpine) does
# not and aborts on the unknown flag — it still validates certificates,
# so emit the flag only when supported. Callers expand the result
# unquoted on purpose (empty → no extra argument).
wget_tls_opts() {
    if wget --help 2>&1 | grep -q -- --secure-protocol; then
        printf %s "--secure-protocol=TLSv1_2"
    fi
}

download() {
    url="$1"
    out="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto '=https' --tlsv1.2 -o "$out" "$url" \
            || err "download failed: $url"
    elif command -v wget >/dev/null 2>&1; then
        # shellcheck disable=SC2046
        wget -q $(wget_tls_opts) -O "$out" "$url" \
            || err "download failed: $url"
    else
        err "need curl or wget to download fsend"
    fi
}

# winpath converts an MSYS/Cygwin path to a Windows path for native
# tools (System32 tar.exe, PowerShell), which can't resolve /tmp/...
# Falls through to the raw path where cygpath doesn't exist.
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

# POSIX sh has no function scope: bare assignments here would clobber the
# caller's globals. The archive name in particular is reused for extraction
# after this returns, so keep these parameters underscore-prefixed.
# verify_signature checks cosign's keyless signature on checksums.txt before
# any checksum derived from it is trusted. The SHA-256 check alone only proves
# the archive matches checksums.txt — both fetched over the same channel — so
# it catches corruption, not a tampered release. The signature proves the file
# came from the release workflow.
#
# cosign is optional: requiring it would break the common curl|sh path on hosts
# without it. When absent we fall back to checksum-only and say so; a present
# cosign that reports a bad signature is always fatal. Set
# FSEND_REQUIRE_SIGNATURE=1 to refuse to install without verification.
verify_signature() {
    _sums="$1"  # checksums.txt
    _base="$2"  # release download base URL
    _dir="$3"   # temp dir
    if ! command -v cosign >/dev/null 2>&1; then
        if [ "${FSEND_REQUIRE_SIGNATURE:-0}" = "1" ]; then
            err "FSEND_REQUIRE_SIGNATURE=1 but cosign is not installed"
        fi
        warn "cosign not found — verifying checksum only, not the release signature."
        warn "  install cosign for full authenticity, or set FSEND_REQUIRE_SIGNATURE=1 to require it."
        return 0
    fi
    download "${_base}/checksums.txt.bundle" "${_dir}/checksums.txt.bundle"
    cosign verify-blob \
        --bundle "${_dir}/checksums.txt.bundle" \
        --certificate-identity-regexp "$COSIGN_IDENTITY_REGEXP" \
        --certificate-oidc-issuer "$COSIGN_ISSUER" \
        "$_sums" >/dev/null 2>&1 \
        || err "cosign signature verification failed for checksums.txt — refusing to install"
    ok "signature verified"
}

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

    if mkdir -p "$PREFIX" 2>/dev/null; then
        return 0
    fi

    if [ "$PREFIX_EXPLICIT" = "1" ]; then
        info "creating $PREFIX (requires elevation)"
        run_elevated mkdir -p "$PREFIX" \
            || err "cannot create $PREFIX (no sudo/doas available)"
        return 0
    fi

    err "cannot create $PREFIX"
}

install_binary() {
    src="$1"
    dst="$PREFIX/$(basename "$src")"

    if [ -w "$PREFIX" ]; then
        mv "$src" "$dst"
        chmod 755 "$dst"
        return 0
    fi

    if [ "$PREFIX_EXPLICIT" = "0" ]; then
        err "$PREFIX is not writable (auto-selected). Set PREFIX=... to override."
    fi

    info "$PREFIX is not writable, attempting elevation"
    if ! run_elevated mv "$src" "$dst"; then
        err "could not move binary into $PREFIX (no sudo/doas available;
  re-run as root, or set PREFIX=\$HOME/.local/bin)"
    fi
    run_elevated chmod 755 "$dst" \
        || err "could not chmod $dst"
}

print_path_hint() {
    case "${SHELL:-}" in
        */fish)
            printf '    fish_add_path %s\n' "$PREFIX"
            ;;
        */zsh)
            printf '    echo '"'"'export PATH="%s:$PATH"'"'"' >> ~/.zshrc\n' "$PREFIX"
            ;;
        */bash)
            if [ "$(uname -s)" = "Darwin" ]; then
                printf '    echo '"'"'export PATH="%s:$PATH"'"'"' >> ~/.bash_profile\n' "$PREFIX"
            else
                printf '    echo '"'"'export PATH="%s:$PATH"'"'"' >> ~/.bashrc\n' "$PREFIX"
            fi
            ;;
        *)
            printf '    export PATH="%s:$PATH"\n' "$PREFIX"
            ;;
    esac
}

main() {
    need uname
    need mkdir
    need rm
    need awk
    need sed
    need grep

    os="$(detect_os)"
    arch="$(detect_arch)"
    check_release_target "$os" "$arch"

    if [ "$PREFIX_EXPLICIT" = "0" ]; then
        PREFIX="$(default_prefix "$os")"
    fi

    tmp="$(mktemp -d 2>/dev/null || mktemp -d -t fsend)"
    trap 'rm -rf "$tmp"' EXIT INT TERM HUP

    version="$FSEND_VERSION"
    if [ "$version" = "latest" ]; then
        # Resolve "latest" through the plain release-asset redirect, NOT
        # the GitHub API: unauthenticated API calls are capped at 60/hr
        # per IP, which fails installs from shared egress IPs (offices,
        # CI, universities). checksums.txt is needed anyway; the version
        # is recovered from the archive names inside it, and the tag is
        # rebuilt as "v<version>" (Go module tags are always v-prefixed).
        info "looking up latest release..."
        download "https://github.com/${REPO}/releases/latest/download/checksums.txt" "$tmp/checksums.txt"
        vnum="$(sed -n 's/.*[[:space:]]fsend_\([^_]*\)_.*/\1/p' "$tmp/checksums.txt" | head -n1)"
        [ -n "$vnum" ] || err "could not resolve latest version"
        version="v${vnum}"
    else
        # Accept "-v 1.0.0" and "-v v1.0.0" alike: release tags are
        # always v-prefixed, archive names never are.
        vnum="${version#v}"
        version="v${vnum}"
        info "downloading checksums"
        download "https://github.com/${REPO}/releases/download/${version}/checksums.txt" "$tmp/checksums.txt"
    fi
    info "verifying signature"
    verify_signature "$tmp/checksums.txt" \
        "https://github.com/${REPO}/releases/download/${version}" "$tmp"

    info "installing fsend ${version} for ${os}-${arch} into ${PREFIX}"

    case "$os" in
        windows) ext="zip";    bin_file="${BINARY}.exe" ;;
        *)       ext="tar.gz"; bin_file="${BINARY}"     ;;
    esac
    archive="fsend_${vnum}_${os}_${arch}.${ext}"

    info "downloading $archive"
    download "https://github.com/${REPO}/releases/download/${version}/${archive}" "$tmp/$archive"

    info "verifying checksum"
    verify_checksum "$tmp/$archive" "$tmp/checksums.txt"
    ok "checksum verified"

    info "extracting"
    case "$ext" in
        tar.gz) need tar; tar -xzf "$tmp/$archive" -C "$tmp" ;;
        zip)    extract_zip "$tmp/$archive" "$tmp" ;;
    esac
    [ -f "$tmp/$bin_file" ] || err "binary $bin_file not found in archive"

    ensure_prefix
    install_binary "$tmp/$bin_file"

    ok "installed: $PREFIX/$bin_file"

    found="$(command -v "$BINARY" 2>/dev/null || true)"
    if [ "$found" = "$PREFIX/$bin_file" ]; then
        installed_version="$("$BINARY" --version 2>/dev/null | head -n1 || echo "?")"
        ok "verify: $installed_version"
    elif [ -n "$found" ]; then
        # A stale install earlier on PATH shadows the fresh one — a
        # different problem than a missing PATH entry, so say which it is.
        printf '\n'
        warn "another fsend at $found takes precedence on your PATH. Put $PREFIX first with:"
        print_path_hint
        printf '\n  Then restart your shell, or run %s directly.\n' "$PREFIX/$bin_file"
    else
        printf '\n'
        warn "$PREFIX is not on your PATH. Add it with:"
        print_path_hint
        printf '\n  Then restart your shell, or run it now to try fsend.\n'
    fi

    printf '\n'
    printf 'Next: send a file with  fsend <path>\n'
    printf '      see all options:  fsend --help\n'
}

while getopts ":p:v:h" opt; do
    case "$opt" in
        p)  PREFIX="$OPTARG"; PREFIX_EXPLICIT=1 ;;
        v)  FSEND_VERSION="$OPTARG" ;;
        h)  usage; exit 0 ;;
        :)  err "option -$OPTARG requires an argument (use -h for help)" ;;
        \?) err "unknown option: -$OPTARG (use -h for help)" ;;
    esac
done
shift $((OPTIND - 1))

main "$@"
