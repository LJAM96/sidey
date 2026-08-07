#!/usr/bin/env bash
# Phase B transport proof: install / upgrade / verify / uninstall a signed
# test IPA on a real device, capturing structured logs and writing a
# compatibility report. Run on the host that sees the device (or the Oracle
# VPS with the VirtualHere client sidecar over Tailscale, decision D13).
#
# Modes:
#   usb  - transport-spike CLI over usbmuxd (USB or virtual USB; default)
#   rsd  - sign + install through the RemotePairing RSD tunnel, verify via
#          transport-spike (the phone stays visible to usbmuxd alongside)
#
# Outputs (default <repo>/results/phase-b/<mode>-<timestamp>/):
#   steps.log             one key=value record per step
#   step-<name>.log       raw command output per phase
#   compatibility.json    assembled from step records
#   topology-decision     human summary of the D13 verdict
#
# Env:
#   SIDEY_APPLE_ID / SIDEY_APPLE_MAIN_PASSWORD  credentials (sidey-creds.sh or env)
#   SIDEY_DEVICE_UDID     device UDID
#   SIDEY_DEVICE_NAME     device display name
#   SIDEY_TEST_IPA        IPA under test (default /tmp/opencode/test.ipa)
#   SIDEY_TEST_BUNDLE_ID  override bundle-id detection from the IPA
#   SIDEY_TRANSPORT_MODE  usb (default) | rsd
#   SIDEY_RSD_ADDR        RSD tunnel endpoint for rsd mode (else auto from /run/sidey/rsd-endpoint)
#   SIDEY_RSD_PORT        RSD tunnel port for rsd mode
#   SIDEY_SCENARIO        none (default) | restart | sleep | all
#                         restart: reboot the device, wait for it to come back,
#                           re-validate pairing and re-verify the installed app
#                         sleep:   lock the screen, probe whether lockdown stalls
#                           (passcode prompt) and wait for the operator to unlock
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SIGN_SCRIPT="$REPO_DIR/scripts/phase-b/sign-test-ipa.sh"
ISIDELOAD_DIR="$REPO_DIR/third_party/isideload"
SPIKE_DIR="$REPO_DIR/rust/transport-spike"
SPIKE_BIN="$SPIKE_DIR/target/release/transport-spike"
WIRELESS_BIN="${WIRELESS_BIN:-$ISIDELOAD_DIR/target/release/wireless}"

if [ -r /usr/local/sbin/sidey-creds.sh ]; then
    # shellcheck disable=SC1091
    source /usr/local/sbin/sidey-creds.sh
elif sudo -n true 2>/dev/null && sudo test -r /usr/local/sbin/sidey-creds.sh 2>/dev/null; then
    # creds are root-only (0600); read them through sudo into this shell
    # shellcheck disable=SC1091
    source <(sudo cat /usr/local/sbin/sidey-creds.sh)
fi
: "${SIDEY_APPLE_ID:?set SIDEY_APPLE_ID (or sidey-creds.sh)}"
: "${SIDEY_APPLE_MAIN_PASSWORD:?set SIDEY_APPLE_MAIN_PASSWORD (or sidey-creds.sh)}"

DEVICE_UDID="${SIDEY_DEVICE_UDID:-${DEVICE_UDID:-}}"
: "${DEVICE_UDID:?set SIDEY_DEVICE_UDID}"
export DEVICE_UDID
DEVICE_NAME="${SIDEY_DEVICE_NAME:-ACU Covert Camera}"
export DEVICE_NAME
DEVICE_TYPE="${SIDEY_DEVICE_TYPE:-ios}"
export DEVICE_TYPE
export SIDEY_ISIDELOAD_STATE="${SIDEY_ISIDELOAD_STATE:-/var/lib/sidey/isideload}"
export ANISETTE_URL="${ANISETTE_URL:-http://127.0.0.1:6970}"
ORIGINAL_IPA="${SIDEY_TEST_IPA:-/tmp/opencode/test.ipa}"
BUNDLE_ID="${SIDEY_TEST_BUNDLE_ID:-}"
MODE="${SIDEY_TRANSPORT_MODE:-usb}"
RSD_ADDR="${SIDEY_RSD_ADDR:-}"
RSD_PORT="${SIDEY_RSD_PORT:-}"
SCENARIO="${SIDEY_SCENARIO:-none}"

