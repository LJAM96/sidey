#!/bin/bash
# Wireless (no-USB) SideStore re-sign + install via the RemotePairing RSD tunnel.
#
# Prereqs (one-time, over USB):
#   pymobiledevice3 lockdown wifi-connections --state on
#   pymobiledevice3 lockdown remotepairing --pair
#
# Usage:
#   wireless-install.sh [path/to/app.ipa] [--device-udid UDID] [--device-ip IP]
#
# Env: SIDEY_APPLE_ID / SIDEY_APPLE_MAIN_PASSWORD (sidey-creds.sh), ANISETTE_URL
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ISIDELOAD_DIR="$REPO_DIR/third_party/isideload"
TUNNEL_SCRIPT="$REPO_DIR/scripts/wireless-tunnel.py"
VENV_PY=/opt/sidey/venv-pmd3/bin/python3

DEVICE_UDID="${DEVICE_UDID:-00008120-001E11211184C01E}"
DEVICE_IP="${DEVICE_IP:-100.110.172.118}"          # phone tailnet IP
RSD_PORT_FILE=/tmp/opencode/rsd-endpoint.txt

IPA_PATH="${1:-/tmp/opencode/SideStore.ipa}"
[ -f "$IPA_PATH" ] || { echo "IPA not found: $IPA_PATH" >&2; exit 1; }

# 1. Ensure the wireless tunnel is up.
if ! pgrep -f wireless-tunnel.py >/dev/null; then
    echo "Starting wireless tunnel to $DEVICE_IP (RemotePairing daemon)..."
    setsid nohup sudo "$VENV_PY" "$TUNNEL_SCRIPT" \
        --udid "$DEVICE_UDID" --address "$DEVICE_IP" > /tmp/opencode/wireless-tunnel.log 2>&1 &
fi

RSD_ADDR=""
RSD_PORT=""
for _ in $(seq 1 30); do
    read -r RSD_ADDR RSD_PORT < /tmp/opencode/wireless-tunnel.log 2>/dev/null || true
    [ -n "${RSD_ADDR:-}" ] && [ -n "${RSD_PORT:-}" ] && break
    sleep 1
done
if [ -z "${RSD_ADDR:-}" ]; then
    echo "Tunnel did not come up; see /tmp/opencode/wireless-tunnel.log" >&2
    exit 1
fi
echo "RSD tunnel: $RSD_ADDR:$RSD_PORT"

# 2. Build the wireless install binary if missing.
BIN="$ISIDELOAD_DIR/target/release/wireless"
if [ ! -x "$BIN" ]; then
    echo "Building wireless install binary..."
    source ~/.cargo/env
    (cd "$ISIDELOAD_DIR" && cargo build --release -p wireless)
fi

# 3. Sign + install over the tunnel.
echo "Installing $(basename "$IPA_PATH") over RSD tunnel..."
sudo bash -c "source /usr/local/sbin/sidey-creds.sh && \
    env ANISETTE_URL='${ANISETTE_URL:-http://127.0.0.1:6970}' \
        RSD_ADDR='$RSD_ADDR' RSD_PORT='$RSD_PORT' \
        DEVICE_UDID='$DEVICE_UDID' \
        '$BIN' \"\$SIDEY_APPLE_ID\" \"\$SIDEY_APPLE_MAIN_PASSWORD\" '$IPA_PATH'"
echo "Done."
