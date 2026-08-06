# Upstream projects

Status: Phase A, verified 2026-08-06 via GitHub API and raw file fetches. Commits pinned against the default branch of each repository. Re-pin before any Phase B/G/H/L work touches these.

## Register

| Project | Repository | Branch | Pinned commit | Latest release | Language | License | Role in Sidey |
|---|---|---|---|---|---|---|---|
| atvloadly | [bitxeno/atvloadly](https://github.com/bitxeno/atvloadly) | master | `df201956449635815d1d816d0eaf20c4baf4f9e6` | v0.4.6 | Go | AGPL-3.0 | Exec/helper for tvOS provider (D1); licence decision pending |
| Impactor | [claration/Impactor](https://github.com/claration/Impactor) | main | `68664c71018dd25a53e871e348bdf8c8096e9891` | v2.6.0 | Rust | MIT | Extract signing modules into the worker (ADR-0002) |
| idevice | [jkcoxson/idevice](https://github.com/jkcoxson/idevice) | master | `7fe8adbdf8c72385971b05a86857e02d6fa2ed7c` | v0.1.65 | Rust | MIT | Library for the iOS provider |
| idevice_pair | [jkcoxson/idevice_pair](https://github.com/jkcoxson/idevice_pair) | master | `a78b2bb1166928f300c9a3e46d9f0c7df5fb6b6d` | v1.0.0 | Rust | Declared MIT in README, no LICENSE file | Onboarding/recovery workflow |
| iloader | [nab138/iloader](https://github.com/nab138/iloader) | main | `8547013c50b86087fb542bc14aafa4c6c60e6638` | v2.3.1 | TypeScript | MIT | Reference for onboarding UX and pairing file management |
| LiveContainer | [LiveContainer/LiveContainer](https://github.com/LiveContainer/LiveContainer) | main | `ac2186e2558ad7cacc6b5a2970b92ce5d4d0d60c` | 3.8.0 | Swift | AGPL-3.0 (README claims Apache 2.0 — inconsistency confirmed) | Guest runtime; official upstream in v1, fork candidate in Phase L |
| SideStore | [SideStore/SideStore](https://github.com/SideStore/SideStore) | develop | `a9238cb974ae56cf0f17d14dc517d5e06e21b52a` | 0.6.3 | Swift | AGPL-3.0 | Reference implementation only |
| sidestore-vpn | [xddxdd/sidestore-vpn](https://github.com/xddxdd/sidestore-vpn) | master | `1cb5f5d94e6f0baf4ed432d30b926bb0c39a9904` | none | Rust | Unlicense | Optional Compose profile only |
| anisette-v3-server | [Dadoum/anisette-v3-server](https://github.com/Dadoum/anisette-v3-server) | main | `2ef18d7da2abe3a6d070aa478f774538b947aaa2` | none | D | None declared | Optional compatibility profile only |

## Notes and open items

1. **atvloadly is AGPL-3.0.** The plan's original assumption was permissive reuse; the verified licence is copyleft with a network clause. Resolved 2026-08-06: Sidey is AGPL-3.0 (D11) and atvloadly's core is forked into the Go helper. The fork must retain the AGPL-3.0 notice and the pinned upstream commit for provenance.
2. **idevice_pair has no licence file**; README declares MIT. Treat as MIT pending written confirmation; an alternative is extracting pairing logic from `idevice` directly.
3. **anisette-v3-server has no licence at all.** It must not be redistributed; the optional profile runs it only for SideStore/AltServer compatibility and the preferred path is the anisette implementation inside Impactor (plan "anisette-v3-server and Omnisette").
4. **LiveContainer**: LICENSE file is AGPL-3.0, README links "Apache License 2.0" to the AGPL file. The LICENSE file governs: AGPL-3.0. Installing the official signed release in v1 does not redistribute source; the Phase L fork must remain AGPL.
5. **SideStore pins against `develop`**, not `main`.
6. SideStore maintains its own fork of `idevice` (`SideStore/idevice`, MIT) — a fallback if upstream `jkcoxson/idevice` becomes unmaintained.
7. **iloader** is built on `jkcoxson/idevice` and `idevice_pair`; it is a TypeScript/Electron GUI and is reused for concepts only, not code (plan "iloader").
