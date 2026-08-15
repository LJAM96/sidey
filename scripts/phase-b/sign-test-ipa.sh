#!/bin/bash
# Phase B / D6 signed test IPA - the "Impactor run" in headless form.
#
# Signs a test IPA for a specific device using the isideload signonly binary
# (the Phase F headless signing worker) instead of the Impactor GUI, so Phase B
# device transport tests have a signing path before the server workers exist.
#
# Usage:
#   sign-test-ipa.sh [input.ipa] [output.ipa]
#
# Env:
#   SIDEY_APPLE_ID / SIDEY_APPLE_MAIN_PASSWORD  Apple credentials (sidey-creds.sh)
#   DEVICE_UDID         target device UDID (must be registered with the team)
#   DEVICE_NAME         target device display name
#   DEVICE_TYPE         ios|tvos|watchos (default ios)
#   MACHINE_NAME        certificate identity name (default isideload-minimal)
#   SIDEY_ISIDELOAD_STATE  isideload state dir (default /var/lib/sidey/isideload)
#   ANISETTE_URL         anisette v3 provider (default http://127.0.0.1:6970)
#   SIGNONLY_2FA_CODE_FILE path to a file the operator drops the 2FA code into
#                          (default /tmp/opencode/2fa-code.txt)
#
# On success prints a single JSON line (status=ok, profile_expiry_at, cert_serial
# and team_id) to stdout; on failure prints {"status":"error",...} and exits 1.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ISIDELOAD_DIR="$REPO_DIR/third_party/isideload"
SIGNONLY_BIN="${SIGNONLY_BIN:-$ISIDELOAD_DIR/target/release/signonly}"

INPUT_IPA="${1:-/tmp/opencode/test.ipa}"
OUTPUT_IPA="${2:-$PWD/test-signed.ipa}"

: "${DEVICE_UDID:?DEVICE_UDID is required: signing without a target device cannot activate the app}"
[ -f "$INPUT_IPA" ] || { echo "input IPA not found: $INPUT_IPA" >&2; exit 1; }

# Build the headless signer once (mirrors wireless-install.sh).
if [ ! -x "$SIGNONLY_BIN" ]; then
    echo "building signonly..." >&2
    source ~/.cargo/env 2>/dev/null || true
    (cd "$ISIDELOAD_DIR" && cargo build --release -p signonly)
fi

echo "signing for $DEVICE_UDID (${DEVICE_TYPE:-ios})..." >&2
# sudo resets the environment; pass every variable the binary reads explicitly
# (same pattern as wireless-install.sh). The password travels via env, never argv.
sudo bash -c "source /usr/local/sbin/sidey-creds.sh && \
    env SIDEY_APPLE_MAIN_PASSWORD=\"\$SIDEY_APPLE_MAIN_PASSWORD\" \
        SIDEY_ISIDELOAD_STATE='${SIDEY_ISIDELOAD_STATE:-/var/lib/sidey/isideload}' \
        ANISETTE_URL='${ANISETTE_URL:-http://127.0.0.1:6970}' \
        DEVICE_UDID='$DEVICE_UDID' \
        DEVICE_NAME='${DEVICE_NAME:-ACU Covert Camera}' \
        DEVICE_TYPE='${DEVICE_TYPE:-ios}' \
        MACHINE_NAME='${MACHINE_NAME:-isideload-minimal}' \
        SIGNONLY_2FA_CODE_FILE='${SIGNONLY_2FA_CODE_FILE:-/tmp/opencode/2fa-code.txt}' \
        '$SIGNONLY_BIN' \"\$SIDEY_APPLE_ID\" '$INPUT_IPA' '$OUTPUT_IPA'"
echo "signed test IPA: $OUTPUT_IPA" >&2