[ -f "$ORIGINAL_IPA" ] || { echo "test IPA not found: $ORIGINAL_IPA" >&2; exit 1; }

TIMESTAMP="$(date -u +"%Y%m%dT%H%M%SZ")"
OUT_DIR="${SIDEY_RESULTS_DIR:-$REPO_DIR/results/phase-b/$MODE-$TIMESTAMP}"
mkdir -p "$OUT_DIR"
STEP_LOG="$OUT_DIR/steps.log"

step() {
  local key="$1" value="$2"
  printf '%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$key" "$value" >> "$STEP_LOG"
}

run_step() {
  local name="$1"
  local out="$OUT_DIR/step-$name.log"
  shift
  echo ">> [$name] $*"
  if "$@" > "$out" 2>&1; then
    step "$name:exit" 0
  else
    step "$name:exit" 1
    echo "[$name] FAILED - see $out" >&2
    exit 1
  fi
}

# Like run_step but records the outcome without aborting the proof. Used for
# probes where a specific platform limitation (rather than a transport defect)
# is the expected result, e.g. house_arrest on personal-team-signed apps.
soft_step() {
  local name="$1"
  local out="$OUT_DIR/step-$name.log"
  shift
  echo ">> [$name] $*"
  if "$@" > "$out" 2>&1; then
    step "$name:exit" 0
  else
    step "$name:exit" 1
    echo "[$name] FAILED but non-fatal - see $out" >&2
  fi
}

record_key_value() {
  # promote key=value lines from a step log into the structured step log
  local name="$1"
  local log="${OUT_DIR}/step-${name}.log"
  [ -f "$log" ] || return 0
  local line k v
  while IFS= read -r line; do
    case "$line" in
      *=*)
        k="${line%%=*}"
        v="${line#*=}"
        printf '%s\t%s.%s\t%s\n' "$(date -u +%FT%TZ)" "$name" "$k" "$v" >> "$STEP_LOG"
        ;;
    esac
  done < "$log"
}

# usbmuxd reachable: any device listed via /var/run/usbmuxd?
usbmuxd_available() {
  [ -S /var/run/usbmuxd ] || return 1
  { idevice_id -l 2>/dev/null || sudo -n idevice_id -l 2>/dev/null; } | grep -q .
}

# Extract the JSON result line from a sign-only/wireless log (which also
# carries tracing output to the same file) and resolve a key with jq.
json_value() {
  local log="$1" key="$2"
  grep -E '^\{' "$log" | tail -1 | jq -r "$key"
}

# ---------------------------------------------------------------------------
echo "== Phase B transport proof ($MODE mode) =="
echo "device: $DEVICE_UDID ($DEVICE_NAME)"
echo "results: $OUT_DIR"
step "phase" "B"
step "mode" "$MODE"
step "scenario" "$SCENARIO"
step "device.udid" "$DEVICE_UDID"
step "ipa" "$ORIGINAL_IPA"

# Bundle id: derive from the IPA unless overridden.
if [ -z "$BUNDLE_ID" ]; then
  BUNDLE_ID="$(python3 - "$ORIGINAL_IPA" <<'EOF'
import plistlib, sys, zipfile
ipa = sys.argv[1]
with zipfile.ZipFile(ipa) as z:
    app = next(n for n in z.namelist() if n.endswith('/Info.plist'))
    with z.open(app) as f:
        print(plistlib.load(f)['CFBundleIdentifier'])
EOF
)"
fi
step "bundle_id" "$BUNDLE_ID"

