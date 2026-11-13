#!/usr/bin/env python3
"""validate-ux.py — empirical colour/UI audit for the fsend CLI.

Builds the CLI from the working tree, starts an isolated pairing server
on loopback, and runs every user-facing scenario as concurrent pty
sessions (sender + receiver). Both sides' raw stderr is captured and
audited per character: each checked element must render in exactly the
semantic colour token the design assigns it, with the right weight (bold,
faint). With the background unknown — the default here: no emulator
answers the OSC 11 query in a bare pty — output may use only the
terminal's ANSI colours; with a declared or detected background every
truecolour value must clear WCAG AA (4.5:1) on that theme's usual
backgrounds. The grid holds (glyphs in column 0, text from column 2),
green appears only on ✓, only ✓ ✗ ! ? → ← and the spinner are glyphs,
and degraded modes stay byte-clean (NO_COLOR, FSEND_PLAIN, piped stderr
→ zero colour escapes, ASCII glyph fallbacks). Text checks assert on
what the user reads (headlines, hints, exit codes), not just on escapes.

Scenarios: single file, folder, conflict + overwrite prompt, password
flow (wrong → correct), decline, text send, error rendering, NO_COLOR,
--help, piped degradation, light and dark palettes, a 40-column
terminal, Ctrl-C while waiting, a late second receiver, shell
completion, notifications, usage-error paths and plain mode. Exit
status: 0 when every check passes.

Unix only (uses pty). Run from the repo root:

    python3 scripts/validate-ux.py
"""
import json
import os
import pty
import re
import select
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import fcntl
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Truecolour hex per semantic token and palette. These must match
# internal/uxlog/glyphs.go's colourToken table (dark, light).
PALETTES = {
    'dark':  {'accent': '#e0805a', 'primary': '#bb9af7', 'success': '#9ece6a',
              'warning': '#e5c07b', 'error': '#f7768e', 'info': '#7dcfff'},
    'light': {'accent': '#b54c22', 'primary': '#7847bd', 'success': '#587539',
              'warning': '#7d6500', 'error': '#b3261e', 'info': '#007197'},
}
# The usual backgrounds of each kind of theme: black, VS Code, One Dark,
# Solarized, Tokyo Night; white, Solarized, off-white, Catppuccin Latte,
# Gruvbox.
BACKGROUNDS = {
    'dark': [(0, 0, 0), (0x1e, 0x1e, 0x1e), (0x28, 0x2c, 0x34), (0x00, 0x2b, 0x36), (0x1a, 0x1b, 0x26)],
    'light': [(255, 255, 255), (0xfd, 0xf6, 0xe3), (0xfa, 0xfa, 0xfa), (0xef, 0xf1, 0xf5), (0xfb, 0xf1, 0xc7)],
}
# ANSI SGR per token when the background is unknown.
ANSI = {'accent': '36', 'primary': '35', 'success': '32', 'warning': '33', 'error': '31', 'info': '36'}


def sgr(hexcol):
    r, g, b = (int(hexcol[i:i + 2], 16) for i in (1, 3, 5))
    return f'38;2;{r};{g};{b}'


def tokens(theme):
    if theme == 'unknown':
        return tuple(ANSI[k] for k in ('accent', 'primary', 'success', 'warning', 'error', 'info'))
    p = PALETTES[theme]
    return tuple(sgr(p[k]) for k in ('accent', 'primary', 'success', 'warning', 'error', 'info'))


# The harness sets neither FSEND_THEME nor COLORFGBG and no emulator answers
# the background query, so the default runs use the terminal's ANSI colours.
ORANGE, VIOLET, GREEN, YELLOW, RED, CYAN = tokens('unknown')
DEFAULT = None                 # terminal default foreground
GLYPHS = '✓✗!?→←'
SPINNER = '⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'
OLD_GLYPHS = '⚠ℹ⟳⇢⇣⇄┌│└'


def rel_lum(r, g, b):
    def lin(c):
        c /= 255
        return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4
    return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)


def contrast(a, b):
    la, lb = rel_lum(*a), rel_lum(*b)
    return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)


class Ctx:
    def __init__(self):
        self.fg, self.bold, self.faint = None, False, False


OSC = re.compile(r'\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)')


def line_colors(line):
    """Walk one raw line; return (plain_text, per-char (fg, bold, faint) list).
    OSC sequences (the background query) are invisible."""
    line = OSC.sub('', line)
    ctx, chars, colors = Ctx(), [], []
    i = 0
    while i < len(line):
        if line[i] == '\x1b' and i + 1 < len(line) and line[i + 1] == '[':
            # CSI: skip to its final byte; only SGR (ending in 'm') mutates
            # state — cursor moves and erase-sequences pass through.
            j = i + 2
            while j < len(line) and not ('@' <= line[j] <= '~'):
                j += 1
            if j >= len(line):
                break
            if line[j] != 'm':
                i = j + 1
                continue
            parts = line[i + 2:j].split(';')
            k = 0
            while k < len(parts):
                p = parts[k]
                if p in ('0', ''):
                    ctx.fg, ctx.bold, ctx.faint = None, False, False
                elif p == '1':
                    ctx.bold = True
                elif p == '2':
                    ctx.faint = True
                elif p == '22':
                    ctx.bold, ctx.faint = False, False
                elif p == '39':
                    ctx.fg = None
                elif p == '38':
                    if k + 1 < len(parts) and parts[k + 1] == '2':
                        ctx.fg = '38;2;' + ';'.join(parts[k + 2:k + 5])
                        k += 4
                    elif k + 1 < len(parts) and parts[k + 1] == '5':
                        ctx.fg = '38;5;' + parts[k + 2]
                        k += 2
                elif p in ('30', '31', '32', '33', '34', '35', '36', '37',
                           '90', '91', '92', '93', '94', '95', '96', '97'):
                    ctx.fg = p
                k += 1
            i = j + 1
            continue
        chars.append(line[i])
        colors.append((ctx.fg, ctx.bold, ctx.faint))
        i += 1
    return ''.join(chars), colors


def split_stream(stream):
    """Lines on \n AND \r: spinner/bar redraws reuse the line via \r, so
    every redraw frame must audit as its own line."""
    return stream.replace('\r\n', '\n').replace('\r', '\n').split('\n')


