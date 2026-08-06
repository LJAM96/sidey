# Sidey signing-worker
# Build context: repository root
FROM rust:1.85-alpine AS builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY rust/signer/Cargo.toml ./Cargo.toml
COPY rust/signer/src/ ./src/
RUN cargo build --release

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /src/target/release/signer /usr/local/bin/signing-worker
USER 65534:65534
EXPOSE 8090
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/signing-worker"]
