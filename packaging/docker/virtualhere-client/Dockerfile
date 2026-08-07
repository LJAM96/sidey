# Sidey virtualhere-client sidecar
# Proprietary third-party client (https://www.virtualhere.com) that makes USB
# devices plugged into a VirtualHere server on the tailnet appear as local
# USB ports to the device agent's usbmuxd (D13).
#
# Vendor binary names per architecture:
#   amd64 -> vhclientx86_64, arm64 -> vhclientarm64
#
# The image needs a privileged container: the client loads the host's
# vhci-hcd kernel module and exposes virtual ports under /dev/bus/usb.
#
# Build context: repository root
FROM debian:bookworm-slim
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) BIN=vhclientx86_64 ;; \
      arm64) BIN=vhclientarm64 ;; \
      *) echo "unsupported arch: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl kmod; \
    rm -rf /var/lib/apt/lists/*; \
    curl -fsSL "https://www.virtualhere.com/sites/default/files/usbclient/${BIN}" -o /usr/local/bin/vhclient; \
    chmod +x /usr/local/bin/vhclient
COPY packaging/docker/virtualhere-client/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Client config (auto-use list) lives in /root/.vhui; persist it.
VOLUME ["/root"]
ENTRYPOINT ["/entrypoint.sh"]
