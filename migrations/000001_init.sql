-- 000001_init.sql
-- Sidey core data model (plan.md "Core data model", Phase D).
-- PostgreSQL 16+. Migration files apply in lexical order by deploy/migrate.

CREATE TABLE users (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username          text NOT NULL UNIQUE,
    role              text NOT NULL DEFAULT 'admin',
    password_hash     text,
    mfa_enabled       boolean NOT NULL DEFAULT false,
    mfa_secret_ref    text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agents (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL,
    architecture      text,
    operating_system  text,
    software_version  text,
    tailnet_identity  text,
    connection_state  text NOT NULL DEFAULT 'offline',
    last_heartbeat_at timestamptz,
    capabilities      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    udid                  text NOT NULL UNIQUE,
    platform              text NOT NULL,
    device_name           text,
    model                 text,
    os_version            text,
    agent_id              uuid REFERENCES agents(id) ON DELETE SET NULL,
    pairing_status        text NOT NULL DEFAULT 'unknown',
    developer_mode_enabled boolean,
    last_connected_at     timestamptz,
    last_inventory_scan_at timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_devices_agent ON devices(agent_id);

CREATE TABLE apple_accounts (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    label                    text NOT NULL,
    team_identifier          text,
    team_type                text NOT NULL DEFAULT 'free',
    auth_state               text NOT NULL DEFAULT 'not_authenticated',
    two_factor_state         text NOT NULL DEFAULT 'not_started',
    last_auth_at             timestamptz,
    failure_count            integer NOT NULL DEFAULT 0,
    locked                   boolean NOT NULL DEFAULT false,
    registered_app_id_count  integer NOT NULL DEFAULT 0,
    registered_device_count  integer NOT NULL DEFAULT 0,
    credential_ref           text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE certificates (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    serial_number text,
    account_id    uuid NOT NULL REFERENCES apple_accounts(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expiry_at     timestamptz,
    revoked       boolean NOT NULL DEFAULT false,
    key_ref       text NOT NULL,
    fingerprint   text,
    CONSTRAINT uq_cert_serial UNIQUE (serial_number)
);

CREATE INDEX idx_certificates_account ON certificates(account_id);

CREATE TABLE applications (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  text NOT NULL,
    publisher             text,
    description           text,
    icon_ref              text,
    default_update_policy text NOT NULL DEFAULT 'next_refresh',
    trust_classification  text NOT NULL DEFAULT 'unclassified',
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE application_channels (
    id                         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id             uuid NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    platform                   text NOT NULL,
    expected_bundle_identifier text,
    min_os_version             text,
    update_source              jsonb,
    asset_selection            jsonb,
    release_channel            text NOT NULL DEFAULT 'stable',
    account_id                 uuid REFERENCES apple_accounts(id),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_channel_app_platform UNIQUE (application_id, platform, release_channel)
);

CREATE TABLE artifacts (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256            text NOT NULL UNIQUE,
    filename          text NOT NULL,
    version           text,
    build_number      text,
    bundle_identifier text,
    platform          text,
    min_os_version    text,
    entitlements      jsonb,
    extensions        jsonb,
    source            text,
    release_id        text,
    release_tag       text,
    quarantine_state  text NOT NULL DEFAULT 'quarantined',
    imported_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_artifacts_bundle ON artifacts(bundle_identifier);

CREATE TABLE signed_artifacts (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_artifact_id       uuid NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    device_id                uuid NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
    account_id               uuid NOT NULL REFERENCES apple_accounts(id),
    certificate_id           uuid REFERENCES certificates(id),
    provisioning_profile_ref text,
    signed_bundle_identifier text,
    signed_at                timestamptz NOT NULL DEFAULT now(),
    profile_expiry_at        timestamptz,
    signed_ipa_sha256        text,
    created_at               timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_signed_source ON signed_artifacts(source_artifact_id);
CREATE INDEX idx_signed_device ON signed_artifacts(device_id);

CREATE TABLE deployments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id           uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    channel_id          uuid NOT NULL REFERENCES application_channels(id) ON DELETE CASCADE,
    target              text NOT NULL DEFAULT 'direct',
    desired_version     text,
    update_policy       text NOT NULL DEFAULT 'next_refresh',
    refresh_policy      jsonb,
    current_state       text NOT NULL DEFAULT 'pending',
    rollback_artifact_id uuid REFERENCES artifacts(id),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_deployment_device_channel UNIQUE (device_id, channel_id),
    CONSTRAINT chk_deployment_target CHECK (target IN ('direct', 'livecontainer'))
);

CREATE TABLE installation_records (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id             uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    installed_version         text,
    installed_build           text,
    installed_bundle_identifier text,
    provisioning_expiry_at    timestamptz,
    verified_at               timestamptz,
    installation_method       text NOT NULL DEFAULT 'direct',
    created_at                timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_installation_deployment ON installation_records(deployment_id);

CREATE TABLE jobs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type          text NOT NULL,
    device_id         uuid REFERENCES devices(id),
    application_id    uuid REFERENCES applications(id),
    state             text NOT NULL DEFAULT 'pending',
    attempt           integer NOT NULL DEFAULT 0,
    progress          integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    completed_at      timestamptz,
    error_category    text,
    error_details     text,
    retry_at          timestamptz,
    idempotency_key   text NOT NULL UNIQUE,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_jobs_state ON jobs(state, retry_at);
CREATE INDEX idx_jobs_device ON jobs(device_id);

CREATE TABLE audit_events (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor          text NOT NULL,
    action         text NOT NULL,
    device_id      uuid,
    application_id uuid,
    artifact_sha256 text,
    previous_state jsonb,
    new_state      jsonb,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    result         text NOT NULL DEFAULT 'ok'
);

CREATE INDEX idx_audit_occurred ON audit_events(occurred_at DESC);
