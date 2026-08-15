-- 000009_device_service.sql
-- ADR-0008: the same-host device service is the default deployment. It is
-- represented in `agents` by a node row keyed by a fixed, non-secret
-- api_key_id sentinel and role 'device_service' (see internal/api/device.go).
-- Remote-node device services enrol with the same role via a normal
-- enrolment token. This migration simply extends the server-controlled role
-- allowlist so both forms of the role are representable in the database.
--
-- No data migration is required: the default topology provisions its node
-- row lazily on first use and through the Unix socket, not through SQL here.

ALTER TABLE agents
    DROP CONSTRAINT chk_agent_role,
    ADD CONSTRAINT chk_agent_role CHECK (
        role IN ('device_agent', 'refresh_agent', 'signing_worker', 'tvos_agent', 'device_service'));
