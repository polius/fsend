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
every line that waits for an answer starts with ?, every duration has a
space before its unit, no list hides exactly one row, lines that are
clipped fit the terminal in display columns (wide scripts included),
and degraded modes stay byte-clean (NO_COLOR, FSEND_PLAIN, piped stderr
→ zero colour escapes, ASCII glyph fallbacks). Text checks assert on
what the user reads (headlines, hints, exit codes), not just on escapes.

Scenarios: single file, folder, conflict + overwrite prompt, password
flow (wrong → correct), decline, text send, error rendering, NO_COLOR,
--help, piped degradation, light and dark palettes, a 40-column
terminal, Ctrl-C while waiting and mid-transfer, a late second receiver,
shell completion, notifications, usage-error paths, plain mode, the
detected background, the visual system at three widths, the clipboard,
the prompts outside the main flow, wide (CJK) names at 40 columns,
skipped-file lists, the install-and-receive hint and the update notice
for each kind of install. Exit status: 0 when every check passes, or
fails only where EXPECTED_FAILURES says it will (reported as XFAIL).

Unix only (uses pty). Run from the repo root:

    python3 scripts/validate-ux.py            # everything (CI runs this)
    python3 scripts/validate-ux.py s14 s21    # only these scenarios

With working multicast, sender and receiver find each other over mDNS
and use the local-network route, as two real machines on one network do.
Without it they pair through the harness's server and the checks still
pass, on the slower QUIC path. On Linux, loopback has multicast off by
default: `sudo ip link set lo multicast on` (CI does).
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
import unicodedata
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
OLD_GLYPHS = '⚠ℹ⟳⇢⇣⇄┌│└›'
# A duration written Go's way: the unit glued to the number ("152ms",
# "2.8s", "1m30s"). On screen it is "152 ms". Tokens only — a digit and a
# unit inside a path or a name is not a duration.
GLUED_DURATION = re.compile(r'(?<![\w/.\-])\d+(?:\.\d+)?(?:ms|s|m|h)(?:\d+(?:ms|s|m))?(?![\w/\-])')
# What a line that waits for an answer looks like: its options, or the
# label of a typed answer.
PROMPT = re.compile(r'\[(?:Y/n|y/N|s/R|K\]eep both)|(?:Enter the password the sender shared|'
                    r'Press Enter to use it, or type your own|Code from the sender|'
                    r'Wrong password — try again \(\d/\d\)): ')


def cols_of(text):
    """Display columns of text: East Asian Wide/Fullwidth characters take
    two, combining marks and format characters none."""
    n = 0
    for ch in text:
        if unicodedata.category(ch) in ('Mn', 'Me', 'Cf'):
            continue
        n += 2 if unicodedata.east_asian_width(ch) in ('W', 'F') else 1
    return n


SCREEN_CSI = re.compile(r'\x1b\[([0-9;?]*)([A-Za-z])')
SCREEN_OSC = re.compile(r'\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)')


def plain_screen(text):
    """The lines a terminal is left showing, before any auto-wrap: \\r,
    cursor moves and erases applied, colours dropped. A line wider than the
    pty is one the terminal wrapped into column 0."""
    text = SCREEN_OSC.sub('', text)
    lines, r, c, i = [[]], 0, 0, 0
    while i < len(text):
        ch = text[i]
        if ch == '\x1b':
            m = SCREEN_CSI.match(text, i)
            if m:
                arg, cmd = m.groups()
                i = m.end()
                n = int(arg) if arg.isdigit() else 1
                if cmd == 'A':
                    r = max(0, r - n)
                elif cmd == 'B':
                    r += n
                elif cmd == 'C':
                    c += n
                elif cmd == 'D':
                    c = max(0, c - n)
                elif cmd == 'G':
                    c = n - 1
                elif cmd == 'K':
                    if arg in ('', '0'):
                        del lines[r][c:]
                    elif arg in ('1', '2'):
                        lines[r] = [] if arg == '2' else [' '] * c + lines[r][c:]
                elif cmd == 'J':
                    del lines[r][c:]
                    del lines[r + 1:]
                while len(lines) <= r:
                    lines.append([])
                continue
            i += 2
            continue
        if ch == '\r':
            c = 0
        elif ch == '\n':
            r += 1
            c = 0
            while len(lines) <= r:
                lines.append([])
        elif ch == '\b':
            c = max(0, c - 1)
        elif ch >= ' ':
            ln = lines[r]
            while len(ln) < c:
                ln.append(' ')
            if c < len(ln):
                ln[c] = ch
            else:
                ln.append(ch)
            c += 1
        i += 1
    return [''.join(ln).rstrip() for ln in lines]


def unbreakable(line):
    """A command to copy or a path, in a list row, is never broken: a copy
    of half of it would not work. Such a row may run past the edge."""
    if not line.startswith('    '):
        return False
    body = line.strip()
    return body.startswith(('curl ', 'iex ', 'fsend ')) or any(len(t) > 20 and '/' in t for t in body.split())


# Every PtyRun of the scenario that is running, for the width check.
RUNS = []


