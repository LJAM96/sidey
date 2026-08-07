-- 000002_agent_enrolment.sql
-- Phase D: agent enrolment tokens, agent API keys, job leases and payloads.

ALTER TABLE agents
    ADD COLUMN api_key_hash text,
    ADD COLUMN enrolled_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

CREATE TABLE agent_enrolment_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash    text NOT NULL,
    label         text NOT NULL,
    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,
    used_at       timestamptz,
    used_by_agent uuid REFERENCES agents(id)
);

CREATE INDEX idx_enrolment_tokens_hash ON agent_enrolment_tokens(token_hash);

ALTER TABLE jobs
    ADD COLUMN claimed_by       uuid REFERENCES agents(id),
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN parameters       jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN result           jsonb;

CREATE INDEX idx_jobs_pending_device ON jobs(device_id, state)
    WHERE state = 'pending';
CREATE INDEX idx_jobs_lease ON jobs(lease_expires_at)
    WHERE lease_expires_at IS NOT NULL;
