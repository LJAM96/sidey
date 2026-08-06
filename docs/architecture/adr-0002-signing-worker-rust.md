# ADR-0002: Signing worker in Rust, extracted from Impactor

- Status: Accepted
- Date: 2026-08-06
- Related: plan.md "Impactor" reuse section, D6

## Context

Impactor (MIT, Rust) already performs Apple authentication, device registration, certificate creation, App ID registration, entitlement extraction, provisioning profile retrieval, IPA modification and signing through `apple-codesign-rs`. The graphical interface must not be embedded in the server.

## Decision

Extract or wrap the reusable Impactor modules into a headless Rust signing worker exposing a documented internal API (CreateAppleSession, RegisterDevice, CreateOrReuseCertificate, RegisterApplication, CreateProvisioningProfile, InspectEntitlements, SignIPA, ExportP12, RevokeCertificate).

## Rationale

- Reuses verified Apple service logic instead of reimplementing it.
- Impactor is MIT, so the extracted code can be incorporated without copyleft obligations.
- Rust matches the surrounding device agent ecosystem (`idevice` crate).
- Keeps Apple credentials and signing keys inside a dedicated worker with minimal secrets (plan "Initial service model").

## Consequences

- The worker must report authentication, certificate, provisioning, entitlement and code signing failures as distinct error categories (Phase F exit criteria).
- Private keys are stored through envelope encryption (Phase M).
- Phase B uses a manual Impactor CLI run to sign test IPAs (D6).
