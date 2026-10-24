#!/usr/bin/env python3
"""validate-ux.py — empirical colour/UI audit for the fsend CLI.

Builds the CLI from the working tree, starts an isolated pairing server
on loopback, and runs every user-facing scenario as concurrent pty
sessions (sender + receiver). Both sides' raw stderr is captured and
audited per character: each checked element must render in exactly the
semantic colour token the design assigns it (truecolour SGR), every
colour emitted must clear WCAG AA (4.5:1) against the background(s) its
palette targets, global invariants must hold (no dim attribute, no muted
grey), and degraded modes must stay byte-clean (NO_COLOR, FSEND_PLAIN,
piped stderr → zero colour escapes, ASCII glyph fallbacks). Text checks
assert on what the user reads (headlines, hints, exit codes), not just
on escapes.

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
# internal/uxlog/glyphs.go's colourToken table (dark, light, both).
PALETTES = {
    'dark':  {'accent': '#ff9e64', 'primary': '#bb9af7', 'success': '#9ece6a',
              'warning': '#e0af68', 'error': '#f7768e', 'info': '#7dcfff'},
    'light': {'accent': '#b15c00', 'primary': '#7847bd', 'success': '#587539',
              'warning': '#8c6c3e', 'error': '#b3261e', 'info': '#007197'},
    'both':  {'accent': '#c85000', 'primary': '#8f58dc', 'success': '#47851b',
              'warning': '#a06b00', 'error': '#da3644', 'info': '#007fa9'},
}
# The backgrounds each palette must read on.
BACKGROUNDS = {'dark': [(0, 0, 0)], 'light': [(255, 255, 255)],
               'both': [(255, 255, 255), (0, 0, 0)]}


def sgr(hexcol):
    r, g, b = (int(hexcol[i:i + 2], 16) for i in (1, 3, 5))
    return f'38;2;{r};{g};{b}'


def tokens(theme):
    p = PALETTES[theme]
    return tuple(sgr(p[k]) for k in ('accent', 'primary', 'success', 'warning', 'error', 'info'))


# The harness sets neither FSEND_THEME nor COLORFGBG, so the default runs
# get the unknown-background ("both") palette.
ORANGE, VIOLET, GREEN, YELLOW, RED, CYAN = tokens('both')
DEFAULT = None                 # terminal default foreground


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
        self.fg, self.bold = None, False


def line_colors(line):
    """Walk one raw line; return (plain_text, per-char (fg, bold) list)."""
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
                if p == '0':
                    ctx.fg, ctx.bold = None, False
                elif p == '1':
                    ctx.bold = True
                elif p == '38':
                    if k + 1 < len(parts) and parts[k + 1] == '2':
                        ctx.fg = '38;2;' + ';'.join(parts[k + 2:k + 5])
                        k += 4
                    elif k + 1 < len(parts) and parts[k + 1] == '5':
                        ctx.fg = '38;5;' + parts[k + 2]
                        k += 2
                elif p in ('31', '32', '33', '34', '35', '36', '37',
                           '90', '91', '92', '93', '94', '96', '97'):
                    ctx.fg = p
                k += 1
            i = j + 1
            continue
        chars.append(line[i])
        colors.append((ctx.fg, ctx.bold))
        i += 1
    return ''.join(chars), colors


def split_stream(stream):
    """Lines on \n AND \r: spinner/bar redraws reuse the line via \r, so
    every redraw frame must audit as its own line."""
    return stream.replace('\r\n', '\n').replace('\r', '\n').split('\n')


def probe(stream, context, needle):
    """(fg, bold) set covering `needle` on the first plain line that
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


def check(sc, label, stream, context, needle, expect_fg, expect_bold=None):
    got = probe(stream, context, needle)
    if got is None:
        RESULTS.append((sc, label, 'NOT FOUND', False))
        return
    fgs = {fg for fg, _ in got}
    bolds = {b for _, b in got}
    ok = fgs == ({expect_fg} if expect_fg else {None})
    if expect_bold is not None:
        ok = ok and bolds == {expect_bold}
    RESULTS.append((sc, label,
                    f'fg={sorted(fgs, key=str)} bold={sorted(bolds, key=str)}', ok))


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


def contrast_ok(sc, stream, theme='both'):
    """Every truecolour foreground in stream clears 4.5:1 on the theme's
    background(s) — the WCAG AA bar the palette promises."""
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


