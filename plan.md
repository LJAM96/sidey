# Sidey  Platform

## Phased implementation plan

## Executive summary

Sidey should be a self hosted application library, signing service and deployment manager for iPhone, iPad and Apple TV.

It should support:

iOS and iPadOS direct application installation

tvOS direct application installation

Automatic certificate and provisioning profile renewal

Automatic application refreshing before expiry

A managed IPA repository with immutable version history

Automatic update discovery from GitHub Releases

Installing the latest verified application version during its next scheduled refresh

Manual approval, pinned version and delayed update policies

LiveContainer installation and refreshing

LiveContainer guest application delivery

Docker deployment on Oracle Cloud, NAS hardware and ordinary Linux servers

Tailscale connectivity between the central server and local device agents

The platform should not depend on SideStore being installed on the phone. The intended installation path is an external paired device agent using Apple device services, with SideStore retained only as an optional compatibility mode.

atvloadly already provides a Docker based Apple TV web service with pairing, multiple Apple Account support and automatic application refreshing. Impactor already performs Apple authentication, device registration, certificate creation, provisioning, signing and installation for iOS, iPadOS and tvOS. The `idevice` Rust library already implements pairing, AFC, House Arrest, provisioning profile management and Installation Proxy operations. These projects provide most of the difficult low level functionality required for the platform.

## Recorded decisions

Recorded 2026-08-06. These decisions constrain the phases below and are replayed into the Phase A architecture decision records.

### D1: Device agent composition

The device agent is a single Rust process. The iOS provider is native Rust using `idevice`. The tvOS provider is implemented behind the `DeviceProvider` trait as a wrapper around an atvloadly derived Go helper process. The provider trait is the only boundary the agent core knows; a future Rust port of the tvOS provider can replace the helper without changing the agent core.

### D2: Control plane and web UI are our own

atvloadly is reused for tvOS pairing and installation behaviour only. Its web UI, API, SQLite persistence and scheduler are not inherited. The control plane (Go) and web dashboard are written by us from the start.

### D3: Free Apple development team first

The first release targets a free Apple development team with its normal seven day profile and three App ID limits. The intended application set is our own client application, LiveContainer and one custom application. Paid teams remain supported but secondary.

### D4: Single user

The first release has a single administrator. Role fields exist in the data model, but only the admin role is active. Multi user roles and RBAC arrive later.

### D5: Scale target

One user with a small number of devices (iPhone, iPad, Apple TV). PostgreSQL remains the job queue; Redis is not needed. The control plane and dashboard are built for this scale and must not assume hundreds of devices.

### D6: Phase B uses Impactor for signing

The transport spike signs test IPAs with a manual Impactor run before installation. A signing path therefore exists before the Phase F worker.

### D7: GitHub Releases deferred

GitHub Releases discovery and automatic updates are planned after the first stable release. Development is git versioned with local builds and testing first. Manual IPA upload is the first release update path.

### D8: Repository naming

The monorepo root directory is `sidey-server`; the product remains named Sidey.

### D9: Artifact inspection runs in the control plane

IPA inspection, quarantine and repository management run inside `control-plane` for the first release. A separate artifact worker is introduced only when workload justifies it.

### D10: Modular boundaries

All domain logic is organised behind small interfaces (device providers, job runner, notification dispatcher, source managers) so individual components can be replaced or fixed without touching the rest of the system.

### D11: Sidey is AGPL-3.0

Adopted 2026-08-06. atvloadly is AGPL-3.0 (verified in Phase A), and the tvOS provider derives from its core (D1). Sidey itself is therefore licensed AGPL-3.0, and atvloadly's core is forked into the Go helper. LiveContainer (AGPL-3.0) and its future Phase L fork are compatible with this licence.

### D12: Multiple Apple accounts

Adopted 2026-08-06. The platform supports enrolling more than one Apple account from day one. Each account is a separate team with its own limits (three App IDs, ten registered devices, seven day profiles on free accounts). Each application channel keeps a stable team assignment so refresh always uses the signing team. Signing may fall back to another account when the primary account cannot sign, provided the fallback team registers the bundle ID itself. The dashboard shows per account slot usage (registered App ID and device counts) and warns before an account is full.

## Product boundaries

### Included

The platform manages applications the user is authorised to sign and install.

It stores original IPA files and produces separate signed derivatives for individual devices.

It manages free and paid Apple development teams.

It refreshes installed applications before their provisioning profiles expire.

It tracks GitHub Releases and deploys validated updates according to a configured policy.

It manages iPhone, iPad and Apple TV devices through platform specific device providers.

It supports LiveContainer as both a managed host application and a guest deployment target.

### Excluded

The project will not bypass Apple’s certificate or provisioning rules.

