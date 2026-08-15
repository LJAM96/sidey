# ADR-0008: VPS-hosted device service is the default; remote node optional

- Status: Accepted
- Date: 2026-08-15
- Related: plan.md D1, D13, ADR-0003, ADR-0005

## Context

The platform first documented a control plane on an Oracle VPS plus a
separate local "edge" host carrying a "device agent" (plan "Target
architecture"). During Phase B the direct Oracle-to-device topology (D13) was
validated: wireless iOS install/upgrade/verify over the tailnet RSD tunnel on
a locked phone with restarts, and Phase G proved Apple TV over the tailnet
(RemotePairing PIN, RSD tunnel and tvOS-family installs). Initial iOS pairing
is bootstrapped over a VirtualHere (or usbip) USB-over-tailnet session.

That made a required distributed "edge" tier an unnecessary assumption: the
old wording ("edge agent", "device agent", "edge host") described a forced
deployment topology rather than what ships. The security boundaries that
matter are between processes with different secrets, not between machines.

## Decision

- The default deployment is fully VPS hosted. The device service (renamed
  from "device agent") runs on the same Oracle VPS as the control plane,
  signing worker, PostgreSQL and artifact store (D13).
- The control plane and device service communicate over a localhost Unix
  socket (e.g. `/run/sidey/device.sock`) guarded by filesystem permissions.
  No enrolment tokens, API keys, heartbeat or capability handshakes are
  required for the same-host default.
- Optional "remote node" mode: the same device service on a separate host
  over the tailnet, for multi-site installs. It uses the existing agent
  enrolment protocol and is recorded as a device service node.
- The signing worker remains a separate process on the same host with its own
  internal API; it holds Apple credentials and signing keys, never pairing
  records or device mounts.
- Terminology: "edge agent"/"device agent" becomes "device service"; "edge
  host" becomes the device host (the VPS by default, an optional remote node
  in multi-site mode).

## Rationale

- All three processes can share the VPS safely because they are isolated by
  process and Unix-socket boundaries, each holding only its own secrets
  (THREAT_MODEL.md "Process boundaries").
- Dropping required remote machinery removes enrolment tokens, bearer keys
  and capability plumbing from the happy path, shrinking the attack surface
  and the operational footprint.
- The device service keeps receiving only pairing records and signed IPAs; it
  never holds Apple credentials or signing keys (KEY: separation preserved
  even in a single-host deployment).
- Direct communication was no longer "experimental": D13 was proven in Phase B
  and Phase G.

## Consequences

- Documentation (plan.md, ARCHITECTURE.md, THREAT_MODEL.md, recovery.md,
  SUPPORTED_PLATFORMS.md) describes the VPS default with the remote node as
  optional.
- Deployment: the device service belongs in the default Compose stack (base
  model or a `compose.device.yaml` overlay); `compose.edge.yaml` is repurposed
  as the optional remote-node overlay.
- Code: the renamed `device-service` crate (formerly `device-agent`) is the
  device service; internal API agent endpoints are retained for the optional
  remote-node protocol, and a same-host Unix socket transport is added.
- Provider interface and composition decisions (ADR-0003, ADR-0005) are
  unchanged; only the deployment context changed.