def widths_ok(sc):
    """U1: no line fsend leaves on screen is wider than its pty. stdout data
    (a preview list, a received text) is the user's, so runs marked
    data=True are skipped."""
    for run in RUNS:
        if run.data or run.out == b'':
            continue
        wide = [(cols_of(ln), ln) for ln in plain_screen(run.text)
                if cols_of(ln) > run.cols and not unbreakable(ln)]
        label = 'no line wider than %d columns: %s' % (run.cols, ' '.join(os.path.basename(a) for a in run.argv)[:40])
        glob_ok(sc, label, not wide, '; '.join('%d: %s' % w for w in wide[:3]) or None)
    RUNS.clear()


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
    redraws overwrite each other), with the plain text and colours. The bell
    that follows a notification prints nothing, so it is no line."""
    out = []
    for raw in OSC.sub('', stream).replace('\x07', '').replace('\r\n', '\n').split('\n'):
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

# Checks that fail today for a known reason in the product, keyed by
# (scenario, check label), with the reason. A marked check that fails is
# reported as XFAIL and doesn't fail the run, so the CI job stays green
# while the fix is on its way. A marked check that passes fails the run
# (XPASS): the mark has to go with the fix, or it would hide the next
# regression.
EXPECTED_FAILURES = {}


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
    # V7: a line that waits for an answer starts with ? in the gutter.
    # A question too wide for the terminal puts its options on the rows
    # under it, from column 2: those rows belong to the ? line above.
    bare, asking = [], False
    for pl, _ in screen_lines(stream):
        if pl.startswith('? '):
            asking = True
        elif not pl.startswith('  ') or pl.startswith('    '):
            asking = False
        if PROMPT.search(pl) and not pl.startswith('? ') and not asking:
            bare.append(pl)
    glob_ok(sc, pre + 'every prompt starts with ?', not bare, repr(bare[:1]) if bare else None)
    # V9: "188 ms", never "188ms".
    glued = [pl for pl, _ in screen_lines(stream) if GLUED_DURATION.search(pl)]
    glob_ok(sc, pre + 'durations have a space before the unit', not glued, repr(glued[:1]) if glued else None)
    # U8: "… and 1 more" costs the row it hides.
    one = [pl for pl, _ in screen_lines(stream) if re.search(r'… and 1 more\b', pl)]
    glob_ok(sc, pre + 'no list hides exactly one row', not one, repr(one[:1]) if one else None)


def prompt_ok(sc, label, stream, question):
    """V7: the line asking `question` has the bold primary ? in column 0
    and its text from column 2."""
    for plain, colors in screen_lines(stream):
        if question not in plain:
            continue
        ok = plain.startswith('? ') and colors[0] == (VIOLET, True, False)
        glob_ok(sc, label, ok, None if ok else repr(plain[:60]))
        return
    glob_ok(sc, label, False, f'missing {question!r}')


class PtyRun(threading.Thread):
    """Runs argv in a pty; captures raw output while feeds fire on schedule."""

    def __init__(self, argv, cwd, feed=None, runtime=30, env=None, cols=100, bg=None, data=False):
        super().__init__(daemon=True)
        self.data = data  # stdout carries the user's data: no width check
        RUNS.append(self)
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
        # Seconds from the fork to the child's exit (or the runtime cap).
        # The child starts after the fork, so its own clock reads less.
        self.elapsed = None

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
        start = time.time()
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(self.cwd)
            os.execvpe(self.argv[0], self.argv, self.child_env())
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 30, self.cols, 0, 0))
        fi = 0
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
        self.elapsed = time.time() - start
        try:
            os.close(fd)
        except OSError:
            pass
        self.done = True

    def wait_for(self, pattern, timeout=20, poll=0.2):
        rx = re.compile(pattern)
        deadline = time.time() + timeout
        while True:
            m = rx.search(self.text)
            if m or time.time() >= deadline:
                return m
            time.sleep(poll)

    def signal(self, sig):
        """Send sig to the child, if it is still there."""
        if self.pid and self.status is None and not self.done:
            try:
                os.kill(self.pid, sig)
            except OSError:
                pass

    def type(self, data):
        """Type data into the child's terminal. False when it is gone."""
        try:
            os.write(self.fd, data)
            return True
        except (OSError, TypeError):
            return False

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
    # The sender's run includes waiting for the receiver to answer, so a
    # slow runner can push it past NotifyAfter (10 s), where the bell is
    # right. Only a run that ended sooner must stay silent; S16 covers the
    # long one.
    if snd.elapsed is not None and snd.elapsed < 10:
        glob_ok(s, 'no notification for a short run', '\x1b]9;' not in sp and '\x07' not in sp)
    else:
        RESULTS.append((s, f'no notification for a short run (skipped: the run took {snd.elapsed or 0:.1f} s)', 'n/a', True))
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
    prompt_ok(s, 'sender instruction behind the ? gutter', sp, 'Press Enter to use it')
    glob_ok(s, 'suggestion is a neutral line in column 2',
            any(pl.startswith('  Suggested password: ') for pl, _ in screen_lines(sp)))
    check(s, 'receiver prompt primary', rv, 'Enter the password',
          'Enter the password the sender shared:', VIOLET)
    prompt_ok(s, 'receiver prompt behind the ? gutter', rv, 'Enter the password')
    check(s, 'password context ! warning', rv, 'password-protected', '!', YELLOW, True)
    check(s, 'retry prompt primary', rv, 'Wrong password', 'Wrong password — try again (2/3):', VIOLET)
    prompt_ok(s, 'retry prompt behind the ? gutter', rv, 'Wrong password — try again')
    check(s, 'chip warning colour', rv, 'password required', 'password required', YELLOW)
    check(s, 'transfer succeeded', rv, 'Saved', '✓', GREEN)
    has_text(s, 'sender waits on the password, naming the device', sp, ' to enter the password')
    glob_ok(s, 'suggestion is four words', re.fullmatch(r'[a-z]+(-[a-z]+){3}', pw) is not None, pw)
    has_text(s, 'code block says a password is needed', sp, 'they need the password too')
    ansi_only(s, sp + rv)
    grid_ok(s, sp + rv)
    # R4: a receiver out of password tries; the sender's notice fits 80 and
    # 40 columns as a headline and a faint line (widths_ok checks the rest).
    for cols in (80, 40):
        snd = PtyRun([H.bin, 'small.txt', '--password'], d, feed=[(2.0, b'\n')], runtime=30, cols=cols)
        snd.start()
        code_m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
        if not code_m:
            snd.stop()
            RESULTS.append((s, f'{cols} cols: sender produced no code', 'TIMEOUT', False))
            continue
        time.sleep(0.5)
        rcv = PtyRun([H.bin, code_m.group(1), '--out', 'recv_out'], d,
                     feed=[(3.0, b'nope1\n'), (5.0, b'nope2\n'), (7.0, b'nope3\n')], runtime=15, cols=cols)
        rcv.start(); rcv.join()
        snd.wait_for(r'They can rerun', timeout=6)
        snd.stop()
        has_text(s, f'{cols} cols: wrong-password headline', snd.text, ' — still waiting')
        has_text(s, f'{cols} cols: the rerun on a faint line', snd.text, 'They can rerun')
        grid_ok(s, snd.text, f'wrong password at {cols}')


