FROM golang:1.25-alpine AS builder
# Injected by docker.yml so released images don't report "fsend dev".
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ENV CGO_ENABLED=0
WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags "-s -w \
    -X github.com/polius/fsend/internal/version.Version=${VERSION} \
    -X github.com/polius/fsend/internal/version.Commit=${COMMIT} \
    -X github.com/polius/fsend/internal/version.Date=${DATE}" \
    -o /out/fsend ./cmd/fsend

FROM scratch
# CA bundle so the image also works as a client (sending/receiving needs
# to verify the HTTPS pairing server) — server mode never dials out.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/fsend /fsend
ENTRYPOINT ["/fsend"]
