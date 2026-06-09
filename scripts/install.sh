#!/usr/bin/env sh
set -eu

REPO="polius/fsend"
BINARY="fsend"
FSEND_VERSION="${FSEND_VERSION:-latest}"
PREFIX="${PREFIX:-}"

if [ -n "$PREFIX" ]; then
    PREFIX_EXPLICIT=1
else
    PREFIX_EXPLICIT=0
fi

err()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[36m›\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m!\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*" >&2; }

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

Environment variables (overridden by the matching flag):
  PREFIX, FSEND_VERSION

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

resolve_latest() {
    api="https://api.github.com/repos/${REPO}/releases/latest"
    # Same hardened TLS as download(): explicit https+TLS 1.2+ so a
    # downgrade can't feed us a tampered tag and the rest of the install
    # follows.
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto '=https' --tlsv1.2 "$api" \
            | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
            | head -n1
    else
        wget -q --secure-protocol=TLSv1_2 -O- "$api" \
            | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
            | head -n1
    fi
}

verify_checksum() {
    archive="$1"
    sums="$2"
    expected="$(grep " $(basename "$archive")$" "$sums" | awk '{print $1}')"
    [ -n "$expected" ] || err "no checksum found for $(basename "$archive")"

    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$archive" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$archive" | awk '{print $1}')"
    elif command -v sha256 >/dev/null 2>&1; then
        actual="$(sha256 -q "$archive")"
    else
        err "no sha256 tool available (need sha256sum, shasum, or sha256)"
    fi

    [ "$actual" = "$expected" ] \
        || err "checksum mismatch: expected $expected, got $actual"
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
        err "could not move binary into $PREFIX (no sudo/doas available;\n  re-run as root, or set PREFIX=\$HOME/.local/bin)"
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

    if [ "$PREFIX_EXPLICIT" = "0" ]; then
        PREFIX="$(default_prefix "$os")"
    fi

    version="$FSEND_VERSION"
    if [ "$version" = "latest" ]; then
        info "looking up latest release..."
        version="$(resolve_latest)"
        [ -n "$version" ] || err "could not resolve latest version"
    fi
    info "installing fsend ${version} for ${os}-${arch} into ${PREFIX}"

    vnum="${version#v}"
    case "$os" in
        windows) ext="zip";    bin_file="${BINARY}.exe" ;;
        *)       ext="tar.gz"; bin_file="${BINARY}"     ;;
    esac
    archive="fsend_${vnum}_${os}_${arch}.${ext}"
    archive_url="https://github.com/${REPO}/releases/download/${version}/${archive}"
    sums_url="https://github.com/${REPO}/releases/download/${version}/checksums.txt"

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
    [ -f "$tmp/$bin_file" ] || err "binary $bin_file not found in archive"

    ensure_prefix
    install_binary "$tmp/$bin_file"

    ok "installed: $PREFIX/$bin_file"

    if command -v "$BINARY" >/dev/null 2>&1 \
       && [ "$(command -v "$BINARY")" = "$PREFIX/$bin_file" ]; then
        installed_version="$("$BINARY" --version 2>/dev/null | head -n1 || echo "?")"
        ok "verify: $installed_version"
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
