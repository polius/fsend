#!/usr/bin/env bash
# Emulates a network on a Mac's loopback for transfer benchmarks
# (docs/comparison.md#emulated-networks). macOS only: pf and dummynet.
#
#   sudo scripts/bench/shape.sh up lan|wifi|internet   start (or switch) emulation
#   sudo scripts/bench/shape.sh down                   remove everything it set
#   scripts/bench/shape.sh status                      show what is active
#
# Scope: only loopback traffic between two ephemeral ports (49152-65535: fsend's
# local TCP route and its direct QUIC route) and croc's ports (9009-9013 local,
# 9109-9113 its relay). Nothing else on the machine is shaped. The rules live in
# their own anchor (com.apple/fsendbench), so the system's pf rules are untouched;
# pf is enabled with a reference token that `down` releases. Loopback's MTU is set
# to 1500 like a real network while up, and restored on `down`.
set -euo pipefail
ANCHOR=com.apple/fsendbench
STATE=/private/tmp/fsendbench-shape.state
P1=7701 P2=7702 P3=7703

profile() {
  case $1 in
    # Gigabit Ethernet on one switch: full rate, sub-millisecond round trip.
    lan)      BW=1000Mbit/s DELAY=0  PLR=0      HALF=0 ;;
    # Good Wi-Fi 5/6: about 400 Mbit/s of goodput, 4 ms round trip, rare loss.
    wifi)     BW=400Mbit/s  DELAY=2  PLR=0.0002 HALF=1 ;;
    # Two homes on 100 Mbit/s fibre: 30 ms round trip, 0.1% loss.
    internet) BW=100Mbit/s  DELAY=15 PLR=0.001  HALF=8 ;;
    # The same path without random loss, and with a little (0.01%).
    internet-clean) BW=100Mbit/s DELAY=15 PLR=0      HALF=8 ;;
    internet-low)   BW=100Mbit/s DELAY=15 PLR=0.0001 HALF=8 ;;
    *) echo "unknown profile: $1 (lan, wifi, internet, internet-clean, internet-low)" >&2; exit 2 ;;
  esac
}

down() {
  pfctl -a "$ANCHOR" -F all >/dev/null 2>&1 || true
  dnctl pipe delete $P1 $P2 $P3 >/dev/null 2>&1 || true
  if [ -f "$STATE" ]; then
    # shellcheck disable=SC1090
    . "$STATE"
    [ -n "${TOKEN:-}" ] && pfctl -X "$TOKEN" >/dev/null 2>&1 || true
    [ -n "${MTU:-}" ] && ifconfig lo0 mtu "$MTU" || true
    rm -f "$STATE"
  fi
}

case ${1:-} in
  up)
    [ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
    profile "${2:-}"
    down
    MTU=$(ifconfig lo0 | awk '/mtu/ {print $NF; exit}')
    ifconfig lo0 mtu 1500
    dnctl pipe $P1 config bw "$BW" delay "$DELAY" plr "$PLR" queue 100
    # croc's relay sits between the two ends: each leg gets half the delay.
    dnctl pipe $P2 config bw "$BW" delay "$HALF" plr "$PLR" queue 100
    dnctl pipe $P3 config bw "$BW" delay "$HALF" plr "$PLR" queue 100
    pfctl -a "$ANCHOR" -f - <<EOF
dummynet out quick on lo0 proto { tcp udp } from any port >= 49152 to any port >= 49152 pipe $P1
dummynet out quick on lo0 proto tcp from any to any port 9009:9013 pipe $P1
dummynet out quick on lo0 proto tcp from any port 9009:9013 to any pipe $P1
dummynet out quick on lo0 proto tcp from any to any port 9109:9113 pipe $P2
dummynet out quick on lo0 proto tcp from any port 9109:9113 to any pipe $P3
EOF
    TOKEN=$(pfctl -E 2>&1 | awk '/Token/ {print $NF}')
    printf 'MTU=%s\nTOKEN=%s\nPROFILE=%s\n' "$MTU" "$TOKEN" "$2" > "$STATE"
    echo "emulating $2: $BW, ${DELAY} ms each way, loss $PLR (lo0 mtu 1500, was $MTU)"
    ;;
  down)
    [ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
    down
    echo "emulation off; lo0 mtu $(ifconfig lo0 | awk '/mtu/ {print $NF; exit}')"
    ;;
  status)
    if [ -f "$STATE" ]; then grep PROFILE "$STATE"; else echo "off"; fi
    ;;
  *) echo "usage: sudo $0 up lan|wifi|internet|internet-clean|internet-low | sudo $0 down | $0 status" >&2; exit 2 ;;
esac