It will not use leaked, shared or enterprise certificates intended for unauthorised public distribution.

It will not promise that every entitlement, extension or capability can be retained under a free development account.

It will not depend on undocumented background execution inside a custom iPhone client.

It will not require a custom iPhone client for the first release.

Free Apple development provisioning remains limited to short lived profiles and a restricted number of applications and components. Impactor currently documents the normal seven day free account limitation and handles application and plugin registration accordingly.

# Target architecture

## Production topology

The recommended production design separates the Internet facing control plane from a local device agent.

```text
Oracle VPS
│
├── Web interface
├── Control API
├── PostgreSQL
├── Scheduler
├── GitHub release watcher
├── IPA inspection worker (inside control-plane for v1, decision D9)
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
├── iOS and iPadOS provider
├── tvOS provider
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

The local edge host could be your UGREEN NAS, a Raspberry Pi, a small Linux computer or an always on Mac.

This is preferable to placing all device communication directly on Oracle because Apple TV discovery relies on local network pairing and Avahi, while persistent wireless iPhone communication requires validated pairing and working local Apple device services. atvloadly currently requires Avahi on the host and mounts the host DBus and Avahi sockets into its container. `idevice_pair` also states that network pairing requires both systems to be on the same network, and that iOS 27 network onboarding records are retained only in memory for that session.

Direct Oracle to device communication through Tailscale should remain an experimental topology until it has passed the transport proof phase.

## Development topology

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

This lets developers perform deterministic testing before introducing wireless discovery, Tailscale or multiple hosts.

# Initial service model

The first implementation should not begin as a large microservice system. It should use a modular control plane plus separate security sensitive workers.

```text
control-plane
├── REST API
├── Web dashboard
├── Scheduler
├── GitHub source manager
├── Deployment planner
├── Notification dispatcher
└── Audit service

signing-worker
├── Apple authentication
├── Certificate operations
├── Provisioning operations
├── Entitlement processing
└── IPA signing

device-agent
├── Device discovery
├── Pairing validation
├── iOS provider
├── tvOS provider
├── Installation
├── Upgrade
├── Verification
└── LiveContainer file delivery

postgres
└── System of record