def s29_server_down():
    """R4: with the server down the sender says so in a headline and says
    what still works on a faint line under it, at 80 and 40 columns."""
    s = 'S29 server down'
    d = fresh('s29')
    for cols in (80, 40):
        snd = PtyRun([H.bin, 'small.txt'], d, runtime=8, cols=cols,
                     env={'FSEND_SERVER': 'http://127.0.0.1:9'})
        snd.start()
        snd.wait_for(r'local network can connect', timeout=7)
        snd.stop()
        has_text(s, f'{cols} cols: the headline', snd.text, 'Server unavailable; retrying')
        has_text(s, f'{cols} cols: what still works', snd.text, 'Until then,')
        grid_ok(s, snd.text, f'server down at {cols}')


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
    # ... and so it does behind --receive, which used to call it a usage error.
    forced = PtyRun([H.bin, '--receive', 'abc', 'defg', 'jkm'], d, runtime=20)
    forced.start(); forced.join()
    has_text(s, '--receive takes the three words too', forced.text, '[E002]')
    lacks_text(s, '--receive with three words is not a usage error', forced.text, '[E024]')
    glob_ok(s, '--receive with three words exits 2 (no sender), not 24', forced.status == 2, f'exit {forced.status}')
    two = PtyRun([H.bin, '--receive', 'abc', 'defg'], d, runtime=6)
    two.start(); two.join()
    has_text(s, '--receive with words that spell no code says what it takes', two.text,
             '--receive takes one code (abc-defg-jkm, or its three words) [E024]')
    glob_ok(s, 'that is still a usage error (24)', two.status == 24, f'exit {two.status}')
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
    for o in (out, out2, out3, out4, out5, forced, two):
        lacks_text(s, 'no E099 on an error path', o.text, 'E099')
    grid_ok(s, ''.join(o.text for o in (out, out2, out3, out4, out5, forced, two)))
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
    wide = [b for b in bars if cols_of(b) > 39]
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
        wide = [x for x in frames if cols_of(x) > cols - 1]
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
                wide = [l for l in block if cols_of(l) > cols - 1]
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
    snd = PtyRun([H.bin, 'big.bin'], d, runtime=90, cols=80)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
        return
    time.sleep(0.5)
    # No --yes: the prompt is a point where nothing moves yet.
    rcv = PtyRun([H.bin, m.group(1), '--out', 'recv_out'], d, cols=80, runtime=60)
    rcv.start()
    if not rcv.wait_for(r'Save to', timeout=30):
        rcv.stop(); snd.stop()
        RESULTS.append((s, 'receiver never asked to save', 'TIMEOUT', False))
        return
    # The local route moves these 500 MB in under half a second, so a
    # Ctrl-C sent "a moment after the first progress" can arrive after
    # the end. Here the transfer cannot finish first: the sender is
    # stopped (SIGSTOP) before the receiver says yes, and then runs a few
    # milliseconds at a time, each slice far smaller than the file, until
    # the receiver's bar shows its first percent. The Ctrl-C goes in
    # while the sender is still stopped.
    moving = None
    snd.signal(signal.SIGSTOP)
    try:
        rcv.type(b'y\n')
        for _ in range(400):
            snd.signal(signal.SIGCONT)
            time.sleep(0.005)
            snd.signal(signal.SIGSTOP)
            # The bar redraws every 100 ms; give it a few frames.
            moving = rcv.wait_for(r'(?<![\d.])[1-9]\d?%', timeout=0.4, poll=0.02)
            if moving or rcv.status is not None:
                break
        glob_ok(s, 'interrupted while the transfer was running', moving is not None and rcv.status is None,
                None if moving else 'no progress seen')
        rcv.type(b'\x03')
    finally:
        snd.signal(signal.SIGCONT)
    rcv.join(); snd.stop()
    rv = rcv.text
    glob_ok(s, 'cancel exits 130', rcv.status == 130, f'exit {rcv.status}')
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


