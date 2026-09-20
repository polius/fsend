#!/usr/bin/env sh
# End-to-end smoke test for scripts/install.sh: builds a fake release
# tree, serves it over local HTTP, and asserts the installer's behavior
# (latest/pinned resolution, checksum verification, install location,
# PATH handling, flags, root refusal).
#
# Runs locally and in CI. The installer is pointed at the local server
# via FSEND_RELEASE_BASE_URL, so no network and no real release is used.
#
# Usage: sh scripts/smoke-install.sh [--with-root-test] [--with-busybox-test]
set -eu

WITH_ROOT=0
WITH_BUSYBOX=0
for arg in "$@"; do
    case "$arg" in
        --with-root-test) WITH_ROOT=1 ;;
        --with-busybox-test) WITH_BUSYBOX=1 ;;
        *) printf 'smoke: unknown flag: %s\n' "$arg" >&2; exit 2 ;;
    esac
done

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALLER="$HERE/install.sh"
VER=9.9.9

WORK="$(mktemp -d "${TMPDIR:-/tmp}/fsend-smoke.XXXXXXXX")"
SERVER_PID=""
cleanup() {
    if [ -n "$SERVER_PID" ]; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { PASS=$((PASS + 1)); printf 'ok   %s\n' "$1"; }
PASS=0

for tool in python3 curl tar; do
    command -v "$tool" >/dev/null 2>&1 \
        || { printf 'smoke: missing %s — skipping\n' "$tool" >&2; exit 0; }
done

# --- fixture release ------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) printf 'smoke: unsupported arch %s — skipping\n' "$arch" >&2; exit 0 ;;
esac
case "$os" in
    linux|darwin) ;;
    *) printf 'smoke: unsupported os %s — skipping\n' "$os" >&2; exit 0 ;;
esac

FIX="$WORK/fixture"
ARCHIVE="fsend_${VER}_${os}_${arch}.tar.gz"
mkdir -p "$FIX/download/v$VER" "$FIX/latest/download"
cat > "$FIX/fsend" <<EOF
#!/bin/sh
printf 'fsend $VER (smoke)\n'
EOF
chmod 755 "$FIX/fsend"
tar -czf "$FIX/download/v$VER/$ARCHIVE" -C "$FIX" fsend
(
    cd "$FIX/download/v$VER" \
        && { sha256sum "$ARCHIVE" 2>/dev/null || shasum -a 256 "$ARCHIVE"; }
) > "$FIX/download/v$VER/checksums.txt"

# The busybox scenario installs from inside an Alpine container, which
# asks for LINUX archives while the fixture targets the host's os/arch —
# add the linux ones (both arches; a tar of a shell stub is tiny).
if [ "$WITH_BUSYBOX" = "1" ]; then
    for A in amd64 arm64; do
        tar -czf "$FIX/download/v$VER/fsend_${VER}_linux_${A}.tar.gz" -C "$FIX" fsend
        (
            cd "$FIX/download/v$VER" \
                && { sha256sum "fsend_${VER}_linux_${A}.tar.gz" 2>/dev/null || shasum -a 256 "fsend_${VER}_linux_${A}.tar.gz"; }
        ) >> "$FIX/download/v$VER/checksums.txt"
    done
fi
cp "$FIX/download/v$VER/checksums.txt" "$FIX/latest/download/checksums.txt"

# --- local release server --------------------------------------------------
PORT=18763
while ! python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',$PORT));s.close()" 2>/dev/null; do
    PORT=$((PORT + 1))
    [ "$PORT" -gt 18800 ] && fail "no free port found"
done
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$FIX" >/dev/null 2>&1 &
SERVER_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
    curl -fsS -o /dev/null "http://127.0.0.1:$PORT/latest/download/checksums.txt" 2>/dev/null && break
    sleep 0.3
done
BASE="http://127.0.0.1:$PORT"

# Runs the installer fully isolated: fixed PATH (no cosign, no host junk),
# per-scenario HOME, /bin/sh as the shell so rc handling is deterministic.
SMOKE_PATH=/usr/bin:/bin
EXTRA_ENV=""
run_installer() {
    _home="$1"
    shift
    # shellcheck disable=SC2086  # EXTRA_ENV is deliberate word splitting
    env -i PATH="$SMOKE_PATH" HOME="$_home" SHELL=/bin/sh \
        FSEND_RELEASE_BASE_URL="$BASE" $EXTRA_ENV \
        sh "$INSTALLER" "$@"
}

