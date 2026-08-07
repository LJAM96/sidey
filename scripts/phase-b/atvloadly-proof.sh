#!/usr/bin/env bash
# Phase B Apple TV proof via atvloadly (AGPL-3.0, reused as-is for the tvOS
# pairing/installation behaviour only, decision D2).
#
# Runs the atvloadly container on the AVRos Avahi + DBus sockets (required
# for Apple TV discovery and pairing) and captures everything to
# results/phase-b/atvloadly-<timestamp>/. Use the web UI to pair the Apple TV,
# enrol the Apple account, upload a tvOS IPA and install it once; the
# transcript is reaped from the captured logs so Phase B compatibility
# results can record the tvOS outcome.
#
# The atvloadly web UI must be reached on the host port (default 8080). It
# persists its own SQLite state in a named volume.
#
# Env:
#   ATVLOADLY_IMAGE   REQUIRED: container image for the pinned atvloadly commit
#                     (third_party/versions.lock: bitxeno/atvloadly @ df20195, v0.4.6;
#                     build it with deploy/compose or pull the vendor image)
#   ATVLOADLY_PORT    host web port (default 8080)
#   ATVLOADLY_DATA    named volume for its persistence (default atv-loadly-data)
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMG="${ATVLOADLY_IMAGE:?set ATVLOADLY_IMAGE (atvloadly @ df201956449635815d1d816d0eaf20c4baf4f9e6, v0.4.6 - see third_party/versions.lock)}"
PORT="${ATVLOADLY_PORT:-8080}"
VOL="${ATVLOADLY_DATA:-atvloadly-data}"
TIMESTAMP="$(date -u +"%Y%m%dT%H%M%SZ")"
OUT_DIR="${SIDEY_RESULTS_DIR:-$REPO_DIR/results/phase-b/atvloadly-$TIMESTAMP}"
mkdir -p "$OUT_DIR"

echo "== atvloadly Apple TV proof =="
echo "image: $IMG"
echo "web:   http://$(hostname -I | awk '{print $1}')${PORT:+:}$PORT  (open in a browser)"
echo "data volume: $VOL"
echo "logs: $OUT_DIR"

# Avahi and DBus host sockets the container needs for discovery / pairing.
[ -S /run/avahi-daemon/socket ] || echo "WARN: /run/avahi-daemon/socket not present - Apple TV discovery may fail" >&2
[ -S /run/dbus/system_bus_socket ] || echo "WARN: /run/dbus/system_bus_socket not present - dbus required" >&2

RUNTIME="${RUNTIME:-docker}"
command -v "$RUNTIME" >/dev/null || { echo "container runtime missing: $RUNTIME" >&2; exit 1; }

# Pre-create the state volume so docker does not grant it unexpected perms.
"$RUNTIME" volume inspect "$VOL" >/dev/null 2>&1 || "$RUNTIME" volume create "$VOL"

# Start, then tail the container logs to the capture file while the operator
# runs through: 1) pair the Apple TV, 2) enrol the Apple account (2FA if any),
# 3) upload the test tvOS IPA, 4) install. Host networking is required so the
# container sees the mDNS/Avahi multicast used for Apple TV discovery.
CONTAINER="atvloadly-proof-$$"
"$RUNTIME" run -d --name "$CONTAINER" \
  --network host \
  -v "$VOL":/app/data \
  -v /run/avahi-daemon/socket:/run/avahi-daemon/socket \
  -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket \
  "${IMG}" > /dev/null

trap '"$RUNTIME" rm -f "$CONTAINER" >/dev/null 2>&1 || true' EXIT

echo "container: $CONTAINER"
echo "Follow the web UI: http://127.0.0.1:8080/ (or your machine IP)"
echo "Capture logs: \"$RUNTIME\" logs -f \"$CONTAINER\" | tee \"$OUT_DIR/container.log\" &"
echo
echo "When done, stop this script; the captured log becomes the Phase B tvOS record."