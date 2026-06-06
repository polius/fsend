# Multi-stage build for fsend-server. The final image is FROM scratch
# with just the static binary.
#
#   docker build -t fsend-server .
#   docker run -p 443:443/udp -p 8080:8080/tcp fsend-server

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
    -o /out/fsend-server ./cmd/fsend-server

# ---------- final ----------
FROM scratch

# CA certs for any outbound HTTPS the server might make. The server's
# own listener is plaintext HTTP — TLS terminates at the operator's
# reverse proxy.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/fsend-server /fsend-server

EXPOSE 8080/tcp
EXPOSE 443/udp

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/fsend-server", "--health-check"]

ENTRYPOINT ["/fsend-server"]
