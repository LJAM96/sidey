-- Phase F: certificate lifecycle control.
--
-- The signing worker upserts certificates by serial and reuses the account's
-- certificate identity across jobs. The control plane adds deliberate
-- revocation (with a reason and timestamp) so operators can retire a cert
-- without touching the portal, and so slot-usage views can distinguish
-- revoked certificates from live ones.

ALTER TABLE certificates
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN revoked_reason text;

CREATE INDEX idx_certificates_revoked ON certificates (revoked);