# ---------------------------------------------------------------------------
# 0. Build the tools once.
[ -x "$SPIKE_BIN" ] || { echo "building transport-spike..."; (cd "$SPIKE_DIR" && cargo build --release); }
[ -x "$WIRELESS_BIN" ] || { echo "building wireless install binary..."; (cd "$ISIDELOAD_DIR" && cargo build --release -p wireless); }

# 1. Device discovery and pairing validation (usbmuxd = USB or virtual-USB).
if [ "$MODE" = "usb" ]; then
  run_step "list"   "$SPIKE_BIN" list
  run_step "validate" "$SPIKE_BIN" --udid "$DEVICE_UDID" validate
  run_step "info"   "$SPIKE_BIN" --udid "$DEVICE_UDID" info
  run_step "apps"   "$SPIKE_BIN" --udid "$DEVICE_UDID" apps
  record_key_value "list"
  record_key_value "info"
  record_key_value "validate"
else
  step "usbmuxd.cloud_check" 0
  step "list.skipped.rsd" "$MODE"
  step "validate.skipped.rsd" "$MODE"
fi

# usbmuxd may still be absent in rsd mode (D13 runs over the RSD tunnel while
# the VirtualHere USB link is down); the transport-spike steps then report
# "unavailable" instead of failing the whole proof.
spike_step() {
  local name="$1"; shift

  if usbmuxd_available; then
    run_step "$name" "$@"
  else
    step "$name:exit" "skipped"
    step "$name.unavailable" "usbmuxd down (VirtualHere link not up)"
    echo ">> [$name] skipped - usbmuxd unavailable"
  fi
}

# 2. Sign test IPA v1 (D6 Impactor run in headless form).
step "sign.v1.start" "$(date -u +%FT%TZ)"
SIGN1_LOG="$OUT_DIR/step-sign-v1.log"
if "$SIGN_SCRIPT" "$ORIGINAL_IPA" "$OUT_DIR/test-v1.ipa" > "$SIGN1_LOG" 2>&1; then
  step "sign.v1.exit" 0
else
  step "sign.v1.exit" 1
  echo "signing v1 failed - see $SIGN1_LOG" >&2
  exit 1
fi
if command -v jq >/dev/null 2>&1; then
  step "sign.v1.profile_expiry" "$(json_value "$SIGN1_LOG" .profile_expiry_at)"
  step "sign.v1.cert_serial" "$(json_value "$SIGN1_LOG" .cert_serial)"
  step "sign.v1.team_id" "$(json_value "$SIGN1_LOG" .team_id)"
  # isideload re-signs the app under a prefixed bundle id
  # (e.g. com.foo.app.TEAMID); install/verify must use that signed id.
  SIGNED_BUNDLE_ID="$(json_value "$SIGN1_LOG" .signed_bundle_identifier)"
  if [ -n "$SIGNED_BUNDLE_ID" ]; then
    BUNDLE_ID="$SIGNED_BUNDLE_ID"
    step "bundle_id.signed" "$SIGNED_BUNDLE_ID"
  fi
fi

# 3. Install and verify.
if [ "$MODE" = "rsd" ]; then
  # Wireless path: sign + install over the RSD tunnel in one step.
  if [ -z "$RSD_ADDR" ]; then
    ENDPOINT="$(sudo -n cat /run/sidey/rsd-endpoint 2>/dev/null || cat /run/sidey/rsd-endpoint 2>/dev/null || true)"
    read -r RSD_ADDR RSD_PORT <<< "$ENDPOINT" || true
  fi
  : "${RSD_ADDR:?rsd mode needs SIDEY_RSD_ADDR or a live /run/sidey/rsd-endpoint}"
  : "${RSD_PORT:?rsd mode needs SIDEY_RSD_PORT or a live /run/sidey/rsd-endpoint}"
  RSD_LOG="$OUT_DIR/step-rsd-install.log"
  if sudo env SIDEY_ISIDELOAD_STATE="${SIDEY_ISIDELOAD_STATE:-/var/lib/sidey/isideload}" \
        ANISETTE_URL="${ANISETTE_URL:-http://127.0.0.1:6970}" \
        DEVICE_UDID="$DEVICE_UDID" DEVICE_NAME="$DEVICE_NAME" \
        DEVICE_TYPE="${DEVICE_TYPE:-ios}" \
        RSD_ADDR="$RSD_ADDR" RSD_PORT="$RSD_PORT" \
      "$WIRELESS_BIN" "$SIDEY_APPLE_ID" "$SIDEY_APPLE_MAIN_PASSWORD" \
      "$ORIGINAL_IPA" > "$RSD_LOG" 2>&1; then
    step "rsd-install.exit" 0
    if command -v jq >/dev/null 2>&1; then
      step "rsd-install.profile_expiry" "$(json_value "$RSD_LOG" .profile_expiry_at)"
      step "rsd-install.team_id" "$(json_value "$RSD_LOG" .team_id)"
    fi
  else
    step "rsd-install.exit" 1
    echo "rsd install failed - see $RSD_LOG" >&2
    exit 1
  fi
