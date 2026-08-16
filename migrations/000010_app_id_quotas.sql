-- 000010_app_id_quotas.sql
-- Persist Apple's authoritative App ID quota alongside the registration count.
-- listAppIds returns maxQuantity/availableQuantity; free teams report
-- maxQuantity 10 (not the commonly assumed 3, which is the concurrent
-- provisioning/template-profile limit). The dashboard shows the real quota
-- instead of a hardcoded 3.

ALTER TABLE apple_accounts
    ADD COLUMN app_id_max_quantity     integer,
    ADD COLUMN app_id_available_quantity integer;