# Development

Build and test `fsend` (CLI and server are the same binary; the server
is the `fsend server` subcommand) from source.

## Prerequisites

- **Go ≥ 1.25.11** (matches `go.mod`).
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

Runs unit and E2E tests with coverage and prints the merged total. The
E2E suite builds `fsend` with `-cover -coverpkg=./...` when `go test`
is invoked with `-cover`, so orchestration code exercised by the E2E
harness (sender/receiver pairing, ICE, relay) shows up in the number
instead of reading as 0% the way per-package profiling does. Current
total hovers around 77%.

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
FSEND_HTTP_ADDR=":18080" \
FSEND_UDP_ADDR=":18443" \
FSEND_PUBLIC_ADDR="127.0.0.1:18443" \
FSEND_LOG_LEVEL=debug \
/tmp/fsend server
```

Ports `18080`/`18443` are chosen to avoid the privileged-port requirement
of `:443` (would need root) and the common port collision on `:8080`.
`FSEND_PUBLIC_ADDR` is what the server advertises to clients for relay —
must be dialable from the client side.

Point a client at it (persists to the config file — see "Isolated config" below for the path):

```sh
/tmp/fsend --connect http://127.0.0.1:18080
```

Reset to the public default:

```sh
/tmp/fsend --connect default
```

Health check (the env var tells the probe where to look):

```sh
FSEND_HTTP_ADDR=":18080" /tmp/fsend server --health-check
```

### Common server env vars

| Var | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | Signaling listener |
| `FSEND_UDP_ADDR` | `:443` | Relay listener (UDP) |
| `FSEND_PUBLIC_ADDR` | = `FSEND_UDP_ADDR` | What clients see |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | |

Full list: `/tmp/fsend server --help`. For deployment, ports, and all
tunables, see [Self-hosting](self-hosting.md).

## Isolated config

To experiment with `--connect` without overwriting your real config,
point the client at a throwaway config root:

```sh
XDG_CONFIG_HOME=/tmp/fsend-dev /tmp/fsend --connect http://127.0.0.1:18080
```

Config lives at `~/.config/fsend/config.json` (Linux),
`~/Library/Application Support/fsend/config.json` (macOS), or
`%APPDATA%\fsend\config.json` (Windows).
