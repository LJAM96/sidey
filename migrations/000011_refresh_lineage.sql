-- Refresh lineage: explicit parent workflow and authoritative install state.
--
-- A refresh is a parent workflow (refresh -> sign child -> install child).
-- installation_records.signed_artifact_id becomes the authoritative answer
-- to "which signed IPA is installed for this deployment", replacing the
-- device-level latest-install-job heuristic in the scheduler.

-- Workflow relationships on jobs. parent_job_id is SET NULL (not cascading)
-- because the reaper deletes dead rows; deleting a dead parent must not take
-- its children with it, nor fail on the constraint.
ALTER TABLE jobs
    ADD COLUMN parent_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL;
ALTER TABLE jobs
    ADD COLUMN purpose text;
CREATE INDEX idx_jobs_parent ON jobs(parent_job_id);
COMMENT ON COLUMN jobs.parent_job_id IS 'Parent workflow job (e.g. refresh); sign/install children point here.';
COMMENT ON COLUMN jobs.purpose IS 'Workflow purpose: deploy, refresh, manual_refresh, update. NULL for standalone jobs.';

-- Authoritative install state per deployment. One installation record per
-- deployment: the scheduler and dashboard join on deployment_id assuming a
-- single row, so enforce it.
ALTER TABLE installation_records
    ADD COLUMN signed_artifact_id uuid REFERENCES signed_artifacts(id) ON DELETE SET NULL;
ALTER TABLE installation_records
    ADD COLUMN install_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL;
CREATE INDEX idx_installation_signed_artifact ON installation_records(signed_artifact_id);
ALTER TABLE installation_records
    ADD CONSTRAINT uq_installation_deployment UNIQUE (deployment_id);
COMMENT ON COLUMN installation_records.signed_artifact_id IS 'Signed artifact currently installed for this deployment (authoritative; schedulers resolve source IPA and Apple account through it).';

-- Backfill where unambiguous: latest completed install per deployment whose
-- artifact is a known signed derivative with a matching bundle identifier.
-- Deployments with no matching install history keep NULL (scheduler treats
-- them as not refreshable rather than guessing).
UPDATE installation_records ir
SET signed_artifact_id = sub.said,
    install_job_id = sub.jid
FROM (
    SELECT DISTINCT ON (dep.id)
        dep.id AS depid,
        sa.id AS said,
        j.id AS jid
    FROM deployments dep
    JOIN jobs j
        ON j.device_id = dep.device_id
        AND j.job_type = 'install'
        AND j.state = 'completed'
    JOIN signed_artifacts sa
        ON sa.id = CASE
            WHEN j.parameters->>'artifact_id' ~ '^[0-9a-fA-F-]{36}$'
            THEN (j.parameters->>'artifact_id')::uuid
            ELSE NULL
        END
    WHERE j.parameters->>'bundle_id' IS NULL
        OR j.parameters->>'bundle_id' = sa.signed_bundle_identifier
    ORDER BY dep.id, j.created_at DESC
) sub
WHERE ir.deployment_id = sub.depid
    AND ir.signed_artifact_id IS NULL;
