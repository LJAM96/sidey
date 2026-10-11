-- Update detection snapshots for AltStore-format app sources (Phase 2).
--
-- The source watcher records the newest catalog listing per (source,
-- bundle ID). Update detection compares deployed versions against this
-- snapshot; nothing is downloaded or installed automatically. Snapshots
-- refresh on every fresh catalog fetch (dashboard browse or watcher loop).
CREATE TABLE app_source_listings (
    source_id text NOT NULL,
    bundle_id text NOT NULL,
    name text NOT NULL DEFAULT '',
    latest_version text NOT NULL DEFAULT '',
    latest_download_url text NOT NULL DEFAULT '',
    latest_size bigint NOT NULL DEFAULT 0,
    latest_updated_at text NOT NULL DEFAULT '',
    seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, bundle_id)
);
CREATE INDEX idx_source_listings_bundle ON app_source_listings(bundle_id);
COMMENT ON TABLE app_source_listings IS 'Newest catalog listing per (source, bundle ID) for update detection; snapshots only, never auto-installed.';