def screen_lines(stream):
    """What stays on screen: per \n-line, the last \r-frame (spinner and bar
    redraws overwrite each other), with the plain text and colours."""
    out = []
    for raw in OSC.sub('', stream).replace('\r\n', '\n').split('\n'):
        frames = [f for f in raw.split('\r') if line_colors(f)[0].strip()]
        if frames:
            out.append(line_colors(frames[-1]))
    return out


def probe(stream, context, needle):
    """(fg, bold, faint) set covering `needle` on the first plain line that
    contains `context` (None = any line). None when not found."""
    for line in split_stream(stream):
        plain, colors = line_colors(line)
        if context and context not in plain:
            continue
        idx = plain.find(needle)
        if idx == -1:
            continue
        if idx + len(needle) > len(colors):
            return None
        return {colors[k] for k in range(idx, idx + len(needle))}
    return None


RESULTS = []


def check(sc, label, stream, context, needle, expect_fg, expect_bold=None, expect_faint=None):
    got = probe(stream, context, needle)
    if got is None:
        RESULTS.append((sc, label, 'NOT FOUND', False))
        return
    fgs = {fg for fg, _, _ in got}
    bolds = {b for _, b, _ in got}
    faints = {f for _, _, f in got}
    ok = fgs == ({expect_fg} if expect_fg else {None})
    if expect_bold is not None:
        ok = ok and bolds == {expect_bold}
    if expect_faint is not None:
        ok = ok and faints == {expect_faint}
    RESULTS.append((sc, label,
                    f'fg={sorted(fgs, key=str)} bold={sorted(bolds, key=str)} faint={sorted(faints, key=str)}', ok))


def glob_ok(sc, label, ok, got=None):
    RESULTS.append((sc, label, got or ('ok' if ok else 'VIOLATION'), ok))


def has_text(sc, label, stream, needle):
    """The plain text (escapes stripped) contains needle."""
    plain = '\n'.join(line_colors(l)[0] for l in split_stream(stream))
    glob_ok(sc, label, needle in plain, None if needle in plain else f'missing {needle!r}')


def lacks_text(sc, label, stream, needle):
    plain = '\n'.join(line_colors(l)[0] for l in split_stream(stream))
    glob_ok(sc, label, needle not in plain, None if needle not in plain else f'found {needle!r}')


SGR_FG = re.compile(r'\x1b\[(?:[0-9;]*;)?38;2;(\d+);(\d+);(\d+)m')
SGR_ANY = re.compile(r'\x1b\[([0-9;]*)m')


def contrast_ok(sc, stream, theme):
    """Every truecolour foreground in stream clears 4.5:1 on the theme's
    usual backgrounds — the WCAG AA bar the palette promises."""
    worst = None
    for m in SGR_FG.finditer(stream):
        fg = tuple(int(x) for x in m.groups())
        for bg in BACKGROUNDS[theme]:
            c = contrast(fg, bg)
            if worst is None or c < worst[0]:
                worst = (c, fg, bg)
    ok = worst is None or worst[0] >= 4.5
    got = 'no colours' if worst is None else f'min {worst[0]:.2f}:1 ({worst[1]} on {worst[2]})'
    glob_ok(sc, f'every colour ≥ 4.5:1 ({theme})', ok, got)


def ansi_only(sc, stream):
    """With the background unknown, only SGR 30-37 / 90-97 colours, plus
    bold, faint and their resets — the terminal's own palette."""
    allowed = {'', '0', '1', '2', '22', '39', '30', '31', '32', '33', '34', '35', '36', '37',
               '90', '91', '92', '93', '94', '95', '96', '97'}
    bad = sorted({m.group(1) for m in SGR_ANY.finditer(stream)
                  if not set(m.group(1).split(';')) <= allowed})
    glob_ok(sc, 'unknown background: ANSI colours only', not bad, repr(bad[:3]) if bad else None)


def grid_ok(sc, stream, label=''):
    """The grid on what stays on screen: a line starts with a glyph (then a
    space) in column 0, or with 2, 4 or 6 spaces; green only on ✓; no glyph
    outside the small set every monospace font carries."""
    off, green, old = [], [], []
    for plain, colors in screen_lines(stream):
        lead = len(plain) - len(plain.lstrip(' '))
        if lead == 0:
            if not (plain[0] in GLYPHS + SPINNER and plain[1:2] == ' '):
                off.append(plain)
        elif lead not in (2, 4, 6):
            off.append(plain)
        for ch, (fg, _, _) in zip(plain, colors):
            if fg == GREEN and ch != '✓':
                green.append(plain)
                break
        if any(g in plain for g in OLD_GLYPHS):
            old.append(plain)
    pre = f'{label}: ' if label else ''
    glob_ok(sc, pre + 'glyphs in column 0, text from column 2', not off, repr(off[:2]) if off else None)
    glob_ok(sc, pre + 'green only on ✓', not green, repr(green[:1]) if green else None)
    glob_ok(sc, pre + 'only ✓ ✗ ! ? → ← and the spinner', not old, repr(old[:1]) if old else None)


