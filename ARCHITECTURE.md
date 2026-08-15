# Sidey Architecture

This document records the target architecture for the Sidey platform as constrained by the recorded decisions in `plan.md` (D1 to D14). It is the live architecture reference; individual decisions are recorded in the ADRs under `docs/architecture/`.

## 1. Purpose

Sidey is a self hosted application library, signing service and deployment manager for iPhone, iPad and Apple TV. It stores original IPA files, produces signed derivatives for individual devices, refreshes installations before provisioning expiry, and manages devices through external Apple device services rather than through a custom iPhone client.

## 2. Topology

### 2.1 Production

The default deployment is fully VPS hosted: the control plane and the device service that owns device communication run on the same Oracle VPS (D13, ADR-0008). An optional remote-node mode deploys the device service on a second host.

```text
Oracle VPS (default)
│
├── Web interface
├── Control API
├── PostgreSQL
├── Scheduler
├── GitHub release watcher (post-v1)
├── IPA inspection worker (inside control-plane for v1, D9)
├── Signing worker
├── Artifact storage
├── Notification service
├── Backup service
├── Device service
│   ├── iOS and iPadOS provider (Rust, idevice)
│   ├── tvOS provider (Go helper behind provider trait, D1)
│   ├── Pairing record vault
│   ├── usbmuxd integration
│   └── Avahi and mDNS integration
└── Tailscale
        │
        │ Tailscale connection to devices (USB only during initial pairing)
        ▼
    Devices
    ├── iPhone
    ├── iPad
    └── Apple TV
```

The control plane and device service run on the same host and exchange work over localhost (Unix socket, ADR-0008). The signing worker also runs on this host but only talks to the control plane over its internal API; it has no device mounts and no pairing records.

Initial USB pairing is bootstrapped once through a VirtualHere (or usbip) session, then the pairing record lands in the device service vault directly. Day to day device communication runs over Tailscale: the phone carries the Tailscale app (or the home network has a Tailscale subnet router), so no local host is required after provisioning.

Optional remote-node mode: the device service runs on a separate host (NAS, Raspberry Pi, small Linux computer or always on Mac) connected over the tailnet, useful for multi-site installs. It is not required and is not the default; a single VPS install includes the device service by default.

### 2.2 Development

```text
Development workstation
│
├── Control plane containers
├── PostgreSQL
├── Signing worker
├── Device service
├── Test artifact storage
└── USB connected test device
```

## 3. Service model

The first implementation is a modular control plane plus separate security sensitive workers. Not a microservice system.

```text
control-plane (Go)
├── REST API
├── Web dashboard
├── Scheduler
├── IPA inspection, quarantine and repository management (D9)
├── Deployment planner
├── Notification dispatcher
└── Audit service

signing-worker (Rust)
├── Apple authentication
├── Certificate operations
├── Provisioning operations
├── Entitlement processing
└── IPA signing

device-service (Rust core)
├── Device discovery
├── Pairing validation
├── iOS provider (Rust, idevice)
├── tvOS provider (Go helper, D1)
├── Installation
├── Upgrade
├── Verification
└── LiveContainer file delivery (Phase L)

postgres
└── System of record and initial job queue (D5)

artifact-store
└── Original and signed IPA files, content addressed
```

The signing worker and device service are separate processes because they handle different sensitive materials and require different operating system permissions (D1, ADR-0003, ADR-0008).

## 4. Language and composition (D1, D2)

| Component | Language | Basis |
|---|---|---|
| control-plane | Go | Our own implementation; atvloadly's web UI, API and scheduler are not reused (D2) |
| signing-worker | Rust | Modules extracted from Impactor (MIT) |
| device-service core | Rust | Our own implementation |
| iOS provider | Rust | `idevice` crate (MIT) |
| tvOS provider | Go | atvloadly derived helper (D1, AGPL-3.0 per D11) |
| web dashboard | Go/TS | Our own implementation |

