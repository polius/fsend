# Multi-stage build for the fsend binary. The final image is FROM scratch
# with just the static binary; ENTRYPOINT is `fsend` so the image stays
# usable both as the CLI and (with `command: ["server"]` in compose, or
# `docker run ... fsend server`) as the rendezvous + relay daemon.
#
#   # Run as server:
#   docker build -t fsend .
#   docker run -p 443:443/udp -p 8080:8080/tcp fsend server
#
#   # Run as CLI:
#   docker run --rm fsend --version

# ---------- builder ----------
FROM golang:1.25-alpine AS builder

# Fully static binary so it can run in FROM scratch (no libc).
ENV CGO_ENABLED=0 GOOS=linux

WORKDIR /src

# Separate layer for deps — only invalidates when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version metadata injected at build time via --build-arg.
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# -trimpath + -s -w: reproducible build with stripped symbols.
RUN go build -trimpath \
    -ldflags="-s -w -buildid= \
      -X github.com/polius/fsend/internal/version.Version=${VERSION} \
      -X github.com/polius/fsend/internal/version.Commit=${COMMIT} \
      -X github.com/polius/fsend/internal/version.Date=${DATE}" \
    -o /out/fsend ./cmd/fsend

# ---------- final ----------
FROM scratch

# CA certs for any outbound HTTPS the server might make. The server's
# own listener is plaintext HTTP — TLS terminates at the operator's
# reverse proxy.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/fsend /fsend

# Ports documented here are server-mode only; the CLI doesn't bind any.
EXPOSE 8080/tcp
EXPOSE 443/udp

# HEALTHCHECK lives in deploy/compose/docker-compose.yml — only the
# operator running this image as a server can meaningfully define
# "healthy". Hard-coding it here would mark CLI invocations unhealthy.

ENTRYPOINT ["/fsend"]
