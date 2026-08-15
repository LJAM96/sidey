# Sidey control-plane
# Build context: repository root
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/
COPY webassets.go ./
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 -S sidey \
    && adduser -u 10001 -S sidey -G sidey \
    && mkdir -p /var/lib/sidey/artifacts /run/sidey \
    && chown -R 10001:10001 /var/lib/sidey/artifacts /run/sidey
COPY --from=builder /out/control-plane /usr/local/bin/control-plane
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/control-plane"]
