#!/usr/bin/env sh
# fsend install script
#
# Usage:
#   curl -fsSL https://fs.alzina.dev/install.sh | sh
# or:
#   wget -qO- https://fs.alzina.dev/install.sh | sh
#
# Flags (set via env var):
#   PREFIX=/path/to/dir   Install location. If unset, picked automatically:
#                         a user-writable directory is preferred so no sudo
#                         is required. On Linux/macOS/*BSD: /usr/local/bin
#                         when writable (or running as root), else
#                         $HOME/.local/bin. On Windows shells (MSYS/Git Bash):
#                         $HOME/bin.
#   FSEND_VERSION=v0.1.0  Version to install (default: latest)
#
# Works on Linux, macOS, FreeBSD, and Windows under Git Bash / MSYS2 / Cygwin.
# Native Windows users without a POSIX shell should grab the .zip from the
# releases page directly.
#
# Source: https://github.com/polius/fsend/blob/main/install.sh
# Audit before piping into your shell. This script does what it says on the tin.

set -eu

# ---------- Configuration ----------

REPO="polius/fsend"
BINARY="fsend"
FSEND_VERSION="${FSEND_VERSION:-latest}"

# Track whether the user explicitly chose PREFIX. If they did, we respect it
# and use elevation when needed; if they didn't, we auto-pick a writable dir.
if [ "${PREFIX+set}" = "set" ] && [ -n "$PREFIX" ]; then
    PREFIX_EXPLICIT=1
else
    PREFIX_EXPLICIT=0
    PREFIX=""
fi

# ---------- Helpers ----------

err()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[36m›\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m!\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*" >&2; }

need() {
    command -v "$1" >/dev/null 2>&1 || err "missing required command: $1"
}

# Run a command, escalating via sudo or doas if the current user is not root.
# Returns 127 (command-not-found-style) if elevation is needed but no tool
# is available, so callers can decide whether to fall back.
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

# Pick a sensible default install prefix that the user can write to without
# elevation. Used only when PREFIX is not set in the environment.
default_prefix() {
    pf_os="$1"
    case "$pf_os" in
        windows)
            # Under MSYS/MinGW, $HOME is set and $HOME/bin is the natural place.
            echo "${HOME:-/usr/local}/bin"
            ;;
        *)
            # Prefer /usr/local/bin if we're root or it's already writable
            # (e.g. Homebrew on Intel macOS). Otherwise XDG-style user dir.
            if [ "$(id -u 2>/dev/null || echo 1000)" = "0" ] \
               || [ -w "/usr/local/bin" ]; then
                echo "/usr/local/bin"
            else
                echo "${HOME:-/tmp}/.local/bin"
            fi
            ;;
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
    elif command -v sha256 >/dev/null 2>&1; then
        actual="$(sha256 -q "$archive")"
    else
        err "no sha256 tool available (need sha256sum, shasum, or sha256)"
    fi

    [ "$actual" = "$expected" ] \
        || err "checksum mismatch: expected $expected, got $actual"
}

# ---------- Install ----------

# Ensure $PREFIX exists. Creates it without elevation when possible; only
# falls back to sudo/doas when the user explicitly chose a privileged path.
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

    # Fast path: $PREFIX is writable by the current user.
    if [ -w "$PREFIX" ]; then
        mv "$src" "$dst"
        chmod 755 "$dst"
        return 0
    fi

    # Auto-picked prefix shouldn't land here — default_prefix only returns
    # paths we can write to. If it does, surface the error rather than
    # silently prompting for a password the user didn't ask for.
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

# ---------- PATH guidance ----------

# Print a hint for adding $PREFIX to PATH, tailored to the user's shell.
print_path_hint() {
    case "${SHELL:-}" in
        */fish)
            printf '    fish_add_path %s\n' "$PREFIX"
            ;;
        */zsh)
            printf '    echo '"'"'export PATH="%s:$PATH"'"'"' >> ~/.zshrc\n' "$PREFIX"
            ;;
        */bash)
            # macOS bash uses ~/.bash_profile for login shells; Linux uses ~/.bashrc.
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

    if [ "$PREFIX_EXPLICIT" = "0" ]; then
        PREFIX="$(default_prefix "$os")"
    fi

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
        windows) ext="zip";    bin_file="${BINARY}.exe" ;;
        *)       ext="tar.gz"; bin_file="${BINARY}"     ;;
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
    [ -f "$tmp/$bin_file" ] || err "binary $bin_file not found in archive"

    ensure_prefix
    install_binary "$tmp/$bin_file"

    ok "installed: $PREFIX/$bin_file"

    # Quick sanity check — only if PREFIX is on PATH.
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

main "$@"