# 1. fresh default install: resolves "latest", verifies, installs, writes rc.
H1="$WORK/home1"
mkdir -p "$H1"
if [ -w /usr/local/bin ]; then
    # Don't pollute the host; default-prefix selection is asserted in CI.
    run_installer "$H1" -p "$H1/.local/bin" >"$WORK/out1" 2>&1 \
        || { cat "$WORK/out1" >&2; fail "scenario1: installer failed"; }
    [ -x "$H1/.local/bin/fsend" ] || fail "scenario1: binary not installed"
else
    run_installer "$H1" >"$WORK/out1" 2>&1 \
        || { cat "$WORK/out1" >&2; fail "scenario1: installer failed"; }
    [ -x "$H1/.local/bin/fsend" ] || fail "scenario1: binary not at \$HOME/.local/bin"
fi
grep -q "✓ verified" "$WORK/out1" || fail "scenario1: no verification line"
grep -q "fsend v$VER installed" "$WORK/out1" || fail "scenario1: no outro line"
grep -qF "export PATH=\"$H1/.local/bin:\$PATH\"" "$H1/.profile" \
    || fail "scenario1: PATH line not appended to .profile"
pass "fresh install: latest resolution, checksum, rc PATH line, outro"

# 2. pinned version, with and without the v prefix.
H2="$WORK/home2"
mkdir -p "$H2"
run_installer "$H2" -p "$H2/bin" -v "$VER" >"$WORK/out2" 2>&1 \
    || { cat "$WORK/out2" >&2; fail "scenario2: pinned $VER failed"; }
[ -x "$H2/bin/fsend" ] || fail "scenario2: pinned install missing"
run_installer "$H2" -p "$H2/bin2" --version "v$VER" >"$WORK/out2b" 2>&1 \
    || { cat "$WORK/out2b" >&2; fail "scenario2: v-prefixed install failed"; }
[ -x "$H2/bin2/fsend" ] || fail "scenario2: v-prefixed install missing"
pass "pinned versions ($VER and v$VER)"

# 3. --no-modify-path leaves rc files alone.
H3="$WORK/home3"
mkdir -p "$H3"
run_installer "$H3" -p "$H3/bin" -n >"$WORK/out3" 2>&1 \
    || { cat "$WORK/out3" >&2; fail "scenario3: installer failed"; }
[ -x "$H3/bin/fsend" ] || fail "scenario3: install missing"
[ ! -e "$H3/.profile" ] || fail "scenario3: rc written despite --no-modify-path"
pass "--no-modify-path skips rc files"

# 4. FSEND_PREFIX env is honored.
H4="$WORK/home4"
mkdir -p "$H4"
EXTRA_ENV="FSEND_PREFIX=$H4/bin"
run_installer "$H4" >"$WORK/out4" 2>&1 \
    || { cat "$WORK/out4" >&2; fail "scenario4: installer failed"; }
EXTRA_ENV=""
[ -x "$H4/bin/fsend" ] || fail "scenario4: FSEND_PREFIX ignored"
pass "FSEND_PREFIX env honored"

# 5. upgrade awareness: a second run reports what's already installed.
H5="$WORK/home5"
mkdir -p "$H5"
run_installer "$H5" -p "$H5/bin" >/dev/null 2>&1 || fail "scenario5: first install failed"
SMOKE_PATH="$H5/bin:/usr/bin:/bin"
run_installer "$H5" -p "$H5/bin" >"$WORK/out5" 2>&1 \
    || { cat "$WORK/out5" >&2; fail "scenario5: reinstall failed"; }
SMOKE_PATH=/usr/bin:/bin
grep -q "currently installed: fsend $VER (smoke)" "$WORK/out5" \
    || fail "scenario5: no 'currently installed' line on reinstall"
pass "reinstall reports currently installed version"

# 6. GitHub Actions: install dir lands in $GITHUB_PATH.
H6="$WORK/home6"
GHFILE="$WORK/ghpath"
mkdir -p "$H6"
: > "$GHFILE"
EXTRA_ENV="GITHUB_ACTIONS=true GITHUB_PATH=$GHFILE"
run_installer "$H6" -p "$H6/bin" >"$WORK/out6" 2>&1 \
    || { cat "$WORK/out6" >&2; fail "scenario6: installer failed"; }
EXTRA_ENV=""
grep -qF "$H6/bin" "$GHFILE" || fail "scenario6: \$GITHUB_PATH not populated"
pass "GITHUB_PATH populated in Actions"

