-- Phase F: add a sha256-derived key id to agents so the API can resolve an
-- agent API key with a single indexed query instead of scanning every stored
-- bcrypt hash. Existing (legacy) rows keep their api_key_hash and are still
-- verified by the fallback path.

ALTER TABLE agents
    ADD COLUMN api_key_id text;

CREATE UNIQUE INDEX uq_agents_api_key_id
    ON agents (api_key_id)
    WHERE api_key_id IS NOT NULL;