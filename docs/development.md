# Development

Build and test `fsend` (CLI and server are the same binary; the server
is the `fsend server` subcommand) from source.

## Prerequisites

- **Go ≥ 1.26** (matches `go.mod`).
- Optional: **Docker** + **docker compose v2**, only for the reverse-proxy
  stack in [`deploy/compose/`](../deploy/compose/).

## Build

```sh
go build -o /tmp/fsend ./cmd/fsend
```

```sh
/tmp/fsend --version
/tmp/fsend server --help
```

## Tests

```sh
go test ./...           # unit + E2E (~30s)
go test -short ./...    # skip E2E
go test -race ./...
go test -v ./test/e2e/
```

The E2E suite builds its own binary in a temp dir and runs an isolated
server on loopback — no shell wrapper needed.

## Coverage

```sh
scripts/coverage.sh
```

Runs unit and E2E tests with coverage and prints the merged total. When
invoked with `-cover`, the E2E suite builds `fsend` with
`-cover -coverpkg=./...`, so the orchestration code it exercises —
sender/receiver pairing, ICE, relay — counts toward the number instead of
reading as 0% (which is how per-package profiling reports it). Run the
script for the current total.

## LAN smoke test (no server needed)

`fsend` uses mDNS for direct peer-to-peer on the same LAN.

Terminal A (sender):
```sh
echo "hello" > /tmp/hello.txt
/tmp/fsend /tmp/hello.txt
# → abc-defg-jkm
```

Terminal B (receiver):
```sh
mkdir -p /tmp/recv && cd /tmp/recv
/tmp/fsend --yes abc-defg-jkm
```

## Run the server locally

For internet transfers (one peer behind NAT), the client needs a pairing
server. To exercise the full client↔server↔client path:

```sh
FSEND_PAIRING_ADDR=":18080" \
FSEND_RELAY_ADDR=":18443" \
FSEND_LOG_LEVEL=debug \
/tmp/fsend server
```

Ports `18080`/`18443` are chosen to avoid the privileged-port requirement
of `:443` (would need root) and the common port collision on `:8080`.

Point a client at it (this writes to your real config file; see
[Isolated config](#isolated-config) below to use a throwaway one instead):

```sh
/tmp/fsend --connect 127.0.0.1:18080
```

Reset to the public default:

```sh
/tmp/fsend --connect default
```

Health check (the env var tells the probe where to look):

```sh
FSEND_PAIRING_ADDR=":18080" /tmp/fsend server --health-check
```

### Common server env vars

| Var | Default | Notes |
|---|---|---|
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FSEND_PAIRING_ADDR` | `:8080` | Signaling listener (TCP) |
| `FSEND_RELAY_ADDR` | `:443` | Relay listener (UDP); also the STUN endpoint |
| `FSEND_RELAY_MAX_BYTES_PER_SESSION` | `1GB` | Per-session relay cap (wire bytes, post-compression). |

Full list: `/tmp/fsend server --help`. For deployment, ports, and all
tunables, see [Self-hosting](self-hosting.md).

## Isolated config

To experiment with `--connect` without overwriting your real config,
point the client at a throwaway config root:

```sh
XDG_CONFIG_HOME=/tmp/fsend-dev /tmp/fsend --connect 127.0.0.1:18080
```

Config lives in the per-OS path listed in
[Usage](usage.md#choosing-a-server---connect).