artifact-store
└── Original and signed IPA files
```

The signing worker and device agent should remain separate processes because they handle different sensitive materials and require different operating system permissions.

# Open source reuse strategy

## atvloadly

atvloadly should supply the working tvOS behaviour only (decision D2).

It is written in Go and contains Apple TV discovery, pairing, multiple Apple Account support, signing and automatic application refresh. Its web interface, SQLite persistence and cron scheduling are not reused; the control plane and dashboard are our own.

The preferred approach is to reuse its core as a Go helper behind a generic device provider interface:

```text
DeviceProvider
├── TVOSProvider
└── IOSProvider
```

Its current Docker configuration should not simply be copied unchanged. It uses a legacy Compose declaration, an unconfined seccomp profile and direct mounts of DBus and Avahi sockets. Those privileges should be isolated to the local edge agent rather than granted to the central Oracle control plane.

## Impactor

Impactor should supply the signing and Apple developer service implementation.

Its current workflow performs device registration, certificate creation, App ID registration, entitlement extraction, provisioning profile retrieval, application modification, signing through `apple-codesign-rs` and installation through `idevice`. It supports Linux and describes itself as supporting iOS, iPadOS and tvOS. It can also export P12 material for LiveContainer.

The graphical interface should not be embedded in the server. Reusable Rust modules should be extracted into a headless signing worker.

The worker interface should resemble:

```text
CreateAppleSession
RegisterDevice
CreateOrReuseCertificate
RegisterApplication
CreateProvisioningProfile
InspectEntitlements
SignIPA
ExportP12
RevokeCertificate
```

## idevice

`idevice` should be the primary device communication library.

It already exposes AFC, House Arrest, Installation Proxy, Install Coordination Proxy, provisioning profile management, pairing, TCP communication, usbmuxd communication, notification services and protected developer service tunnels.

The iOS provider should use it for:

```text
Device discovery
Device information
Installed application inventory
IPA staging
Application installation
Application upgrade
Application removal
Provisioning profile inspection
House Arrest access
Post installation verification
System logging
```

The project includes a minimal `ideviceinstaller` style implementation for installing and upgrading applications, which can be used as the first proof rather than writing an installer from scratch.

## idevice_pair

`idevice_pair` should provide the onboarding and recovery workflow.

It supports USB and WiFi device discovery, Lockdown and remote pairing records, validation, wireless debugging and writing pairing files directly into application Documents directories. It is cross platform and MIT licensed.

Its GUI may initially remain a separate setup utility. The underlying pairing logic can later be moved into the edge agent.

For persistent iPhone operation, Lockdown pairing created through usbmuxd should be preferred. The newer iOS 27 network onboarding flow should be treated as a convenience feature because the current tool documents that the resulting network pairing is retained only while the application remains open.

## iloader

iloader should remain an optional desktop onboarding and diagnostic utility.

It can install arbitrary IPAs, place pairing records, revoke development certificates and App IDs, and provide user friendly installation errors. It is built on `idevice`, Apple signing components and pairing logic that overlap directly with the proposed platform.

The project should reuse its concepts and permissively licensed components where appropriate, but it should not automate the iloader graphical interface.

## LiveContainer

LiveContainer should remain the guest application runtime.

It already handles guest application loading, multiple guest containers, application signing, application shortcuts and multiple LiveContainer instances. Its current documented installation process remains interactive: the user opens LiveContainer and selects an IPA using the plus button.

The first release should use official LiveContainer without modification. A later phase can introduce a maintained integration fork with a signed deployment inbox and App Intent.

LiveContainer supports iOS and iPadOS, not tvOS. Guest applications also have important limitations. They share host permissions, guest containers are not fully sandboxed, remote push notifications do not work and application extensions have restrictions.

There is currently a licensing inconsistency that must be resolved before redistribution. GitHub identifies the repository as AGPL 3.0 while the README text refers to Apache 2.0. The project must pin an exact commit and obtain a clear licence determination before shipping a fork.

## SideStore

SideStore should be treated as a reference implementation and optional compatibility mode, not a runtime dependency.

Its source remains valuable for understanding Apple authentication, provisioning, App ID management, profile renewal and on device installation. SideStore combines a sandboxed iOS application, Minimuxer and a loopback VPN mechanism to make the device communicate with a simulated local developer host.

The platform should avoid depending on this path because the external device agent can communicate with actual Apple services from outside the iOS sandbox.

## sidestore-vpn

`sidestore-vpn` should be offered only through an optional Compose profile.

The service creates a TUN interface, receives traffic intended for `10.7.0.1`, swaps source and destination addresses and reflects the packets to SideStore’s simulated local host. It is useful for users who still want SideStore, but it provides no benefit to the primary external installation agent.

## anisette-v3-server and Omnisette

A standalone anisette server should be optional.

Impactor already credits SideStore’s Grandslam and Omnisette work as part of its own authentication implementation. The preferred architecture is to use the authentication code inside the signing worker rather than making an HTTP request to a separate anisette container.

For SideStore or AltServer compatibility, `anisette-v3-server` can be exposed through an optional profile. It supports SideStore anisette v1 and v3 protocols and AltServer Linux, and provides an official Docker invocation with persistent state storage.

# Docker design

## Compose file layout

```text
deploy/
├── compose.yaml
├── compose.oracle.yaml
├── compose.edge.yaml
├── compose.compatibility.yaml
├── compose.monitoring.yaml
├── compose.development.yaml
├── secrets/
├── config/
└── scripts/
```

`compose.yaml` defines the common application model.

`compose.oracle.yaml` adds the Internet facing control plane, storage, scheduler and Tailscale connectivity.

`compose.edge.yaml` adds the local device agent, Avahi access, usbmuxd access and pairing vault.

`compose.compatibility.yaml` adds `sidestore-vpn` and `anisette-v3-server`.

`compose.monitoring.yaml` adds metrics and log collection.

`compose.development.yaml` adds source mounts, debug ports and test dependencies.

Docker Compose supports profiles and multiple Compose files, making optional compatibility and monitoring components possible without maintaining unrelated deployment instructions.

## Initial Oracle services

```text
gateway
control-plane
postgres
signing-worker
backup
tailscale
```

The GitHub watcher, scheduler and notifier should initially run inside `control-plane`. They can become separate workers when workload or operational requirements justify it.

IPA inspection, quarantine and repository management also run inside `control-plane` for the first release; a separate artifact worker is introduced only when workload justifies it (decision D9).

## Initial edge services

```text
device-agent
tailscale
```

The edge agent may require host access to:

```text
/var/run/usbmuxd
/var/run/dbus
/var/run/avahi-daemon
```

Only the edge host should receive those mounts.

For tvOS discovery, host networking may be preferable to routed bridge networking. This should be selected after testing rather than assumed.

## Optional services

```text
anisette-v3
sidestore-vpn
minio
prometheus
grafana
loki
```

MinIO should not be required for a single node installation. A filesystem backed artifact store is simpler for the first release.

## Persistent storage

The following data must survive container replacement:

```text
PostgreSQL database
Original IPA repository
Signed IPA cache
Application icons
Pairing records
Signing certificates and encrypted private keys
Tailscale node state
Anisette state when enabled
Audit records
Backup archives
```

Docker volumes persist independently from container recreation and support backup and restoration workflows. Tailscale also requires its state directory to persist or the container will appear as a new tailnet node after each restart.

## Secrets

Sensitive values must be mounted as Docker secrets rather than embedded into images or ordinary environment variables.

Secrets include:

```text
Database password
Master encryption key
Apple credential encryption key
GitHub App private key
Webhook secret
Session signing key
Backup encryption key
Tailscale authentication key during first enrolment
```

Docker Compose can grant individual services access to specific secrets as read only files under `/run/secrets`. Docker recommends this over environment variables because environment values can be exposed to processes and logs.

## Health and startup behaviour

Every long running container must define a health check.

PostgreSQL must be healthy before migrations run.

The API must be healthy before the gateway accepts traffic.

The signer must report both process health and Apple service readiness separately.

The device agent must distinguish between healthy process state and device availability.

Compose health checks and dependency conditions should be used rather than fixed sleep timers.

## Image publishing

Each image should be published for:

```text
linux/amd64
linux/arm64
```

This covers Oracle ARM instances, ordinary x86 servers and common NAS platforms.

Production deployments should use release tags or image digests rather than `latest`.

Each release should publish:

```text
Container image
SBOM
Source commit
Dependency manifest
Migration version
Image digest
Release notes
```

# Core data model

## User

Stores identity, role, authentication state and security settings.

## Agent

Represents a device communication node.

Fields include:

```text
Agent ID
Name
Architecture
Operating system
Software version
Tailnet identity
Connection state
Last heartbeat
Capabilities
```

## Device

Represents an iPhone, iPad or Apple TV.

Fields include:

```text
UDID
Platform
Device name
Model
Operating system version
Agent assignment
Pairing status
Developer mode state
Last successful connection
Last inventory scan
```

## Apple account

Stores an encrypted credential reference rather than plain credentials.

Fields include:

```text
Account label
Team identifier
Team type
Authentication state
Two factor state
Last successful authentication
Failure count
Locked status
Registered App ID count
Registered device count
```

## Certificate

Fields include:

```text
Serial number
Team identifier
Creation time
Expiry time
Revocation state
Encrypted private key reference
Certificate fingerprint
```

## Application

Represents the logical application independent of version and platform.

```text
Application name
Publisher
Description
Icon
Default update policy
Trust classification
```

## Application channel

Represents a platform specific build stream.

```text
Application
Platform
Expected bundle identifier
Minimum operating system
Update source
Asset selection rule
Release channel
```

An application supporting both iOS and tvOS will normally have separate channels.

## Artifact

Represents one immutable IPA.

```text
SHA256
Filename
Version
Build number
Bundle identifier
Platform
Minimum operating system
Entitlements
Extensions
Source
Release ID
Release tag
Import time
Quarantine state
```

## Signed artifact

Represents a generated derivative.

```text
Source artifact hash
Device
Apple team
Certificate
Provisioning profile
Signed bundle identifier
Signing time
Profile expiry
Signed IPA hash
```

## Deployment

Represents the desired application state on a device.

```text
Device
Application channel
Deployment target
Desired version
Update policy
Refresh policy
Current state
```

The deployment target values are:

```text
direct
livecontainer
```

## Installation record

Represents the last verified device state.

```text
Installed version
Installed build
Installed bundle identifier
Provisioning expiry
Verification time
Installation method
```

## Job

Tracks all asynchronous work.

```text
Job type
Device
Application
State
Attempt
Progress
Created time
Start time
Completion time
Error category
Error details
Retry time
Idempotency key
```

## Audit event

Records every security relevant action.

```text
Actor
Action
Device
Application
Artifact hash
Previous state
New state
Timestamp
Result
```

# GitHub release update design

## Source configuration

The user should be able to paste either a repository URL or a Releases URL. The server normalises it into:

```text
owner
repository
```

A source configuration should contain:

```json
{
  "provider": "github",
  "repository": "owner/project",
  "channel": "stable",
  "assetPattern": "(?i).*ios.*\\.ipa$",
  "excludedAssetPattern": "(?i).*(debug|rootless).*",
  "expectedPlatform": "ios",
  "expectedBundleIdentifier": "com.example.application",
  "updatePolicy": "next_refresh",
  "releaseDelayHours": 24,
  "requireApproval": false
}
```

## Discovery

For stable updates, the watcher uses GitHub’s latest release endpoint. GitHub defines that result as the most recent release that is neither a draft nor a prerelease.

For beta channels, the watcher lists releases and applies prerelease and tag rules itself.

For repositories controlled by the user, the server can accept GitHub `release` webhooks.

For third party repositories, the server polls on a configurable schedule.

GitHub recommends webhooks instead of unnecessary polling. When polling is required, it recommends authenticated conditional requests using `ETag` or `Last-Modified`; a correctly authorised `304 Not Modified` response does not consume the primary rate limit.

## Asset selection

The source must never choose the first `.ipa` blindly.

Selection should consider:

```text
Filename regular expression
Excluded filename expression
Expected platform
Expected bundle identifier
Expected application name
Expected minimum operating system
Expected extension set
Expected publisher or signing provenance when available
```

When multiple assets still match, the update must enter manual review.

## Quarantine and inspection

Every downloaded release enters quarantine.

```text
Download asset
        │
        ▼