class PtyRun(threading.Thread):
    """Runs argv in a pty; captures raw output while feeds fire on schedule."""

    def __init__(self, argv, cwd, feed=None, runtime=30, env=None, cols=100):
        super().__init__(daemon=True)
        self.argv, self.cwd, self.feed, self.runtime, self.env = argv, cwd, feed, runtime, env
        self.cols = cols
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
                  'FSEND_PASSWORD'):
            e.pop(k, None)
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
        env = dict(os.environ, **self.client_env(FSEND_SERVER_ADDR=f'127.0.0.1:{self.port}'))
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
        subprocess.run([self.bin, '--connect', f'127.0.0.1:{self.port}'],
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


def pair(sc, d, sender_args, recv_args, recv_feed, sender_runtime=40, env=None, cols=100):
    """Concurrent sender (pty) + receiver (pty). Returns (sender, receiver)."""
    snd = PtyRun([H.bin] + sender_args, d, runtime=sender_runtime, env=env, cols=cols)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((sc, 'sender produced no code', 'TIMEOUT', False))
        return snd, None
    code = m.group(1)
    time.sleep(0.5)
    rcv = PtyRun([H.bin, code] + recv_args, d, feed=recv_feed, runtime=25, env=env, cols=cols)
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
    check(s, 'arrow accent', sp, 'Sending', '⇢', ORANGE)
    check(s, 'artifact name green', sp, 'Sending', 'small.txt', GREEN)
    check(s, 'box border accent', sp, None, '┌', ORANGE)
    check(s, 'share code accent+bold', sp, None, code, ORANGE, True)
    check(s, 'spinner accent', sp, 'Waiting for receiver', '⠋', ORANGE)
    check(s, 'summary ✓ success', sp, None, '✓', GREEN)
    check(s, 'summary name green', sp, 'Sent', 'small.txt', GREEN)
    check(s, 'summary size default', sp, None, '52 B', DEFAULT, False)
    check(s, 'recv arrow accent', rv, 'Incoming from', '⇣', ORANGE)
    check(s, 'recv artifact name green', rv, None, 'small.txt', GREEN)
    check(s, 'question primary', rv, 'Save to', 'Save to', VIOLET)
    check(s, 'destination green+bold', rv, 'Save to', 'recv_out/', GREEN, True)
    check(s, 'recv summary name green', rv, 'Saved', 'small.txt', GREEN)
    check(s, 'recv summary dest green', rv, 'Saved', 'recv_out', GREEN)
    check(s, 'bar chip green', rv, None, 'small.txt', GREEN)
    glob_ok(s, 'no dim attribute', '\x1b[2m' not in sp and '\x1b[2m' not in rv)
    glob_ok(s, 'no grey 244', '38;5;244' not in sp and '38;5;244' not in rv)
    # What the user reads: who connected, what went where.
    has_text(s, 'sender names who accepted', sp, ' accepted')
    has_text(s, 'sender summary names subject + peer', sp, 'Sent small.txt to ')
    has_text(s, 'receiver names the saved file', rv, 'Saved ./recv_out/small.txt')
    lacks_text(s, 'no progress bar under 1 MB', rv, '100%')
    glob_ok(s, 'no notification for a short run', '\x1b]9;' not in sp and '\x07' not in sp)
    contrast_ok(s, sp + rv)


def s2_folder():
    s = 'S2 folder accept'
    d = fresh('s2')
    snd, rcv = pair(s, d, ['--exclude', 'node_modules', 'myproj'],
                    ['--out', 'recv_out'], [(4.0, b'y\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'sender header dir green', sp, 'Sending', 'myproj/', GREEN)
    check(s, 'sender listing name green', sp, 'video1.bin', 'assets/video1.bin', GREEN)
    check(s, 'sender size default', sp, 'video1.bin', '20 MB', DEFAULT, False)
    check(s, 'recv artifact dir green', rv, 'files', 'myproj/', GREEN)
    check(s, 'recv listing name green', rv, 'video1.bin', 'assets/video1.bin', GREEN)
    check(s, 'recv bar chip green', rv, None, 'video1.bin', GREEN)
    check(s, 'recv summary dest green', rv, 'Saved', 'recv_out', GREEN)
    glob_ok(s, 'no dim attribute', '\x1b[2m' not in sp and '\x1b[2m' not in rv)
    has_text(s, 'sender summary names the folder', sp, 'Sent myproj/ to ')
    has_text(s, 'receiver names folder and place', rv, 'Saved myproj/ to ./recv_out/')
    contrast_ok(s, sp + rv)


def s3_conflict():
    s = 'S3 conflict + overwrite prompt'
    d = fresh('s3')
    with open(os.path.join(d, 'recv_out', 'small.txt'), 'wb') as f:
        f.write(os.urandom(12))
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'],
                    [(4.0, b'y\n'), (6.0, b'n\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'prompt header primary', rv, 'incoming copy?',
          'Replace 1 file with the incoming copy?', VIOLET)
    check(s, 'delete consequence warning', rv, 'incoming copy?',
          '⚠ your local version is deleted', YELLOW)
    check(s, 'conflict name green', rv, '→', 'small.txt', GREEN)
    check(s, 'local size diff-removed', rv, '→', '12 B', RED)
    check(s, 'incoming size diff-added', rv, '→', '52 B', GREEN)
    check(s, 'prompt primary', rv, None, 'Replace your local copy?', VIOLET)
    check(s, 'kept clause warning', rv, 'kept', '1 file kept', YELLOW)
    check(s, 'sender warn glyph', sp, 'Nothing sent', '⚠', YELLOW)
    check(s, 'Nothing sent default', sp, 'Nothing sent', 'Nothing sent', DEFAULT, False)
    glob_ok(s, 'no dim attribute', '\x1b[2m' not in sp and '\x1b[2m' not in rv)
    # Answering "n" is a choice, not a failure.
    glob_ok(s, 'kept-by-choice exits 0', rcv is not None and rcv.status == 0,
            f'exit {rcv.status if rcv else None}')
    contrast_ok(s, sp + rv)


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
    check(s, 'password context warning', rv, 'password-protected', '⚠', YELLOW)
    check(s, 'retry prompt primary', rv, 'Wrong password', 'Wrong password — try again', VIOLET)
    check(s, 'chip warning glyph', rv, 'password required', '⚠', YELLOW)
    check(s, 'chip text default', rv, 'password required', 'password required', DEFAULT, False)
    check(s, 'transfer succeeded', rv, 'Saved', '✓', GREEN)
    glob_ok(s, 'suggestion is four words', re.fullmatch(r'[a-z]+(-[a-z]+){3}', pw) is not None, pw)
    has_text(s, 'code box says a password is needed', sp, 'password required')
    contrast_ok(s, sp + rv)


def s5_decline():
    s = 'S5 receiver declines'
    d = fresh('s5')
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b'n\n')])
    rv = rcv.text if rcv else ''
    check(s, 'decline info glyph', rv, 'Declined', 'ℹ', CYAN)
    check(s, 'E006 tag default', rv, 'Declined', '[E006]', DEFAULT, False)
    glob_ok(s, "receiver's own decline exits 0", rcv is not None and rcv.status == 0,
            f'exit {rcv.status if rcv else None}')
    glob_ok(s, 'sender sees the decline as E006 (exit 6)', snd.status == 6, f'exit {snd.status}')


def s6_text():
    s = 'S6 text send'
    d = fresh('s6')
    snd, rcv = pair(s, d, ['--text', 'wifi: hunter2'], [], [(3.0, b'\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'arrow accent', sp, 'Sending text', '⇢', ORANGE)
    check(s, '"text" NOT path-green', sp, 'Sending text', 'Sending text', DEFAULT, False)
    check(s, 'recv question primary', rv, 'Show the text?', 'Show the text?', VIOLET)
    has_text(s, 'sender summary says text', sp, 'Sent text to ')


def s7_errors():
    s = 'S7 error rendering'
    d = fresh('s7')
    out = PtyRun([H.bin, 'nope.txt'], d, runtime=6)
    out.start(); out.join()
    check(s, 'cross error glyph', out.text, 'E025', '✗', RED)
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
    contrast_ok(s, out.text + out2.text + out3.text)


def s8_no_color():
    s = 'S8 NO_COLOR degradation'
    d = fresh('s8')
    sp = PtyRun([H.bin, 'small.txt'], d, runtime=6, env={'NO_COLOR': '1'})
    sp.start(); sp.join()
    glob_ok(s, 'sender zero colour SGR',
            not re.search(r'\x1b\[(3[0-9]|38;|1;38|9[0-7])m', sp.text))
    glob_ok(s, 'plain block fallback', 'On the other machine, run:' in sp.text)


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
    contrast_ok(s, t)


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
    glob_ok(s, 'plain block fallback', 'On the other machine, run:' in err)
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
        check(s, f'{theme}: question primary', rv, 'Save to', 'Save to', pri)
        check(s, f'{theme}: summary ✓ success', sp, None, '✓', suc)
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
    wide = [b for b in bars if len(b) > 40]
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


SCENARIOS = (s1_single_file, s2_folder, s3_conflict, s4_password, s5_decline,
             s6_text, s7_errors, s8_no_color, s9_help, s10_piped, s11_themes,
             s12_narrow, s13_cancel, s14_late_arriver, s15_completion, s16_notify,
             s17_plain)


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
