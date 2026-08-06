# Sidey control-plane
# Build context: repository root
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY cmd/control-plane/ ./
RUN CGO_ENABLED=0 go build -o /out/control-plane .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/control-plane /usr/local/bin/control-plane
USER 65534:65534
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/control-plane"]