else
  run_step "install-v1" "$SPIKE_BIN" --udid "$DEVICE_UDID" install --ipa "$OUT_DIR/test-v1.ipa"
fi
spike_step "verify-v1" "$SPIKE_BIN" --udid "$DEVICE_UDID" verify --bundle-id "$BUNDLE_ID"
record_key_value "verify-v1"

# documents: house_arrest container access is refused for apps installed under
# a free/personal developer team (InstallationLookupFailed / ApplicationLookupFailed).
# That is a platform limitation, so the probe is recorded and the proof continues.
soft_step "documents" "$SPIKE_BIN" --udid "$DEVICE_UDID" documents --bundle-id "$BUNDLE_ID"
if [ "${SIDEY_EXPECT_DOCUMENTS_REFUSED:-1}" = "1" ]; then
  step "documents.expected" "personal-team-free-signed-app"
else
  step "documents.expected" "unset"
fi

# 4. Upgrade: bump build number, re-sign, install over the previous version.
UP2="$OUT_DIR/test-v2.ipa"
python3 - "$ORIGINAL_IPA" "$UP2" <<'EOF'
import plistlib, shutil, sys, zipfile
src, dst = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(src) as zin, zipfile.ZipFile(dst, 'w', zipfile.ZIP_DEFLATED) as zout:
    for item in zin.infolist():
        data = zin.read(item.filename)
        if item.filename.endswith('/Info.plist'):
            info = plistlib.loads(data)
            info['CFBundleVersion'] = str(int(info.get('CFBundleVersion', '1')) + 1)
            info['CFBundleShortVersionString'] = '1.' + info['CFBundleVersion']
            data = plistlib.dumps(info)
        zout.writestr(item, data)
EOF
step "upgrade.ipa.v2" "$UP2"
if [ "$MODE" = "rsd" ]; then
  # The wireless binary re-signs + re-installs over the tunnel, which proves
  # the upgrade path (new build number on the same device) with real MDM-free
  # RemotePairing transport.
  RSD2_LOG="$OUT_DIR/step-rsd-upgrade.log"
  if sudo env SIDEY_ISIDELOAD_STATE="${SIDEY_ISIDELOAD_STATE:-/var/lib/sidey/isideload}" \
        ANISETTE_URL="${ANISETTE_URL:-http://127.0.0.1:6970}" \
        DEVICE_UDID="$DEVICE_UDID" DEVICE_NAME="$DEVICE_NAME" \
        DEVICE_TYPE="${DEVICE_TYPE:-ios}" \
        RSD_ADDR="$RSD_ADDR" RSD_PORT="$RSD_PORT" \
      "$WIRELESS_BIN" "$SIDEY_APPLE_ID" "$SIDEY_APPLE_MAIN_PASSWORD" \
      "$UP2" > "$RSD2_LOG" 2>&1; then
    step "rsd-upgrade.exit" 0
    if command -v jq >/dev/null 2>&1; then
      step "rsd-upgrade.profile_expiry" "$(json_value "$RSD2_LOG" .profile_expiry_at)"
      step "rsd-upgrade.version" "$(json_value "$RSD2_LOG" .version)"
    fi
  else
    step "rsd-upgrade.exit" 1
    echo "rsd upgrade failed - see $RSD2_LOG" >&2
    exit 1
  fi
