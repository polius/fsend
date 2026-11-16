# How it works

When you send, fsend tries two paths at once:

1. **The local network.** The sender listens and announces itself over
   mDNS.
2. **The pairing server.** The sender registers there too.

The receiver asks the local network first and the server 300 ms later.
Whichever path pairs first wins.

The default server is `fsend.alzina.dev`. You can
[run your own](self-hosting.md).

## Why a server?

On one network, devices find each other over mDNS. Across the internet
they can't: NATs hide them. The server is a meeting point where both
sides get introduced.

They meet at a **slot**, a value both sides derive from the code
(argon2id, salted with the UTC day). The server matches equal slots. It
never learns the code or anything about your files
([Security](security.md#what-the-pairing-server-can-see)).

The server has two jobs:

- **Pairing:** always, for transfers between networks.
- **Relay:** only when no direct path exists. It forwards encrypted data
  it can't read.

On one network, neither is needed. Local transfers work even with the
server offline.

## Same network

```
   Sender                                              Receiver
     │  listens + announces over mDNS                     │
     │  (also registers with the server)                  │  asks over mDNS
     │ ◄──────────── found each other over mDNS ────────► │
     │ ═══════════════ direct local transfer ═══════════► │
```

- The receiver connects over **TCP** first, the fastest route on a
  local link. If TCP doesn't connect within 300 ms, it uses **QUIC**.
- Both run the same TLS 1.3 and code check.
- The data never touches the server.

If mDNS is blocked (Wi-Fi client isolation, a firewall), the devices pair
through the server instead. They can still connect directly across the
local network.

## Different networks

The receiver joins the server, where the sender is waiting. Both share
their possible addresses (ICE candidates) through it, then try to
connect.

```
   Sender               Pairing server               Receiver
     │ ── register ──────► │ ◄────────────── join ──── │
     │ ── addresses ─────► │ ◄───────── addresses ──── │
     │ ◄── peer address ── │ ── peer address ────────► │
     │                                                  │
     │ ════════ direct, through both NATs (QUIC) ═════► │   usual case
     │ ═══════► relay (encrypted) ═══════►              │   fallback
```

- **Direct:** the NATs are hole-punched, and the data goes straight
  between the two devices. The slower connection sets the speed.
- **Relay:** if no direct path is found within 5 seconds, the server
  forwards the encrypted data. Its bandwidth sets the speed.
- **Relay over TCP:** if UDP gets no answer within 2 seconds, both sides
  reach the relay over a WebSocket on HTTPS (TCP 443). This works on
  networks that allow only HTTPS, but it's slower.

The relay over TCP doesn't help when an HTTPS proxy intercepts traffic
(`E047`), the server is too old (`E042`), or the server has its relay
turned off.

For speeds on each route, see the [FAQ](faq.md#how-fast-is-it).

## If the connection drops

fsend retries in place and continues from what the receiver already has.
If that fails, both sides look for each other again. After the first
handshake they use a resume key instead of the code, so only the
original receiver can reconnect.

## If the server is down

Local transfers keep working. The sender shows:

```
! Server unavailable; retrying — until then only receivers on your local network can connect.
```

It keeps retrying in the background. To use another server:
`fsend --connect <host>`.
