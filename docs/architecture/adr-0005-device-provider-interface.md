# ADR-0005: Device provider interface

- Status: Accepted
- Date: 2026-08-06
- Related: plan.md D1, D10, ADR-0003

## Context

The device service must support iPhone, iPad and Apple TV through different transports (usbmuxd/wireless for iOS, mDNS/Avahi pairing for tvOS). Providers will be swapped and extended (a Rust tvOS port is planned to eventually replace the Go helper). Domain code must not know which provider handles a device.

## Decision

Define a `DeviceProvider` trait as the only boundary the device service core knows:

```text
DeviceProvider
├── TVOSProvider (Go helper wrapper)
└── IOSProvider (native Rust)
```

The trait covers discovery, pairing validation, device information, installed application inventory, IPA staging, installation, upgrade, removal, provisioning profile inspection, House Arrest access, post installation verification and system logging (plan "idevice" reuse list).

Provider specific states are surfaced through an explicit connection capability state: connected, paired_but_unreachable, pairing_invalid, device_locked, developer_mode_required, offline.

## Rationale

- Provider implementations have entirely different transports and failure modes; the trait keeps that contained.
- A future native Rust tvOS provider can replace the Go helper without device service core changes (D1).
- Explicit connection states let the scheduler and dashboard distinguish pairing, signing and installation failures (Phase G/H exit criteria).

## Consequences

- Every new provider must implement the full trait surface; the tvOS helper wrapper is a thin adapter over the Go subprocess JSON interface.
- Capability reports from the device service must describe which providers it can run.
