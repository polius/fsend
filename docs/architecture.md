# How it works

When you run `fsend file.pdf`, the sender opens **two paths at the same
time** — a LAN listener (mDNS-announced) and a session on the pairing
server. Whichever path the receiver reaches first wins; the other is
cancelled. There is **no timeout** between the two — neither side ever
waits on a deadline.

## Why a server?

Two computers on the same Wi-Fi can find each other with mDNS. Across
the internet they can't — random machines behind random NATs have no
way to find each other on their own. That's what the pairing server is
for: a shared rendezvous keyed by the transfer code.

It plays two roles, and only the first is always needed:

- **Pairing** — both sides post the code; the server tells each one
  where the other is. The server learns the code (it has to, to match
  the sides) but nothing about the file.
- **Relay** *(fallback only)* — when NAT topology blocks a direct
  connection, the server forwards encrypted UDP between the peers. TLS
  terminates at the peers, so the server is moving bytes it can't
  decrypt.

On the same LAN, neither role is needed — mDNS handles the rendezvous
and the bytes go straight over the LAN. The pairing server can be
entirely offline and same-LAN transfers still work.

## Same network (sender and receiver on the same Wi-Fi)

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener  ──────────────►  ◄──── mDNS query (300 ms)
     │                                       │
     └─ Server register (standby)            └─ HIT → dial LAN port
                                                       │
                                                       ▼
                                          direct P2P over LAN
                                          (server path cancelled)
```

Pairs in well under a second. Bytes never touch the pairing server or
cross NAT. Works even if the pairing server is offline.

## Different networks (sender at home, receiver at a café)

When the LAN path finds no peer, both sides join the pairing server.
What happens next depends on whether the two NATs can be hole-punched.

### Direct transfer (common case)

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener (no one comes)          ├─ mDNS query (300 ms, miss)
     │                                       │
     └─ Server register  ──────────► ◄────── └─ Join server
                                  │
                                  ▼
                       ICE exchanges candidates
                       both NATs hole-punched
                                  │
                                  ▼
                       Sender ◄──────► Receiver
                       direct P2P over internet
                       (server only signaled)
```

The server's only job was to introduce the peers. Once ICE finds a
usable path, bytes flow directly between the two machines at their own
bandwidth.

### Relay (fallback)

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener (no one comes)          ├─ mDNS query (300 ms, miss)
     │                                       │
     └─ Server register  ──────────► ◄────── └─ Join server
                                  │
                                  ▼
                       ICE candidates exchanged
                       hole-punch fails (hard NAT)
                                  │
                                  ▼
                   Sender ──► Server ──► Receiver
                   encrypted UDP forwarded
                   (server never sees plaintext)
```

When NAT topology defeats hole-punching (hard symmetric NAT, some
locked-down corporate networks), the pairing server forwards encrypted
UDP datagrams between the peers. TLS terminates at the peers, so the
server is moving bytes it can't decrypt.

## Why this design

The two paths run concurrently rather than sequentially because waiting
is always wrong for one of the two cases: same-network users would
needlessly wait before the cross-network path even starts, and
cross-network users would be blocked behind a same-network attempt that
will never succeed. Running both at once gives you the fastest answer
either way.

## When the pairing server is unreachable

Same-LAN transfers continue working — the LAN path doesn't depend on the
server. The sender surfaces a one-line warning so you know cross-network
receivers can't connect right now:

```
⚠ Server unreachable — only same-LAN receivers can connect.
```

You can keep transferring on the local network or use `fsend --connect
<other-host>` to point at a different server. See [Self-hosting](self-hosting.md)
to run your own.
