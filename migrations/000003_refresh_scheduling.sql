-- Refresh scheduling (Phase I): track when each deployment's profile expires
-- and when the next refresh is due, plus the outcome of the last refresh.
-- The scheduler (in-process in the control plane) creates `refresh` jobs when
-- a deployment becomes due; the refresh agent runs the install and reports
-- back, and the job completion hook updates these columns.

ALTER TABLE deployments
    ADD COLUMN next_refresh_due_at timestamptz,
    ADD COLUMN last_refresh_at timestamptz,
    ADD COLUMN last_refresh_result text,
    ADD COLUMN last_refresh_error text;

CREATE INDEX idx_deployments_refresh_due ON deployments(next_refresh_due_at);
