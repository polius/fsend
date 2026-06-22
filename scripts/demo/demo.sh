#!/usr/bin/env bash
#
# Drives a scripted two-pane (sender / receiver) fsend transfer, used to
# record the README demo (see demo.tape). Everything runs locally and
# offline: an isolated fsend pairing server on loopback, two tmux panes,
# and a background "director" that types the commands, scrapes the dynamic
# share code out of the sender pane, and pastes it into the receiver.
#
# Usage:
#   scripts/demo/demo.sh          # set up + play in one terminal (manual preview)
#   scripts/demo/demo.sh setup    # build, start server, build the tmux session
#   scripts/demo/demo.sh play     # attach + run the director (used by demo.tape)
#
# Nothing here touches your real fsend config or home dir.
set -euo pipefail

SESSION=fsenddemo
ROOT="${FSEND_DEMO_ROOT:-/tmp/fsend-demo}"
HTTP_ADDR="${FSEND_DEMO_HTTP:-127.0.0.1:8799}"
UDP_ADDR="${FSEND_DEMO_UDP:-127.0.0.1:8798}"
HOME_DIR="$ROOT/home"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# What gets transferred. A 1 GiB file so the transfer runs a few seconds and
# shows the progress bar; a realistic name (not a 1 GB "pdf"). The payload is
# fresh /dev/urandom (incompressible, so zstd can't shrink it away) and cached.
FILE="drone-4k.mov"
PAYLOAD="$ROOT/payload.bin"

# Two stacked panes: sender on top, receiver below. The terminal padding is the
# only top margin (no spacer pane), so the SENDER card sits right under the
# window bar; the director sizes the gap between the cards and the bottom margin
# (see "balance the layout").
P0="$SESSION.0"     # sender
P1="$SESSION.1"     # receiver

# ensure_payload generates the cached 1 GiB transfer payload if missing. Fresh
# /dev/urandom so it stays incompressible (a real ~1 GB video would be too).
PAYLOAD_BYTES=500000000   # ~500 MB as fsend displays it; ~6s transfer in-render
ensure_payload() {
  [ -f "$PAYLOAD" ] && [ "$(wc -c <"$PAYLOAD")" -eq "$PAYLOAD_BYTES" ] && return
  mkdir -p "$(dirname "$PAYLOAD")"
  dd if=/dev/urandom of="$PAYLOAD" bs=1000000 count=500 2>/dev/null
}

# ---------------------------------------------------------------------------
# setup: build the binary, start an isolated pairing server, and lay out the
# three-pane tmux session (spacer, sender, receiver) with clean prompts.
# ---------------------------------------------------------------------------
do_setup() {
  local bin="${FSEND_BIN:-$ROOT/fsend}"
  if [ -z "${FSEND_BIN:-}" ]; then
    ( cd "$REPO_ROOT" && go build -o "$bin" ./cmd/fsend )
  fi

  tmux kill-session -t "$SESSION" 2>/dev/null || true
  [ -f "$ROOT/server.pid" ] && kill "$(cat "$ROOT/server.pid")" 2>/dev/null || true

  rm -rf "$ROOT/cfg" "$HOME_DIR"
  mkdir -p "$ROOT/cfg" "$HOME_DIR/Downloads"
  export XDG_CONFIG_HOME="$ROOT/cfg"
  export FSEND_NO_UPDATE_CHECK=1
  # Panes inherit this: silence macOS bash's "default shell is now zsh" notice.
  export BASH_SILENCE_DEPRECATION_WARNING=1

  # The 1 GiB payload is cached (generation is the slow part — record.sh
  # pre-warms it before recording). Hardlink it into the sender's home so each
  # run is instant; it's outside HOME_DIR, which is wiped above.
  ensure_payload
  ln -f "$PAYLOAD" "$HOME_DIR/$FILE"

  FSEND_HTTP_ADDR="$HTTP_ADDR" FSEND_UDP_ADDR="$UDP_ADDR" FSEND_LOG_LEVEL=error \
    "$bin" server >"$ROOT/server.log" 2>&1 &
  echo $! > "$ROOT/server.pid"
  disown

  # Point the isolated client config at the local server.
  local i
  for i in $(seq 1 50); do
    "$bin" --connect "$HTTP_ADDR" >/dev/null 2>&1 && break
    sleep 0.1
  done

  # A `fsend` shim on the panes' PATH pins the transfer to the direct P2P
  # path (--mode=direct). This keeps the recording deterministic and free of
  # environment-specific LAN-discovery notices, while the on-screen command
  # the viewer sees stays a plain `fsend ...`.
  local shim="$ROOT/shim"
  mkdir -p "$shim"
  cat >"$shim/fsend" <<EOF
#!/usr/bin/env bash
args=(--mode=direct)
# Advertise a neutral peer name (set per pane) instead of the real hostname.
[ -n "\${FSEND_DEMO_NAME:-}" ] && args+=(--name "\$FSEND_DEMO_NAME")
exec '$bin' "\${args[@]}" "\$@"
EOF
  chmod +x "$shim/fsend"

  # One rcfile per pane: a clean prompt, the isolated env, and the right cwd.
  # $2 = prompt label, $3 = ANSI color, $4 = starting directory.
  # One rcfile per pane: env, prompt persona, cwd, and a coral card header so
  # each pane reads as a distinct machine without tmux border chrome.
  # $2 prompt label, $3 prompt color, $4 cwd, $5 peer name, $6 role, $7 host.
  mkrc() {
    cat >"$1" <<EOF
export HOME='$HOME_DIR'
export XDG_CONFIG_HOME='$ROOT/cfg'
export PATH='$shim':"\$PATH"
export FSEND_NO_UPDATE_CHECK=1
export FSEND_DEMO_NAME='$5'
PS1='\[\e[1;$3m\]$2\[\e[0m\]:\w\$ '
cd '$4'
clear
printf '$8\033[38;2;224;128;90m▌\033[0m \033[1m%s\033[0m  \033[2m%s\033[0m\n\n' '$6' '$7'
EOF
  }
  #                                                                            $8 = leading blank lines
  mkrc "$ROOT/sender.rc" "you@laptop"     "32" "$HOME_DIR"           "laptop"  "SENDER"   "· your laptop"  ""
  mkrc "$ROOT/recver.rc" "friend@desktop" "36" "$HOME_DIR/Downloads" "desktop" "RECEIVER" "· their laptop" ""

  # Two stacked panes top→bottom: sender, receiver. The divider between them is
  # hidden (blended into the bg) so the gap reads as whitespace. The director
  # sizes them after the recorder attaches (see "balance the layout").
  tmux new-session  -d -s "$SESSION" -x 115 -y 31 "bash --rcfile '$ROOT/sender.rc' -i"
  tmux split-window -v -t "$SESSION" "bash --rcfile '$ROOT/recver.rc' -i"

  tmux set -t "$SESSION" status off
  tmux set -t "$SESSION" pane-border-status off
  tmux set -t "$SESSION" pane-border-style       "fg=#1e1e2e"
  tmux set -t "$SESSION" pane-active-border-style "fg=#1e1e2e"
  tmux select-pane -t "$P1"  # receiver active → cursor sits at its prompt
}

