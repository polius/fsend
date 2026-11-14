# Version compatibility

Which fsend versions can transfer to each other, and which can use which
pairing server. Release notes for every version are on
[GitHub Releases](https://github.com/polius/fsend/releases); the
[changelog](../CHANGELOG.md) lists the breaking ones.

## Between two devices

Two devices transfer only if they speak the same wire protocol. The
protocol changed twice in 1.x, so releases fall into three groups:

| Sender ↓ / receiver → | 1.0 – 1.5 | 1.6 – 1.15 | 1.16 and later |
|---|---|---|---|
| **1.0 – 1.5** (protocol 1) | ✅ | ❌ | ❌ |
| **1.6 – 1.15** (protocol 2) | ❌ | ✅ | ❌ |
| **1.16 and later** (protocol 3) | ❌ | ❌ | ✅ |

Within a group every pair works: newer features are negotiated, and a
side that lacks one falls back to what both support.

Across groups the transfer fails before any file data moves, and the
error doesn't say "version":

- **1.16 and later with anything older:** the two sides look for each
  other in different places (1.16 salts codes with the day), so they
  never meet. The receiver gets `E002` ("code not found"). Through a 1.16
  or later server, the older side is refused first (see below).
- **1.6 – 1.15 with 1.0 – 1.5:** the two sides connect, then the first
  message fails the version check with `E034`.

Either way, run `fsend --update` on the older device.

## With a pairing server

| Client ↓ / server → | 1.15 or older | 1.16 and later |
|---|---|---|
| **1.15 or older** | ✅ (between clients of one group, above) | ❌ HTTP 426 |
| **1.16 and later** | ⚠️ pairs and transfers directly; the relay fails (`E042`) | ✅ |

- A 1.16 or later server refuses clients older than 1.16 with HTTP 426.
  Clients from 1.16 show it as `E060`. Older clients print the server's
  message as they got it ("fsend client too old for this server…"),
  under `E099`.
- A server's operator can also refuse releases below a set version with
  `FSEND_MIN_CLIENT_VERSION` (see [Self-hosting](self-hosting.md)). The
  answer is the same 426.
- If you self-host, update the server along with your clients: clients
  from 1.16 can't register with an older server's relay, so networks
  without a direct path fail.

The 1.15 and 1.16 rows were checked with 1.15.1 and 1.16 binaries, on
loopback, through each server and over the local network. The 1.0 – 1.5
row comes from the code: releases before 1.16 accept only their own
protocol version.
