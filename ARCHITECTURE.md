# Sidey Architecture

This document records the target architecture for the Sidey platform as constrained by the recorded decisions in `plan.md` (D1 to D10). It is the live architecture reference; individual decisions are recorded in the ADRs under `docs/architecture/`.

## 1. Purpose

Sidey is a self hosted application library, signing service and deployment manager for iPhone, iPad and Apple TV. It stores original IPA files, produces signed derivatives for individual devices, refreshes installations before provisioning expiry, and manages devices through external Apple device services rather than through a custom iPhone client.

## 2. Topology

### 2.1 Production

The Internet facing control plane is separated from a local edge agent that owns device communication.

```text
Oracle VPS
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
└── Tailscale
        │
        │ Encrypted tailnet connection
        ▼
Local edge host
│
├── Device agent
├── iOS and iPadOS provider (Rust, idevice)
├── tvOS provider (Go helper behind provider trait, D1)
├── Pairing record vault
├── usbmuxd integration
├── Avahi and mDNS integration
└── Tailscale
        │
        │ Local Apple device services
        ▼
Devices
├── iPhone
├── iPad
└── Apple TV
```

The edge host may be a NAS, Raspberry Pi, small Linux computer or always on Mac. Direct Oracle to device communication through Tailscale remains experimental until the Phase B transport proof.

### 2.2 Development

```text
Development workstation
│
├── Control plane containers
├── PostgreSQL
├── Signing worker
├── Device agent
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

device-agent (Rust core)
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

The signing worker and device agent are separate processes because they handle different sensitive materials and require different operating system permissions (D1, ADR-0003).

## 4. Language and composition (D1, D2)

| Component | Language | Basis |
|---|---|---|
| control-plane | Go | Our own implementation; atvloadly's web UI, API and scheduler are not reused (D2) |
| signing-worker | Rust | Modules extracted from Impactor (MIT) |
| device-agent core | Rust | Our own implementation |
| iOS provider | Rust | `idevice` crate (MIT) |
| tvOS provider | Go | atvloadly derived helper (D1, AGPL-3.0 per D11) |
| web dashboard | Go/TS | Our own implementation |

## 5. Provider interface

The device agent core only knows the provider trait. A future Rust port of the tvOS provider can replace the Go helper without changing the agent core (D1, ADR-0005).

```text
DeviceProvider
├── TVOSProvider (Go helper wrapper)
└── IOSProvider (native Rust)
```

Agent core responsibilities that are provider independent: job execution, progress reporting, heartbeat, capability reporting, pairing record lifecycle, verification orchestration.

## 6. Key flows

### 6.1 Agent enrolment and heartbeat

1. Operator creates a one time enrolment token in the dashboard.
2. Agent presents the token to the control plane API.
3. Control plane issues agent credentials; agent starts heartbeating with capability report.
4. Jobs are claimed with idempotency keys and a per device lock. PostgreSQL is both the system of record and the job queue (D5); Redis is not used.

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
- The device agent holds pairing records and device services access only; it never sees Apple account credentials or GitHub credentials.
- The signing worker sees Apple credentials and signing keys, not pairing records.
- The control plane cannot read raw signing private keys without the configured encryption service.
- Tailscale provides the transport between control plane and edge agent; ACLs restrict agent access.

## 9. Out of scope for the first release

- GitHub Releases discovery and automatic updates (D7); manual IPA upload is the v1 path.
- Multi user roles and RBAC beyond a single admin (D4).
- A separate artifact worker process (D9).
- Automated LiveContainer guest inbox processing (Phase L; can ship after the direct installation platform).
- SideStore as a runtime dependency; it remains a reference implementation and optional compatibility mode.
- sidestore-vpn and anisette-v3-server (optional Compose profiles only).