## 5. Provider interface

The device service core only knows the provider trait. A future Rust port of the tvOS provider can replace the Go helper without changing the device service core (D1, ADR-0005).

```text
DeviceProvider
├── TVOSProvider (Go helper wrapper)
└── IOSProvider (native Rust)
```

Device service core responsibilities that are provider independent: job execution, progress reporting, capability reporting, pairing record lifecycle, verification orchestration.

## 6. Key flows

### 6.1 Device service control channel

1. Default (same VPS): the control plane and device service are adjacent. The device service attaches to the control plane over a localhost Unix socket; no enrolment tokens or remote credentials are involved (ADR-0008).
2. Remote node (optional): the operator creates a one time enrolment token in the dashboard; the remote node presents it to the control plane API over Tailscale and receives device service credentials, then reports capability state on an ongoing basis.
3. Jobs are claimed with idempotency keys and a per device lock. PostgreSQL is both the system of record and the job queue (D5); Redis is not used.

Deployment wiring: the control plane container binds `/run/sidey` (mode 0700) and listens on `SIDEY_DEVICE_SOCKET` (`/run/sidey/device.sock`); the device service runs as a systemd unit (`deploy/host/sidey-device.service`) executing `scripts/sidey-device.py`, which claims intents over the socket — same-host mode uses no credentials, remote-node mode switches to the agent API with `SIDEY_API_URL`/`SIDEY_ENROLMENT_TOKEN`.

### 6.2 Refresh with update check

```text
Refresh job starts
        │
        ▼
Resolve deployment policy
        │
        ▼
Check for approved newer artifact
        ├── No update
        │      ▼
        │   Resign installed version
        └── Update available (post-v1, D7)
               ▼
            Sign new version
               ▼
            Upgrade application
               ▼
            Verify version and expiry
               ▼
            Mark previous version as rollback candidate
```

Updates use upgrade semantics with a stable signed bundle identifier; the previous version is never uninstalled first.

### 6.3 IPA lifecycle

```text
Upload / download → SHA256 during streaming → quarantine
        ▼
ZIP structure validation → Info.plist parse → entitlement and executable inspection
        ▼
Platform and bundle identifier validation → compare with source configuration
        ▼
Promote to approved repository (content addressed, immutable) or reject
        ▼
Signing worker produces device specific derivative (SignedArtifact)
```

## 7. Storage model (ADR-0004)

- PostgreSQL: system of record, job queue, audit events.
- Filesystem artifact store: original IPAs stored by content hash, byte identical after signing.
- Docker named volumes for: PostgreSQL data, original IPA repository, signed IPA cache, icons, pairing records, certificates with encrypted private keys, Tailscale state, audit records, backups.
- No MinIO for the first release; a filesystem backed store is simpler at D5 scale.

## 8. Security boundaries

Detailed in `THREAT_MODEL.md`. Summary:

- Secrets are mounted as Docker secrets, never environment variables or image layers.
- Apple credentials, signing keys and pairing records are encrypted at rest with envelope encryption (Phase M).
- The device service holds pairing records and device services access only; it never sees Apple account credentials, signing keys or GitHub credentials.
- The signing worker sees Apple credentials and signing keys, not pairing records.
- The control plane cannot read raw signing private keys without the configured encryption service.
- In the default same-VPS mode the control plane and device service exchange work over a localhost Unix socket; in the optional remote-node mode Tailscale provides the transport and ACLs restrict the node.

## 9. Out of scope for the first release

- GitHub Releases discovery and automatic updates (D7); manual IPA upload is the v1 path.
- Multi user roles and RBAC beyond a single admin (D4).
- A separate artifact worker process (D9).
- Automated LiveContainer guest inbox processing (Phase L; can ship after the direct installation platform).
- SideStore as a runtime dependency; it remains a reference implementation and optional compatibility mode.
- sidestore-vpn and anisette-v3-server (optional Compose profiles only).