def s23_prompts():
    """V7: the prompts outside the main flow — the code, the code-or-file
    question, uninstall, a bare --password, a long text — take the form of
    the accept prompt: ? in the gutter, the question from column 2, the
    options two spaces on."""
    s = 'S23 prompts outside the main flow'
    d = fresh('s23')
    # Bare --receive asks for the code; a wrong answer re-asks.
    ask = PtyRun([H.bin, '--receive'], d, feed=[(1.5, b'nonsense\n'), (3.0, b'\x04')], runtime=6)
    ask.start(); ask.join()
    prompt_ok(s, 'code prompt behind the ? gutter', ask.text, 'Code from the sender:')
    check(s, 'code prompt label primary', ask.text, 'Code from the sender', 'Code from the sender:', VIOLET)
    has_text(s, 'the correction is a neutral line', ask.text, "\n  That isn't a code — codes look like ")
    grid_ok(s, ask.text, 'code prompt')
    # An argument that is both a code and a file.
    with open(os.path.join(d, 'abc-defg-jkm'), 'w') as f:
        f.write('x')
    both = PtyRun([H.bin, 'abc-defg-jkm'], d, feed=[(1.5, b'x\n'), (3.0, b'\x04')], runtime=6)
    both.start(); both.join()
    prompt_ok(s, 'code-or-file prompt behind the ? gutter', both.text, 'Send this file, or receive with this code?')
    has_text(s, 'code-or-file: options two spaces after the question', both.text,
             '? Send this file, or receive with this code?  [s/R] ')
    has_text(s, 'code-or-file: the re-ask hint in column 2', both.text, '\n  Please answer s or r.')
    grid_ok(s, both.text, 'code-or-file')
    os.remove(os.path.join(d, 'abc-defg-jkm'))
    # A folder named like a subcommand (R1): a terminal is asked; "s" sends it.
    os.makedirs(os.path.join(d, 'server'), exist_ok=True)
    with open(os.path.join(d, 'server', 'main.go'), 'w') as f:
        f.write('package main\n')
    cmd = PtyRun([H.bin, 'server', '--preview'], d, feed=[(1.5, b'x\n'), (3.0, b's\n')], runtime=8)
    cmd.start(); cmd.join()
    has_text(s, 'command-or-folder: the collision is named', cmd.text,
             '"server" is an fsend command AND a local folder.')
    prompt_ok(s, 'command-or-folder prompt behind the ? gutter', cmd.text, 'Send this folder, or run fsend server?')
    has_text(s, 'command-or-folder: options two spaces after the question', cmd.text,
             '? Send this folder, or run fsend server?  [s/R] ')
    has_text(s, 'command-or-folder: "s" sends the folder', cmd.text, 'main.go')
    grid_ok(s, cmd.text.split('path,size')[0], 'command-or-folder')  # the preview list is stdout data
    # --send skips the question and sends the folder.
    forced = PtyRun([H.bin, '--send', 'server', '--preview'], d, runtime=6)
    forced.start(); forced.join()
    lacks_text(s, '--send server: no unknown-flag error', forced.text, 'Unknown flag')
    has_text(s, '--send server: the folder is previewed', forced.text, 'main.go')
    shutil.rmtree(os.path.join(d, 'server'))
    # --uninstall asks before removing anything; "n" removes nothing.
    un = PtyRun([H.bin, '--uninstall'], d, feed=[(1.5, b'what\n'), (3.0, b'n\n')], runtime=6)
    un.start(); un.join()
    prompt_ok(s, 'uninstall prompt behind the ? gutter', un.text, 'Continue?')
    has_text(s, 'uninstall: options two spaces after the question', un.text, '? Continue?  [y/N] ')
    has_text(s, 'uninstall: what goes is a list in column 4', un.text, '\n    - binary:     ')
    has_text(s, 'uninstall: declined, nothing removed', un.text, '\n  Not uninstalled.')
    glob_ok(s, 'uninstall: the binary is still there', os.path.exists(H.bin))
    glob_ok(s, 'uninstall: "n" exits 0', un.status == 0, f'exit {un.status}')
    grid_ok(s, un.text, 'uninstall')
    # A bare --password on both sides: the sender's suggestion, the
    # receiver's hidden prompt.
    snd = PtyRun([H.bin, 'small.txt', '--password'], d, feed=[(2.0, b'hunter2\n')], runtime=30)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
        return
    time.sleep(0.5)
    rcv = PtyRun([H.bin, m.group(1), '--password', '--yes', '--out', 'recv_out'], d,
                 feed=[(2.0, b'hunter2\n')], runtime=20)
    rcv.start(); rcv.join(); snd.join(timeout=8); snd.stop()
    prompt_ok(s, 'bare --password (receiver) behind the ? gutter', rcv.text, 'Enter the password the sender shared:')
    lacks_text(s, 'the typed password is not echoed', rcv.text, 'hunter2')
    has_text(s, 'bare --password transfer completes', rcv.text, 'Saved ./recv_out/small.txt')
    grid_ok(s, snd.text + rcv.text, 'bare --password')
    # A long text is asked about before it fills the screen.
    long_text = '\n'.join(f'line {i}' for i in range(60))
    snd, rcv = pair(s, d, ['--text', long_text], [], [(4.0, b'what\n'), (5.5, b'n\n')])
    rv = rcv.text if rcv else ''
    prompt_ok(s, 'long-text prompt behind the ? gutter', rv, 'Show the text (')
    has_text(s, 'long-text prompt: options two spaces after the question', rv, ' 60 lines)?  [Y/n] ')
    lacks_text(s, 'long text not shown after "n"', rv, 'line 59')
    grid_ok(s, rv, 'long text')


