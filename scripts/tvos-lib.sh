#!/bin/bash
# Shared tvOS tunnel lifecycle for the repository install/uninstall
# orchestrators. Sourced by tvos-install.sh / tvos-uninstall.sh.
#
# Provides tvos_tunnel_ensure and tvos_tunnel_wait_env (sets RSD_ADDR and
# RSD_PORT). Requirements: VENV_PY, SCRIPT_DIR, DEVICE_IDENTIFIER,
# DEVICE_IP, DEVICE_PORT, RSD_ENDPOINT_FILE must be set by the caller.

# Bring the RSD tunnel up (systemd unit preferred, like wireless-install.sh).
tvos_tunnel_ensure() {
    local unit=sidey-tvos-tunnel.service
    if systemctl list-unit-files 2>/dev/null | grep -q "$unit"; then
        if ! systemctl is-active --quiet "$unit"; then
            echo "Waiting for $unit to come up..."
            systemctl start "$unit" 2>/dev/null || true
            for _ in $(seq 1 60); do
                systemctl is-active --quiet "$unit" && break
                sleep 5
            done
        fi
        systemctl is-active --quiet "$unit" || {
            echo "Tunnel unit not active; check 'systemctl status $unit'" >&2
            return 1
        }
    elif ! pgrep -f tvos-tunnel.py >/dev/null; then
        echo "Starting tvOS tunnel to $DEVICE_IP (RemotePairing daemon)..."
        setsid nohup sudo "$VENV_PY" "$SCRIPT_DIR/tvos-tunnel.py" \
            --udid "$DEVICE_IDENTIFIER" --address "$DEVICE_IP" \
            --port "$DEVICE_PORT" \
            --endpoint-file "$RSD_ENDPOINT_FILE" > /var/log/sidey-tvos-tunnel.log 2>&1 &
    fi
    return 0
}

# Wait for a live RSD listener at the endpoint; sets RSD_ADDR and RSD_PORT.
tvos_tunnel_wait() {
    local addr="" port=""
    for _ in $(seq 1 30); do
        if [ -s "$RSD_ENDPOINT_FILE" ]; then
            read -r addr port < "$RSD_ENDPOINT_FILE" || true
        fi
        if [ -n "$addr" ] && [ -n "$port" ] \
            && "$VENV_PY" -c "import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), 2)" "$addr" "$port" 2>/dev/null; then
            RSD_ADDR="$addr"
            RSD_PORT="$port"
            echo "RSD tunnel: $RSD_ADDR:$RSD_PORT"
            return 0
        fi
        addr=""; port=""
        sleep 1
    done
    echo "Tunnel did not come up; check 'systemctl status sidey-tvos-tunnel.service'" >&2
    return 1
}