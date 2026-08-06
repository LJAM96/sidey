# Threat model

Status: Phase A definition. Revise during Phase M (security hardening) and whenever the topology changes.

## 1. Assets

| Asset | Location | Highest consequence |
|---|---|---|
| Apple account credentials | signing worker, encrypted at rest (Phase M) | Full control of the user's Apple developer identity; irreversible certificate actions |
| Signing certificate private keys | signing worker, envelope encrypted (Phase M) | Signing abuse; certificates linked to the user's identity |
| Provisioning profiles | DB + devices | Short lived by design (7 days free); drift if compromised |
| Pairing records | edge agent vault, encrypted (Phase M) | Device impersonation; access to device services |
| Original IPA repository | artifact store | Supply chain tampering of distributed apps |
| Signed derivatives | artifact store cache | Tampered applications installed on devices |
| Web session credentials | control plane | Full administrative access |
| Job queue and audit log | PostgreSQL | Obfuscating unauthorised actions |
| GitHub App credentials (post-v1) | control plane | Private repository access |
| LiveContainer guest manifests (Phase L) | agent + device Documents | Unauthorised guest installs/removals on devices |
| Backup archives | storage volumes | Offline disclosure of everything above |

## 2. Actors

| Actor | Trust level |
|---|---|
| Administrator (single user, D4) | Fully trusted; the only human principal in v1 |
| Device agent | Trusted on the tailnet, limited to device services and pairing records; must not access Apple or GitHub credentials |
| External internet attacker | Untrusted; faces the gateway, API, webhooks (post-v1) |
| Compromised container | Untrusted; workload isolation is the boundary |
| Malicious IPA author | Untrusted; IPA parsing happens in quarantine with validation before approval |
| Network observer | Untrusted; all external traffic should be TLS, inter-host traffic via Tailscale |

## 3. Threats and mitigations

### 3.1 Credentials and signing keys

| Threat | Mitigation | Phase |
|---|---|---|
| Database leak exposes raw Apple credentials | Store only encrypted credential references; envelope encryption with a separate key encryption key not stored in the database | M |
| Signing private key read by control plane | Keys live only in signing worker storage, encrypted; control plane has no path to raw keys | M, F |
| Key loss makes certificates unrecoverable | Backup of encrypted keys with separate backup encryption key; certificate revocation and recovery path | F, M, N |
| Credential brute force / lockout | Account lockout protection, failure counting on Apple account records | M |
| Secrets leak via logs | Structured redaction of secrets and pairing material | M |
| Apple credential compromise during agent enrolment | Enrolment uses one time tokens; agents never receive Apple credentials | D, M |

### 3.2 Pairing records

| Threat | Mitigation | Phase |
|---|---|---|
| Pairing record theft enables device impersonation | Encrypted at rest on the edge host; separate vault; redacted from logs | H, M |
| Lost pairing record bricks remote device management | Recovery workflow; pairing records backed up separately | H, N |
| Edge agent compromise exposes device services | Agent scoped to minimum secrets; Tailscale ACLs; device services only reachable from agent | M |

### 3.3 IPA repository and signing pipeline

| Threat | Mitigation | Phase |
|---|---|---|
| Malformed or malicious IPA breaks parsers | Quarantine before inspection; ZIP structure validation; reject malformed archives | E |
| Tampered IPA distributed to devices | Content addressed store keyed by SHA256; byte identical originals; hash verification of signed derivatives | E, N |
| Wrong platform/bundle identifier deployed | Inspector validates platform, bundle identifier, minimum OS, entitlements against source config | E, J |
| Same version re-signed with changed content | Changed artifact with same displayed version treated as suspicious, requires approval | J (post-v1) |
| Unauthorised upload | Single admin auth; audit events on every state change | D, M |

### 3.4 Device communication

| Threat | Mitigation | Phase |
|---|---|---|
| Devices reached from the wrong host | Device services and pairing isolated to the edge agent | M |
| Tailscale loss mid-installation | Job resumption, idempotent job claims, per device locks | B, D |
| Locked/offline device causes endless retry | Explicit connection capability state; retry backoff; critical expiry alerts | H, I |
| tvOS discovery privileges leak to control plane | Avahi/DBus/usbmuxd mounts exist only in the edge container | C |

### 3.5 Web and API

| Threat | Mitigation | Phase |
|---|---|---|
| Brute force on admin login | MFA for administrators (v1: single admin), account lockout | M |
| Session theft | Session signing key as a Docker secret; short lived sessions | C, M |
| Webhook spoofing (post-v1) | Webhook signature validation and replay protection shipped with the webhook feature, not later | J |
| CSRF on dashboard | Standard dashboard CSRF protection | D |

### 3.6 Supply chain

| Threat | Mitigation | Phase |
|---|---|---|
| Malicious dependency in an image | Dependency manifests, SBOM per release, container scanning | C, M, O |
| Untrusted upstream component | Pinned commits in third_party/versions.lock; licence register in LICENSING.md | A |
| Modified upstream (LiveContainer fork, Phase L) | Pinned fork commit; manifest signature verification; hash verification | A, L |

### 3.7 LiveContainer guest delivery (Phase L)

| Threat | Mitigation | Phase |
|---|---|---|
| Unauthorised job placed in the inbox | Signed job manifest verified by LiveContainer | L |
| Altered IPA in the inbox | Hash verification against the signed manifest | L |
| Failed update destroys guest data | Install/update/remove actions preserve data containers; failed update leaves previous guest usable | L |

## 4. Trust boundaries

```text
Internet ──TLS──► gateway ──► control-plane ──tailnet──► device-agent ──Apple services──► devices
                            │
                            ├── postgres (DB: no raw keys)
                            └── signing-worker (Apple creds + keys only)
```

- Control plane: never holds raw signing keys or Apple credential plaintext after enrolment.
- Signing worker: never holds pairing records; never talks to devices.
- Device agent: never holds Apple account credentials or GitHub credentials.
- Devices: only accept installs from the edge agent using validated pairing.

## 5. Residual risks

- Free team Apple accounts impose short profile lifetimes; a long outage can let all profiles lapse simultaneously. Mitigated by critical expiry alerts and backup of profiles, but a full lapse requires an online Apple interaction to recover.
- iOS determines whether suspended applications receive execution time; Phase L automation is best effort by design.
- atvloadly licensing is resolved (Sidey is AGPL-3.0, D11); the LiveContainer README licence discrepancy is a documentation bug to raise upstream, not an operational risk.
