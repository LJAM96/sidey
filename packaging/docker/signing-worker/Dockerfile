# Sidey signing-worker (Phase F)
# Build context: repository root
#
# Stage 1: the isideload signonly binary (Rust, headless signing).
FROM rust:1.97-alpine AS signonly-builder
RUN apk add --no-cache musl-dev openssl-dev pkgconfig
WORKDIR /src
COPY third_party/isideload/Cargo.toml ./Cargo.toml
COPY third_party/isideload/Cargo.lock ./Cargo.lock
COPY third_party/isideload/isideload ./isideload
COPY third_party/isideload/examples ./examples
RUN cargo build --release -p signonly -p p12export

# Stage 2: the signing worker service (Go).
FROM golang:1.24-alpine AS worker-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -o /out/signing-worker ./cmd/signing-worker

# Stage 3: runtime. Needs openssl + ca-certificates for Apple TLS and anisette.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates openssl \
    && addgroup -g 10001 -S sidey \
    && adduser -u 10001 -S sidey -G sidey \
    && mkdir -p /var/lib/sidey/signing-worker /run/sidey /run/sidey/signing-state \
    && chown -R 10001:10001 /var/lib/sidey/signing-worker /run/sidey
COPY --from=signonly-builder /src/target/release/signonly /usr/local/bin/signonly
COPY --from=signonly-builder /src/target/release/p12export /usr/local/bin/p12export
COPY --from=worker-builder /out/signing-worker /usr/local/bin/signing-worker
USER 10001:10001
EXPOSE 8090
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/signing-worker"]