class PtyRun(threading.Thread):
    """Runs argv in a pty; captures raw output while feeds fire on schedule."""

    def __init__(self, argv, cwd, feed=None, runtime=30, env=None, cols=100, bg=None):
        super().__init__(daemon=True)
        self.argv, self.cwd, self.feed, self.runtime, self.env = argv, cwd, feed, runtime, env
        self.cols = cols
        # bg: an "rgb:RRRR/GGGG/BBBB" the fake terminal answers the OSC 11
        # query with (then DA1), as a real emulator would. None: silence.
        self.bg = bg
        self.answered = False
        self.out = b''
        self.pid = None
        self.fd = None
        self.done = False
        self.status = None

    def child_env(self):
        e = dict(os.environ)
        e['TERM'] = 'xterm-256color'
        e['COLORTERM'] = 'truecolor'
        e['LANG'] = 'en_US.UTF-8'
        # A deterministic baseline: nothing from the caller's shell may pick
        # the palette, plain mode, the server or a code.
        for k in ('NO_COLOR', 'FORCE_COLOR', 'FSEND_THEME', 'COLORFGBG', 'FSEND_PLAIN',
                  'LC_ALL', 'LC_CTYPE', 'FSEND_NO_NOTIFY', 'FSEND_CODE', 'FSEND_SERVER',
                  'FSEND_PASSWORD', 'FSEND_NO_CLIPBOARD', 'SSH_CONNECTION', 'SSH_TTY'):
            e.pop(k, None)
        # Never the user's real clipboard: S22 opts back in with a fake helper.
        e['FSEND_NO_CLIPBOARD'] = '1'
        # The isolated HOME and config, so every client pairs through the
        # harness's own server and never reads or writes the user's real
        # fsend config (or reaches the public server it may point at).
        if H is not None:
            e.update(H.client_env())
        e['FSEND_NO_UPDATE_CHECK'] = '1'
        if self.env:
            e.update(self.env)
        return e

    def run(self):
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(self.cwd)
            os.execvpe(self.argv[0], self.argv, self.child_env())
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 30, self.cols, 0, 0))
        start, fi = time.time(), 0
        while time.time() - start < self.runtime:
            try:
                r, _, _ = select.select([fd], [], [], 0.2)
            except OSError:
                break
            if r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    break
                if not data:
                    break
                self.out += data
                if self.bg and not self.answered and b'\x1b]11;?' in self.out:
                    os.write(fd, f'\x1b]11;{self.bg}\x1b\\\x1b[?62;22c'.encode())
                    self.answered = True
            while self.feed and fi < len(self.feed) and time.time() - start > self.feed[fi][0]:
                os.write(fd, self.feed[fi][1])
                fi += 1
            done, st = os.waitpid(pid, os.WNOHANG)
            if done:
                self.status = os.waitstatus_to_exitcode(st)
                while True:
                    try:
                        r, _, _ = select.select([fd], [], [], 0.2)
                    except OSError:
                        break
                    if not r:
                        break
                    try:
                        data = os.read(fd, 65536)
                    except OSError:
                        break
                    if not data:
                        break
                    self.out += data
                break
        # The pty hit EOF (the child exited) before WNOHANG caught it: reap
        # it for the exit status, briefly, so a wedged child can't hang us.
        for _ in range(25):
            if self.status is not None:
                break
            try:
                done, st = os.waitpid(pid, os.WNOHANG)
            except ChildProcessError:
                break
            if done:
                self.status = os.waitstatus_to_exitcode(st)
                break
            time.sleep(0.2)
        try:
            os.close(fd)
        except OSError:
            pass
        self.done = True

    def wait_for(self, pattern, timeout=20):
        rx = re.compile(pattern)
        for _ in range(int(timeout / 0.2)):
            m = rx.search(self.text)
            if m:
                return m
            time.sleep(0.2)
        return None

    def stop(self):
        if self.pid and not self.done:
            try:
                os.kill(self.pid, signal.SIGKILL)
            except OSError:
                pass
        if self.fd:
            try:
                os.close(self.fd)
            except OSError:
                pass
        self.join(timeout=3)

    @property
    def text(self):
        return self.out.decode('utf-8', 'replace')


