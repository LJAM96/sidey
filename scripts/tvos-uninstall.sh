#!/bin/bash
# tvOS uninstall over the RemotePairing RSD tunnel (Phase G flow mirror of
# tvos-install.sh). Removes an app on-device via InstallationProxy using the
# shared tunnel bring-up from tvos-lib.sh.
#
# Usage:
#   tvos-uninstall.sh BUNDLE_ID
#
# Env: DEVICE_IDENTIFIER (RemotePairing record identifier, optional =
#      DEVICE_UDID), DEVICE_IP, DEVICE_PORT (default 49152),
#      SIDEY_TVOS_DATA_DIR, VENV_PY (pmd3 venv python)
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_DIR="$REPO_DIR/scripts"
VENV_PY="${VENV_PY:-/opt/sidey/venv-pmd3/bin/python3}"

BUNDLE_ID="${1:?usage: tvos-uninstall.sh BUNDLE_ID}"
DEVICE_IDENTIFIER="${DEVICE_IDENTIFIER:-$DEVICE_UDID}"
DEVICE_IP="${DEVICE_IP:?set DEVICE_IP (tailnet IP of the Apple TV)}"
DEVICE_PORT="${DEVICE_PORT:-49152}"
WORK_DIR="${SIDEY_TVOS_DATA_DIR:-/var/lib/sidey/tvs}"
RSD_ENDPOINT_FILE="${RSD_ENDPOINT_FILE:-$WORK_DIR/tvs-endpoint}"

[ -x "$VENV_PY" ] || { echo "pmd3 venv python not found: $VENV_PY" >&2; exit 1; }

# Ensure the tunnel is up and wait for a live RSD listener.
. "$SCRIPT_DIR/tvos-lib.sh"
tvos_tunnel_ensure
tvos_tunnel_wait

# Uninstall over the tunnel (unprivileged).
"$VENV_PY" "$SCRIPT_DIR/tvos-uninstall.py" \
    --bundle-identifier "$BUNDLE_ID" \
    --endpoint-file "$RSD_ENDPOINT_FILE"

echo "Done."