-- Phase F: headless signing worker.
--
-- The signing worker reports the team it signed for; apple_accounts rows are
-- upserted by team identifier so a team is never duplicated.

CREATE UNIQUE INDEX uq_apple_accounts_team_identifier
    ON apple_accounts (team_identifier)
    WHERE team_identifier IS NOT NULL;
