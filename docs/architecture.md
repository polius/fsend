# How it works

When you run `fsend file.pdf`, the sender opens **two paths at the same
time** — a LAN listener (mDNS-announced) and a session on the pairing
server. Whichever path the receiver reaches first wins; the other is
cancelled. There is **no timeout** between the two — neither side ever
waits on a deadline.

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

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener (no one comes)          ├─ mDNS query (300 ms, miss)
     │                                       │
     └─ Server register  ──────────► ◄────── └─ Join server
                                                       │
                                                       ▼
                                            ICE hole-punch (common case)
                                            ─── or, on hard NAT ───
                                            UDP relay (encrypted, opaque)
                                            (LAN path cancelled)
```

When the two NATs can be punched through, bytes flow directly between
the two peers. When NAT topology makes hole-punching impossible (hard
symmetric NAT, some locked-down corporate networks), the pairing
server forwards encrypted UDP datagrams between the peers — it never
sees plaintext, since TLS terminates at the peers, not at the server.

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
