# How it works

When you run `fsend file.pdf`, the sender opens **two paths at once**: a
local-network listener (announced over mDNS) and a session on the pairing
server. The receiver starts an mDNS query first, gives it a 300 ms head
start, then also contacts the server while the query keeps running (for
up to 1.5 s). Whichever path finishes pairing first wins; the other is
torn down.

The default pairing server is `fsend.alzina.dev` — free, best-effort, run
by the maintainer. Point elsewhere with `fsend --connect host:port`
(saved), `--server host` for a single run, or
[run your own](self-hosting.md).

## Why a server?

Two computers on the same Wi-Fi can find each other over mDNS. Across the
internet they can't: random machines behind random NATs have no way to
reach each other on their own. The pairing server solves that — it's a
shared rendezvous point where both peers show up and get introduced.

They meet at a *slot*: a one-way value derived from the share code (an
argon2id stretch, salted with the UTC day, that both peers compute
locally and identically). The server matches the two slots that come out
the same. It learns the slot, the peers' IP addresses and ICE
candidates, and their fsend versions — never the code or anything about
your files (see [Security](security.md#what-the-pairing-server-can-see)).

It has two roles, and only the first always runs:

- **Pairing (always, across networks)** — both sides post their slot,
  and the server tells each one where the other is. It never sees the
  code itself — only the memory-hard stretch of it — and nothing about
  your files.
- **Relay (fallback only)** — if NAT topology blocks a direct connection,
  the server forwards the encrypted QUIC packets between the peers, over
  UDP or, when UDP is blocked, over a WebSocket on its HTTPS port. More
  on that below.

On the same LAN, neither role is needed: mDNS handles the introduction
and the bytes go straight across. The pairing server can be completely
offline and same-LAN transfers still work.

## Same network (sender and receiver on the same Wi-Fi)

```
   ┌──────────────┐                                        ┌──────────────┐
   │    Sender    │                                        │   Receiver   │
   └──────┬───────┘                                        └──────┬───────┘
          │                                                       │
          │  opens LAN listener (mDNS-announced)                  │
          │  + registers with pairing server                      │
          │                                                       │  queries mDNS
          │                                                       │  (300 ms head start)
          │                                                       │
          │ ◄────────── matched via mDNS multicast ──────────────►│
          │                                                       │
          │ ═══════════ direct LAN transfer ═════════════════════►│
          │                                                       │
          ▼                                                       ▼
```

Discovery itself is RTT-scale: the query is re-sent after 100, 200 and
then every 400 ms, because Wi-Fi power saving can hold multicast for a
few hundred milliseconds. Both sides also compute the argon2id stretch
(64 MiB) before they can announce or query, which takes tens of
milliseconds on a laptop and longer on small devices. The bytes never
touch the server or cross a NAT, and the server session is torn down
once the LAN path wins — so it never sees the file. If the mDNS answer
arrives after the receiver has started on the server path, it still wins
as long as the server join hasn't completed, so this works even if the
pairing server is offline or slow.

mDNS discovery can fail even on one network: Wi-Fi client isolation, a
firewall that drops the listener's port, or macOS Local Network privacy
can block it. The receiver then pairs through the server, and ICE may
still find a direct path across the LAN.

## Different networks (sender at home, receiver at a café)

When the receiver's mDNS query hasn't found the sender within its 300 ms
head start, it joins the pairing server, where the sender is already
waiting. What happens next comes down to one
question: can the two NATs be hole-punched?

### Direct transfer (common case)

```
   ┌──────────────┐         ┌────────────────┐         ┌──────────────┐
   │    Sender    │         │ Pairing server │         │   Receiver   │
   └──────┬───────┘         └────────┬───────┘         └──────┬───────┘
          │                          │                        │
          │  ──── register ────────► │                        │
          │                          │ ◄──────── join ─────── │
          │                          │                        │
          │ ──── ICE candidates ───► │ ◄── ICE candidates ─── │
          │ ◄─── peer addresses ──── │ ──── peer addresses ─► │
          │                          │                        │
          │     both NATs hole-punched via ICE connectivity   │
          │     checks (STUN-protocol messages between peers) │
          │                          │                        │
          │ ═══════════ direct P2P over the internet ════════►│
          │                          │                        │
          ▼                          ▼                        ▼
```

The server's whole job was to introduce the peers and broker the ICE
exchange. Once a hole-punched path works, the bytes flow directly between
the two machines — and the server never sees one. Throughput is then
bounded by the slower of the two links, and also by latency and packet
loss on the path and by CPU on either end (the transfer engine has
reached about 1.1–1.3 GB/s for one 512 MiB file over loopback on an M2
Pro, as recorded in commit `8fb5210`). The sender gives ICE 5 seconds to
find a path before choosing the relay, and tells the receiver which it
chose so both sides agree.

### Relay (fallback)

```
   ┌──────────────┐         ┌────────────────┐         ┌──────────────┐
   │    Sender    │         │ Pairing server │         │   Receiver   │
   └──────┬───────┘         └────────┬───────┘         └──────┬───────┘
          │                          │                        │
          │  ──── register ────────► │                        │
          │                          │ ◄──────── join ─────── │
          │                          │                        │
          │ ──── ICE candidates ───► │ ◄── ICE candidates ─── │
          │                          │                        │
          │     hole-punch fails (hard symmetric NAT)         │
          │                          │                        │
          │ ═══════════════════════► │ ═════════════════════► │
          │       opaque UDP         │       opaque UDP       │
          │                          │                        │
          ▼                          ▼                        ▼
```

Some networks defeat hole-punching — a hard symmetric NAT, or a
locked-down corporate network. When that happens, the pairing server
forwards encrypted UDP datagrams between the peers. TLS terminates at the
peers, so the server is shuttling bytes it can't decrypt. Relayed
throughput is limited by the server's bandwidth and any per-session or
daily caps its operator set.

Before forwarding anything, the relay checks that each peer really owns
its source address: it answers the peer's first message with a cookie
sent to that address, and registers the peer only when the cookie comes
back. An allocation holds exactly two peers.

### TCP fallback (UDP blocked)

Some networks block outbound UDP entirely. If the relay doesn't answer
over UDP within 2 seconds (or sending UDP fails outright), each peer
attaches to the same relay allocation over a WebSocket on the server's
HTTPS port (`/v1/relay/ws`, TCP 443 behind the bundled Caddy). Each
WebSocket message carries one relay frame, so QUIC runs unchanged on top
and a TCP peer can pair with a UDP peer. No extra port is needed.

This gets fsend through networks that allow only HTTPS. It doesn't help
when:

- **HTTPS is intercepted** by a proxy or captive portal whose certificate
  your system doesn't trust. Signaling fails first: the receiver stops
  with `E047`, and the sender warns that the server's certificate isn't
  trusted.
- **The server is too old** to offer the fallback, or the WebSocket
  connection fails. The transfer stops with `E042`.
- **The server runs in pairing-only mode** (`FSEND_RELAY_ENABLED=false`).
  There is no relay to fall back to.

Carrying QUIC over TCP adds head-of-line blocking, so a transfer over
the fallback can be slower than over the UDP relay. `fsend doctor` checks both
the UDP relay and the TCP fallback.

## If the connection drops

A transfer that drops (network change, laptop sleep) retries in place
and resumes from what the receiver already wrote. If that doesn't
recover, the receiver re-runs discovery up to twice, and the sender goes
back to "Waiting for receiver". From the first successful handshake
onwards, both sides pair on a resume key derived from their session
secret instead of the code, so only the original receiver device can
reconnect. See [Security → Share codes](security.md#share-codes).

## When the pairing server is unreachable

Same-LAN transfers keep working — the LAN path doesn't depend on the
server. The sender prints a one-line warning so you know cross-network
receivers can't reach you right now:

```
⚠ Server unavailable; retrying — until then only receivers on your local network can connect.
```

The sender keeps retrying the server in the background. You can keep
transferring locally, or point at a different server with
`fsend --connect <other-host>` (or `--server <other-host>` for one run). See [Self-hosting](self-hosting.md) to run
your own.