class Harness:
    def __init__(self):
        self.tmp = tempfile.mkdtemp(prefix='fsend-ux-')
        self.bin = os.path.join(self.tmp, 'fsend')
        self.home = os.path.join(self.tmp, 'home')
        self.server = None
        self.port = None
        subprocess.run(['go', 'build', '-o', self.bin, './cmd/fsend'],
                       cwd=ROOT, check=True)
        os.makedirs(self.home)

    def client_env(self, **extra):
        # Isolated HOME: config persists into the temp dir, never the
        # user's real fsend configuration.
        e = {'HOME': self.home, 'XDG_CONFIG_HOME': os.path.join(self.home, '.config')}
        e.update(extra)
        return e

    def start_server(self):
        s = socket.socket()
        s.bind(('127.0.0.1', 0))
        self.port = s.getsockname()[1]
        s.close()
        # The relay gets its own free UDP port too: the default (:443) would
        # collide with any other server running on this machine.
        u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        u.bind(('127.0.0.1', 0))
        relay = u.getsockname()[1]
        u.close()
        env = dict(os.environ, **self.client_env(FSEND_SERVER_ADDR=f'127.0.0.1:{self.port}',
                                                 FSEND_RELAY_ADDR=f'127.0.0.1:{relay}'))
        self.server = subprocess.Popen(
            [self.bin, 'server'], cwd=self.tmp, env=env,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 15
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(
                        f'http://127.0.0.1:{self.port}/health', timeout=1) as r:
                    if r.status == 200:
                        return
            except OSError:
                time.sleep(0.2)
        raise RuntimeError('pairing server did not become healthy')

    def configure_client(self):
        # http://: the harness server speaks plain HTTP, and a bare
        # host:port means HTTPS.
        subprocess.run([self.bin, '--connect', f'http://127.0.0.1:{self.port}'],
                       cwd=self.tmp, env=dict(os.environ, **self.client_env()),
                       check=True, capture_output=True)

    def run_client(self, args, **kw):
        env = dict(os.environ, **self.client_env())
        return subprocess.run([self.bin] + args, cwd=self.tmp, env=env, **kw)

    def cleanup(self):
        for _ in range(2):
            out = subprocess.run(['pgrep', '-f', self.bin], capture_output=True, text=True)
            for pid in out.stdout.split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except (OSError, ValueError):
                    pass
            time.sleep(0.3)
        if self.server:
            try:
                self.server.kill()
            except OSError:
                pass
        shutil.rmtree(self.tmp, ignore_errors=True)

    def kill_clients(self):
        out = subprocess.run(['pgrep', '-f', self.bin + ' '], capture_output=True, text=True)
        for pid in out.stdout.split():
            # The server's command line matches too ("<bin> server"); it
            # must outlive the scenarios.
            if self.server and pid == str(self.server.pid):
                continue
            try:
                os.kill(int(pid), signal.SIGKILL)
            except (OSError, ValueError):
                pass


H = None  # set in main()


def fresh(name):
    d = os.path.join(H.tmp, 'scenarios', name)
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    with open(os.path.join(d, 'small.txt'), 'wb') as f:
        f.write(os.urandom(52))
    os.makedirs(os.path.join(d, 'myproj', 'assets'))
    for fn, sz in (('assets/video1.bin', 20_000_000), ('assets/video2.bin', 15_000_000),
                   ('README.md', 7)):
        with open(os.path.join(d, 'myproj', fn), 'wb') as f:
            f.write(os.urandom(sz))
    os.makedirs(os.path.join(d, 'recv_out'))
    return d


def pair(sc, d, sender_args, recv_args, recv_feed, sender_runtime=40, env=None, cols=100, bg=None):
    """Concurrent sender (pty) + receiver (pty). Returns (sender, receiver)."""
    snd = PtyRun([H.bin] + sender_args, d, runtime=sender_runtime, env=env, cols=cols, bg=bg)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((sc, 'sender produced no code', 'TIMEOUT', False))
        return snd, None
    code = m.group(1)
    time.sleep(0.5)
    rcv = PtyRun([H.bin, code] + recv_args, d, feed=recv_feed, runtime=25, env=env, cols=cols, bg=bg)
    rcv.start()
    rcv.join()
    snd.join(timeout=8)
    snd.stop()
    return snd, rcv


# ---------------------------------------------------------------- scenarios
def s1_single_file():
    s = 'S1 single-file accept'
    d = fresh('s1')
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b'y\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    m = re.search(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b', sp)
    code = m.group(1) if m else 'zzz-zzzz-zzz'
    check(s, 'arrow accent', sp, 'Sending', '→', ORANGE)
    check(s, 'subject bold default', sp, 'Sending', 'small.txt', DEFAULT, True)
    check(s, 'size detail faint', sp, '1 file', '1 file · 52 B', DEFAULT, False, True)
    check(s, 'share code accent+bold', sp, None, code, ORANGE, True)
    check(s, '"fsend" bold default', sp, code, 'fsend', DEFAULT, True)
    lacks_text(s, 'no box around the command', sp, '│')
    has_text(s, 'command line is runnable as is', sp, '      fsend ' + code + '\n')
    check(s, 'tagline faint', sp, 'One receiver', 'One receiver', DEFAULT, False, True)
    # The install-and-receive hint pairs through the default server; this
    # harness uses its own, so the hint must stay away.
    lacks_text(s, 'no install hint on a server of its own', sp, 'No fsend there yet')
    check(s, 'spinner accent', sp, 'Waiting for receiver', '⠋', ORANGE)
    check(s, 'summary ✓ success', sp, 'Sent', '✓', GREEN)
    check(s, 'summary subject bold', sp, 'Sent', 'small.txt', DEFAULT, True)
    check(s, 'summary detail faint', sp, '52 B ·', '52 B', DEFAULT, False, True)
    check(s, 'recv arrow accent', rv, 'Incoming', '←', ORANGE)
    check(s, 'recv subject bold', rv, 'Incoming', 'small.txt', DEFAULT, True)
    check(s, 'prompt ? primary+bold', rv, 'Save to', '?', VIOLET, True)
    check(s, 'destination bold', rv, 'Save to', './recv_out/', DEFAULT, True)
    check(s, 'recv summary subject bold', rv, 'Saved', './recv_out/small.txt', DEFAULT, True)
    glob_ok(s, 'no grey 244', '38;5;244' not in sp and '38;5;244' not in rv)
    # What the user reads: who connected, what went where.
    has_text(s, 'sender names who accepted', sp, ' accepted')
    has_text(s, 'sender summary names subject + peer', sp, 'Sent small.txt to ')
    has_text(s, 'receiver names the saved file', rv, 'Saved ./recv_out/small.txt')
    lacks_text(s, 'no progress bar under 1 MB', rv, '100%')
    glob_ok(s, 'no notification for a short run', '\x1b]9;' not in sp and '\x07' not in sp)
    ansi_only(s, sp + rv)
    grid_ok(s, sp + rv)


