# Sidey device-agent
# Build context: repository root
FROM rust:1.85-alpine AS builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY rust/device-agent/Cargo.toml ./Cargo.toml
COPY rust/device-agent/src/ ./src/
RUN cargo build --release

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /src/target/release/device-agent /usr/local/bin/device-agent
USER 65534:65534
EXPOSE 8091
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8091/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/device-agent"]
