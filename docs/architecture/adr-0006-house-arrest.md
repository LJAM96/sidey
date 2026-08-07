# ADR-0006: House Arrest is unavailable for personal-team-signed apps

- Status: Accepted
- Date: 2026-08-07
- Related: plan.md D13, ADR-0005

## Context

The Phase B transport proofs (usb mode and rsd mode) install a test app
(`org.sidey.phasetest`) signed for a free/personal Apple developer team via the
isideload signonly binary. The transport-spike also probes `house_arrest`
(`documents` subcommand, `VendDocuments` over AFC) as a feature that could be
used later to deliver the IPA bundle and job manifest into an app sandbox
(i.e. the upstream-Impactor/SideStore "Documents" delivery path).

On the test device the probe returns
`InstallationLookupFailed`/`ApplicationLookupFailed` from the device. The
plan/ADR-0005 assumed House Arrest is a baseline capability ("the `idevice`
Rust library already implements ... House Arrest ... required low level
functionality") with a reuse list entry: "House Arrest access".

## Decision

House Arrest is **unavailable for apps signed with a free (personal team)
provisioning profile**. A `VendDocuments` call against such an app returns
`InstallationLookupFailed` (and `ApplicationLookupFailed` when addressed by
team-prefixed full app id). The device only grants container access to apps
signed under a team where the host is registered as a developer (paid
account), or those distributed outside a personal team.

The proof therefore records documents as a "metrics collected, expected
refusal" soft-fail, not a transport defect. This reflects an Apple policy, not
a Sidey bug.

## Consequences

- Sidey's device agent must not depend on House Arrest for personal-team
  targets; the sandbox/Documents path to On-VPS file delivery needs an
  alternate mechanism (e.g. sideload of a helper with the target in a shared
  container is also affected; treat as a feature risk for that route).
- The `documents` proof step uses `soft_step` so the D1.3 verdict remains
  "viable" (install/upgrade/verify/uninstall/pairing all pass).
- ADR-0005's feature list should note this restriction as a provider
  capability flag for the agent to report to the dashboard.

## Consequences

- Phone-side: House Arrest docs access CANNOT be used as evidence of the
  D13 topology viability.
- Future: verify is still trustworthy for profile expiry; container
  delivery will need a tunneled/Documents-free route.