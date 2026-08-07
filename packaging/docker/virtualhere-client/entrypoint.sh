#!/bin/sh
# VirtualHere client sidecar entrypoint.
# Runs the client in console mode (foreground) and drives it over IPC:
#   - adds the configured server (VH_SERVER) as a manual hub
#   - auto-uses the first USB device found on that hub (single-device setup)
# Auto-use state persists in /root/.vhui (vh-client-state volume).
set -eu

VHCLIENT=/usr/local/bin/vhclient
VH_SERVER="${VH_SERVER:-}"

"$VHCLIENT" -l=/var/log/vhclient.log &
CLIENT_PID=$!
trap 'kill "$CLIENT_PID" 2>/dev/null || true; exit 0' TERM INT

i=0
while [ ! -S /tmp/vhclient ] && [ "$i" -lt 30 ]; do
  sleep 1
  i=$((i + 1))
done

if [ -n "$VH_SERVER" ]; then
  "$VHCLIENT" -t "MANUAL HUB ADD,$VH_SERVER" || true
  sleep 3
fi

auto_use() {
  [ -n "$VH_SERVER" ] || return 0
  LIST="$("$VHCLIENT" -t "LIST" 2>/dev/null || true)"
  # The hub labels itself by its own hostname (e.g. MiWiFi-R3600-srv), which
  # differs from the VH_SERVER address we connected to, so match by device
  # name instead. NEVER auto-use anything but an iPhone/iPad: the hub also
  # carries the desktop's keyboard, mouse, Bluetooth radio and other
  # peripherals, and grabbing those steals them from the local machine.
  ADDR="$(printf '%s\n' "$LIST" | sed -n 's/^ *--> \(iPhone\|iPad[^ (]*\) (\([^)]*[.][0-9]\{1,\}\)).*/\2/p' | head -n 1)"
  [ -n "$ADDR" ] || return 0
  "$VHCLIENT" -t "USE,$ADDR" >/dev/null 2>&1 || true
  "$VHCLIENT" -t "AUTO USE DEVICE PORT,$ADDR" >/dev/null 2>&1 || true
}

# Re-attach periodically so a re-plugged phone is picked up again.
while kill -0 "$CLIENT_PID" 2>/dev/null; do
  auto_use
  sleep 30
done
wait "$CLIENT_PID"
