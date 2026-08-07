# Phase B live run: restart / locked scenario (PENDING)

Status: **PENDING** - code merged (commit `b3ef512`), live run deferred because
the VirtualHere USB link was down on the VPS (no iPhone on the home hub,
`usbmuxd` dead). Run this when the iPhone is visible again.

Related: plan.md Phase B ("restart and locked-device scenarios"), ADR-0006.

## When to run

The iPhone must be attached to the VirtualHere hub at home (so usbmuxd on the
VPS sees it). Quick preflight on the VPS (oi-3):

```bash
lsusb -d 05ac:          # expect an Apple iPhone entry
systemctl is-active usbmuxd
tailscale status        # iphone should be active on the tailnet
```

The iPhone also needs a passcode or Face ID set - the sleep scenario's whole
point is that a locked device refuses/stalls lockdown exchanges.

## Preflight: proof scripts must be current

```bash
cd /home/ubuntu/git/sidey-server
git pull --ff-only origin main        # get b3ef512 + any later work
cargo build --release --manifest-path rust/transport-spike/Cargo.toml
```

The test IPA is expected at `/tmp/opencode/sidetest.ipa` (remote). If missing,
build one with `scripts/phase-b/sign-test-ipa.sh`.

## Run 1: restart scenario

```bash
DEVICE_UDID=00008120-001E11211184C01E \
SIDEY_DEVICE_UDID=00008120-001E11211184C01E \
SIDEY_TEST_IPA=/tmp/opencode/sidetest.ipa \
SIDEY_SCENARIO=restart \
timeout 900 bash scripts/phase-b/transport-proof.sh
```

This installs v1, upgrades to v2, then reboots the phone (diagnostics relay),
waits up to 300 s for it to reappear, re-validates the pairing record and
re-verifies the installed app.

Expected verdict (in `results/phase-b/usb-<ts>/compatibility.json`):

```json
"restart_survived": "pass"
```

A `fail` here means the pairing record did not survive the device reboot -
that blocks the exit criterion "a pairing record survives the expected restart
scenario" and the D13 design would need rework.

## Run 2: sleep (locked) scenario

```bash
DEVICE_UDID=00008120-001E11211184C01E \
SIDEY_DEVICE_UDID=00008120-001E11211184C01E \
SIDEY_TEST_IPA=/tmp/opencode/sidetest.ipa \
SIDEY_SCENARIO=sleep \
timeout 1800 bash scripts/phase-b/transport-proof.sh
```

The script locks the screen, probes `validate`/`info` with a 20 s bound each
(expected: stall or refusal while locked), then prints a prompt and polls until
the operator unlocks the phone (up to 5 min). **The operator must physically
unlock the phone during the wait** or the run ends with
`unlocked_after_prompt: no`.

Expected verdict:

```json
"locked_stalls_lockdown": "yes",
"unlocked_after_prompt":  "yes"
```

`locked_stalls_lockdown: no` would be surprising (means a locked phone still
answered lockdown, i.e. no passcode set) - re-check the phone's lock settings.

## Run 3 (optional): both

`SIDEY_SCENARIO=all` does restart then sleep in one run. Restart runs before
sleep deliberately, because the sleep scenario ends with a locked phone that
needs the operator.

## After a successful run

1. Record the result in plan.md (Phase B "Remaining Phase B work" paragraph):
   note both verdicts and the run directories under `results/phase-b/`.
2. Commit the result logs (`results/phase-b/` currently ships with the repo)
   and any plan.md edits.
3. Phase B is then complete apart from the Apple TV atvloadly proof
   (`scripts/phase-b/atvloadly-proof.sh`, not yet run).

## Troubleshooting

- `transport-spike list` fails with "Connection refused": usbmuxd is dead /
  the VirtualHere link is down; the watchdog `sidey-usbmuxd-watch` restarts
  usbmuxd, but if the phone is not on the hub nothing helps - check
  `docker logs sidey-virtualhere-client-1` and the home hub state.
- `wait` times out: the phone took longer than 5 min to boot, or it dropped
  off the tailnet - retry, or bump `--timeout` in the scenario block.
- The RSD tunnel is unaffected by USB being down; `SIDEY_TRANSPORT_MODE=rsd`
  still works for install/upgrade but the scenario probes (restart/wait/
  validate) run over usbmuxd and are skipped with `usbmuxd unavailable`.
