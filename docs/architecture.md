# How it works

When you run `fsend file.pdf`, the sender opens **two paths at the same
time**: a local-network listener (announced via mDNS) and a session on
the pairing server. The receiver tries the LAN path first with a 300 ms
mDNS query; if there's no answer, it joins the server. Whichever path
completes the pairing wins, the other is torn down.

The default pairing server is `fsend.alzina.dev` — best-effort, free,
operated by the maintainer. Override with `fsend --connect host:port`,
or [run your own](self-hosting.md).

## Why a server?

Two computers on the same Wi-Fi can find each other with mDNS. Across
the internet they can't — random machines behind random NATs have no
way to find each other on their own. That's what the pairing server is
for: a shared rendezvous keyed by a one-way *slot* derived from the
share code (an argon2id stretch — both peers compute it locally and
identically).

It plays two roles, and only the first is always needed:

- **Pairing** — both sides post the slot; the server tells each one
  where the other is. It never learns the code itself (only the
  memory-hard stretch of it) and nothing about the file.
- **Relay** *(fallback only)* — when NAT topology blocks a direct
  connection, the server forwards encrypted UDP between the peers.
  Details below.

On the same LAN, neither role is needed — mDNS handles the rendezvous
and the bytes go straight over the LAN. The pairing server can be
entirely offline and same-LAN transfers still work.

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

Pairs in well under a second. Bytes never touch the pairing server or
cross NAT. The server session is cancelled the moment the LAN path
wins — it never sees the file. Works even if the pairing server is
offline.

## Different networks (sender at home, receiver at a café)

When the receiver's mDNS query finds no peer, it joins the pairing
server — where the sender is already waiting. What happens next depends
on whether the two NATs can be hole-punched.

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

The server's only job was to introduce the peers and broker the ICE
exchange. Once a hole-punched path works, bytes flow directly between
the two machines at their own bandwidth — the server never sees a byte.

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

When NAT topology defeats hole-punching (hard symmetric NAT, some
locked-down corporate networks), the pairing server forwards encrypted
UDP datagrams between the peers. TLS terminates at the peers, so the
server is moving bytes it can't decrypt.

## When the pairing server is unreachable

Same-LAN transfers continue working — the LAN path doesn't depend on the
server. The sender surfaces a one-line warning so you know cross-network
receivers can't connect right now:

```
⚠ Server unreachable — only receivers on your local network can connect.
```

You can keep transferring on the local network or use `fsend --connect
<other-host>` to point at a different server. See [Self-hosting](self-hosting.md)
to run your own.