def s24_wide_names():
    """U2: names in a wide script (CJK: two columns a character) at 40 and
    60 columns. The spinner names the peer's device and the bar the current
    file; counted in runes, either line overflows, and a line that wraps
    leaves a row behind on every redraw."""
    s = 'S24 wide names fit the width'
    snd_name, rcv_name = '田中さんのノートパソコン', '受信用のデスクトップ機です'
    for cols in (40, 60):
        d = fresh(f's24-{cols}')
        proj = os.path.join(d, '写真アルバム')
        os.makedirs(proj)
        for fn, sz in (('富士山の夜明けの写真その一.bin', 12_000_000), ('京都の紅葉と清水寺の舞台.bin', 9_000_000),
                       ('メモ.txt', 7)):
            with open(os.path.join(proj, fn), 'wb') as f:
                f.write(os.urandom(sz))
        snd, rcv = pair(s, d, ['--device-name', snd_name, '写真アルバム'],
                        ['--device-name', rcv_name, '--out', 'recv_out'], [(5.0, b'y\n')], cols=cols)
        sp, rv = snd.text, rcv.text if rcv else ''
        tag = f'{cols} cols'
        for side, text in (('sender', sp), ('receiver', rv)):
            segs = [line_colors(x)[0] for x in split_stream(text)]
            spins = [x for x in segs if x[:1] and x[0] in SPINNER]
            wide = [x for x in spins if cols_of(x) > cols - 1]
            glob_ok(s, f'{tag} {side}: spinner frames fit', bool(spins) and not wide,
                    repr(wide[:1]) if wide else f'{len(spins)} frames')
            bars = [x for x in segs if re.search(r'\d+%', x)]
            wide = [x for x in bars if cols_of(x) > cols - 1]
            glob_ok(s, f'{tag} {side}: progress frames fit', bool(bars) and not wide,
                    repr(wide[:1]) if wide else f'{len(bars)} frames')
            # A wrapped redraw leaves a row that starts mid-line: every
            # non-empty segment starts on the grid.
            stray = [x for x in segs if x.strip() and not x.startswith(tuple(SPINNER + GLYPHS)) and not x.startswith('  ')]
            glob_ok(s, f'{tag} {side}: no redraw row left behind', not stray, repr(stray[:1]) if stray else None)
        waited = [line_colors(x)[0] for x in split_stream(sp) if 'Waiting for 受信' in line_colors(x)[0]]
        glob_ok(s, f'{tag}: the spinner names the wide device', bool(waited), f'{len(waited)} frames')
        # 40 columns cannot hold the name: it is cut, by columns, with an
        # ellipsis. 60 can, whole.
        longest = max(waited, key=cols_of) if waited else ''
        if cols == 40:
            glob_ok(s, f'{tag}: its name is cut with an ellipsis, in columns',
                    '…' in longest and cols_of(longest) <= cols - 1, repr(longest))
        else:
            glob_ok(s, f'{tag}: its name is shown whole', rcv_name + ' to accept' in longest, repr(longest))
        has_text(s, f'{tag}: the transfer still completes', rv, 'Saved 写真アルバム/ to ./recv_out/')
        # Rows of the listing line their sizes up under names of mixed width.
        rows = [pl for pl, _ in screen_lines(rv) if pl.startswith('    ') and ' MB' in pl]
        starts = {cols_of(pl[:pl.index(' MB')]) for pl in rows}
        glob_ok(s, f'{tag}: listing sizes align under wide names', len(rows) >= 2 and len(starts) == 1,
                repr(rows[:2]))


