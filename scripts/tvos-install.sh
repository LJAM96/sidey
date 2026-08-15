#!/bin/bash
# Wireless (no-USB) tvOS sign + install over a RemotePairing RSD tunnel,
# encoding the proven Phase G flow (plan.md status 2026-08-09):
#
#   1. patch the IPA for tvOS (UIDeviceFamily [3], CFBundleSupportedPlatforms
#      [TVOS], Mach-O LC_BUILD_VERSION platform 3)  - tvos-patch-ipa.py
#   2. sign with plumesign (register/device must exist; account session saved)
#   3. tunnel: RemotePairingTunnelService.connect(autopair=False) + TCP
#   4. install over the tunnel via InstallationProxy  - tvos-install.py
#   5. verify the bundle with get_apps
#
# The pair record must have been created from THIS host (it is
# network-scoped); the tunnel step needs root for the TUN device, while
# pair-verify/install run as an unprivileged user.
#
# Usage:
#   tvos-install.sh path/to/app.ipa
#
# Env: SIDEY_APPLE_ID (plumesign session), DEVICE_UDID (TV UDID),
#      DEVICE_IDENTIFIER (RemotePairing record identifier, optional =
#      DEVICE_UDID), DEVICE_IP, DEVICE_PORT (default 49152),
#      SIDEY_TVOS_DATA_DIR, VENV_PY (pmd3 venv python)
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_DIR="$REPO_DIR/scripts"
VENV_PY="${VENV_PY:-/opt/sidey/venv-pmd3/bin/python3}"

DEVICE_UDID="${DEVICE_UDID:?set DEVICE_UDID (Apple TV UDID)}"
DEVICE_IDENTIFIER="${DEVICE_IDENTIFIER:-$DEVICE_UDID}"
DEVICE_IP="${DEVICE_IP:?set DEVICE_IP (tailnet IP of the Apple TV)}"
DEVICE_PORT="${DEVICE_PORT:-49152}"
SIDEY_APPLE_ID="${SIDEY_APPLE_ID:?set SIDEY_APPLE_ID (plumesign session account)}"
WORK_DIR="${SIDEY_TVOS_DATA_DIR:-/var/lib/sidey/tvs}"
RSD_ENDPOINT_FILE="${RSD_ENDPOINT_FILE:-$WORK_DIR/tvs-endpoint}"

IPA_PATH="${1:?usage: tvos-install.sh path/to/app.ipa}"
[ -f "$IPA_PATH" ] || { echo "IPA not found: $IPA_PATH" >&2; exit 1; }
command -v plumesign >/dev/null || { echo "plumesign not on PATH" >&2; exit 1; }
[ -x "$VENV_PY" ] || { echo "pmd3 venv python not found: $VENV_PY" >&2; exit 1; }

mkdir -p "$WORK_DIR"
IO_DIR="$WORK_DIR/io-$(basename "$IPA_PATH" .ipa)-$$"

BUNDLE_ID="$("$VENV_PY" - "$IPA_PATH" <<'PY'
import plistlib, sys, zipfile
ipa = sys.argv[1]
with zipfile.ZipFile(ipa) as z:
    name = next(n for n in z.namelist() if n.endswith("/Info.plist"))
    info = plistlib.loads(z.read(name))
print(info["CFBundleIdentifier"])
PY
)"
VERSION="$("$VENV_PY" - "$IPA_PATH" <<'PY'
import plistlib, sys, zipfile
ipa = sys.argv[1]
with zipfile.ZipFile(ipa) as z:
    name = next(n for n in z.namelist() if n.endswith("/Info.plist"))
    info = plistlib.loads(z.read(name))
print(info.get("CFBundleShortVersionString", ""))
PY
)"
echo "bundle id: $BUNDLE_ID"

# 1. Patch the IPA for tvOS.
"$VENV_PY" "$SCRIPT_DIR/tvos-patch-ipa.py" "$IPA_PATH" "$IO_DIR/patched.ipa"

# 2. Sign with plumesign. The device must be registered on the team
#    (plumesign account register-device --platform tvos in Phase G); this
#    signs only, never `sign-rsd` (which fails pair-verify for pmd3 records).
export SIDEY_TVOS_IO_DIR="$IO_DIR"
plumesign sign --apple-id --udid "$DEVICE_UDID" -u "$SIDEY_APPLE_ID" \
    -p "$IO_DIR/patched.ipa" -o "$IO_DIR/signed.ipa" \
    --output-provision "$IO_DIR/embedded.mobileprovision"

# 3. Ensure the tunnel is up (systemd unit preferred, same as
#    wireless-install.sh).
TUNNEL_UNIT=sidey-tvos-tunnel.service
if systemctl list-unit-files 2>/dev/null | grep -q "$TUNNEL_UNIT"; then
    if ! systemctl is-active --quiet "$TUNNEL_UNIT"; then
        echo "Waiting for $TUNNEL_UNIT to come up..."
        systemctl start "$TUNNEL_UNIT" 2>/dev/null || true
        for _ in $(seq 1 60); do
            systemctl is-active --quiet "$TUNNEL_UNIT" && break
            sleep 5
        done
    fi
    systemctl is-active --quiet "$TUNNEL_UNIT" || { echo "Tunnel unit not active; check 'systemctl status $TUNNEL_UNIT'" >&2; exit 1; }
elif ! pgrep -f tvos-tunnel.py >/dev/null; then
    echo "Starting tvOS tunnel to $DEVICE_IP (RemotePairing daemon)..."
    setsid nohup sudo "$VENV_PY" "$SCRIPT_DIR/tvos-tunnel.py" \
        --udid "$DEVICE_IDENTIFIER" --address "$DEVICE_IP" \
        --port "$DEVICE_PORT" \
        --endpoint-file "$RSD_ENDPOINT_FILE" > /var/log/sidey-tvos-tunnel.log 2>&1 &
fi

# Wait for a live RSD listener at the endpoint.
RSD_ADDR=""; RSD_PORT=""
for _ in $(seq 1 30); do
    if [ -s "$RSD_ENDPOINT_FILE" ]; then
        read -r RSD_ADDR RSD_PORT < "$RSD_ENDPOINT_FILE" || true
    fi
    if [ -n "${RSD_ADDR:-}" ] && [ -n "${RSD_PORT:-}" ] \
        && "$VENV_PY" -c "import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), 2)" "$RSD_ADDR" "$RSD_PORT" 2>/dev/null; then
        break
    fi
    RSD_ADDR=""; RSD_PORT=""
    sleep 1
done
if [ -z "${RSD_ADDR:-}" ]; then
    echo "Tunnel did not come up; check 'systemctl status $TUNNEL_UNIT'" >&2
    exit 1
fi
echo "RSD tunnel: $RSD_ADDR:$RSD_PORT"

# 4. Install over the tunnel (unprivileged).
"$VENV_PY" "$SCRIPT_DIR/tvos-install.py" \
    --ipa "$IO_DIR/signed.ipa" \
    --bundle-identifier "$BUNDLE_ID" \
    --endpoint-file "$RSD_ENDPOINT_FILE"

# Machine-readable result lines for the Go helper to record centrally.
echo "SIDEY_BUNDLE_ID=$BUNDLE_ID"
echo "SIDEY_VERSION=$VERSION"
echo "SIDEY_SIGNED_IPA=$IO_DIR/signed.ipa"
echo "SIDEY_PROVISION=$IO_DIR/embedded.mobileprovision"
echo "Done."