# ---------------------------------------------------------------------------
# director: the puppeteer. Runs in the background while a client is attached,
# typing into the panes and shuttling the share code between them.
# ---------------------------------------------------------------------------
typewriter() { # pane, text — type it character by character, like a person
  local p="$1" t="$2" i
  for ((i = 0; i < ${#t}; i++)); do
    tmux send-keys -t "$p" -l "${t:i:1}"
    sleep 0.05
  done
}
enter() { tmux send-keys -t "$1" Enter; }

await() { # pane, extended-regex — wait until the pane's text matches
  local p="$1" re="$2" i
  for i in $(seq 1 200); do   # up to ~40s; setup/transfer can be slow under render load
    tmux capture-pane -p -t "$p" | grep -qE "$re" && return 0
    sleep 0.2
  done
  return 1
}

grab_code() { # pane — extract the first fsend share code visible in the pane
  tmux capture-pane -p -t "$1" \
    | grep -oE '[a-hjkmnp-z]{3}-[a-hjkmnp-z]{4}-[a-hjkmnp-z]{3}' | head -1
}

director() {
  # Start only once a client (the recorder, or you) has attached, so the
  # whole choreography lands inside the recording.
  local i
  for i in $(seq 1 150); do
    [ "$(tmux display -p -t "$SESSION" '#{session_attached}' 2>/dev/null)" = "1" ] && break
    sleep 0.1
  done

  # Balance the layout, sized to the *final* content so nothing scrolls (the
  # SENDER/RECEIVER card headers must stay visible). Final content is 13 rows
  # (sender) and 11 rows (receiver), each ending in the returned shell prompt.
  # The sender pane gets a middle gap of blank rows below it; the receiver pane
  # takes the remainder (its trailing blanks are the bottom margin). The -1
  # absorbs the invisible divider row between the panes.
  local sc=13 mid=4
  tmux resize-pane -t "$P0" -y $(( sc + mid - 1 ))
  sleep 1.5 # let the panes register first

  # Sender: announce the file and print a share code.
  typewriter "$P0" "fsend ${FILE}"
  sleep 0.7 # show the full command before it runs
  enter "$P0"
  await "$P0" 'fsend [a-z]{3}-[a-z]{4}-[a-z]{3}' || true
  local code; code="$(grab_code "$P0")"
  sleep 2.8 # time to read "on the other machine, run: fsend <code>"

  # Receiver: paste the code, review the incoming transfer, accept.
  typewriter "$P1" "fsend ${code}"
  sleep 0.7
  enter "$P1"
  await "$P1" '\[Y/n\]' || true
  sleep 2.4 # time to read the incoming-transfer breakdown
  typewriter "$P1" "y"
  sleep 0.4
  enter "$P1"
  await "$P1" 'Saved' || true

  # Keep the session alive just past the end of the recording (demo.tape's
  # Sleep). The real end-of-loop pause is added in record.sh as a long delay on
  # the final WebP frame, so we don't record a long tail of identical frames.
  sleep 4
}

# ---------------------------------------------------------------------------
# play: attach and run the director; clean up the server + session on exit.
# ---------------------------------------------------------------------------
do_play() {
  cleanup() {
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    [ -f "$ROOT/server.pid" ] && kill "$(cat "$ROOT/server.pid")" 2>/dev/null || true
    kill "${DIRECTOR_PID:-}" 2>/dev/null || true
  }
  trap cleanup EXIT INT TERM

  director &
  DIRECTOR_PID=$!
  tmux attach -t "$SESSION"
}

case "${1:-all}" in
  payload) ensure_payload ;;   # pre-warm the 1 GiB cache (record.sh calls this)
  setup)   do_setup ;;
  play)    do_play ;;
  all)     do_setup; do_play ;;
  *) echo "usage: $0 [payload|setup|play]" >&2; exit 2 ;;
esac
