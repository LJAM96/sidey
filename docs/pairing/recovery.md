# Pairing recovery procedure

Applies to iPhone, iPad and Apple TV. Phase B must verify this procedure; update it with the verified commands and timings.

## 1. Detect the failure state

Use the device connection capability states (ADR-0005):

| State | Meaning | Action |
|---|---|---|
| `paired_but_unreachable` | Pairing record exists, device unreachable (offline, network changed) | Wait/retry, verify network path (WiFi same LAN, Tailscale route) |
| `pairing_invalid` | Record exists but lockdown rejects the session | Re-pair (section 3) |
| `device_locked` | Device is locked; session may fail | Ask the user to unlock; retry after unlock |
| `developer_mode_required` | Developer Mode disabled on the device | Enable in Settings → Privacy & Security → Developer Mode, reboot the device |

`validate` in the transport spike prints whether the pairing record exists and whether a TLS session starts, which is the fast diagnostic:

```sh
transport-spike --udid <UDID> validate
# pair_record=present / session=ok
```

## 2. Backup the pairing record

The pairing record is encrypted at rest in the edge agent vault (Phase H) and backed up separately (Phase N). While it exists, copy it off the host for safety:

```sh
# usbmuxd keeps records in /var/lib/usbmuxd/ on Linux
sudo tar czf /tmp/pairing-backup.tar.gz /var/lib/usbmuxd
```

## 3. Re-pair USB (iPhone / iPad)

1. Connect the phone over USB with a cable that supports data.
2. Unlock the phone.
3. Pair again:

```sh
transport-spike --udid <UDID> pair
# accept the trust dialog on the phone
```

4. Validate:

```sh
transport-spike --udid <UDID> validate
```

### 3.1 VPS only topology bootstrap (D13)

With no edge host at home, pairing is bootstrapped once from any machine the phone can be plugged into. Preferred path (VirtualHere):

1. Plug the phone into a machine running the VirtualHere server (or usbip); both machines must be on the tailnet. On the VPS, run the VirtualHere client so usbmuxd sees a virtual USB port.
2. Pair through the agent as if the device were local:

```sh
transport-spike --udid <UDID> pair
# accept the trust dialog on the phone
transport-spike --udid <UDID> validate
```

3. The pairing record is stored in the agent vault on the VPS directly.

Fallback path (record transfer), when VirtualHere is not available:

1. On the local machine (Mac or Linux with usbmuxd): pair per section 3, then export the record:

```sh
transport-spike --udid <UDID> pair
python3 - <<'EOF'
# usbmuxd stores records per device; export the record directory for <UDID>
# (Linux: /var/lib/usbmuxd/, macOS: ~/Library/Developer/PrivateFrameworks/
# CoreDevice.framework/.../pairairport/*.plist). Copy the matching record.
EOF
```

2. Transfer the record to the VPS (scp / tailscale serve / secret copy) and import it into the agent's pairing vault (vault path: `pairing-vault` volume, agent reads it at startup).
3. The agent validates with `validate` (it uses the vault record instead of usbmuxd for TCP connections).
4. The phone must be on the tailnet (Tailscale app or home subnet router) so the agent's Tailscale address reaches it.

Once a record exists on the VPS, it survives only if backed up; losing it means another USB session on a local machine.

5. If the device still rejects the record, clear the stale record first:

```sh
# Linux: remove the record file and restart usbmuxd
sudo rm /var/lib/usbmuxd/<UDID>
sudo systemctl restart usbmuxd
# or macOS: settings for the host pairing must be forgotten in Finder/Apple Configurator
```

6. If pairing still fails: on the phone, Settings → General → Transfer or Reset iPhone → Reset → Reset Network Settings (this forgets host trust), then repeat 1-4.

## 4. Wireless debugging

1. Pair over USB first (section 3).
2. On the phone: Settings → Developer → Wireless Debugging, or enable via Xcode / Apple Configurator.
3. Confirm the device appears in `transport-spike list` with a `Network` connection type.
4. Re-validate after the phone has moved to a different network; wireless debugging records are per network segment.
5. Note (plan): iOS 27 network onboarding records may be retained only while the onboarding application stays open; prefer persistent Lockdown pairing through usbmuxd for unattended operation.

## 5. Apple TV

1. Pairing for tvOS uses the local network discovery path (mDNS/Avahi) provided by the atvloadly derived helper (D1) — follow the atvloadly documented flow for the TV's pairing code entry.
2. The edge host must run Avahi and be on the same LAN as the Apple TV; the discovery privileges stay in the edge container, never on the control plane.

## 6. Replacing a lost edge agent

1. Deploy a new edge host with the same Tailscale identity (restore the Tailscale state volume) or a new tailnet node.
2. Restore the encrypted pairing vault from the Phase N backup.
3. Re-attach devices: for each device, run `validate`; devices whose records were lost are re-paired with section 3.
4. Update the device records' `agent_id` in the control plane.

## 7. Post-recovery checks

- `transport-spike --udid <UDID> apps` shows the installed inventory (it must match the control plane's `InstallationRecord`).
- `transport-spike --udid <UDID> verify --bundle-id <id>` shows the profile expiry; the refresh scheduler reschedules from the new verified expiry.
- Log every recovery action as an audit event (Phase D).
