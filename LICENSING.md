# Licensing

Status: Phase A. The register below is verified against repository contents; flags are open items that block Phase A exit criteria ("No production code begins while licensing remains ambiguous").

## 1. Licence register (verified 2026-08-06)

| Component | Verdict | SPDX |
|---|---|---|
| Impactor (Rust signing core) | MIT, LICENSE file verified | MIT |
| idevice (Rust device library) | MIT, `LICENSE.txt` verified | MIT |
| iloader | MIT, LICENSE file verified | MIT |
| sidestore-vpn | Unlicense (public domain dedication), verified | Unlicense |
| atvloadly | AGPL-3.0, LICENSE file verified | AGPL-3.0 |
| LiveContainer | AGPL-3.0 LICENSE file; README claims Apache 2.0 with a link to the AGPL file — **inconsistency confirmed, LICENSE governs** | AGPL-3.0 |
| SideStore | AGPL-3.0, LICENSE file verified | AGPL-3.0 |
| idevice_pair | No LICENSE file; README declares MIT — **flag** | MIT (declared) |
| anisette-v3-server | No licence declared anywhere — **flag, must not be redistributed** | none |

## 2. Open decisions

### 2.1 atvloadly (AGPL-3.0) — resolved 2026-08-06

**Decision: option (A) — Sidey is AGPL-3.0** and atvloadly's core is forked into the Go helper (D11). Atvloadly's LICENSE file is the governing licence; the fork remains AGPL-3.0. The rejected alternatives were running atvloadly unmodified as a separate external process (weaker legal boundary, no modifications allowed) and an independent Rust reimplementation (protocol risk without licence benefit).

### 2.2 idevice_pair (declared MIT, no file)

Ask the maintainer to add a LICENSE file, or pin the current commit with a documented reliance on the README declaration. Fallback: use pairing logic from `idevice` directly and treat idevice_pair as reference only.

### 2.3 anisette-v3-server (no licence)

Not redistributable. If the compatibility profile ships, it runs the upstream binary as an optional extra with the user installing it themselves, or it is dropped in favour of Impactor's built-in anisette.

### 2.4 LiveContainer fork (Phase L)

Only if the Phase L inbox requires modification. A fork must be AGPL-3.0 and pinned. Official upstream must be used unmodified for v1 (D-note in plan: "first release should use official LiveContainer without modification").

### 2.5 Sidey's own licence — resolved 2026-08-06

AGPL-3.0 (D11), consistent with atvloadly, LiveContainer and SideStore. The repository carries the AGPL-3.0 licence text in `LICENSE` (fetched verbatim from https://www.gnu.org/licenses/agpl-3.0.txt).

## 3. Obligations and housekeeping

- Copying Impactor or idevice code into this repository requires retaining their copyright notices (MIT requires it; see third_party/NOTICE.md, created when the first import lands in Phase F).
- Every release publishes an SBOM and dependency manifest (Phase C/O) so licence drift is detectable.
- `third_party/versions.lock` records pinned commits; it is updated only by deliberate action with a review trail.
- The LiveContainer licence inconsistency should be raised upstream so the README stops misstating Apache 2.0.

## 4. Excluded activities

The platform does not bypass Apple's certificate or provisioning rules, and does not use leaked, shared or enterprise certificates for unauthorised public distribution (plan "Excluded").
