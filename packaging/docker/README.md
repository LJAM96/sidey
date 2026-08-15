# Multi-architecture builds
# ---------------------------------------------------------------------------
# Every image must be published for linux/amd64 and linux/arm64 (plan:
# "Image publishing"). With buildx enabled:

#   docker buildx create --use
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t sidey/control-plane:<tag> -f packaging/docker/control-plane/Dockerfile .

# Build context for all Dockerfiles is the repository root.

# Images
#   sidey/control-plane    Go control plane (packaging/docker/control-plane)
#   sidey/signing-worker   Rust signing worker (packaging/docker/signing-worker)
#   sidey/device-service  Rust device service (packaging/docker/device-service)
#   sidey/virtualhere-client  VirtualHere client sidecar
#                             (packaging/docker/virtualhere-client)

# virtualhere-client
#   Proprietary third-party client (https://www.virtualhere.com). The vendor
#   binary is downloaded at build time per architecture (amd64/arm64); mirror
#   the binary into the build context instead if supply-chain pinning is
#   required. Requires a privileged container: the client loads the host's
#   vhci-hcd kernel module and creates virtual USB ports under /dev/bus/usb.
#   Host prerequisites for the VPS-only topology (D13): usbmuxd installed and
#   running on the host (apt install usbmuxd), and modprobe vhci-hcd (works
#   on a full VM with root). The free VirtualHere server is limited; a paid
#   server license is needed for full client-service access. Keep its traffic
#   on the tailnet. Auto-use state persists in the vh-client-state volume.

# Release policy
#   Production deployments use release tags or image digests, never `latest`.
#   Each release publishes: image, SBOM, source commit, dependency manifest,
#   migration version, image digest, release notes.