def s25_skipped_lists():
    """U8: the list of skipped files, on both sides. One over the limit is
    printed whole — "… and 1 more" would cost the row it hides — and two
    over collapses."""
    s = 'S25 skipped-file lists'
    if os.geteuid() == 0:
        RESULTS.append((s, 'skipped (root reads every file)', 'n/a', True))
        return
    for n, want_more in ((11, None), (12, '… and 2 more')):
        d = fresh(f's25-{n}')
        locked = os.path.join(d, 'locked')
        os.makedirs(locked)
        with open(os.path.join(locked, 'ok.txt'), 'w') as f:
            f.write('fine')
        for i in range(n):
            path = os.path.join(locked, f'secret{i:02d}.txt')
            with open(path, 'w') as f:
                f.write('nope')
            os.chmod(path, 0)
        snd, rcv = pair(s, d, ['locked'], ['--out', 'recv_out', '--yes'], [])
        sp, rv = snd.text, rcv.text if rcv else ''
        for side, text in (('sender', sp), ('receiver', rv)):
            lines = [pl for pl, _ in screen_lines(text)]
            k = next((i for i, l in enumerate(lines) if 'skipped:' in l), None)
            if k is None:
                glob_ok(s, f'{n} skipped, {side}: list printed', False, 'missing')
                continue
            rows = []
            for l in lines[k + 1:]:
                if not l.startswith('    '):
                    break
                rows.append(l)
            named = [r for r in rows if 'secret' in r]
            if want_more:
                glob_ok(s, f'{n} skipped, {side}: 10 rows, then "{want_more}"',
                        len(named) == 10 and rows[-1].strip() == want_more, repr(rows[-2:]))
            else:
                glob_ok(s, f'{n} skipped, {side}: all {n} rows, none hidden',
                        len(named) == n and not any('more' in r for r in rows), repr(rows[-2:]))
            has_text(s, f'{n} skipped, {side}: headline counts them', text, f'{n} files ')
        grid_ok(s, sp + rv, f'{n} skipped')
        for i in range(n):
            os.chmod(os.path.join(locked, f'secret{i:02d}.txt'), 0o600)


