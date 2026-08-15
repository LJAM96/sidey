# transport-spike

Phase B transport spike for Sidey. A small Rust CLI proving that external installation, upgrade and refresh operations are possible against iOS/iPadOS devices using the `idevice` crate, without any SideStore or on device client.

This is a temporary crate. Its verified behaviour becomes the iOS provider inside the device service.

## Build

Requires Rust 1.85+ and a running usbmuxd on the host (`apt install usbmuxd` on Debian/Ubuntu, or on a Mac `brew install usbmuxd`).

```sh
cargo build
```

The `idevice` dependency is pinned by commit in Cargo.toml (see third_party/versions.lock).

## Usage

```sh
transport-spike [--udid <UDID>] <command>
```

| Command | Purpose |
|---|---|
| `list` | List devices visible to usbmuxd (USB and network) |
| `info` | Read DeviceName, ProductType, ProductVersion, BuildVersion, UDID, WiFiAddress, DeviceClass from lockdown |
| `apps` | List installed user applications (bundle id, version, build) |
| `pair` | Pair the host with the device (accept the trust dialog) and save the record to usbmuxd |
| `validate` | Check the pairing record exists and a TLS session can be started |
| `install --ipa <path>` | Push IPA to `PublicStaging` over AFC, then install via Installation Proxy |
| `upgrade --ipa <path>` | Same, but upgrade semantics (preserves app data) |
| `uninstall --bundle-id <id>` | Uninstall an application |
| `verify --bundle-id <id>` | Read installed version/build and list provisioning profiles with expiry dates |
| `documents --bundle-id <id> [--path <p>]` | List an app Documents directory via House Arrest |

Run `transport-spike --help` for the full flag list. Set `RUST_LOG=transport_spike=debug` for detailed protocol tracing.

## Test protocol (Phase B exit criteria)

USB first, then wireless, then restart scenarios. Record the output of every run.

1. **USB onboarding**: connect iPhone over USB, `list`, `pair`, `validate`, `info`.
2. **Install**: sign a test IPA with Impactor (D6), `install --ipa test.ipa`, `verify --bundle-id <id>`.
3. **Upgrade**: re-sign with a bumped build number, `upgrade --ipa test.ipa`, `verify` shows the new build and app data is intact.
4. **Documents**: `documents --bundle-id <id>` confirms House Arrest access (required for the LiveContainer inbox later).
5. **Profiles**: `verify` prints profile UUIDs and expiry dates; expiry must be readable for the refresh scheduler.
6. **Wireless**: enable wireless debugging (Xcode or Apple Configurator), `list` shows the device with a Network connection type, repeat 2-5.
7. **Restart**: restart the iPhone and the device service host, repeat `list`, `validate`, `apps`. The pairing record must survive.
8. **States**: repeat with the phone unlocked, locked and recently restarted; record which operations fail in each state.
9. **Lockdown vs iOS 27 network pairing**: test persistent Lockdown pairing (step 1) separately from the newer network onboarding flow.
10. **Uninstall**: `uninstall --bundle-id <id>` after the tests.

Each command prints `key=value` lines; collect them per scenario into `results/` so Phase B compatibility results can be written.