Calculate SHA256
        │
        ▼
Validate ZIP structure
        │
        ▼
Locate principal application bundle
        │
        ▼
Parse Info.plist
        │
        ▼
Inspect executable and entitlements
        │
        ▼
Validate platform and bundle identifier
        │
        ▼
Compare with configured source
        │
        ▼
Promote or reject
```

The original release asset is never modified.

## Version handling

The platform should store both:

```text
CFBundleShortVersionString
CFBundleVersion
```

It must not assume all publishers use semantic versioning correctly.

An update is considered new when one of the following applies:

The release ID changed and the artifact hash changed.

The application version increased.

The build number increased.

The administrator explicitly approved a different artifact.

A changed artifact with the same displayed version should be treated as suspicious and require approval unless the source configuration explicitly allows replacement builds.

## Update policies

### Next refresh

The new version is downloaded and verified immediately, but installed when the current application next needs refreshing.

This should be the default.

### Immediate

The server schedules installation as soon as the update passes validation.

### Approval required

The artifact is retained in quarantine until an administrator approves it.

### Delayed

The artifact becomes eligible after a configured delay.

### Pinned

The deployment remains on a chosen version while its provisioning profile continues to be renewed.

### Manual

The source is monitored but never automatically installed.

## Update during refresh

```text
Refresh job starts
        │
        ▼