def s2_folder():
    s = 'S2 folder accept'
    d = fresh('s2')
    snd, rcv = pair(s, d, ['--exclude', 'node_modules', 'myproj'],
                    ['--out', 'recv_out'], [(4.0, b'y\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'sender subject bold', sp, 'Sending', 'myproj/', DEFAULT, True)
    check(s, 'listing name default', sp, 'video1.bin', 'assets/video1.bin', DEFAULT, False, False)
    check(s, 'listing size faint', sp, 'video1.bin', '20 MB', DEFAULT, False, True)
    has_text(s, 'listing rows in column 4', sp, '\n    assets/video1.bin')
    check(s, 'recv subject bold', rv, 'Incoming', 'myproj/', DEFAULT, True)
    check(s, 'recv listing name default', rv, 'video1.bin', 'assets/video1.bin', DEFAULT, False, False)
    check(s, 'filled bar accent', rv, '%', '━', ORANGE)
    has_text(s, 'sender summary names the folder', sp, 'Sent myproj/ to ')
    has_text(s, 'receiver names folder and place', rv, 'Saved myproj/ to ./recv_out/')
    ansi_only(s, sp + rv)
    grid_ok(s, sp + rv)


def s3_conflict():
    s = 'S3 conflict prompt'
    d = fresh('s3')
    with open(os.path.join(d, 'recv_out', 'small.txt'), 'wb') as f:
        f.write(os.urandom(12))
    # One question covers the accept and the existing file; "s" skips it.
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b's\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'conflict question ? primary', rv, 'already exists', '?', VIOLET, True)
    has_text(s, 'conflict question', rv, '1 file here already exists and differs')
    check(s, 'delete consequence warning', rv, '[r]eplace', '(deletes yours)', YELLOW)
    check(s, 'conflict name default', rv, '→', 'small.txt', DEFAULT, False, False)
    check(s, 'size change faint', rv, '→', '12 B → 52 B', DEFAULT, False, True)
    check(s, 'kept-by-choice: no glyph', rv, 'Saved 0 of 1', 'Saved', DEFAULT)
    has_text(s, 'kept count in the detail', rv, '1 kept')
    has_text(s, 'sender: nothing sent, the receiver kept it', sp, '1 kept by the receiver')
    lacks_text(s, 'sender does not warn about the receiver\'s choice', sp, '! Nothing sent')
    # Skipping is a choice, not a failure.
    glob_ok(s, 'kept-by-choice exits 0', rcv is not None and rcv.status == 0,
            f'exit {rcv.status if rcv else None}')
    ansi_only(s, sp + rv)
    grid_ok(s, sp + rv)


def s4_password():
    s = 'S4 password flow'
    d = fresh('s4')
    snd = PtyRun([H.bin, 'small.txt', '--password'], d, feed=[(2.0, b'\n')], runtime=40)
    snd.start()
    m = snd.wait_for(r'Suggested password: (\S+)')
    if not m:
        RESULTS.append((s, 'sender suggestion missing', 'TIMEOUT', False))
        snd.stop()
        return
    pw = m.group(1)
    code_m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    time.sleep(0.5)
    rcv = PtyRun([H.bin, code_m.group(1), '--out', 'recv_out'], d,
                 feed=[(4.0, b'wrongpw\n'), (6.5, (pw + '\n').encode()), (8.5, b'y\n')],
                 runtime=25)
    rcv.start(); rcv.join(); snd.join(timeout=8); snd.stop()
    sp, rv = snd.text, rcv.text
    check(s, 'sender instruction primary', sp, 'Press Enter to use it',
          'Press Enter to use it, or type your own:', VIOLET)
    check(s, 'receiver prompt primary', rv, 'Enter the password',
          'Enter the password the sender shared:', VIOLET)
    check(s, 'password context ! warning', rv, 'password-protected', '!', YELLOW, True)
    check(s, 'retry prompt primary', rv, 'Wrong password', 'Wrong password — try again', VIOLET)
    check(s, 'chip warning colour', rv, 'password required', 'password required', YELLOW)
    check(s, 'transfer succeeded', rv, 'Saved', '✓', GREEN)
    has_text(s, 'sender waits on the password, naming the device', sp, ' to enter the password')
    glob_ok(s, 'suggestion is four words', re.fullmatch(r'[a-z]+(-[a-z]+){3}', pw) is not None, pw)
    has_text(s, 'code block says a password is needed', sp, 'they need the password too')
    ansi_only(s, sp + rv)


def s5_decline():
    s = 'S5 receiver declines'
    d = fresh('s5')
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b'n\n')])
    rv = rcv.text if rcv else ''
    sp = snd.text if snd else ''
    has_text(s, 'decline: no glyph, text in column 2', rv, '\n  Declined.')
    lacks_text(s, "the receiver's own choice carries no code", rv, '[E006]')
    # U1: the HELLO reply is not consent — the sender waits, naming the
    # device, and a decline never prints "accepted".
    has_text(s, 'sender waits on the device while it decides', sp, ' to accept')
    lacks_text(s, 'a decline never prints "accepted"', sp, ' accepted')
    has_text(s, 'sender names who declined', sp, 'declined the transfer. [E006]')
    glob_ok(s, "receiver's own decline exits 0", rcv is not None and rcv.status == 0,
            f'exit {rcv.status if rcv else None}')
    glob_ok(s, 'sender sees the decline as E006 (exit 6)', snd.status == 6, f'exit {snd.status}')


