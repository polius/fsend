FROM golang:1.25-alpine AS builder
ENV CGO_ENABLED=0
WORKDIR /src
COPY . .
RUN go build -o /out/fsend ./cmd/fsend

FROM scratch
COPY --from=builder /out/fsend /fsend
ENTRYPOINT ["/fsend"]
