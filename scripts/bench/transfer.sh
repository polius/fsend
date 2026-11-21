#!/usr/bin/env bash
# Times one transfer between two processes on this machine and reports
# each side's peak memory (docs/comparison.md#emulated-networks).
#
#   scripts/bench/transfer.sh fsend ITEM [auto|direct]
#   scripts/bench/transfer.sh croc  ITEM [auto|relay]
#
# ITEM is a file or folder inside $WORK/src. Prints one line:
#   TOOL ITEM ROUTE rss SENDER/RECEIVER MiB t SECONDS ok|MISMATCH
#
# fsend runs its own pairing server on 127.0.0.1:18080 (never the public
# one); croc runs its relay on 127.0.0.1:9109. "relay" makes croc skip its
# local-network discovery (--no-local), as between two networks.
#
# Environment: WORK (default ./bench-work), FSEND_BIN (default fsend),
# CROC_BIN (default croc). macOS: peak memory comes from /usr/bin/time -l.
set -euo pipefail
TOOL=${1:?tool: fsend or croc}
ITEM=${2:?item inside \$WORK/src}
ROUTE=${3:-auto}
WORK=$(cd "${WORK:-./bench-work}" && pwd)
FSEND_BIN=${FSEND_BIN:-fsend}
CROC_BIN=${CROC_BIN:-croc}

# Each run gets a clean home, so no settings, resume keys or trust leak in.
export HOME=$WORK/home XDG_CONFIG_HOME=$WORK/home/.config
mkdir -p "$HOME"
chmod -R u+w "$WORK/dst" 2>/dev/null || true
rm -rf "$WORK/dst"
mkdir -p "$WORK/dst"
: > "$WORK/out.txt"

SRV=
cleanup() { [ -n "$SRV" ] && kill "$SRV" 2>/dev/null; wait 2>/dev/null || true; }
trap cleanup EXIT

cd "$WORK/src"
case $TOOL in
fsend)
  export FSEND_NO_UPDATE_CHECK=1 FSEND_NO_CLIPBOARD=1 FSEND_NO_NOTIFY=1
  export FSEND_SERVER=http://127.0.0.1:18080
  ARGS=(--quiet)
  [ "$ROUTE" = direct ] && ARGS+=(--mode=direct)
  (FSEND_SERVER_ADDR=127.0.0.1:18080 FSEND_RELAY_ADDR=127.0.0.1:18443 exec "$FSEND_BIN" server >/dev/null 2>&1) &
  SRV=$!
  sleep 0.7
  /usr/bin/time -l "$FSEND_BIN" "${ARGS[@]}" "$ITEM" > "$WORK/out.txt" 2> "$WORK/snd.time" &
  SND=$!
  for _ in $(seq 100); do [ -s "$WORK/out.txt" ] && break; sleep 0.1; done
  RECV=("$FSEND_BIN" "${ARGS[@]}" --yes "$(head -1 "$WORK/out.txt")")
  ;;
croc)
  (exec "$CROC_BIN" relay --host 127.0.0.1 --port 9109 --ports 9109,9110,9111,9112,9113 >/dev/null 2>&1) &
  SRV=$!
  sleep 1
  ARGS=(--relay 127.0.0.1:9109 --pass pass123 --yes) # pass123: the relay's default password
  NOLOCAL=()
  [ "$ROUTE" = relay ] && NOLOCAL=(--no-local)
  export CROC_SECRET=bench-code-$$
  /usr/bin/time -l "$CROC_BIN" "${ARGS[@]}" send ${NOLOCAL[@]+"${NOLOCAL[@]}"} "$ITEM" > "$WORK/out.txt" 2> "$WORK/snd.time" &
  SND=$!
  sleep 3
  RECV=("$CROC_BIN" "${ARGS[@]}" --overwrite)
  ;;
*) echo "unknown tool: $TOOL (fsend or croc)" >&2; exit 2 ;;
esac

cd "$WORK/dst"
/usr/bin/time -l "${RECV[@]}" > /dev/null 2> "$WORK/rcv.time"
wait "$SND"

mib() { tr '\r' '\n' < "$1" | awk '/maximum resident/ {printf "%.0f", $1/1048576}'; }
secs() { tr '\r' '\n' < "$1" | grep -Eo '[0-9,.]+ real' | awk '{print $1}'; }
OK=MISMATCH
diff -rq "$WORK/src/$ITEM" "$WORK/dst/$ITEM" >/dev/null 2>&1 && OK=ok
echo "$TOOL $ITEM $ROUTE rss $(mib "$WORK/snd.time")/$(mib "$WORK/rcv.time") MiB t $(secs "$WORK/rcv.time") $OK"
