-- Device API tokens for the mobile companion (Phase 0).
--
-- Long-lived, independently revocable bearer credentials with narrow scopes
-- (read, control). Secrets are bcrypt hashes behind a sha256 key id, same
-- scheme as agent keys; only the key id is indexed.
CREATE TABLE device_api_tokens (
    id uuid NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    name text NOT NULL,
    key_id text NOT NULL UNIQUE,
    token_hash text NOT NULL,
    scopes text[] NOT NULL DEFAULT '{read}',
    device_id uuid REFERENCES devices(id) ON DELETE SET NULL,
    expires_at timestamptz,
    revoked_at timestamptz,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_device_tokens_key ON device_api_tokens(key_id);
COMMENT ON TABLE device_api_tokens IS 'Scoped mobile-companion credentials; narrow, revocable, audited.';
