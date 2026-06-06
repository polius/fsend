# Development

How to build and test `fsend` and `fsend-server` from the current source
tree on your local machine — no release binaries, no Docker, just `go
build` from whatever's checked out right now.

## Prerequisites

- **Go ≥ 1.25.5** (matches `go.mod`). Check with `go version`.
- A POSIX shell. The E2E harness (`test/e2e/run_e2e.sh`) is bash.
- That's it. No CGo, no system libraries.

Optional:
- **Docker** + **docker compose v2**, only if you want to test the
  reverse-proxy stack in `deploy/compose/`. For ordinary dev you don't
  need it — you can run `fsend-server` as a plain process.

## 1 · Build from source

From the repo root:

```sh
go build -o /tmp/fsend        ./cmd/fsend
go build -o /tmp/fsend-server ./cmd/fsend-server
```

Both binaries link statically (pure Go) and report `version dev` —
`internal/version.Version` is only overridden by the release build via
`-ldflags`. That's fine for development; nothing in the protocol cares
about the version string.

Quick sanity check:

```sh
/tmp/fsend        --version    # → fsend dev (build unknown, unknown)
/tmp/fsend-server --version
```

Putting the binaries in `/tmp/` matches what `test/e2e/run_e2e.sh`
expects by default, so the same builds drive both manual testing and the
E2E suite. Pick a different prefix if you prefer — see the env vars
below.

> Tip: while iterating, `go install ./cmd/fsend ./cmd/fsend-server` puts
> them in `$GOBIN` (or `$GOPATH/bin`) on your `PATH`, so you can just
> type `fsend` instead of `/tmp/fsend`. Use whichever workflow you
> prefer.

## 2 · Test it works without a server (LAN only)

`fsend` does direct peer-to-peer over the LAN via mDNS — **no rendezvous
server is needed when both peers are on the same machine or LAN**. This
is the fastest smoke test of the binary you just built.

Terminal A (sender):
```sh
echo "hello from dev build" > /tmp/hello.txt
/tmp/fsend /tmp/hello.txt
# prints a code like:  abc-defgh-jkm
```

Terminal B (receiver):
```sh
mkdir -p /tmp/recv && cd /tmp/recv
/tmp/fsend --yes abc-defgh-jkm
cat hello.txt
```

If that works, your build is functional. No server, no internet, no
config.

## 3 · Run `fsend-server` locally

For internet transfers (one peer behind NAT, etc.) `fsend` needs a
rendezvous server. To test changes to server code, or to test the full
client↔server↔client path locally, run the server you just built:

```sh
FSEND_HTTP_ADDR=":18080" \
FSEND_UDP_ADDR=":18443" \
FSEND_PUBLIC_ADDR="127.0.0.1:18443" \
FSEND_LOG_LEVEL=debug \
/tmp/fsend-server
```

Why non-standard ports: `:443` requires root on Linux/macOS, and `:8080`
often collides with other dev services. `18080`/`18443` are arbitrary;
pick whatever's free.

Why `FSEND_PUBLIC_ADDR`: the server tells clients what UDP address to
dial for relay. The default value would advertise the bind address
verbatim (`:18443`), which clients can't dial. On a dev machine,
`127.0.0.1:18443` is what you want.

In another terminal, point your clients at it (once per machine — it's
persisted to `~/.config/fsend/config.toml`):

```sh
/tmp/fsend --connect http://127.0.0.1:18080
# → confirms the new server and writes the config
```

Then send/receive as in §2. To go back to the public default later:

```sh
/tmp/fsend --connect default
```

Verify the server is alive at any time:

```sh
FSEND_HTTP_ADDR=":18080" /tmp/fsend-server --health-check
echo $?    # 0 means healthy
```

### Useful server env vars

All optional; defaults shown in parentheses.

| Var | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | Signaling listener |
| `FSEND_UDP_ADDR` | `:443` | Relay listener (UDP) |
| `FSEND_PUBLIC_ADDR` | = `FSEND_UDP_ADDR` | What clients see — override for dev |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | `500m`, `1GiB`, etc. |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | Go duration |

Run `/tmp/fsend-server --help` for the full list.

## 4 · Iterate on changes

The rebuild loop is just `go build` again. There's no codegen, no
generated assets:

```sh
go build -o /tmp/fsend ./cmd/fsend && /tmp/fsend --version
```

If you want a one-liner that rebuilds and re-runs while you edit:

```sh
# requires `entr` (brew install entr)
find cmd internal -name '*.go' | entr -r sh -c \
  'go build -o /tmp/fsend-server ./cmd/fsend-server && \
   FSEND_HTTP_ADDR=:18080 FSEND_UDP_ADDR=:18443 \
   FSEND_PUBLIC_ADDR=127.0.0.1:18443 FSEND_LOG_LEVEL=debug \
   /tmp/fsend-server'
```

## 5 · Tests

### Unit tests

```sh
go test ./...
```

Fast (<10s) and covers every internal package.

### End-to-end suite

`test/e2e/run_e2e.sh` brings up a fresh `fsend-server`, drives the
client through every dispatch path, and asserts byte-for-byte transfers.
It expects the binaries to already exist at `/tmp/fsend` and
`/tmp/fsend-server`, so build first:

```sh
go build -o /tmp/fsend        ./cmd/fsend
go build -o /tmp/fsend-server ./cmd/fsend-server
./test/e2e/run_e2e.sh
```

Override paths or ports via env vars (`FSEND`, `FSEND_SERVER`,
`HTTP_PORT`, `UDP_PORT`, `WORKDIR`). The harness preserves `$WORKDIR`
on failure so you can inspect logs.

### Race detector / verbose runs

```sh
go test -race ./...
go test -v -run TestSomething ./internal/somepkg
```

## 6 · Config locations

Your dev `fsend` writes config to the standard XDG location:

- macOS / Linux: `~/.config/fsend/config.toml`
- Windows: `%APPDATA%\fsend\config.toml`

`--connect default` resets it. To run an isolated client that doesn't
touch your real config (the E2E harness does this):

```sh
XDG_CONFIG_HOME=/tmp/fsend-dev-xdg /tmp/fsend --connect http://127.0.0.1:18080
```

## 7 · Cleaning up

```sh
rm /tmp/fsend /tmp/fsend-server
rm -rf /tmp/fsend-e2e-*           # leftover E2E workdirs (if any)
go clean -cache -testcache        # only if you want to force-rebuild deps
```
