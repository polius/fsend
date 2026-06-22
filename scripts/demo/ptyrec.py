#!/usr/bin/env python3
# Minimal asciinema-v2 cast recorder with a *fixed* PTY size.
#
# Why this exists instead of `asciinema rec`: the demo is a full-screen tmux
# session that must record at an exact 100x28 geometry, and svg-term needs a
# cast as input. This forces the master PTY window size so tmux renders both
# panes fully, then streams output to a v2 cast. Run from the repo root.
#
#   python3 scripts/demo/ptyrec.py out.cast
import os, pty, sys, time, json, fcntl, termios, struct, select, codecs

COLS, ROWS = 100, 28
OUT = sys.argv[1] if len(sys.argv) > 1 else "demo.cast"
CMD = ["bash", "-lc", "scripts/demo/demo.sh play"]

os.environ["TERM"] = "xterm-256color"
pid, fd = pty.fork()
if pid == 0:
    os.execvp(CMD[0], CMD)
    os._exit(1)

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
dec = codecs.getincrementaldecoder("utf-8")("replace")
events = []
t0 = time.time()
while True:
    try:
        r, _, _ = select.select([fd], [], [], 1.0)
    except (OSError, ValueError):
        break
    if fd in r:
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        events.append([round(time.time() - t0, 4), "o", dec.decode(data)])

with open(OUT, "w") as f:
    f.write(json.dumps({"version": 2, "width": COLS, "height": ROWS,
                        "env": {"TERM": "xterm-256color", "SHELL": "/bin/bash"}}) + "\n")
    for e in events:
        f.write(json.dumps(e) + "\n")
print(f"wrote {OUT}: {len(events)} events, {events[-1][0] if events else 0}s", file=sys.stderr)
