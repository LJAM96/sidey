-- Phase F: bind signed-derivative uploads to the sign job that produced them.
-- A holder of an agent key must not be able to fabricate signed-artifact
-- records or inject IPA bytes without holding the matching sign job.

ALTER TABLE signed_artifacts
    ADD COLUMN job_id uuid REFERENCES jobs(id);