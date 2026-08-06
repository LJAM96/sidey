# ADR-0001: Control plane language

- Status: Accepted
- Date: 2026-08-06
- Related: plan.md D2, D5

## Context

The platform needs a control plane that owns the REST API, web dashboard, scheduler, job queue client, IPA inspection and audit. Options considered were Go, Rust and TypeScript. The plan reuses atvloadly (Go) for tvOS device logic and a Rust signer/agent for device and Apple services. The control plane does not need direct access to Apple services or device communication.

## Decision

Write the control plane in Go.

## Rationale

- The team's existing hard protocol work (tvOS pairing logic in atvloadly, signing in Impactor) is split Go and Rust; the control plane is the integration layer and Go is the most maintainable choice for CRUD, HTTP, scheduling and PostgreSQL.
- A separate security sensitive worker (signer) and agent already cover the domains where Rust earns its keep; the control plane benefits more from Go's deployment simplicity and fast iteration.
- Single static binary fits multi architecture Docker images.

## Consequences

- Two languages in the repository (Go control plane, Rust signer/agent, Go tvOS helper). Boundaries are process level so this stays manageable.
- The dashboard frontend is our own implementation (D2) and is language independent.
