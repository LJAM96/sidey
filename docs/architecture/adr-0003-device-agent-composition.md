# ADR-0003: Device service composition

- Status: Accepted (terminology updated 2026-08-15 to "device service", ADR-0008)
- Date: 2026-08-06
- Related: plan.md D1, ADR-0005, ADR-0008

## Context

The device service must communicate with iOS, iPadOS and tvOS devices. The `idevice` Rust crate covers iOS device services; the only working tvOS pairing/installation implementation available is atvloadly (Go, AGPL-3.0). Porting atvloadly to Rust is protocol risk; embedding Go code directly into the Rust service is not possible.

## Decision

The device service is a single Rust process. The iOS provider is native Rust using `idevice`. The tvOS provider is implemented behind the `DeviceProvider` trait as a wrapper around an atvloadly derived Go helper process. The helper is supervised by the device service, communicates over a local JSON interface, and is replaced by a native Rust implementation behind the same trait when justified.

## Rationale

- A single device service process keeps process management, job execution, capability reporting and pairing vault lifecycle in one place.
- The Go helper preserves the tested atvloadly behaviour instead of gambling a protocol reimplementation.
- The provider trait (ADR-0005) makes the helper replaceable without touching the device service core.
- Licensing implications of the atvloadly derivation are tracked separately in LICENSING.md and must be resolved before Phase G.

## Consequences

- The device service container must carry two binaries (Rust service + Go helper).
- The helper is a fork of atvloadly (AGPL-3.0); Sidey is AGPL-3.0 (D11) so the derivation is compatible and the fork retains the AGPL notice and pinned commit.
- Host privileges (usbmuxd, Avahi, DBus) remain isolated to the device service container (plan "Docker design").
