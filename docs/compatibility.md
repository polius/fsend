# Version compatibility

Release notes for every version are on
[GitHub Releases](https://github.com/polius/fsend/releases).

## Between two devices

Both devices need the same wire protocol:

| Sender ↓ / receiver → | 1.0 – 1.5 | 1.6 – 1.15 | 1.16 and later |
|---|---|---|---|
| **1.0 – 1.5** (protocol 1) | ✅ | ❌ | ❌ |
| **1.6 – 1.15** (protocol 2) | ❌ | ✅ | ❌ |
| **1.16 and later** (protocol 3) | ❌ | ❌ | ✅ |

Within a group, every pair works. Across groups the transfer fails
before any data moves:

- **1.16+ with an older version:** they never find each other. The
  receiver gets `E002` (from 1.17, it mentions the version).
- **1.6–1.15 with 1.0–1.5:** `E034` on the newer side.

Fix: run `fsend --update` on the older device.

## With a pairing server

| Client ↓ / server → | 1.15 or older | 1.16 and later |
|---|---|---|
| **1.15 or older** | ✅ | ❌ refused (`E060`, or `E099` on old clients) |
| **1.16 and later** | ⚠️ direct works, the relay fails (`E042`) | ✅ |

- Self-hosting? Update the server together with your clients.
- A server can also refuse versions below a minimum
  (`FSEND_MIN_CLIENT_VERSION`, see [Self-hosting](self-hosting.md)).