def s6_text():
    s = 'S6 text send'
    d = fresh('s6')
    snd, rcv = pair(s, d, ['--text', 'wifi: hunter2'], [], [(3.0, b'\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'arrow accent', sp, 'Sending text', '→', ORANGE)
    check(s, '"Sending text" default', sp, 'Sending text', 'Sending text', DEFAULT, False)
    # Short text is sanitized and printed, never saved: no question first.
    lacks_text(s, 'short text needs no prompt', rv, 'Show the text?')
    has_text(s, 'text printed', rv, 'wifi: hunter2')
    has_text(s, 'sender summary says text', sp, 'Sent text to ')


def s7_errors():
    s = 'S7 error rendering'
    d = fresh('s7')
    out = PtyRun([H.bin, 'nope.txt'], d, runtime=6)
    out.start(); out.join()
    check(s, 'cross error glyph', out.text, 'E025', '✗', RED)
    has_text(s, 'sentence leads, code follows', out.text, 'No such file or directory: nope.txt [E025]')
    # U3: a code typed as three words receives (no sender here: E002).
    words = PtyRun([H.bin, 'abc', 'defg', 'jkm'], d, runtime=20)
    words.start(); words.join()
    has_text(s, 'three-word code receives', words.text, '[E002]')
    lacks_text(s, 'three-word code is not three missing files', words.text, 'No such file')
    out2 = PtyRun([H.bin, 'abc-defg-jk'], d, runtime=6)
    out2.start(); out2.join()
    check(s, 'code-typo hint accent', out2.text, 'receive code', 'abc-defg-jkm', ORANGE, True)
    # Usage errors lead with the specific reason, and typos get a "did you mean".
    out3 = PtyRun([H.bin, '--overwirte', 'x'], d, runtime=6)
    out3.start(); out3.join()
    has_text(s, 'unknown flag suggests the real one', out3.text, 'did you mean --overwrite?')
    lacks_text(s, 'no generic "Invalid usage" over a reason', out3.text, 'Invalid usage')
    glob_ok(s, 'usage error exits 24', out3.status == 24, f'exit {out3.status}')
    out4 = PtyRun([H.bin, 'recieve'], d, runtime=6)
    out4.start(); out4.join()
    has_text(s, 'subcommand typo hint', out4.text, 'no "receive" subcommand')
    out5 = PtyRun([H.bin, 'help', 'nope'], d, runtime=6)
    out5.start(); out5.join()
    glob_ok(s, 'unknown help topic is E024', '[E024]' in out5.text and out5.status == 24,
            f'exit {out5.status}')
    for o in (out, out2, out3, out4, out5):
        lacks_text(s, 'no E099 on an error path', o.text, 'E099')
    check(s, 'catalog code faint, after the sentence', out.text, 'nope.txt', '[E025]', DEFAULT, False, True)
    ansi_only(s, out.text + out2.text + out3.text)


def s8_no_color():
    s = 'S8 NO_COLOR degradation'
    d = fresh('s8')
    sp = PtyRun([H.bin, 'small.txt'], d, runtime=6, env={'NO_COLOR': '1'})
    sp.start(); sp.join()
    glob_ok(s, 'sender zero colour SGR',
            not re.search(r'\x1b\[(3[0-9]|38;|1;38|9[0-7])m', sp.text))
    glob_ok(s, 'command block', 'On the other machine, run' in sp.text)


def s9_help():
    s = 'S9 --help'
    d = fresh('s9')
    out = PtyRun([H.bin, '--help'], d, runtime=5)
    out.start(); out.join()
    t = out.text
    check(s, 'flag info accent', t, None, '--yes', CYAN)
    check(s, 'URL info accent', t, None, 'https://github.com/polius/fsend', CYAN)
    check(s, 'header bold', t, None, 'RECEIVING', None, True)
    piped = H.run_client(['--help'], capture_output=True)
    glob_ok(s, 'piped help zero escapes', b'\x1b[' not in piped.stdout)
    n = piped.stdout.decode().count('\n')
    glob_ok(s, 'everyday help fits ~35 lines', n <= 36, f'{n} lines')
    adv = H.run_client(['help', 'advanced'], capture_output=True)
    glob_ok(s, 'help advanced lists the environment', b'FSEND_CODE' in adv.stdout and adv.returncode == 0)
    ansi_only(s, t)


def s10_piped():
    s = 'S10 piped degradation'
    d = fresh('s10')
    env = dict(os.environ, **H.client_env())
    p = subprocess.Popen([H.bin, 'small.txt'], cwd=d, env=env,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         stdin=subprocess.DEVNULL)
    time.sleep(3)
    p.kill()
    err = p.stderr.read().decode('utf-8', 'replace')
    out = p.stdout.read().decode('utf-8', 'replace')
    glob_ok(s, 'piped sender zero colour SGR',
            not re.search(r'\x1b\[(3[0-9]|38;|1;38|9[0-7])m', err))
    glob_ok(s, 'ASCII glyph fallback', '[*] Waiting for receiver' in err)
    glob_ok(s, 'command block', 'On the other machine, run' in err)
    glob_ok(s, 'piped stderr has no query or escape', '\x1b' not in err)
    glob_ok(s, 'no Ctrl-C hint in logs', 'Ctrl-C' not in err)
    glob_ok(s, 'piped stdout carries the bare code',
            re.fullmatch(r'[a-z]{3}-[a-z]{4}-[a-z]{3}\n', out) is not None, repr(out))
    # An empty stdin (cron, CI) is not a send: help, exit 0, no code.
    q = H.run_client([], capture_output=True, stdin=subprocess.DEVNULL, timeout=10)
    glob_ok(s, '/dev/null stdin shows help, not a send',
            b'USAGE' in q.stdout and b'Sending stdin' not in q.stderr and q.returncode == 0,
            f'exit {q.returncode}')


def s11_themes():
    s = 'S11 light/dark palettes'
    for theme in ('light', 'dark'):
        d = fresh('s11' + theme)
        snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b'y\n')],
                        env={'FSEND_THEME': theme})
        sp, rv = snd.text, rcv.text if rcv else ''
        acc, pri, suc, _, _, _ = tokens(theme)
        m = re.search(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b', sp)
        check(s, f'{theme}: share code accent', sp, None, m.group(1) if m else 'zzz', acc, True)
        check(s, f'{theme}: prompt ? primary', rv, 'Save to', '?', pri, True)
        check(s, f'{theme}: summary ✓ success', sp, None, '✓', suc)
        glob_ok(s, f'{theme}: declared background skips the query', '\x1b]11;?' not in sp + rv)
        contrast_ok(s, sp + rv, theme)


def s12_narrow():
    s = 'S12 40-column terminal'
    d = fresh('s12')
    snd, rcv = pair(s, d, ['myproj'], ['--out', 'recv_out', '--yes'], [], cols=40)
    rv = rcv.text if rcv else ''
    bars = [line_colors(l)[0] for l in split_stream(rv) if '%' in line_colors(l)[0]]
    glob_ok(s, 'progress frames rendered', bool(bars), f'{len(bars)} frames')
    cut = [b for b in bars if re.search(r'\d…|…\d', b)]
    glob_ok(s, 'no number cut mid-digit', not cut, repr(cut[:1]) if cut else None)
    wide = [b for b in bars if len(b) > 39]
    glob_ok(s, 'frames fit the width', not wide, repr(wide[:1]) if wide else None)


def s13_cancel():
    s = 'S13 Ctrl-C while waiting'
    d = fresh('s13')
    snd = PtyRun([H.bin, 'small.txt'], d, feed=[(2.5, b'\x03')], runtime=10)
    snd.start(); snd.join()
    segs = [line_colors(l)[0] for l in split_stream(snd.text)]
    glued = [x for x in segs if 'Cancelling' in x and 'Waiting' in x]
    glob_ok(s, 'cancel notice on its own line', not glued and any('Cancelling' in x for x in segs),
            repr(glued[:1]) if glued else None)
    glob_ok(s, 'cancel exits 130', snd.status == 130, f'exit {snd.status}')


def s14_late_arriver():
    s = 'S14 late second receiver'
    d = fresh('s14')
    snd = PtyRun([H.bin, 'myproj'], d, runtime=40)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
        return
    code = m.group(1)
    time.sleep(0.5)
    first = PtyRun([H.bin, code, '--out', 'recv_out'], d, feed=[(12.0, b'y\n')], runtime=30)
    first.start()
    time.sleep(4)
    late = PtyRun([H.bin, code, '--out', 'recv_out'], d, runtime=25)
    late.start(); late.join(); first.join(); snd.join(timeout=8); snd.stop()
    lacks_text(s, 'late receiver never sees E099', late.text, 'E099')
    lines = [line_colors(l)[0].strip() for l in split_stream(late.text)]
    errs = [l for l in lines if '[E0' in l]
    glob_ok(s, 'late receiver gets E003', any('[E003]' in l for l in errs),
            errs[-1] if errs else repr([l for l in lines if l][-1:]))


def s15_completion():
    s = 'S15 shell completion'
    files = H.run_client(['__complete', ''], capture_output=True, text=True)
    glob_ok(s, 'positional completes files', files.stdout.rstrip().endswith(':0'),
            files.stdout.strip().splitlines()[-1:])
    dirs = H.run_client(['__complete', '--out', ''], capture_output=True, text=True)
    glob_ok(s, '--out completes directories', dirs.stdout.rstrip().endswith(':16'),
            dirs.stdout.strip().splitlines()[-1:])


def s16_notify():
    s = 'S16 notification after a long run'
    for env, want in (({}, True), ({'FSEND_NO_NOTIFY': '1'}, False)):
        d = fresh('s16' + str(want))
        snd = PtyRun([H.bin, 'small.txt'], d, runtime=40, env=env)
        snd.start()
        m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
        if not m:
            snd.stop()
            RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
            return
        time.sleep(10.5)  # past NotifyAfter: the user may have looked away
        rcv = PtyRun([H.bin, m.group(1), '--out', 'recv_out', '--yes'], d, runtime=20, env=env)
        rcv.start(); rcv.join(); snd.join(timeout=8); snd.stop()
        got = '\x1b]9;fsend: Sent' in snd.text
        label = 'notifies after 10 s' if want else 'FSEND_NO_NOTIFY silences it'
        glob_ok(s, label, got == want)
        if want:
            seg = snd.text.split('\x1b]9;', 1)[-1]
            glob_ok(s, 'one bell, nothing glued after', seg.count('\x07') == 2 and 'E0' not in seg)


def s17_plain():
    s = 'S17 plain mode'
    d = fresh('s17')
    for env in ({'FSEND_PLAIN': '1'}, {'LC_ALL': 'C'}):
        p = PtyRun([H.bin, 'small.txt'], d, runtime=4, env=env)
        p.start(); p.join()
        tag = ' '.join(f'{k}={v}' for k, v in env.items())
        glob_ok(s, f'{tag}: no escape sequences', '\x1b[' not in p.text)
        glob_ok(s, f'{tag}: ASCII markers, one line per state',
                '[*] Waiting for receiver' in p.text and '⠋' not in p.text)


def s18_spinner_width():
    """U2: a waiting sender keeps exactly one spinner row at every width —
    a line wider than the pane would leave a row behind on each redraw —
    and the share code stays on screen."""
    s = 'S18 waiting spinner fits the width'
    runs = []
    for cols in (40, 60, 80, 100):
        d = fresh(f's18-{cols}')
        p = PtyRun([H.bin, 'small.txt'], d, runtime=16, cols=cols)
        p.start()
        runs.append((cols, p))
    for cols, p in runs:
        p.join()
        # Each \r redraw is its own segment; a wrapped frame leaves a
        # segment that starts mid-message ("l", " to cancel").
        segs = [line_colors(x)[0] for x in split_stream(p.text)]
        frames = [x for x in segs if 'Waiting for receiver' in x]
        orphans = [x for x in segs if x.strip() and not x.startswith(('⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'))
                   and ('cancel' in x or 'Ctrl-C' in x)]
        wide = [x for x in frames if len(x) > cols - 1]
        glob_ok(s, f'{cols} cols: frames fit', bool(frames) and not wide and not orphans,
                repr((wide or orphans)[:1]) if (wide or orphans) else f'{len(frames)} frames')
        glob_ok(s, f'{cols} cols: elapsed time kept', any(re.search(r'1[45] s', x) for x in frames))
        grid_ok(s, p.text, f'{cols} cols')
        broken = [x for x in segs if re.search(r'\w-$', x.rstrip())]
        glob_ok(s, f'{cols} cols: tagline breaks between words', not broken, repr(broken[:1]) if broken else None)
        has_text(s, f'{cols} cols: share code still shown', p.text, 'fsend ')


def s19_detected_background():
    """V1: with nothing declared, fsend asks the terminal (OSC 11, then DA1)
    and picks the palette for the answer; a silent terminal gets ANSI."""
    s = 'S19 detected background'
    for name, bg in (('dark', 'rgb:2828/2c2c/3434'), ('light', 'rgb:fdfd/f6f6/e3e3')):
        d = fresh('s19' + name)
        p = PtyRun([H.bin, 'small.txt'], d, runtime=4, bg=bg)
        p.start(); p.join()
        acc = tokens(name)[0]
        m = re.search(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b', p.text)
        check(s, f'{name} answer: accent from the {name} palette', p.text, None,
              m.group(1) if m else 'zzz', acc, True)
        contrast_ok(s, p.text, name)
        # The query is answered and consumed: nothing of it leaks as text.
        lacks_text(s, f'{name}: no query residue on screen', p.text, '11;rgb')


DEV_SND, DEV_RCV = 'sender-device-16', 'receiver-dev-16c'   # 16 characters each


def s20_visual_system():
    """V3-V8 at 60, 80 and 100 columns, background unknown / dark / light: a
    folder send with an existing differing file, both summaries."""
    s = 'S20 visual system'
    for theme in ('unknown', 'dark', 'light'):
        env = {} if theme == 'unknown' else {'FSEND_THEME': theme}
        for cols in (60, 80, 100):
            tag = f'{theme}/{cols}'
            d = fresh(f's20-{theme}-{cols}')
            os.makedirs(os.path.join(d, 'recv_out', 'myproj'))
            mine = os.path.join(d, 'recv_out', 'myproj', 'README.md')
            with open(mine, 'w') as f:
                f.write('mine, edited\n')
            later = time.time() + 3600
            os.utime(mine, (later, later))  # edited after the sender's copy
            snd, rcv = pair(s, d, ['--device-name', DEV_SND, 'myproj'],
                            ['--device-name', DEV_RCV, '--out', 'recv_out'], [(4.0, b'k\n')],
                            env=env, cols=cols)
            sp, rv = snd.text, rcv.text if rcv else ''
            grid_ok(s, sp + rv, tag)
            if theme == 'unknown':
                ansi_only(s, sp + rv)
            else:
                contrast_ok(s, sp + rv, theme)
            # Summary block: the outcome line and the detail lines under it.
            for side, text, head in (('sender', sp, 'Sent'), ('receiver', rv, 'Saved')):
                lines = [pl for pl, _ in screen_lines(text)]
                i = next((k for k, l in enumerate(lines) if l[2:].startswith(head)), None)
                if i is None:
                    glob_ok(s, f'{tag} {side}: summary found', False, 'missing')
                    continue
                block = lines[i:i + 3]
                block = [block[0]] + [l for l in block[1:] if l.startswith('  ') and not l.startswith('   ')]
                wide = [l for l in block if len(l) > cols - 1]
                glob_ok(s, f'{tag} {side}: summary fits', not wide, repr(wide[:1]) if wide else None)
            check(s, f'{tag}: route faint in the summary', sp, 'B ·', 'local network', DEFAULT, False, True)
            check(s, f'{tag}: listing "differs" needs a decision', rv, 'README.md', 'differs', tokens(theme)[3])
            check(s, f'{tag}: listing "up to date"-free row size faint', rv, 'video1.bin', '20 MB', DEFAULT, False, True)
            has_text(s, f'{tag}: sender names the receiver', sp, 'Sent myproj/ to ' + DEV_RCV)
            # U5: the edited local copy is the newer one, and the prompt says so.
            has_text(s, f'{tag}: conflict row says which is newer', rv, 'yours is newer by')
            has_text(s, f'{tag}: warns that replace would lose work', rv, 'replace would lose your changes')


def s21_cancel_resume():
    """V5: Ctrl-C mid-transfer leaves the bar, then one block — the outcome,
    where the data is, the command that resumes — on the grid."""
    s = 'S21 Ctrl-C mid-transfer'
    d = fresh('s21')
    with open(os.path.join(d, 'big.bin'), 'wb') as f:
        for _ in range(50):
            f.write(os.urandom(10_000_000))
    snd = PtyRun([H.bin, 'big.bin'], d, runtime=30, cols=80)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
        return
    time.sleep(0.5)
    rcv = PtyRun([H.bin, m.group(1), '--yes', '--out', 'recv_out'], d, cols=80, runtime=20)
    rcv.start()
    rcv.wait_for(r'\d+%')
    time.sleep(0.6)
    if rcv.fd:
        os.write(rcv.fd, b'\x03')
    rcv.join(); snd.stop()
    rv = rcv.text
    has_text(s, 'outcome says where it stopped', rv, 'Cancelled at ')
    has_text(s, 'partial data named', rv, '  Partial data kept in ')
    has_text(s, 'resume command given', rv, '  Resume while the sender waits: fsend ')
    lacks_text(s, 'a Ctrl-C carries no code', rv, '[E026]')
    grid_ok(s, rv)
    lines = [pl for pl, _ in screen_lines(rv)]
    k = next((i for i, l in enumerate(lines) if 'Cancelled at' in l), None)
    glob_ok(s, 'the bar stays above the block', k is not None and '%' in lines[k - 1],
            repr(lines[k - 1:k + 1]) if k else None)


def s22_clipboard():
    """U6: the sender copies "fsend <code>" through the platform helper and
    says so only when the helper confirmed; never for --password sends."""
    s = 'S22 clipboard'
    fake = os.path.join(H.tmp, 'fakeclip')
    os.makedirs(fake, exist_ok=True)
    sink = os.path.join(H.tmp, 'clipboard.txt')
    for name in ('pbcopy', 'wl-copy', 'clip.exe'):
        with open(os.path.join(fake, name), 'w') as f:
            f.write(f'#!/bin/sh\ncat > {sink}\n')
        os.chmod(os.path.join(fake, name), 0o755)
    env = {'FSEND_NO_CLIPBOARD': '', 'PATH': fake + os.pathsep + os.environ.get('PATH', ''),
           'WAYLAND_DISPLAY': 'wayland-0'}
    for args, want in ((['small.txt'], True), (['small.txt', '--password=abc'], False)):
        if os.path.exists(sink):
            os.remove(sink)
        d = fresh('s22')
        p = PtyRun([H.bin] + args, d, runtime=4, env=env)
        p.start(); p.join()
        m = re.search(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b', p.text)
        got = open(sink).read() if os.path.exists(sink) else ''
        tag = 'password send' if '--password=abc' in args else 'plain send'
        if want:
            glob_ok(s, f'{tag}: the command is on the clipboard', m is not None and got == f'fsend {m.group(1)}', repr(got))
            has_text(s, f'{tag}: says it was copied', p.text, 'Copied to clipboard')
        else:
            glob_ok(s, f'{tag}: clipboard untouched', got == '', repr(got))
            lacks_text(s, f'{tag}: says nothing about copying', p.text, 'Copied')


SCENARIOS = (s1_single_file, s2_folder, s3_conflict, s4_password, s5_decline,
             s6_text, s7_errors, s8_no_color, s9_help, s10_piped, s11_themes,
             s12_narrow, s13_cancel, s14_late_arriver, s15_completion, s16_notify,
             s17_plain, s18_spinner_width, s19_detected_background, s20_visual_system,
             s21_cancel_resume, s22_clipboard)


def main():
    global H
    if os.name == 'nt':
        print('validate-ux.py requires a Unix pty; skip on Windows.')
        return 0
    H = Harness()
    try:
        H.start_server()
        H.configure_client()
        for fn in SCENARIOS:
            try:
                fn()
            except Exception as e:  # noqa: BLE001 — one broken scenario ≠ lost matrix
                RESULTS.append((fn.__name__, 'scenario crashed', repr(e), False))
            H.kill_clients()
            time.sleep(0.5)
    finally:
        H.cleanup()
    width = max(len(r[1]) for r in RESULTS) + 2
    fails = 0
    cur = None
    for sc, label, got, ok in RESULTS:
        if sc != cur:
            print(f'\n{sc}')
            cur = sc
        mark = 'PASS' if ok else 'FAIL'
        if not ok:
            fails += 1
        print(f'  {mark}  {label:<{width}} {got}')
    print(f'\n{"=" * 60}\n{len(RESULTS) - fails}/{len(RESULTS)} checks passed, {fails} failed')
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
