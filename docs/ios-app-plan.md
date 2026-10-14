# Sidey for iOS — phased implementation plan

Status: plan only, no code. Companion to `applan.md` (proposal) with the
server gaps it identified now closed or tracked: release version history
(done 2026-10-11), GitHub synthetic bundle IDs excluded from snapshots
(done 2026-10-11), API auth holes (tracked below as Phase 0 gate).

Goal: a native SwiftUI remote for one Sidey server. The Go API stays
authoritative; both dashboard and app consume the same state and trigger
the same jobs. No on-device signing in v1 (separate Phase 5 track).

## Design language

iOS 26+, Swift 6, SwiftUI + Observation. Native components only — the
system renders Liquid Glass; the app must not fake it:

| Need | Native API |
|---|---|
| Main navigation | TabView (glass tab bar is automatic) |
| Detail navigation | NavigationStack |
| Lists | List with native sections, swipe actions |
| Settings | Form |
| Search | .searchable() |
| Reload | .refreshable() |
| Release selection | Picker (segmented or menu) |
| Local IPA import | fileImporter |
| Job progress | ProgressView |
| Destructive confirms | confirmationDialog |
| Icons | SF Symbols only |
| Networking | URLSession, async/await |
| Credentials | Keychain Services (never UserDefaults) |
| Haptics | UIImpactFeedback / UINotificationFeedback |
| Custom floating controls | .glassEffect(), system materials only |

Accessibility (Dynamic Type, VoiceOver labels, Reduce Motion) and dark
mode come free with native components — do not regress them with custom
drawing. Stretch: WidgetKit expiry glance, App Intent for Shortcuts
("refresh Apollo").

## Phase 0 — server prerequisites (DONE 2026-10-12, live-tested)

1. Admin auth fixed: `/api/v1/config` no longer leaks the key,
   header-based bypass removed (dashboard works via session cookie),
   `SIDEY_ADMIN_API_KEY` rotated.
2. Scoped device tokens live: `device_api_tokens` table, mint/list/
   revoke endpoints, `read` + `control` scopes wired across dashboard,
   store, refresh, import, approval and 2FA routes (verified matrix:
   mint→use→scope-deny→revoke→deny).
3. Pairing modal in dashboard with QR (vendored MIT qrcodejs) + copy.
4. Mobile API contract: tolerate unknown JSON fields client-side; do not
   rename dashboard endpoints without versioning.
