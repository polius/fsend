#!/usr/bin/env python3
"""validate-ux.py — empirical colour/UI audit for the fsend CLI.

Builds the CLI from the working tree, starts an isolated pairing server
on loopback, and runs every user-facing scenario as concurrent pty
sessions (sender + receiver). Both sides' raw stderr is captured and
audited per character: each checked element must render in exactly the
semantic colour token the design assigns it (truecolour SGR), global
invariants must hold (no dim attribute, no muted grey), and degraded
modes must stay byte-clean (NO_COLOR, piped stderr → zero colour
escapes, ASCII glyph fallbacks).

Scenarios: single file, folder, conflict + overwrite prompt, password
flow (wrong → correct), decline, text send, error rendering, NO_COLOR,
--help, piped degradation. Exit status: 0 when every check passes.

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

# Expected truecolour SGR parameters per semantic token. These must match
# internal/uxlog/glyphs.go's colourToken table.
ORANGE  = '38;2;255;158;100'   # accent  — act: code, box, spinner, arrows
VIOLET  = '38;2;187;154;247'   # primary — decide: questions, prompts
GREEN   = '38;2;158;206;106'   # success — reassure + file references
YELLOW  = '38;2;224;175;104'   # warning — caution: ⚠, differs, kept
RED     = '38;2;247;118;142'   # error   — failure: ✗, diff-removed
CYAN    = '38;2;125;207;255'   # info    — status: ℹ, --help flags, links
DEFAULT = None                 # terminal default foreground


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


def glob_ok(sc, label, ok):
    RESULTS.append((sc, label, 'ok' if ok else 'VIOLATION', ok))


class PtyRun(threading.Thread):
    """Runs argv in a pty; captures raw output while feeds fire on schedule."""

    def __init__(self, argv, cwd, feed=None, runtime=30, env=None):
        super().__init__(daemon=True)
        self.argv, self.cwd, self.feed, self.runtime, self.env = argv, cwd, feed, runtime, env
        self.out = b''
        self.pid = None
        self.fd = None
        self.done = False

    def child_env(self):
        e = dict(os.environ)
        e['TERM'] = 'xterm-256color'
        e['COLORTERM'] = 'truecolor'
        e.pop('NO_COLOR', None)
        if self.env:
            e.update(self.env)
        return e

    def run(self):
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(self.cwd)
            os.execvpe(self.argv[0], self.argv, self.child_env())
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 100, 0, 0))
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
            done, _ = os.waitpid(pid, os.WNOHANG)
            if done:
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


def pair(sc, d, sender_args, recv_args, recv_feed, sender_runtime=40):
    """Concurrent sender (pty) + receiver (pty). Returns (sender, receiver)."""
    snd = PtyRun([H.bin] + sender_args, d, runtime=sender_runtime)
    snd.start()
    m = snd.wait_for(r'([a-z]{3}-[a-z]{4}-[a-z]{3})\b')
    if not m:
        snd.stop()
        RESULTS.append((sc, 'sender produced no code', 'TIMEOUT', False))
        return snd, None
    code = m.group(1)
    time.sleep(0.5)
    rcv = PtyRun([H.bin, code] + recv_args, d, feed=recv_feed, runtime=25)
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


def s3_conflict():
    s = 'S3 conflict + overwrite prompt'
    d = fresh('s3')
    with open(os.path.join(d, 'recv_out', 'small.txt'), 'wb') as f:
        f.write(os.urandom(12))
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'],
                    [(4.0, b'y\n'), (6.0, b'n\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'prompt header primary', rv, 'differs from your local copies',
          '1 file differs from your local copies:', VIOLET)
    check(s, 'conflict name green', rv, '→', 'small.txt', GREEN)
    check(s, 'local size diff-removed', rv, '→', '12 B', RED)
    check(s, 'incoming size diff-added', rv, '→', '52 B', GREEN)
    check(s, 'prompt primary', rv, None, 'Overwrite all?', VIOLET)
    check(s, 'kept clause warning', rv, 'kept', '1 file kept', YELLOW)
    check(s, 'sender warn glyph', sp, 'Nothing sent', '⚠', YELLOW)
    check(s, 'Nothing sent default', sp, 'Nothing sent', 'Nothing sent', DEFAULT, False)
    glob_ok(s, 'no dim attribute', '\x1b[2m' not in sp and '\x1b[2m' not in rv)


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
    check(s, 'receiver prompt primary', rv, 'Password for this transfer',
          'Password for this transfer:', VIOLET)
    check(s, 'retry prompt primary', rv, 'Wrong password', 'Wrong password — try again', VIOLET)
    check(s, 'chip warning glyph', rv, 'password required', '⚠', YELLOW)
    check(s, 'chip text default', rv, 'password required', 'password required', DEFAULT, False)
    check(s, 'transfer succeeded', rv, 'Saved', '✓', GREEN)


def s5_decline():
    s = 'S5 receiver declines'
    d = fresh('s5')
    snd, rcv = pair(s, d, ['small.txt'], ['--out', 'recv_out'], [(4.0, b'n\n')])
    rv = rcv.text if rcv else ''
    check(s, 'decline info glyph', rv, 'Declined', 'ℹ', CYAN)
    check(s, 'E006 tag default', rv, 'Declined', '[E006]', DEFAULT, False)


def s6_text():
    s = 'S6 text send'
    d = fresh('s6')
    snd, rcv = pair(s, d, ['--text', 'wifi: hunter2'], [], [(3.0, b'\n')])
    sp, rv = snd.text, rcv.text if rcv else ''
    check(s, 'arrow accent', sp, 'Sending text', '⇢', ORANGE)
    check(s, '"text" NOT path-green', sp, 'Sending text', 'Sending text', DEFAULT, False)
    check(s, 'recv Accept? primary', rv, 'Accept?', 'Accept?', VIOLET)


def s7_errors():
    s = 'S7 error rendering'
    d = fresh('s7')
    out = PtyRun([H.bin, 'nope.txt'], d, runtime=6)
    out.start(); out.join()
    check(s, 'cross error glyph', out.text, 'E025', '✗', RED)
    out2 = PtyRun([H.bin, 'abc-defg-jk'], d, runtime=6)
    out2.start(); out2.join()
    check(s, 'code-typo hint accent', out2.text, 'receive code', 'abc-defg-jkm', ORANGE, True)


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
    glob_ok(s, 'piped sender zero colour SGR',
            not re.search(r'\x1b\[(3[0-9]|38;|1;38|9[0-7])m', err))
    glob_ok(s, 'ASCII glyph fallback', '[*] Waiting for receiver' in err)
    glob_ok(s, 'plain block fallback', 'On the other machine, run:' in err)


SCENARIOS = (s1_single_file, s2_folder, s3_conflict, s4_password, s5_decline,
             s6_text, s7_errors, s8_no_color, s9_help, s10_piped)


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
