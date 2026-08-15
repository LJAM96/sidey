-- 000008_security_hardening.sql
-- Phase M (security hardening) follow-up to the threat model audit:
--
--  1. Server-controlled agent roles. `agents.role` is assigned from the
--     enrolment token (an admin-controlled grant), never from client supplied
--     capabilities. `agent_enrolment_tokens.role` records the role a token
--     grants when it is consumed.
--
--  2. Enrolment token public key id. Agent API keys already resolve via
--     `api_key_id`; the same pattern is applied to enrolment tokens so an
--     unauthenticated enrolment request can locate exactly one candidate row
--     (indexed) before running the single bcrypt verification, instead of
--     scanning every outstanding hash (bcrypt CPU amplification).
--
--  3. Finite job retry bookkeeping: `max_attempts`, `dead` state and
--     `requires_attention` are read by the reaper so permanently broken work
--     (revoked certificates, auth failures, corrupt IPAs) stops retrying
--     forever. The `jobs.state` column is free text, so this only adds the
--     metadata columns used by the job service.

ALTER TABLE agents
    ADD COLUMN role text NOT NULL DEFAULT 'device_agent';

ALTER TABLE agent_enrolment_tokens
    ADD COLUMN role text NOT NULL DEFAULT 'device_agent',
    ADD COLUMN token_key text;

CREATE UNIQUE INDEX uq_enrolment_tokens_key
    ON agent_enrolment_tokens (token_key)
    WHERE token_key IS NOT NULL;

ALTER TABLE jobs
    ADD COLUMN max_attempts integer NOT NULL DEFAULT 5,
    ADD COLUMN requires_attention boolean NOT NULL DEFAULT false,
    ADD COLUMN last_failure_class text,
    ADD COLUMN dead_reason text;

CREATE INDEX idx_jobs_dead ON jobs(state, requires_attention)
    WHERE state = 'dead';

-- Supported agent roles. Enforced in Go by the same allowlist; this
-- constraint keeps the database self-consistent too.
ALTER TABLE agents
    ADD CONSTRAINT chk_agent_role CHECK (
        role IN ('device_agent', 'refresh_agent', 'signing_worker', 'tvos_agent'));
