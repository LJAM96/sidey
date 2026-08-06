# Docker secrets for the sidey compose stack.

Generate development values with, for example:

  openssl rand -hex 32 > deploy/secrets/db_password
  openssl rand -hex 32 > deploy/secrets/session_signing_key
  openssl rand -hex 32 > deploy/secrets/apple_credential_key

These files are gitignored (see deploy/secrets/.gitignore). In production,
provision them from your secret store; never commit real values.

Secrets referenced by the compose files:
  db_password           postgres password
  session_signing_key   web session signing key
  apple_credential_key  envelope encryption key for Apple credentials

Remaining planned secrets (plan.md "Secrets"), added with their phases:
  master_encryption_key        Phase M
  github_app_private_key       Phase J
  webhook_secret               Phase J
  backup_encryption_key        Phase N
  tailscale_auth_key           used via TS_AUTHKEY during first enrolment
