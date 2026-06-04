# Multi-stage Dockerfile for fsend-server.
#
# Final image: FROM scratch with just the static binary. Distroless,
# tiny attack surface, fast cold-start.
#
# Build:
#   docker build -t fsend-server .
# Run:
#   docker run -p 443:443/udp -p 8080:8080/tcp fsend-server

# ---------- builder ----------
FROM golang:1.25-alpine AS builder

# CGO_ENABLED=0 → fully static binary that runs in FROM scratch.
ENV CGO_ENABLED=0 GOOS=linux

WORKDIR /src

# Cache go.mod / go.sum layer separately from source — this layer only
# invalidates when deps change.
COPY go.mod go.sum ./
RUN go mod download

# Source tree.
COPY . .

# Inject version metadata at build time. Caller passes --build-arg
# VERSION=0.1.0 (etc.) — defaults to "dev" if unset.
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# -trimpath + -ldflags strip paths and symbols for a reproducible,
# minimal binary.
RUN go build -trimpath \
    -ldflags="-s -w -buildid= \
      -X github.com/polius/fsend/internal/version.Version=${VERSION} \
      -X github.com/polius/fsend/internal/version.Commit=${COMMIT} \
      -X github.com/polius/fsend/internal/version.Date=${DATE}" \
    -o /out/fsend-server ./cmd/fsend-server

# ---------- final ----------
FROM scratch

# CA certs are needed for outbound HTTPS (e.g. if a future feature talks
# to GitHub). The server itself does NOT need certs for its own listener:
# TLS termination is the operator's reverse proxy's job.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /out/fsend-server /fsend-server

EXPOSE 8080/tcp
EXPOSE 443/udp

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/fsend-server", "--health-check"]

ENTRYPOINT ["/fsend-server"]
