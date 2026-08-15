# Supported platforms

Status: Phase A definition. Refresh at every release (Phase O) and after Apple OS releases.

## 1. Devices

| Platform | Provider | Transport | Status |
|---|---|---|---|
| iPhone | iOS provider (Rust, `idevice`) | USB onboarding, then wireless debugging | v1 target |
| iPad | iOS provider (Rust, `idevice`) | USB onboarding, then wireless debugging | v1 target |
| Apple TV | tvOS provider (Go helper behind provider trait) | Local network, mDNS/Avahi pairing | v1 target |

## 2. Operating system coverage

| OS | Coverage in v1 |
|---|---|
| iOS / iPadOS | Current stable major (27.x at the time of writing) and the previous major (26.x), plus the current beta when available. Version floor to be confirmed during Phase B testing. |
| tvOS | Current stable major and previous major, confirmed during Phase B/G testing. |
| Pairing modes | Persistent Lockdown pairing through usbmuxd preferred; iOS 27 network onboarding treated as a convenience feature (records may be retained only while the onboarding application stays open). |

Exact floors must be recorded in this file when Phase B transport results land.

## 3. Apple development teams

| Type | Status | Constraints |
|---|---|---|
| Free team | Primary target (D3) | Seven day profiles, three App IDs, limited device registrations. Planned App ID set: our own client application, LiveContainer, one custom application. Multiple accounts may be enrolled to extend capacity and provide signing fallback (D12). |
| Paid team | Secondary | Longer profiles; no fixed App ID limit relevant to the platform. |

## 4. Hosts

| Host | Architecture | Role |
|---|---|---|
| Oracle Cloud VPS (AMD or ARM) | amd64, arm64 | Default full stack: control plane, signing worker, device service (production) |
| UGREEN NAS / Raspberry Pi / generic Linux | arm64/amd64 | Optional remote node: device service on a second host |
| macOS workstation | amd64/arm64 | Development host |
| Ordinary Linux server | amd64/arm64 | Full stack single host (non-VPS alternative) |

Docker images are published for linux/amd64 and linux/arm64 (plan "Image publishing").

## 5. Deployment targets

| Target | Support |
|---|---|
| direct | iOS, iPadOS, tvOS |
| livecontainer | iOS, iPadOS only (LiveContainer does not support tvOS) |

## 6. Explicitly out of scope for v1

- Windows hosts or clients.
- SideStore as a runtime dependency (optional compatibility mode only).
- Android, macOS or visionOS devices.
- Guest application features in LiveContainer: remote push notifications, full sandboxing of guests, unrestricted application extensions.