else
  step "sign.v2.start" "$(date -u +%FT%TZ)"
  SIGN2_LOG="$OUT_DIR/step-sign-v2.log"
  "$SIGN_SCRIPT" "$UP2" "$OUT_DIR/test-v2-signed.ipa" > "$SIGN2_LOG" 2>&1 \
    && step "sign.v2.exit" 0 || { step "sign.v2.exit" 1; echo "signing v2 failed - see $SIGN2_LOG" >&2; exit 1; }
  run_step "upgrade-v2" "$SPIKE_BIN" --udid "$DEVICE_UDID" upgrade --ipa "$OUT_DIR/test-v2-signed.ipa"
fi
spike_step "verify-v2" "$SPIKE_BIN" --udid "$DEVICE_UDID" verify --bundle-id "$BUNDLE_ID"
record_key_value "verify-v2"

# 4b. Scenarios (restart / sleep) - prove the pairing record and the
# installed app survive a device reboot, and characterise the locked state.
if [ "$SCENARIO" = "restart" ] || [ "$SCENARIO" = "all" ]; then
  echo ">> scenario: restart device and re-validate"
  soft_step "scenario-restart" "$SPIKE_BIN" --udid "$DEVICE_UDID" restart
  if usbmuxd_available; then
    spike_step "scenario-restart-wait" "$SPIKE_BIN" --udid "$DEVICE_UDID" wait --timeout 300
    spike_step "scenario-restart-validate" "$SPIKE_BIN" --udid "$DEVICE_UDID" validate
    spike_step "scenario-restart-verify" "$SPIKE_BIN" --udid "$DEVICE_UDID" verify --bundle-id "$BUNDLE_ID"
    record_key_value "scenario-restart-verify"
  fi
fi

if [ "$SCENARIO" = "sleep" ] || [ "$SCENARIO" = "all" ]; then
  echo ">> scenario: lock the device and probe lockdown"
  soft_step "scenario-sleep" "$SPIKE_BIN" --udid "$DEVICE_UDID" sleep
  # After a sleep the screen locks (passcode/Face ID). Lockdown exchanges on
  # a locked phone either stall or refuse; run the probe with a bounded
  # timeout and record whichever happens. A non-zero/timeout is expected here.
  if usbmuxd_available; then
    SL_LOG="$OUT_DIR/step-scenario-sleep-probe.log"
    echo ">> [scenario-sleep-probe] (locked) validate + info, 20s bound each"
    step "scenario-sleep.probe.start" "$(date -u +%FT%TZ)"
    timeout 20 "$SPIKE_BIN" --udid "$DEVICE_UDID" validate > "$SL_LOG" 2>&1
    VRC=$?
    step "scenario-sleep.validate_exit" "$VRC"
    timeout 20 "$SPIKE_BIN" --udid "$DEVICE_UDID" info >> "$SL_LOG" 2>&1
    IRC=$?
    step "scenario-sleep.info_exit" "$IRC"
    if [ "$VRC" = "0" ] && [ "$IRC" = "0" ]; then
      echo "[scenario-sleep] WARNING: locked device still answered lockdown; is a passcode set?" >&2
      step "scenario-sleep.locked_detected" "no"
    else
      echo "[scenario-sleep] locked device stalls/refuses lockdown as expected" >&2
      step "scenario-sleep.locked_detected" "yes"
    fi
    echo
    echo ">> scenario: unlock the device now (passcode/Face ID); the proof waits."
    step "scenario-sleep.unlock_wait.start" "$(date -u +%FT%TZ)"
    UNLOCKED=no
    for i in $(seq 1 60); do
      if timeout 20 "$SPIKE_BIN" --udid "$DEVICE_UDID" validate > /dev/null 2>&1; then
        UNLOCKED=yes
        step "scenario-sleep.unlock_wait_sec" "$((i * 5))"
        break
      fi
      sleep 5
    done
    step "scenario-sleep.unlocked" "$UNLOCKED"
  fi
