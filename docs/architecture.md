# How it works

When you run `fsend file.pdf`, the sender opens **two paths at once**: a
local-network listener (announced over mDNS) and a session on the pairing
server. The receiver tries the LAN path first — a 300 ms mDNS query — and
falls back to the server if nothing answers. Whichever path finishes
pairing first wins; the other is torn down.

The default pairing server is `fsend.alzina.dev` — free, best-effort, run
by the maintainer. Point elsewhere with `fsend --connect host:port`, or
[run your own](self-hosting.md).

## Why a server?

Two computers on the same Wi-Fi can find each other over mDNS. Across the
internet they can't: random machines behind random NATs have no way to
reach each other on their own. The pairing server solves that — it's a
shared rendezvous point where both peers show up and get introduced.

They meet at a *slot*: a one-way value derived from the share code (an
argon2id stretch that both peers compute locally and identically). The
server matches the two slots that come out the same — and learns nothing
else.

It has two roles, and only the first always runs:

- **Pairing (always)** — both sides post their slot, and the server tells
  each one where the other is. It never sees the code itself — only the
  memory-hard stretch of it — and nothing about your files.
- **Relay (fallback only)** — if NAT topology blocks a direct connection,
  the server forwards encrypted UDP between the peers. More on that below.

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
          │                                                       │  (300 ms)
          │                                                       │
          │ ◄────────── matched via mDNS multicast ──────────────►│
          │                                                       │
          │ ═══════════ direct LAN transfer ═════════════════════►│
          │                                                       │
          ▼                                                       ▼
```

Pairing takes well under a second. The bytes never touch the server or
cross a NAT, and the server session is cancelled the instant the LAN path
wins — so it never sees the file. This works even if the pairing server
is offline.

## Different networks (sender at home, receiver at a café)

When the receiver's mDNS query finds no one, it joins the pairing server,
where the sender is already waiting. What happens next comes down to one
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
the two machines at their own bandwidth — and the server never sees one.

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
peers, so the server is shuttling bytes it can't decrypt.

## When the pairing server is unreachable

Same-LAN transfers keep working — the LAN path doesn't depend on the
server. The sender prints a one-line warning so you know cross-network
receivers can't reach you right now:

```
⚠ Server unavailable — only receivers on your local network can connect.
```

You can keep transferring locally, or point at a different server with
`fsend --connect <other-host>`. See [Self-hosting](self-hosting.md) to run
your own.