def s26_install_hint():
    """D1: on the default server the sender prints a line that installs
    fsend and receives — for sh, and for PowerShell on Windows. Both are
    single unbroken lines (a copy must yield a runnable command) and fit 80
    columns. A config of its own (no --connect) and --local: the default
    server is named, never contacted."""
    s = 'S26 install-and-receive hint'
    home = os.path.join(H.tmp, 'home-default')
    os.makedirs(home, exist_ok=True)
    # No pairing server is used under --local; a dead proxy makes sure of it.
    env = {'HOME': home, 'XDG_CONFIG_HOME': os.path.join(home, '.config'),
           'HTTPS_PROXY': 'http://127.0.0.1:9', 'HTTP_PROXY': 'http://127.0.0.1:9', 'NO_PROXY': ''}
    runs = []
    for cols in (40, 60, 80, 100):
        d = fresh(f's26-{cols}')
        p = PtyRun([H.bin, '--local', 'small.txt'], d, runtime=4, cols=cols, env=env)
        p.start()
        runs.append((cols, p))
    for cols, p in runs:
        p.join()
        t = p.text
        m = re.search(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b', t)
        code = m.group(1) if m else 'zzz-zzzz-zzz'
        sh = f'curl -fsSL https://getfsend.alzina.dev | sh -s -- {code}'
        ps = f'iex "& {{$(irm https://getfsend.alzina.dev/windows)}} -Code {code}"'
        lines = [pl for pl, _ in screen_lines(t)]
        tag = f'{cols} cols'
        glob_ok(s, f'{tag}: sh line, alone and unbroken, in column 4', '    ' + sh in lines)
        glob_ok(s, f'{tag}: PowerShell line, alone and unbroken, in column 4', '    ' + ps in lines,
                repr([l for l in lines if 'irm' in l][:1]))
        check(s, f'{tag}: sh line faint', t, 'curl -fsSL', sh, DEFAULT, False, True)
        check(s, f'{tag}: PowerShell line faint', t, 'irm https', ps, DEFAULT, False, True)
        k = lines.index('    ' + sh) if '    ' + sh in lines else -1
        glob_ok(s, f'{tag}: PowerShell line right under the sh line', k >= 0 and lines[k + 1:k + 2] == ['    ' + ps])
        if cols >= 80:
            wide = [l for l in lines if cols_of(l) > cols - 1 and l[:1] not in SPINNER]
            glob_ok(s, f'{tag}: every line fits', not wide, repr(wide[:1]) if wide else None)
        else:
            # Narrower than the commands: they stay one line each (the
            # terminal wraps them), and the sentence above breaks between
            # words instead of mid-word.
            head = [l for l in lines if l.startswith('  ') and not l.startswith('   ')
                    and ('No fsend' in l or l.strip().endswith('receives:'))]
            glob_ok(s, f'{tag}: the heading wraps between words',
                    bool(head) and all(cols_of(l) <= cols - 1 for l in head), repr(head))
        grid_ok(s, t, tag)
    # --json: the code event carries both, the new field beside the old.
    d = fresh('s26-json')
    e = dict(os.environ, **env)
    e['FSEND_NO_UPDATE_CHECK'] = '1'
    for k in ('FSEND_SERVER', 'FSEND_CODE', 'FSEND_PASSWORD'):
        e.pop(k, None)
    proc = subprocess.Popen([H.bin, '--local', '--json', 'small.txt'], cwd=d, env=e,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL)
    time.sleep(3)
    proc.kill()
    first = proc.stdout.read().decode('utf-8', 'replace').split('\n')[0]
    try:
        ev = json.loads(first)
    except ValueError:
        ev = {}
    c = ev.get('code', '')
    glob_ok(s, 'json: code event keeps "install"', ev.get('event') == 'code'
            and ev.get('install') == f'curl -fsSL https://getfsend.alzina.dev | sh -s -- {c}', first[:80])
    glob_ok(s, 'json: code event gains "install_powershell"',
            ev.get('install_powershell') == f'iex "& {{$(irm https://getfsend.alzina.dev/windows)}} -Code {c}"',
            repr(ev.get('install_powershell')))
    # A server of one's own: neither line, neither field.
    own = subprocess.Popen([H.bin, '--local', '--json', 'small.txt'], cwd=d,
                           env=dict(os.environ, **H.client_env(), FSEND_NO_UPDATE_CHECK='1'),
                           stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL)
    time.sleep(3)
    own.kill()
    first = own.stdout.read().decode('utf-8', 'replace').split('\n')[0]
    glob_ok(s, 'json: no install fields on a server of its own',
            '"event":"code"' in first and 'install' not in first, first[:80])


def s27_update_notice():
    """D2: the notice after a transfer names the action that works for the
    way this fsend was installed. `fsend --update` refuses (E039) on a
    binary that Homebrew or a deb/rpm/apk package owns, so those are told
    what to do instead. A versioned build is installed four ways under the
    harness's temp dir; the "newer release" comes from a seeded check
    cache (fresh, so nothing is looked up; a dead proxy makes sure)."""
    s = 'S27 update notice per install'
    root = os.path.join(H.tmp, 'installs')
    os.makedirs(root)
    built = os.path.join(root, 'fsend-1.0.0')
    subprocess.run(['go', 'build', '-ldflags', '-X github.com/polius/fsend/internal/version.Version=1.0.0',
                    '-o', built, './cmd/fsend'], cwd=ROOT, check=True)
    with open(os.path.join(H.home, '.config', 'fsend', 'update-check.json'), 'w') as f:
        json.dump({'checked_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'latest': '9.9.9'}, f)
    env = {'FSEND_NO_UPDATE_CHECK': '0', 'HTTPS_PROXY': 'http://127.0.0.1:9',
           'HTTP_PROXY': 'http://127.0.0.1:9', 'NO_PROXY': ''}
    link = 'https://github.com/polius/fsend/releases/tag/v9.9.9'
    # (label, where the binary lives, what it is run as, package marker, action line, the command in it)
    installs = [
        ('install script', 'own/bin/fsend', None, None, f'fsend --update · {link}', 'fsend --update'),
        ('Homebrew', 'brew/Cellar/fsend/1.0.0/bin/fsend', 'brew/bin/fsend', None,
         f'brew upgrade fsend · {link}', 'brew upgrade fsend'),
        ('deb', 'deb/usr/bin/fsend', None, 'deb', f'Install the new .deb: {link}', None),
        ('rpm', 'rpm/usr/bin/fsend', None, 'rpm', f'Install the new .rpm: {link}', None),
        ('apk', 'apk/usr/bin/fsend', None, 'apk', f'Install the new .apk: {link}', None),
    ]
    for label, rel, via, marker, action, command in installs:
        binary = os.path.join(root, rel)
        os.makedirs(os.path.dirname(binary))
        shutil.copy2(built, binary)
        run_as = binary
        if via:
            # As brew links it: bin/fsend -> ../Cellar/fsend/<version>/bin/fsend.
            run_as = os.path.join(root, via)
            os.makedirs(os.path.dirname(run_as))
            os.symlink(os.path.relpath(binary, os.path.dirname(run_as)), run_as)
        if marker:
            share = os.path.join(os.path.dirname(os.path.dirname(binary)), 'share', 'fsend')
            os.makedirs(share)
            with open(os.path.join(share, 'package-manager'), 'w') as f:
                f.write(marker + '\n')
        d = fresh('s27-' + label.replace(' ', '-').lower())
        snd = PtyRun([run_as, 'small.txt'], d, runtime=30, env=env, cols=100)
        snd.start()
        m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
        if not m:
            snd.stop()
            RESULTS.append((s, f'{label}: sender produced no code', 'TIMEOUT', False))
            continue
        time.sleep(0.5)
        rcv = PtyRun([H.bin, m.group(1), '--yes', '--out', 'recv_out'], d, runtime=20)
        rcv.start(); rcv.join(); snd.join(timeout=8); snd.stop()
        t = snd.text
        lines = [pl for pl, _ in screen_lines(t)]
        has_text(s, f'{label}: the notice follows the summary', t, '\n  fsend 9.9.9 is available (you have 1.0.0)')
        glob_ok(s, f'{label}: the action for this install, alone in column 2', '  ' + action in lines,
                repr([l for l in lines if 'releases/tag' in l][:1]))
        if command:
            check(s, f'{label}: the command to type is accent + bold', t, 'releases/tag', command, ORANGE, True)
        if command != 'fsend --update':
            lacks_text(s, f'{label}: never sent to fsend --update, which refuses here', t, 'fsend --update')
        check(s, f'{label}: the release link is the link colour', t, 'releases/tag', link, CYAN)
        wide = [l for l in lines if cols_of(l) > 79 and l[:1] not in SPINNER]
        glob_ok(s, f'{label}: the notice fits 80 columns', not [l for l in wide if 'releases/tag' in l or 'is available' in l],
                repr(wide[:1]) if wide else None)
        grid_ok(s, t, label)
        ansi_only(s, t)
    # The refusal the notice must agree with: --update on the packaged
    # binary is E039 and names the same way out (the release's package).
    refused = PtyRun([os.path.join(root, 'deb/usr/bin/fsend'), '--update'], H.tmp, runtime=6, env=env)
    refused.start(); refused.join()
    has_text(s, 'deb: --update refuses (E039), before any lookup', refused.text, '[E039]')
    has_text(s, 'deb: the refusal points at the package too', refused.text, 'installing the new one')
    glob_ok(s, 'deb: --update exits 39', refused.status == 39, f'exit {refused.status}')


def s28_paused_terminal():
    """R3: a paused terminal must not freeze the transfer. Every stderr
    write holds one lock, so with flow control on, Ctrl-S on the sender's
    terminal blocked the next spinner frame and with it the pairing. fsend
    turns IXON off for the run: the receiver finishes while the sender's
    terminal is still "paused", the sender's lines follow in order after
    Ctrl-Q, and the terminal gets IXON back when fsend exits."""
    s = 'S28 paused terminal'
    d = fresh('s28')
    snd = PtyRun([H.bin, 'small.txt'], d, feed=[(1.0, b'\x13'), (14.0, b'\x11')], runtime=30)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((s, 'sender produced no code', 'TIMEOUT', False))
        return
    time.sleep(3.0)  # well past the Ctrl-S
    rcv = PtyRun([H.bin, m.group(1), '--yes', '--out', 'recv_out'], d, runtime=20)
    rcv.start(); rcv.join()
    glob_ok(s, 'receiver finishes while the sender is paused', rcv.status == 0 and rcv.elapsed < 9,
            f'exit {rcv.status} after {rcv.elapsed:.1f} s')
    has_text(s, 'receiver saved the file', rcv.text, 'Saved ./recv_out/small.txt')
    snd.join(timeout=25); snd.stop()
    sp = snd.text
    glob_ok(s, 'sender exits 0', snd.status == 0, f'exit {snd.status}')
    lacks_text(s, 'sender never lost the receiver', sp, 'Connection interrupted')
    order = [sp.find(m.group(1)), sp.find('Sent')]
    glob_ok(s, "sender's lines in order: code, then the summary", -1 not in order and order == sorted(order), order)
    grid_ok(s, sp, 'paused sender')
    # The terminal gets its flow control back.
    tty = PtyRun(['sh', '-c', H.bin + ' --version >/dev/null; stty -a'], d, runtime=6)
    tty.start(); tty.join()
    glob_ok(s, 'IXON restored on exit', re.search(r'(?<![-\w])ixon\b', tty.text) is not None, tty.text[-200:])


SCENARIOS = (s1_single_file, s2_folder, s3_conflict, s4_password, s5_decline,
             s6_text, s7_errors, s8_no_color, s9_help, s10_piped, s11_themes,
             s12_narrow, s13_cancel, s14_late_arriver, s15_completion, s16_notify,
             s17_plain, s18_spinner_width, s19_detected_background, s20_visual_system,
             s21_cancel_resume, s22_clipboard, s23_prompts, s24_wide_names,
             s25_skipped_lists, s26_install_hint, s27_update_notice,
             s28_paused_terminal, s29_server_down)


def main():
    global H
    if os.name == 'nt':
        print('validate-ux.py requires a Unix pty; skip on Windows.')
        return 0
    # Arguments pick scenarios by number: `validate-ux.py s14 s21`.
    only = [a.lower() for a in sys.argv[1:]]
    known = {fn.__name__.split('_')[0] for fn in SCENARIOS}
    if set(only) - known:
        print('unknown scenario: ' + ' '.join(sorted(set(only) - known))
              + ' (known: s1 to s%d)' % len(SCENARIOS))
        return 2
    scenarios = [fn for fn in SCENARIOS if not only or fn.__name__.split('_')[0] in only]
    H = Harness()
    try:
        H.start_server()
        H.configure_client()
        for fn in scenarios:
            try:
                fn()
            except Exception as e:  # noqa: BLE001 — one broken scenario ≠ lost matrix
                RESULTS.append((fn.__name__, 'scenario crashed', repr(e), False))
            widths_ok(fn.__name__.split('_')[0].upper())
            H.kill_clients()
            time.sleep(0.5)
    finally:
        H.cleanup()
    width = max(len(r[1]) for r in RESULTS) + 2
    passed = fails = xfails = 0
    xpassed = []
    cur = None
    for sc, label, got, ok in RESULTS:
        if sc != cur:
            print(f'\n{sc}')
            cur = sc
        expected = (sc, label) in EXPECTED_FAILURES
        if ok and expected:
            mark = 'XPASS'
            xpassed.append((sc, label))
        elif ok:
            mark = 'PASS '
            passed += 1
        elif expected:
            mark = 'XFAIL'
            xfails += 1
            got = f'{got}  (expected: {EXPECTED_FAILURES[(sc, label)]})'
        else:
            mark = 'FAIL '
            fails += 1
        print(f'  {mark}  {label:<{width}} {got}')
    print(f'\n{"=" * 60}\n{passed}/{len(RESULTS)} checks passed, {fails} failed'
          + (f', {xfails} expected to fail' if xfails else '')
          + (f', {len(xpassed)} passed but marked as expected to fail' if xpassed else ''))
    for sc, label in xpassed:
        # A stale mark would hide the next regression of this check.
        print(f'{sc} / {label}: passes now. Remove it from EXPECTED_FAILURES.')
    return 1 if fails or xpassed else 0


if __name__ == '__main__':
    sys.exit(main())
