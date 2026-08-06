# ADR-0004: Storage model

- Status: Accepted
- Date: 2026-08-06
- Related: plan.md "Persistent storage", D5, D9

## Context

The platform stores structured records (users, devices, deployments, jobs, audit events), immutable original IPAs, signed derivatives, pairing records, encrypted signing keys and backup archives. Scale is one user and a small number of devices (D5).

## Decision

- PostgreSQL is the system of record, the initial job queue and the audit store. No Redis.
- Original IPAs are stored on a filesystem artifact store, content addressed by SHA256, byte identical after signing.
- Docker named volumes persist everything that must survive container replacement (PostgreSQL data, artifact repository, pairing records, encrypted keys, Tailscale state, backups).
- MinIO is not required; a filesystem backed store is used for the first release.

## Rationale

- A content addressed store makes deduplication free (uploading the same IPA twice produces one artifact) and gives immutable version history.
- PostgreSQL as the job queue is adequate at the D5 scale; Redis is added only when a demonstrated need exists.
- Filesystem storage is simpler to back up and restore than object storage for a single node.

## Consequences

- Job claiming must use idempotency keys and per device locking in the database (Phase D exit criteria).
- Storage integrity verification and stale signed artifact cleanup are scheduled for Phase N.