Resolve deployment policy
        │
        ▼
Check for approved newer artifact
        │
        ├── No update
        │      ▼
        │   Resign installed version
        │
        └── Update available
               ▼
            Sign new version
               ▼
            Upgrade application
               ▼
            Verify version and expiry
               ▼
            Mark previous version as rollback candidate
```

The update path must use upgrade semantics with a stable signed bundle identifier. The service should not uninstall the previous version first.

# Phased implementation

## Phase A: Product specification and upstream audit

### Objective

Establish technical and licensing boundaries before writing production code.

### Work

Create architecture decision records covering the control plane language, signing worker boundary, device agent boundary, storage model and provider interface.

Replay the recorded decisions (D1 to D10) into the architecture decision records.

Pin exact upstream commits for:

```text
atvloadly
Impactor
idevice
idevice_pair
iloader
LiveContainer
SideStore
sidestore-vpn
anisette-v3-server
```

Build a dependency and licence register.

Resolve the LiveContainer licence inconsistency before modifying or redistributing it.

Document which upstream projects will be forked, imported as libraries, called as executables or used only as references.

Define the first supported platform matrix.

Define threat models for Apple credentials, signing keys, pairing records, uploaded IPAs and device communication.

### Deliverables

```text
ARCHITECTURE.md
UPSTREAM.md
LICENSING.md
THREAT_MODEL.md
SUPPORTED_PLATFORMS.md
Architecture decision records
Initial repository structure
```

### Exit criteria

Every reused component has a pinned commit and documented licence.

The relationship with atvloadly is decided.

The LiveContainer integration is classified as official upstream, local patch or maintained fork.

No production code begins while licensing remains ambiguous.

## Phase B: Device transport proof

### Objective

Prove that external installation and refreshing are possible before building the server.

### Work

Create a small Rust command line program using `idevice`.

Implement:

```text
List devices
Read device information
List installed applications
Sign a test IPA with Impactor
Install test IPA
Upgrade test IPA
Verify installed version
Query provisioning profiles
Access an app Documents directory
```

Test a USB connected iPhone first.

Enable wireless debugging and test the same operations over the local network.

Restart the iPhone and edge host, then repeat.

Test with the phone unlocked, locked and recently restarted.

Test iOS 27 remote pairing separately from persistent Lockdown pairing.

Run the same transport experiment through Oracle and Tailscale.

Perform equivalent Apple TV pairing and installation using atvloadly and Impactor.

### Deliverables

```text
transport-spike CLI (scaffolded in rust/transport-spike, pending device testing)
Captured structured logs
Compatibility results
Network topology decision
Documented pairing recovery procedure
```

### Exit criteria

An iOS application can be installed and upgraded while preserving data.

A tvOS application can be installed and upgraded.

The provisioning expiry can be read or reliably derived.

A pairing record survives the expected restart scenario.

The team has selected either direct Oracle communication or a local edge agent.

Failure at this phase blocks the server implementation until the transport design is corrected.

## Phase C: Docker and repository foundation

### Objective

Create a reproducible development and deployment environment.

### Work

Create the monorepo and shared schemas.

Build multi stage Dockerfiles for the Go control plane, Rust signer and Rust agent.

Add multi architecture image builds.

Create base, Oracle, edge and development Compose files.

Add PostgreSQL migrations.

Add Docker secrets support.

Add health checks and dependency ordering.

Add persistent named volumes.

Add an official Tailscale sidecar option with persistent state.

Tailscale’s Docker documentation requires a persistent state directory and supports authentication through container parameters.

### Deliverables

```text
docker compose up
```

must start a clean development stack.

```text
docker compose -f compose.yaml -f compose.oracle.yaml up -d
```

must start the Oracle stack.

```text
docker compose -f compose.yaml -f compose.edge.yaml up -d
```

must start the edge stack.

### Exit criteria

Fresh deployment succeeds on both amd64 and arm64.

Containers become healthy without manual ordering.

Database migrations are repeatable.

Container replacement does not lose persistent data.

No secret is present inside an image layer or committed configuration file.

## Phase D: Control plane and domain model

### Objective

Create the central source of truth.

### Work

Implement users, agents, devices, Apple Accounts, certificates, applications, channels, artifacts, deployments, jobs and audit events.

Implement agent enrolment using a one time enrolment token.

Implement a heartbeat and capability report.

Implement job claiming with idempotency and per device locking.

Use PostgreSQL as both the system of record and initial job queue. Avoid adding Redis until a demonstrated need exists.

Create the initial web dashboard.

### Deliverables

The dashboard displays agents, devices, applications, deployments and jobs.

The API supports agent registration, heartbeat and job acknowledgement.

### Exit criteria

Two agents cannot execute conflicting jobs for the same device.

Restarting the API or worker does not lose an in progress job.

Every state changing action creates an audit event.

## Phase E: IPA repository and inspection

### Objective

Provide safe, immutable application storage.

### Work

Implement manual IPA upload.

Calculate SHA256 during streaming upload.

Store original artifacts by content hash.

Extract application metadata, icon, platform, bundle identifier, version, minimum operating system, extensions and entitlements.

Reject malformed archives.

Create quarantine, approved and rejected states.

Create retention and cleanup policies for signed derivatives while retaining originals.

### Deliverables

The dashboard provides application and artifact views.

Every artifact displays its source, hash, metadata and validation result.

### Exit criteria

Uploading the same IPA twice produces one original artifact.

A malformed IPA cannot enter the approved repository.

The original IPA remains byte identical after signing operations.

## Phase F: Headless signing worker

### Objective

Convert Impactor’s signing functionality into a server worker.

### Work

Extract or wrap the reusable Impactor modules.

Implement Apple Account enrolment and two factor continuation.

Register devices and applications.

Track registered App ID and device counts against team limits for slot usage warnings (D12).

Create or reuse certificates safely.

Create provisioning profiles.

Process extensions and entitlements.

Sign device specific derivatives.

Return structured progress and errors.

Store certificate private keys through envelope encryption.

Implement certificate revocation and recovery.

### Deliverables

A documented internal signing API.

A worker container requiring access only to signing secrets and artifact storage.

### Exit criteria

The same source IPA can be signed independently for iOS and tvOS.

Signing is deterministic enough to reproduce the same metadata and entitlement decisions.

The control plane can distinguish authentication, certificate, provisioning, entitlement and code signing failures.

## Phase G: tvOS provider

### Objective

Deliver a working platform using the most mature server based path first.

### Work

Generalise atvloadly’s Apple TV implementation behind the provider interface.

Retain pairing, discovery, multiple accounts and automatic refresh (D12).

Move discovery related privileges into the edge container.

Record installed application inventory and verification results centrally.

Add tvOS specific platform validation to the IPA inspector.

### Deliverables

Apple TV enrolment from the dashboard.

Manual tvOS IPA installation.

Application refresh.

Installation verification.

### Exit criteria

A tvOS app remains functional through at least two automated refresh cycles.

Application data remains intact during upgrade.

Failure notifications identify whether the cause was discovery, pairing, signing or installation.

## Phase H: iOS and iPadOS provider

### Objective

Provide external installation without SideStore.

### Work

Implement the provider using `idevice`.

Support USB onboarding and wireless operation.

Store pairing records on the edge host, encrypted at rest.

Implement application inventory.

Implement direct install and upgrade.

Implement provisioning profile verification.

Implement recovery when the pairing record becomes invalid.

Implement an explicit connection capability state:

```text
connected
paired_but_unreachable
pairing_invalid
device_locked
developer_mode_required
offline
```

### Deliverables

iPhone and iPad enrolment.

Manual IPA installation.

Application upgrade.

Verified profile expiry.

Pairing repair workflow.

### Exit criteria

The provider installs and upgrades on each supported operating system version.

The edge agent recovers cleanly after device and agent restart.

No custom iPhone client is required.

SideStore is absent from the deployment path.

## Phase I: Refresh scheduler

### Objective

Keep applications signed without user intervention.

### Work

Calculate refresh eligibility from profile expiry.

Default to refreshing before the final validity window.

Introduce retry backoff and critical expiry alerts.

Check for an approved application update before resigning the current version.

Use a per device deployment lock.

Verify the application after installation rather than treating installer completion as proof.

Add policies for charging, network availability or maintenance windows only when those states can be measured reliably.

### Deliverables

Deployment calendar.

Upcoming expiry view.

Retry queue.

Failure notifications.

### Exit criteria

The scheduler completes repeated refresh cycles without duplication.

A failed refresh cannot remove the currently working application.

The system warns before expiry when retries continue to fail.

## Phase J: GitHub release updates

Planned after the first stable release (decision D7).

### Objective

Automatically acquire and deploy application updates.

### Work

Implement repository URL normalisation.

Implement stable, prerelease and pinned channels.

Implement GitHub App authentication for private repositories.

Implement polling with conditional requests.

Implement webhook handling for managed repositories.

Implement asset matching rules.

Download assets to quarantine.

Validate identity and platform.

Add next refresh, immediate, delayed, approval and pinned policies.

Add release notes and source history to the dashboard.

### Deliverables

A user can paste a GitHub Releases URL and select an IPA asset pattern.

The server identifies the latest valid release.

The approved update is used during the next resign.

### Exit criteria

A repository with multiple IPA assets selects the correct platform build.

A bundle identifier change is quarantined.

A modified same version artifact is flagged.

A failed new version can be rolled back to the previous stored artifact.

## Phase K: LiveContainer host management

### Objective

Support LiveContainer without initially modifying it.

### Work

Add LiveContainer as a managed iOS application.

Install and refresh the host through the external iOS provider.

Generate and manage its required P12 material through the signing worker.

Add LiveContainer as a deployment target.

Generate a private application source or direct download location for guest applications.

Initially require the user to confirm guest import inside official LiveContainer.

### Deliverables

Install LiveContainer from the dashboard.

Refresh LiveContainer automatically.

Select an application for LiveContainer deployment.

Generate a guest installation link or source entry.

### Exit criteria

Refreshing the LiveContainer host preserves guest applications and guest data.

Guest applications can be installed from artifacts stored by Sidey.

The dashboard distinguishes host version, guest version and host provisioning expiry.

## Phase L: Automated LiveContainer guest delivery

### Objective

Reduce guest installation to opening LiveContainer or invoking one Shortcut action.

### Work

Resolve the upstream licensing position.

Create a minimal LiveContainer integration fork only when necessary.

Add a signed inbox format under LiveContainer’s Documents directory.

Use House Arrest and AFC from the device agent to deliver the IPA and job manifest.

Add manifest signature verification.

Add hash verification.

Add install, update and remove actions.

Preserve guest data during updates.

Add an App Intent named Process Pending Deployments.

Report results back to the control plane.

### Deliverables

```text
Documents/SideloadInbox/
├── job.json
├── application.ipa
└── signature.json
```

A Shortcut can open LiveContainer and run Process Pending Deployments.

### Exit criteria

The agent can deliver a guest IPA without the Files picker.

LiveContainer rejects altered or unauthorised jobs.

Updating a guest preserves its selected data container.

A failed guest update leaves the previous guest usable.

Complete background silence should not be promised because iOS controls whether a suspended application receives execution time.

## Phase M: Security hardening

### Objective

Protect credentials, devices and application provenance.

### Work

Implement envelope encryption for Apple credentials, pairing records and signing keys.

Separate key encryption keys from the database.

Restrict each worker to the minimum secrets and storage it needs.

Add role based access (single admin active in the first release, decision D4).

Add multi factor authentication for administrators.

Add Tailscale ACL guidance.

Validate webhook signatures and prevent replay.

Redact secrets and pairing material from logs.

Add account lockout protection.

Add application trust classifications.

Add dependency and container scanning.

Add signed container releases.

### Deliverables

Security deployment guide.

Secret rotation procedure.

Incident response procedure.

Pairing revocation procedure.

### Exit criteria

A database backup alone cannot decrypt credentials.

The control plane cannot read raw signing private keys without the configured encryption service.

The device agent cannot access unrelated GitHub or Apple credentials.

No secret appears in normal or debug logs.

## Phase N: Observability, backups and recovery

### Objective

Make the platform maintainable rather than merely functional.

### Work

Add structured JSON logs.

Add metrics for jobs, signing, device connectivity, release polling and expiry risk.

Add a support bundle export with automatic secret redaction.

Back up PostgreSQL and original artifacts.

Back up encrypted pairing records separately.

Test restoration to a clean host.

Add storage integrity verification.

Add stale signed artifact cleanup.

### Deliverables

Health dashboard.

Backup schedule.

Restore script.

Support bundle.

### Exit criteria

A clean deployment can be restored using documented backups.

Original artifacts retain verified SHA256 values.

A lost edge agent can be replaced and devices reattached through a documented recovery path.

## Phase O: Public beta and release hardening

### Objective

Turn the engineering system into a distributable project.

### Work

Create installation documentation for Oracle, generic Linux, UGREEN NAS and Raspberry Pi.

Publish versioned Compose bundles.

Publish amd64 and arm64 images.

Create upgrade and rollback documentation.

Create a compatibility reporting format.

Run long duration refresh testing.

Test current stable and beta Apple operating systems.

Test free and paid Apple developer teams.

Test large IPAs, application extensions, unusual entitlements and interrupted installations.

### Release gate

The first stable release requires:

```text
Reliable tvOS installation and refresh
Reliable iOS installation and refresh
Managed IPA storage
Rollback
Docker Compose deployment
Encrypted secrets
Backup and restore
Auditable jobs
LiveContainer host management
Manual or assisted LiveContainer guest import
```

Automated LiveContainer guest inbox processing can ship separately if it risks delaying the reliable direct installation platform.

# Testing strategy

## Unit tests

Cover source URL normalisation, release selection, version comparison, asset matching, profile expiry calculations, job transitions, manifest validation and access control.

## Integration tests

Use recorded or mocked Apple and GitHub responses for normal CI.

Test PostgreSQL migrations from every supported release.

Test worker restarts during signing and installation.

Test duplicate webhook delivery.

Test repeated job submission using the same idempotency key.

## Device tests

Maintain at least one test device for each provider.

```text
iPhone
iPad when available
Apple TV
```

Test states include:

```text
Unlocked
Locked
Recently restarted
Offline
Pairing invalidated
Developer mode disabled
Certificate revoked
Apple login requiring two factor authentication
Profile near expiry
Application already installed
Application data migration failure
```

## Network tests

Test local LAN operation.

Test Oracle through a Tailscale connected edge agent.

Test direct Oracle communication as an experimental mode.

Test loss of Tailscale during installation.

Test agent reconnect and job resumption.

## Update tests

Test a GitHub release with one IPA.

Test multiple iOS and tvOS IPAs.

Test draft and prerelease handling.

Test the same version with a changed hash.

Test a changed bundle identifier.

Test an update that cannot be installed.

Test rollback.

# Recommended repository structure

```text
sidey-server/
├── cmd/
│   ├── control-plane/
│   ├── signing-worker/
│   ├── device-agent/
│   └── sideyctl/
├── internal/
│   ├── api/
│   ├── auth/
│   ├── audit/
│   ├── artifacts/
│   ├── deployments/
│   ├── githubsource/
│   ├── jobs/
│   ├── notifications/
│   ├── scheduler/
│   └── storage/
├── rust/
│   ├── signer/
│   ├── device-agent/
│   ├── ios-provider/
│   ├── tvos-provider/
│   └── ipa-inspector/
├── web/
├── schemas/
│   ├── api/
│   ├── agent/
│   └── livecontainer/
├── migrations/
├── deploy/
│   ├── compose.yaml
│   ├── compose.oracle.yaml
│   ├── compose.edge.yaml
│   ├── compose.compatibility.yaml
│   ├── compose.monitoring.yaml
│   └── compose.development.yaml
├── packaging/
│   ├── docker/
│   └── systemd/
├── docs/
│   ├── architecture/
│   ├── installation/
│   ├── pairing/
│   ├── security/
│   └── troubleshooting/
└── third_party/
    ├── NOTICE.md
    └── versions.lock
```

# Recommended first engineering action

Begin with Phase A and Phase B only.

Do not start with the dashboard.

The first milestone should be a command line proof that can:

```text
Detect an iPhone
Validate its pairing
List installed applications
Install a signed test IPA
Upgrade that IPA
Verify its version and provisioning expiry
Repeat wirelessly after a restart
```

The second milestone should repeat the same lifecycle on Apple TV using the atvloadly and Impactor path.

Once both providers are proven, the Docker control plane, repository and update scheduler become conventional engineering rather than an uncertain protocol experiment.
