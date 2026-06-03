#!/usr/bin/env sh
# fsend install script
#
# Usage:
#   curl -fsSL https://fs.alzina.dev/install.sh | sh
# or:
#   wget -qO- https://fs.alzina.dev/install.sh | sh
#
# Flags (set via env var):
#   PREFIX=/path/to/dir   Install location (default: /usr/local/bin)
#   FSEND_VERSION=v0.1.0  Version to install (default: latest)
#
# Source: https://github.com/polius/fsend/blob/main/install.sh
# Audit before piping into your shell. This script does what it says on the tin.

set -eu

# ---------- Configuration ----------

REPO="polius/fsend"
BINARY="fsend"
PREFIX="${PREFIX:-/usr/local/bin}"
FSEND_VERSION="${FSEND_VERSION:-latest}"

# ---------- Helpers ----------

err()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[36m›\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*" >&2; }

need() {
    command -v "$1" >/dev/null 2>&1 || err "missing required command: $1"
}

# ---------- Detect platform ----------

detect_os() {
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$os" in
        linux)        echo "linux" ;;
        darwin)       echo "darwin" ;;
        freebsd)      echo "freebsd" ;;
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
        i386|i686)     echo "386" ;;
        *)             err "unsupported architecture: $arch" ;;
    esac
}

# ---------- Download helpers ----------

# Use curl if present, else wget. Both will fail loudly on HTTP errors.
download() {
    url="$1"
    out="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto '=https' --tlsv1.2 -o "$out" "$url" \
            || err "download failed: $url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$out" "$url" \
            || err "download failed: $url"
    else
        err "need curl or wget to download fsend"
    fi
}

# Resolve the latest release tag via the GitHub API (no auth needed for public).
resolve_latest() {
    api="https://api.github.com/repos/${REPO}/releases/latest"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$api" \
            | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
            | head -n1
    else
        wget -qO- "$api" \
            | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
            | head -n1
    fi
}

# ---------- Verify ----------

verify_checksum() {
    archive="$1"
    sums="$2"
    expected="$(grep " $(basename "$archive")$" "$sums" | awk '{print $1}')"
    [ -n "$expected" ] || err "no checksum found for $(basename "$archive")"

    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$archive" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$archive" | awk '{print $1}')"
    else
        err "no sha256 tool available (need sha256sum or shasum)"
    fi

    [ "$actual" = "$expected" ] \
        || err "checksum mismatch: expected $expected, got $actual"
}

# ---------- Install ----------

install_binary() {
    src="$1"
    dst="$PREFIX/$BINARY"

    if [ -w "$PREFIX" ]; then
        mv "$src" "$dst"
        chmod 755 "$dst"
    else
        info "elevating with sudo to write to $PREFIX"
        sudo mv "$src" "$dst"
        sudo chmod 755 "$dst"
    fi
}

# ---------- Main ----------

main() {
    need uname
    need mkdir
    need rm
    need awk
    need sed
    need grep

    os="$(detect_os)"
    arch="$(detect_arch)"

    # Resolve version
    version="$FSEND_VERSION"
    if [ "$version" = "latest" ]; then
        info "looking up latest release..."
        version="$(resolve_latest)"
        [ -n "$version" ] || err "could not resolve latest version"
    fi
    info "installing fsend ${version} for ${os}-${arch} into ${PREFIX}"

    # Asset names: fsend_<version-without-v>_<os>_<arch>.tar.gz on unix,
    # .zip on windows. (Matches goreleaser default naming.)
    vnum="${version#v}"
    case "$os" in
        windows) ext="zip"   ;;
        *)       ext="tar.gz" ;;
    esac
    archive="fsend_${vnum}_${os}_${arch}.${ext}"
    archive_url="https://github.com/${REPO}/releases/download/${version}/${archive}"
    sums_url="https://github.com/${REPO}/releases/download/${version}/checksums.txt"

    # Stage in a temp dir; clean up on exit.
    tmp="$(mktemp -d 2>/dev/null || mktemp -d -t fsend)"
    trap 'rm -rf "$tmp"' EXIT INT TERM HUP

    info "downloading $archive"
    download "$archive_url" "$tmp/$archive"

    info "downloading checksums"
    download "$sums_url" "$tmp/checksums.txt"

    info "verifying checksum"
    verify_checksum "$tmp/$archive" "$tmp/checksums.txt"
    ok "checksum verified"

    info "extracting"
    case "$ext" in
        tar.gz) need tar; tar -xzf "$tmp/$archive" -C "$tmp" ;;
        zip)    need unzip; unzip -q "$tmp/$archive" -d "$tmp" ;;
    esac
    [ -f "$tmp/$BINARY" ] || err "binary $BINARY not found in archive"

    # Install
    install_binary "$tmp/$BINARY"

    ok "installed: $PREFIX/$BINARY"

    # Quick sanity check
    if command -v "$BINARY" >/dev/null 2>&1; then
        installed_version="$("$BINARY" --version 2>/dev/null | head -n1 || echo "?")"
        ok "verify: $installed_version"
    else
        printf '\n'
        info "$BINARY is not on your PATH yet. Add this to your shell rc:"
        printf '    export PATH="%s:$PATH"\n' "$PREFIX"
    fi

    printf '\n'
    printf 'Next: send a file with  fsend <path>\n'
    printf '      see all options:  fsend --help\n'
}

main "$@"
