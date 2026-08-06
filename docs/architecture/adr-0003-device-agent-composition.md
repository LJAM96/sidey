# ADR-0003: Device agent composition

- Status: Accepted
- Date: 2026-08-06
- Related: plan.md D1, ADR-0005

## Context

The device agent must communicate with iOS, iPadOS and tvOS devices. The `idevice` Rust crate covers iOS device services; the only working tvOS pairing/installation implementation available is atvloadly (Go, AGPL-3.0). Porting atvloadly to Rust is protocol risk; embedding Go code directly into the Rust agent is not possible.

## Decision

The device agent is a single Rust process. The iOS provider is native Rust using `idevice`. The tvOS provider is implemented behind the `DeviceProvider` trait as a wrapper around an atvloadly derived Go helper process. The helper is supervised by the agent, communicates over a local JSON interface, and is replaced by a native Rust implementation behind the same trait when justified.

## Rationale

- A single agent process keeps process management, heartbeats, job execution and pairing vault lifecycle in one place.
- The Go helper preserves the tested atvloadly behaviour instead of gambling a protocol reimplementation.
- The provider trait (ADR-0005) makes the helper replaceable without touching the agent core.
- Licensing implications of the atvloadly derivation are tracked separately in LICENSING.md and must be resolved before Phase G.

## Consequences

- The agent container must carry two binaries (Rust agent + Go helper).
- The helper is a fork of atvloadly (AGPL-3.0); Sidey is AGPL-3.0 (D11) so the derivation is compatible and the fork retains the AGPL notice and pinned commit.
- Host privileges (usbmuxd, Avahi, DBus) remain isolated to the edge agent container (plan "Docker design").
