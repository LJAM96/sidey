-- Per-app update subscriptions (Phase 3).
--
-- Watched bundle IDs that are not necessarily deployed. An entry surfaces
-- in the updates listing when a catalog snapshot is newer than the version
-- seen at subscribe time (or on first appearance, when unknown then).
-- Importing a version advances its seen baseline.
CREATE TABLE app_subscriptions (
    bundle_id text PRIMARY KEY,
    name text NOT NULL DEFAULT '',
    seen_version text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE app_subscriptions IS 'Watched bundle IDs for update detection; detection only, never auto-installed.';