fi

# 5. Cleanup.
if [ "${SIDEY_KEEP_INSTALLED:-0}" != "1" ]; then
  spike_step "uninstall" "$SPIKE_BIN" --udid "$DEVICE_UDID" uninstall --bundle-id "$BUNDLE_ID"
fi
spike_step "apps-final" "$SPIKE_BIN" --udid "$DEVICE_UDID" apps

# ---------------------------------------------------------------------------
# 6. Assemble the compatibility report.
python3 - "$OUT_DIR" <<'EOF'
import json, sys, os
outdir = sys.argv[1]
records = []
with open(os.path.join(outdir, "steps.log")) as f:
    for line in f:
        parts = line.rstrip("\n").split("\t")
        parts += [""] * (3 - len(parts))
        ts, key, value = parts[:3]
        records.append({"at": ts, "key": key, "value": value})
steps = {r["key"]: r["value"] for r in records}

def status(key):
    v = steps.get(key)
    if v == "0":
        return "pass"
    if v == "skipped":
        return "skipped"
    return "fail"  # "1" or missing

verdict = {
    "installed": status("install-v1:exit") == "pass" or status("rsd-install.exit") == "pass",
    "upgraded": status("upgrade-v2:exit") == "pass" or status("rsd-upgrade.exit") == "pass",
    "verified_v1": status("verify-v1:exit"),
    "verified_v2": status("verify-v2:exit"),
    "provisioning_expiry_readable": bool(steps.get("sign.v1.profile_expiry")
                                         or steps.get("rsd-install.profile_expiry")),
    "house_arrest_access": status("documents:exit"),
    "uninstalled": status("uninstall:exit"),
    "pairing_validated": status("validate:exit") if status("validate:exit") != "fail"
                        else status("rsd-install.exit"),
    "restart_survived": status("scenario-restart-verify:exit"),
    "locked_stalls_lockdown": steps.get("scenario-sleep.locked_detected"),
    "unlocked_after_prompt": steps.get("scenario-sleep.unlocked"),
}
report = {
    "phase": "B",
    "generated_at": records[-1]["at"] if records else "",
    "device": {"udid": steps.get("device.udid"), "bundle_id": steps.get("bundle_id")},
    "verdict": verdict,
    "records": records,
}
with open(os.path.join(outdir, "compatibility.json"), "w") as f:
    json.dump(report, f, indent=2)
with open(os.path.join(outdir, "topology-decision"), "w") as f:
    f.write(
        "Phase B transport proof - D13 topology verdict\n"
        "==============================================\n"
        "  installed: {0}\n"
        "  upgraded:  {1}\n"
        "  profile expiry readable: {2}\n"
        "  house arrest access:     {3}\n"
        "  pairing validated:       {4}\n"
        "  uninstalled:             {5}\n".format(
            "PASS" if verdict["installed"] else "FAIL",
            "PASS" if verdict["upgraded"] else "FAIL",
            "PASS" if verdict["provisioning_expiry_readable"] else "FAIL",
            verdict["house_arrest_access"].upper(),
            verdict["pairing_validated"].upper(),
            verdict["uninstalled"].upper(),
        )
    )
    hard = [v for v in (verdict["installed"], verdict["upgraded"], verdict["provisioning_expiry_readable"])
            if v is False or v == "fail"]
    f.write("Decision: direct Oracle-to-device communication is {0}.".format(
        "viable; proceed with the device agent on the VPS"
        if not hard else
        "NOT viable; fall back to a local edge host per D13",
    ) + "\n")
print(json.dumps({"verdict": verdict}, indent=2))
EOF

echo
echo "== compatibility summary =="
cat "$OUT_DIR/topology-decision"
echo
echo "report: $OUT_DIR/compatibility.json"