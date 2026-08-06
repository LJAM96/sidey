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
#   sidey/device-agent     Rust device agent (packaging/docker/device-agent)

# Release policy
#   Production deployments use release tags or image digests, never `latest`.
#   Each release publishes: image, SBOM, source commit, dependency manifest,
#   migration version, image digest, release notes.