# 7. flag handling.
H7="$WORK/home7"
mkdir -p "$H7"
run_installer "$H7" -v >"$WORK/out7a" 2>&1 && fail "scenario7: -v without value accepted"
run_installer "$H7" --bogus >"$WORK/out7b" 2>&1 && fail "scenario7: unknown flag accepted"
run_installer "$H7" positional >"$WORK/out7c" 2>&1 && fail "scenario7: positional arg accepted"
run_installer "$H7" -h >"$WORK/out7d" 2>&1 || fail "scenario7: -h failed"
run_installer "$H7" -p "$H7/bin" -v "$VER" --verbose >"$WORK/out7e" 2>&1 \
    || { cat "$WORK/out7e" >&2; fail "scenario7: --verbose run failed"; }
grep -q "downloading fsend_" "$WORK/out7e" || fail "scenario7: --verbose hides the steps"
pass "flag handling (missing value, unknown flag, positional, -h, --verbose)"

# 8. a corrupted archive is rejected by the checksum. Runs after every
# full-install scenario: it poisons the shared fixture archive.
H8="$WORK/home8"
mkdir -p "$H8"
printf 'X' | dd of="$FIX/download/v$VER/$ARCHIVE" bs=1 seek=50 conv=notrunc 2>/dev/null
run_installer "$H8" -p "$H8/bin" -v "$VER" >"$WORK/out8" 2>&1 \
    && { cat "$WORK/out8" >&2; fail "scenario8: corrupt archive installed"; }
grep -q "checksum mismatch" "$WORK/out8" || fail "scenario8: no checksum mismatch error"
pass "corrupted archive rejected by checksum"

# 9. root refusal (needs passwordless sudo).
if [ "$WITH_ROOT" = "1" ]; then
    if sudo -n true 2>/dev/null; then
        # The redirect is performed by our shell (not sudo) on purpose: we
        # want the installer's output captured for assertion.
        # shellcheck disable=SC2024
        sudo -n env PATH=/usr/bin:/bin HOME=/root FSEND_RELEASE_BASE_URL="$BASE" \
            sh "$INSTALLER" -p "$WORK/rootbin" -v "$VER" >"$WORK/root" 2>&1 \
            && fail "scenario9: root install allowed"
        grep -q "refusing to run as root" "$WORK/root" \
            || { cat "$WORK/root" >&2; fail "scenario9: root refusal message missing"; }
        pass "root refused"
    else
        printf 'smoke: no passwordless sudo — skipping root test\n' >&2
    fi
fi

# 10. busybox (alpine) via docker: the wget fallback path, ash semantics,
# and the root refusal again in a true root context. Skips when docker is
# unavailable. On Linux the container shares the host network; on macOS
# Docker Desktop reaches the host via host.docker.internal.
if [ "$WITH_BUSYBOX" = "1" ]; then
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
        case "$(uname -s)" in
            Darwin) net=""; dbase="http://host.docker.internal:$PORT" ;;
            *) net="--network host"; dbase="$BASE" ;;
        esac
        # The container's default user is root: the installer must refuse.
        # shellcheck disable=SC2086  # $net is empty on macOS Docker Desktop
        docker run --rm $net -v "$HERE:/src:ro" -e FSEND_RELEASE_BASE_URL="$dbase" \
            alpine:latest sh /src/install.sh -p /tmp/fsend-root -v "$VER" \
            >"$WORK/busybox-root" 2>&1 \
            && fail "busybox: root install allowed"
        grep -q "refusing to run as root" "$WORK/busybox-root" \
            || { cat "$WORK/busybox-root" >&2; fail "busybox: root refusal message missing"; }
        # Then a real per-user install: busybox wget (no curl in alpine),
        # ash semantics, rc write, and a binary that runs.
        cat > "$WORK/busybox-user.sh" <<EOF
adduser -D user >/dev/null
su user -c 'export HOME=/home/user SHELL=/bin/sh FSEND_RELEASE_BASE_URL=$dbase; sh /src/install.sh -v $VER'
/home/user/.local/bin/fsend --version
grep -qF 'export PATH="/home/user/.local/bin:\$PATH"' /home/user/.profile
EOF
        # shellcheck disable=SC2086  # $net is empty on macOS Docker Desktop
        docker run --rm $net -v "$HERE:/src:ro" -v "$WORK:/work:ro" \
            alpine:latest sh /work/busybox-user.sh >"$WORK/busybox-out" 2>&1 \
            || { cat "$WORK/busybox-out" >&2; fail "busybox: install failed"; }
        pass "busybox (alpine): root refused, wget-path install + rc line"
    else
        printf 'smoke: docker unavailable — skipping busybox test\n' >&2
    fi
fi

printf '%d scenario(s) passed\n' "$PASS"
