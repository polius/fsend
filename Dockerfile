# Pinned by digest; Dependabot (docker ecosystem) keeps tag and digest current.
# Builds on the runner's native platform and cross-compiles, so multi-arch
# images don't run the Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder
ARG TARGETOS
ARG TARGETARCH
# Injected by docker.yml so released images don't report "fsend dev".
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w \
    -X github.com/polius/fsend/internal/version.Version=${VERSION} \
    -X github.com/polius/fsend/internal/version.Commit=${COMMIT} \
    -X github.com/polius/fsend/internal/version.Date=${DATE}" \
    -o /out/fsend ./cmd/fsend \
 && mkdir /out/state

FROM scratch
# CA bundle so the image also works as a client (sending/receiving needs
# to verify the HTTPS pairing server) — server mode never dials out.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/fsend /fsend
# Writable home for FSEND_STATE_FILE. A fresh named volume mounted here
# inherits this owner.
COPY --from=builder --chown=65532:65532 /out/state /state
# Non-root. Binding UDP/443 relies on net.ipv4.ip_unprivileged_port_start=0,
# Docker's default for bridge-networked containers (compose sets it too).
USER 65532:65532
ENTRYPOINT ["/fsend"]
# Liveness: probe the local /health (legacy combined probe). The binary
# self-invokes with --health-check, so no shell/curl is needed on scratch.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/fsend", "server", "--health-check"]
