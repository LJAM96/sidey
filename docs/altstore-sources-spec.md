# AltStore Source Provider (D7 follow-up)

Status: Phase 1 implemented 2026-10-11 (browse Accretion Core + gated
import into quarantine; no update detection yet). Tonight's stability work
(GSA fix, scheduler backoff, worker circuit breaker) is unaffected.

## 1. Goal

Let Sidey browse a public AltStore-format app catalog (default:
Accretion Core, `https://getaccretion.github.io/curated.json`) and import
chosen apps into the **existing** quarantine → inspection → approval →
sign pipeline. No new trust: catalog metadata is untrusted input, IPA
bytes go through every existing gate.

Explicitly post-v1 / D7 work. Manual IPA upload stays the v1 path.

## 2. Non-goals

- Auto-deploying anything from public sources. The plan.md rule stands:
  sources are monitored, never automatically installed.
- Accretion's site, accounts, Supabase backend, cert shops, private
  hosting. We consume their published JSON over HTTPS, nothing else.
- Hard dependency on Accretion. Any AltStore-format source URL must work;
  Accretion is only the default. (Young single-maintainer project: design
  for substitutability.)

## 3. Source configuration (extends plan.md "Source configuration")

New provider alongside `github`:

```json
{
  "provider": "altstore",
  "source_url": "https://getaccretion.github.io/curated.json",
  "expectedBundleIdentifier": "com.example.application",
  "expectedPlatform": "ios",
  "updatePolicy": "next_refresh",
  "releaseDelayHours": 24,
  "requireApproval": true,
  "allowedBundleIds": [],
  "blockedBundleIds": []
}
```

Notes:

- `requireApproval` defaults to **true** for `altstore` sources (public,
  unreviewed). Accretion's "newest version wins" dedup can swap builds
  under a shared bundle ID, so same-version-changed-bytes must keep
  requiring approval per the existing suspicious-artifact rule.
- `allowedBundleIds` empty = anything may be listed, but import is always
  an explicit operator action per app.
- `source_url` must be https. Non-https download URLs inside the catalog
  are rejected (Accretion already sanitizes to https-only; enforce again).

## 4. Poller

- New control-plane poller behind the existing `source managers`
  interface seam (plan.md line 81), next to the future GitHub watcher.
- Fetches `source_url` every 12–24h (configurable, default 24h) with
  conditional requests (`If-None-Match` / `If-Modified-Since`).
- Parses AltStore format: `apps[]` → `bundleIdentifier`, `versions[]`
  (newest-first arrays — walk the array, don't assume a single version),
  `downloadURL`, `size`, `localizedDescription`.
  Skip entries with missing/invalid bundle ID, http-only downloads,
  or no working version — mirroring Accretion's own spec gate.
- Pins the feed identifiers (`io.github.benwilllopez.accretion` /
  `...accretion.curated` — stable forever, AltStore refuses changes) and
  rejects polls where the identifier mismatches: cheap tamper-evidence
  against silent feed swaps.
- Persists a snapshot per (source, bundle ID): latest version, download
  URL, size, seen_at. New migration, e.g. `app_source_listings`.
- Start with `curated.json` (small). Full `source.json` supported but not
  default: 160+ sources is heavy to parse and mostly noise for one user.

## 5. Dashboard

- "Sources" view: browse listings (name, bundle ID, version, size,
  updated date), filter/search, per-app **Import** button.
- Import downloads the version's `downloadURL` server-side through the
  existing artifact intake: streaming SHA256, size cap
  (`SIDEY_MAX_ARTIFACT_BYTES`), ZIP/Info.plist inspection, quarantine,
  bundle-ID check against the source config's `expectedBundleIdentifier`.
- Failures surface with the catalog's stated version + URL for diagnosis.

## 6. Updates (implemented 2026-10-11: detection + surfacing only)

- Version compare per bundle ID against the artifact a deployment was
  built from. Newer version → dashboard "Available updates" block with an
  Import button (existing quarantine import). No auto-download.
- After approval, the operator deploys the new version through the
  existing deploy flow. Automatic upgrade on refresh (`next_refresh`
  picks up approved newer artifacts) is explicitly deferred: it touches
  the refresh orchestrator and is not attempted during the Apple-throttle
  incident.

## 7. Security / abuse notes

- Catalog + IPAs are untrusted. Quarantine and inspection are not
  bypassable from this path.
- Poller never authenticates to Apple and never signs. Signing stays
  behind the worker's circuit breaker + rate cap.
- Poll on a slow schedule; respect cache headers; back off on repeated
  fetch failures (reuse the terminal-backoff pattern, don't hammer
  public infrastructure either).
- Accretion hosts no third-party files (links only) — Sidey downloads
  from the original hosts. Their 6-hour link checking reduces rot but
  Sidey must still handle dead links gracefully at import time.

## 8. Phasing

1. Read-only: poll `curated.json`, dashboard browse + import into
   quarantine. No update detection. (Done 2026-10-11.)
2. Update detection + surfacing; import still explicit. (Done 2026-10-11:
   `app_source_listings` snapshot table, 12h watcher, dashboard updates
   block. Auto-upgrade deferred.)
3. Custom sources + subscriptions. (Done 2026-10-11: https-only custom
   source URLs with add/list/delete, `app_subscriptions` with seen-version
   baselines, subscription updates in the same dashboard block, import
   advances the baseline. Auto-upgrade on refresh still deferred.)

## 9. Open questions

- Should `source.json` (full) be offered with a warning, or hidden?
- Per-app "watch for updates" subscriptions vs watching whole catalog?
- Minimum Accretion app version pinning for the catalog fetch itself?