5. Bootstrap unchanged: Sidey sideloads the app itself first (reserve bundle ID,
   e.g. the operator's domain + `.sidey`). The app then appears in its
   own My Apps list and rides the normal 7-day resign loop (dogfood).

## Phase 1 — foundation app (DONE 2026-10-12, simulator-verified)

- `ios/` in this repo: Xcode project (Xcode 27, Swift 6 strict,
  iOS 26), API client, models, Keychain store, TabView shell
  (Home + Settings).
- Home: server health, device status, upcoming expirations, active
  signing jobs, attention list (dashboard `/refresh` + `updates`
  endpoints already serve this).
- Settings: server URL/token (editable, re-pair), QR + manual pairing
  (`{"v":1,"url","token"}` matches the dashboard QR), notifications
  toggle, about. Empty/error/offline states for everything.
- Verified: clean simulator build, launches on iPhone 17 Pro sim,
  native Liquid Glass tab bar, correct unpaired empty state.
  Bundle ID final: `com.sidey.companion` (confirmed 2026-10-12; changing it later orphans installs).

## Phase 2 — remote management, My Apps (DONE 2026-10-12, simulator-verified)

- My Apps tab: deployments per device with version (new server-side
  join in `handleListDeployments`), expiry, policy/state; detail view
  with signing info, install history and Refresh Now (confirm →
  `POST admin/refresh` → progress via jobs polling).
- 2FA banner on Home when a recent sign job fails auth; code submit
  sheet posts to the existing 2FA endpoint.
- Server: `current_bundle`/`current_version` joined into the
  deployments list (live-tested).
- Verified: clean build, three-tab glass bar on sim. Paired data paths
  follow the same proven fetch/map patterns as Phase 1.

- Deployments per device: version, profile expiry, install history.
- Manual refresh per deployment; live job progress polling.
- Signing history view.
- **2FA code submission from the phone** (replaces the dashboard code
  box — biggest UX win in this phase).
- BGAppRefresh + local notifications for expiring profiles. No silent
  auto-resign promises: iOS won't allow them reliably; the server keeps
  doing unattended refreshes as backstop.

## Phase 3 — browse and updates (DONE 2026-10-12, simulator-verified)

- Browse tab: searchable catalog, source filter, app detail with
  release Picker across stable/beta/nightly/alpha (server now returns
  full `versions[]` histories), install via LiveContainer/Native,
  Watch toggles, local `.ipa` upload into quarantine.
- Updates tab: deployment + subscription updates with per-row Import
  (deployment device, or chosen target for watched apps).
- Verified: clean build, five-tab glass bar on sim. Import/approve/
  upload endpoints exercised live from curl during server work.

## Phase 4 — polish to the Liquid Glass standard (DONE 2026-10-12, simulator-verified)

- Tab bar: `.tabBarMinimizeBehavior(.onScrollDown)`; no custom glass
  anywhere (audit: no `.background(.material)`, no `presentationBackground`
  overrides, no custom animations — all glass comes free from TabView,
  NavigationStack, toolbars, sheets, search).
- Haptics: `.sensoryFeedback(.success)` on refresh accepted, 2FA
  submitted, import/upload accepted, pairing saved; `.selection` on
  Watch toggle.
- Accessibility: decorative `StatusDot` and row icons hidden from
  VoiceOver; `ExpiryBadge` gains a full-sentence label; icon-only
  toolbar buttons already carry `Label` text. No custom animations to
  gate on Reduce Motion; Dynamic Type flows through native components.
- App icon: generated 1024px indigo/S asset, wired via AppIcon set.
  Launch screen already system-handled (`UILaunchScreen`).
- iPad: universal (`1,2`) + full orientation set; verified on iPad Pro
  11-inch sim (floating glass tab bar, correct empty state).
- iPhone 17 Pro sim re-verified after all changes.
- Full multi-skill review 2026-10-12 (liquid-glass, swiftui-core/pro/
  expert, swift-lang, accessibility, ios-developer, testing-swift,
  review, ui-ux-pro-max): loaded states verified against a stub server
  (all 5 tabs screenshotted, hooks reverted after). Applied: stable
  update identities, position-independent version ForEach, NSNull
  coercion fix, IPA/zip-only picker, cached credentials, precomputed
  lists/history, streaming off-main uploads, stale-while-revalidate,
  unpaired app gating, token preserved on URL-only edits, token validated
  against an authenticated endpoint, scanner availability guard + Cancel,
  real local expiry reminders, in-app quarantine approve, per-mode import
  (Browse confirm dialog, Updates menu), combined VoiceOver row labels,
  async-outcome announcements, value-based NavigationLinks. Tests:
  SideyTests target, 33 Swift Testing tests green.
- Deliberately not applied (audited, rejected with reason): shared
  cross-tab fetch cache, custom AsyncImage loader, search debounce,
  one-type-per-file splits, localization, item-based dialogs (no such
  API), Date.now churn, tabBarMinimizeBehavior removal.

## Phase 5 (separate track) — on-device signing investigation

- Spike only, no commitment: provision cert/profile from server (p12
  push exists), sign locally, validate an install mechanism. Feather
  is GPL-3.0 (verified: claration/Feather) — reference only unless a
  GPL-compliance decision is made. Signing ≠ installing: the install
  mechanism is the hard part, not the crypto.

## Phase 6 — zero-touch onboarding (APP DONE 2026-10-13; SERVER DONE 2026-10-14, live-verified)

Decisions (operator-confirmed): first contact = one-tap on the phone
(found-server row shows name/host + Known/New badge; tap enrolls);
silent auto-attach only ever to already-known server IDs. Scope = LAN
Bonjour discovery + best-effort tailnet candidate probing (mDNS cannot
cross the tailnet — hard protocol limit, so tailnet first-contact is
remembered-host re-probe, not true discovery).

### Server contract (VPS repo `~/git/sidey-server` — DONE, deployed 2026-10-14)

1. Persistent server ID (UUID, config file or DB, never changes):
   - mDNS TXT on `_sidey._tcp` (API port): `id=<uuid> v=1
     name=<display>`, advertised on RFC1918 interfaces by default,
     flag to disable. Needs a Go mDNS lib — pick MIT/Apache-2.0,
     verify against LICENSING.md before vendoring.
   - Unauthenticated `GET /api/v1/server-identity` →
     `{"id","name","version"}` (identity check for the tailnet path
     where there is no mDNS).
2. `POST /api/v1/devices/enroll` (unauthenticated, source-gated):
   body `{"device_name","platform":"ios","app_version"}` →
   `{"token","device_id"}` issuing the standard scoped device token.
   Auto-issue ONLY from loopback/RFC1918/tailnet sources
   (`auto_enroll_cidrs`, default private ranges; 403 otherwise).
   Rate-limit (~10/min/IP), log enrollment, never log the token
   (THREAT_MODEL redaction rule). Dashboard lists enrolled devices
   with Revoke (revoke endpoint exists since Phase 0).
3. Dashboard QR payload unchanged; manual pairing unchanged.

### App (this repo — DONE, 51 tests / 8 suites green)

- `DiscoveryService.swift` (new): `FoundServer`, `ServerIdentity`,
  `KnownServers` (pure, tested), `DiscoveryParse` (TXT/identity/
  candidates, tested), `BonjourTransport` (NetService — NWBrowser
  exposes no TXT records), `StubTransport`, probe/verify with 5s
  timeouts, no subnet scanning ever.
- `APIClient.enroll` (tested: 200/403/404/bad-JSON).
- Known servers in Keychain (`id → {hosts, name, lastSeen}` — no
  UserDefaults for anything credential-adjacent).
- Tailnet probing: concurrent short-timeout (5s) probes of remembered
  hosts + saved-URL host, verified via `server-identity` (`/healthz`
  legacy fallback → id nil). No blind subnet scanning, ever.
- `SessionStore`: persist `serverID`; when a paired check goes
  offline, browse/probe for the known ID and silently switch URL on
  match (LAN DHCP / tailnet IP-change auto-heal). Record host on
  every successful online.
- `PairingView`: "Nearby servers" section with Known/New badge +
  one-tap Connect (enroll → save → probe → dismiss). QR + manual
  unchanged. Legacy servers (no identity/enroll endpoints → 404):
  badge "Unverified", one-tap falls back to manual with a
  server-needs-update message. Nothing may regress against the
  current VPS server.
- `Info.plist`: `NSLocalNetworkUsageDescription` +
  `NSBonjourServices=_sidey._tcp`. The iOS local-network prompt on
  first browse is the one unavoidable system touch.
- Tests: TXT/identity parsing (unknown-field tolerant), candidate
  build/dedup, known-server match policy, enroll mapping, host
  probing (match/mismatch/unreachable). Loaded-state screenshot via
  stub (identity+enroll fixtures, temp injectable transport, hooks
  reverted after, tests re-run green).
- `SessionStore`: `serverID` + known-hosts in Keychain, identity
  refresh on every online check, `rehome`, `heal` (ID-pinned silent
  re-attach; legacy accepts healthy remembered hosts). `SettingsView`
  Search Again when offline; `SideyApp` launch check + heal when
  paired. Legacy servers (no identity/enroll → 404): Unverified
  badge, one-tap falls back to manual; nothing regresses against the
  current VPS server.
- Swift 6 notes: transport seam is lock-guarded nonisolated
  (NetService is not Sendable); `@State error` never referenced
  inside bare `catch` (compiler assertion crash — use a `fail()`
  choke point); discovery tests use their own URLProtocol (suites
  run parallel, the shared stub handler is not cross-suite safe).
- Proposed (not yet live): server-identity, devices/enroll.

## Verified endpoint map (all live on main)

devices, deployments, jobs, refresh calendar, updates, artifacts,
store apps/sources, store install/import, subscriptions + unwatch,
manual refresh, artifact approve, 2FA submit, Apple accounts.

## Open decisions

- Exact companion bundle ID and display name.
- APNs push vs local-notifications-only (APNs needs certificate
  infrastructure; default to local).
- Pure sideload distribution (recommended: dogfoods the resign loop)
  vs TestFlight (requires paid membership path).
