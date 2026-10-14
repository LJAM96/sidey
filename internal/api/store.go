package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"sidey/internal/artifacts"
	"sidey/internal/audit"
)

type StoreSource struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // "altstore" or "github"
	URL       string `json:"url"`  // URL or "owner/repo"
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	// ExpectedID pins the feed's `identifier` when set: a fetched catalog
	// whose identifier is present and different is skipped outright (cheap
	// tamper-evidence against silent feed swaps).
	ExpectedID string `json:"expected_id,omitempty"`
	// ManagedBy marks operator-invisible provenance. The discovered-feed
	// mirror sets it; the sync never touches entries without it, and
	// deleting a managed entry tombstones its URL so it is never re-added.
	ManagedBy string `json:"managed_by,omitempty"`
}

type AppVersion struct {
	Version     string `json:"version"`
	Channel     string `json:"channel"` // "stable", "beta", "nightly", "alpha"
	BundleID    string `json:"bundle_id"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
	UpdatedDate string `json:"updated_date"`
}

type StoreApp struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Developer   string       `json:"developer"`
	Description string       `json:"description"`
	IconURL     string       `json:"icon_url"`
	Source      string       `json:"source"`
	SourceType  string       `json:"source_type"`
	SourceID    string       `json:"source_id"`
	Versions    []AppVersion `json:"versions"`

	// Convenience top-level fields (defaults to primary/stable version)
	BundleID    string `json:"bundle_id"`
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
	UpdatedDate string `json:"updated_date"`
}

var defaultSources = []StoreSource{
	// Official & Foundation
	{
		ID:        "src-altstore-official",
		Type:      "altstore",
		URL:       "https://apps.altstore.io",
		Name:      "AltStore Official",
		IsDefault: true,
	},
	{
		// Aggregated catalog: 160+ public sources, deduped, newest wins.
		// Rebuilt upstream every 6h; identifier pinned (see ExpectedID).
		ID:         "src-accretion-core",
		Type:       "altstore",
		URL:        "https://getaccretion.github.io/curated.json",
		Name:       "Accretion Core",
		IsDefault:  true,
		ExpectedID: "io.github.benwilllopez.accretion.curated",
	},
	{
		ID:        "src-spotcompiled",
		Type:      "altstore",
		URL:       "https://spotc-repo.yodaluca.dev/AltStore%20Repo.json",
		Name:      "SpotCompiled",
		IsDefault: true,
	},
	// Social Media
	{
		ID:        "src-apollo-reborn",
		Type:      "github",
		URL:       "Apollo-Reborn/Apollo-Reborn",
		Name:      "Apollo Reborn",
		IsDefault: true,
	},
	{
		ID:        "src-sparkle-ig",
		Type:      "github",
		URL:       "efibalogh/sparkle-ig",
		Name:      "Sparkle for Instagram",
		IsDefault: true,
	},
	// Media & Streaming
	{
		ID:        "src-apex",
		Type:      "github",
		URL:       "lowiqentity/APEX",
		Name:      "APEX",
		IsDefault: true,
	},
	{
		ID:        "src-youmod",
		Type:      "github",
		URL:       "jaydenjcpy/YouMod",
		Name:      "YouMod",
		IsDefault: true,
	},
	{
		ID:        "src-youproextra",
		Type:      "github",
		URL:       "mrdrvt99/YouProEXTRA",
		Name:      "YouProEXTRA (YouTube)",
		IsDefault: true,
	},
	{
		ID:        "src-spotiflac",
		Type:      "github",
		URL:       "spotiflacapp/SpotiFLAC-Mobile",
		Name:      "SpotiFLAC Mobile",
		IsDefault: true,
	},
	// Games & Emulation
	{
		ID:        "src-gen1recomp",
		Type:      "github",
		URL:       "bryanthaboi/gen1recomp",
		Name:      "Gen1Recomp",
		IsDefault: true,
	},
	{
		ID:        "src-ppsspp",
		Type:      "github",
		URL:       "hrydgard/ppsspp",
		Name:      "PPSSPP Emulator",
		IsDefault: true,
	},
	{
		ID:        "src-provenance",
		Type:      "github",
		URL:       "Provenance-Emu/Provenance",
		Name:      "Provenance-Emu",
		IsDefault: true,
	},
	// General & Utilities
	{
		ID:        "src-reynard-browser",
		Type:      "github",
		URL:       "minh-ton/reynard-browser",
		Name:      "Reynard Browser",
		IsDefault: true,
	},
	{
		ID:        "src-locus",
		Type:      "github",
		URL:       "ChrisMack32/Locus",
		Name:      "Locus",
		IsDefault: true,
	},
	{
		ID:        "src-livecontainer",
		Type:      "github",
		URL:       "khanhduytran0/LiveContainer",
		Name:      "LiveContainer",
		IsDefault: true,
	},
	{
		ID:        "src-utm",
		Type:      "github",
		URL:       "utmapp/UTM",
		Name:      "UTM Virtual Machines",
		IsDefault: true,
	},
	{
		ID:        "src-sidestore",
		Type:      "github",
		URL:       "SideStore/SideStore",
		Name:      "SideStore",
		IsDefault: true,
	},
}

type storeCache struct {
	sync.RWMutex
	apps      []StoreApp
	updatedAt time.Time
}

var globalStoreCache storeCache

func (s *Server) loadStoreSources() []StoreSource {
	sources := make([]StoreSource, len(defaultSources))
	copy(sources, defaultSources)

	path := "/var/lib/sidey/store_sources.json"
	data, err := os.ReadFile(path)
	if err != nil {
		path = "/run/sidey/store_sources.json"
		data, err = os.ReadFile(path)
	}
	if err == nil {
		var custom []StoreSource
		if err := json.Unmarshal(data, &custom); err == nil {
			// Deduplicate against defaults
			for _, c := range custom {
				exists := false
				for _, d := range defaultSources {
					if d.URL == c.URL || d.ID == c.ID {
						exists = true
						break
					}
				}
				if !exists {
					sources = append(sources, c)
				}
			}
		}
	}
	return sources
}

func (s *Server) saveCustomSources(custom []StoreSource) error {
	paths := []string{"/var/lib/sidey/store_sources.json", "/run/sidey/store_sources.json"}
	data, err := json.MarshalIndent(custom, "", "  ")
	if err != nil {
		return err
	}
	for _, p := range paths {
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.WriteFile(p, data, 0o644)
	}
	return nil
}

// accretionDiscoveredURL is the upstream list of community AltStore feeds
// that Accretion merges beyond its hand-picked Core set. Mirroring it
// (add-only) keeps Sidey's catalog breadth in step without manual curation;
// each feed still fetches independently, so a dead one only drops its own apps.
const accretionDiscoveredURL = "https://raw.githubusercontent.com/GETAccretion/getaccretion.github.io/main/discovered.txt"

// discoveredManagedBy marks sources added by the mirror sync below.
const discoveredManagedBy = "accretion-discovered"

func discoveredRemovedPaths() []string {
	return []string{"/var/lib/sidey/store_sources_removed.json", "/run/sidey/store_sources_removed.json"}
}

// loadDiscoveredRemoved returns URLs the operator deleted after they were
// mirrored, so the sync never resurrects them.
func loadDiscoveredRemoved() map[string]bool {
	removed := map[string]bool{}
	for _, p := range discoveredRemovedPaths() {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var urls []string
		if json.Unmarshal(data, &urls) == nil {
			for _, u := range urls {
				removed[u] = true
			}
		}
	}
	return removed
}

func saveDiscoveredRemoved(removed map[string]bool) {
	urls := make([]string, 0, len(removed))
	for u := range removed {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	data, _ := json.MarshalIndent(urls, "", "  ")
	for _, p := range discoveredRemovedPaths() {
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.WriteFile(p, data, 0o644)
	}
}

// loadCustomSourcesFile reads the operator-managed source file (same dual
// path as loadStoreSources, without merging defaults).
func loadCustomSourcesFile() []StoreSource {
	for _, p := range []string{"/var/lib/sidey/store_sources.json", "/run/sidey/store_sources.json"} {
		if data, err := os.ReadFile(p); err == nil {
			var custom []StoreSource
			if json.Unmarshal(data, &custom) == nil {
				return custom
			}
		}
	}
	return nil
}

// SyncDiscoveredSources mirrors Accretion's discovered feed list into the
// custom sources file. It only appends: defaults and user-added entries are
// never modified, and feeds the operator deleted stay deleted.
func (s *Server) SyncDiscoveredSources(ctx context.Context) (added int, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", accretionDiscoveredURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Sidey-ControlPlane/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("discovered list returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	removed := loadDiscoveredRemoved()
	custom := loadCustomSourcesFile()
	known := map[string]bool{}
	for _, d := range defaultSources {
		known[d.URL] = true
	}
	for _, c := range custom {
		known[c.URL] = true
	}
	for _, line := range strings.Split(string(body), "\n") {
		// First whitespace-delimited token; discovered.txt carries
		// trailing "# comment" annotations that must not become URLs.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		u := fields[0]
		if i := strings.Index(u, "#"); i >= 0 {
			u = u[:i]
		}
		u = strings.TrimSpace(strings.ReplaceAll(u, " ", "%20"))
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(u), "https://") {
			continue
		}
		if known[u] || removed[u] {
			continue
		}
		host := strings.TrimPrefix(u, "https://")
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		sum := sha1.Sum([]byte(u))
		custom = append(custom, StoreSource{
			ID:        "src-discovered-" + hex.EncodeToString(sum[:])[:8],
			Type:      "altstore",
			URL:       u,
			Name:      host,
			IsDefault: false,
			ManagedBy: discoveredManagedBy,
		})
		known[u] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if err := s.saveCustomSources(custom); err != nil {
		return 0, err
	}
	globalStoreCache.Lock()
	globalStoreCache.apps = nil
	globalStoreCache.Unlock()
	s.audit.Record(ctx, "system", "store.discovered_synced", audit.WithData(map[string]any{"added": added}))
	return added, nil
}

func (s *Server) handleListStoreSources(w http.ResponseWriter, r *http.Request) {
	sources := s.loadStoreSources()
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleAddStoreSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"` // "altstore" or "github"
		URL  string `json:"url"`  // URL or "owner/repo"
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.URL = strings.TrimSpace(req.URL)
	req.Name = strings.TrimSpace(req.Name)

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url is required"})
		return
	}
	if req.Type == "" {
		if strings.Contains(req.URL, "github.com") || (!strings.HasPrefix(req.URL, "http") && strings.Contains(req.URL, "/")) {
			req.Type = "github"
		} else {
			req.Type = "altstore"
		}
	}
	if req.Type == "github" {
		req.URL = strings.TrimPrefix(req.URL, "https://github.com/")
		req.URL = strings.TrimPrefix(req.URL, "http://github.com/")
		req.URL = strings.TrimSuffix(req.URL, "/")
		if req.Name == "" {
			req.Name = req.URL
		}
	} else {
		if !strings.HasPrefix(strings.ToLower(req.URL), "https://") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "altstore source url must be https"})
			return
		}
		req.URL = strings.ReplaceAll(req.URL, " ", "%20")
		if req.Name == "" {
			req.Name = req.URL
		}
	}

	id := "src-" + uuid.New().String()[:8]
	newSrc := StoreSource{
		ID:        id,
		Type:      req.Type,
		URL:       req.URL,
		Name:      req.Name,
		IsDefault: false,
	}

	var custom []StoreSource
	path := "/var/lib/sidey/store_sources.json"
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &custom)
	}
	custom = append(custom, newSrc)
	_ = s.saveCustomSources(custom)

	globalStoreCache.Lock()
	globalStoreCache.apps = nil
	globalStoreCache.Unlock()

	s.audit.Record(r.Context(), "admin", "store_source.added", audit.WithData(map[string]any{
		"source_id": id,
		"type":      req.Type,
		"url":       req.URL,
	}))

	writeJSON(w, http.StatusCreated, newSrc)
}

func (s *Server) handleDeleteStoreSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source id is required"})
		return
	}

	var custom []StoreSource
	// NOTE: keep in sync with loadCustomSourcesFile: /var/lib/sidey is
	// ephemeral in this container, /run/sidey is the persistent copy.
	for _, p := range []string{"/var/lib/sidey/store_sources.json", "/run/sidey/store_sources.json"} {
		if data, err := os.ReadFile(p); err == nil {
			if json.Unmarshal(data, &custom) == nil && len(custom) > 0 {
				break
			}
		}
	}
	var filtered []StoreSource
	for _, src := range custom {
		if src.ID != id {
			filtered = append(filtered, src)
			continue
		}
		// A deleted mirrored feed is tombstoned so the weekly sync never
		// resurrects it; user-added entries just disappear.
		if src.ManagedBy == discoveredManagedBy {
			removed := loadDiscoveredRemoved()
			removed[src.URL] = true
			saveDiscoveredRemoved(removed)
		}
	}
	_ = s.saveCustomSources(filtered)

	globalStoreCache.Lock()
	globalStoreCache.apps = nil
	globalStoreCache.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

// getStoreApps returns aggregated, unified apps from all sources (cached for 10 minutes).
func (s *Server) getStoreApps(ctx context.Context) []StoreApp {
	globalStoreCache.RLock()
	if len(globalStoreCache.apps) > 0 && time.Since(globalStoreCache.updatedAt) < 10*time.Minute {
		cached := globalStoreCache.apps
		globalStoreCache.RUnlock()
		return cached
	}
	globalStoreCache.RUnlock()

	sources := s.loadStoreSources()
	var allApps []StoreApp
	var mu sync.Mutex
	var wg sync.WaitGroup

	client := &http.Client{Timeout: 10 * time.Second}

	for _, src := range sources {
		src := src
		wg.Add(1)
		go func() {
			defer wg.Done()
			var apps []StoreApp
			if src.Type == "github" {
				apps = fetchGitHubApps(client, src)
			} else {
				apps = fetchAltStoreApps(client, src)
			}
			if len(apps) > 0 {
				for i := range apps {
					apps[i].SourceID = src.ID
				}
				mu.Lock()
				allApps = append(allApps, apps...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	globalStoreCache.Lock()
	globalStoreCache.apps = allApps
	globalStoreCache.updatedAt = time.Now()
	globalStoreCache.Unlock()

	s.snapshotListings(ctx, allApps)

	return allApps
}

// handleListStoreApps fetches and returns aggregated, unified apps from all sources.
func (s *Server) handleListStoreApps(w http.ResponseWriter, r *http.Request) {
	apps := s.getStoreApps(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

// snapshotListings persists the newest listing per (source, bundle ID) for
// update detection. Snapshots only; nothing is downloaded or installed.
// GitHub-sourced rows carry synthetic bundle IDs (com.github.owner.repo)
// that can never match a deployed IPA, so only altstore rows are kept.
// A snapshot failure is logged but never breaks listing.
func (s *Server) snapshotListings(ctx context.Context, apps []StoreApp) {
	for _, a := range apps {
		if a.SourceType != "altstore" {
			continue
		}
		if a.BundleID == "" || a.DownloadURL == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO app_source_listings (source_id, bundle_id, name, latest_version, latest_download_url, latest_size, latest_updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (source_id, bundle_id) DO UPDATE SET
				name = EXCLUDED.name,
				latest_version = EXCLUDED.latest_version,
				latest_download_url = EXCLUDED.latest_download_url,
				latest_size = EXCLUDED.latest_size,
				latest_updated_at = EXCLUDED.latest_updated_at,
				seen_at = now()`,
			a.SourceID, a.BundleID, a.Name, a.Version, a.DownloadURL, a.Size, a.UpdatedDate); err != nil {
			s.logger.Warn("source snapshot failed", "source", a.SourceID, "bundle", a.BundleID, "error", err)
			continue
		}
	}
}

// SnapshotSources refreshes the catalog and persists listing snapshots.
// Safe to call periodically; the 10-minute fetch cache bounds upstream traffic.
func (s *Server) SnapshotSources(ctx context.Context) int {
	return len(s.getStoreApps(ctx))
}

// handleListUpdates compares deployed versions against catalog snapshots and
// reports deployments with a newer version available. Detection only: the
// operator imports (quarantine) and deploys explicitly via existing flows.
func (s *Server) handleListUpdates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.pool.Query(ctx, `
		SELECT dep.id, dep.device_id, COALESCE(d.device_name, ''),
		       a.bundle_identifier, COALESCE(a.version, '')
		FROM deployments dep
		JOIN devices d ON d.id = dep.device_id
		JOIN installation_records ir ON ir.deployment_id = dep.id AND ir.signed_artifact_id IS NOT NULL
		JOIN signed_artifacts sa ON sa.id = ir.signed_artifact_id
		JOIN artifacts a ON a.id = sa.source_artifact_id
		WHERE a.bundle_identifier IS NOT NULL AND a.bundle_identifier <> ''`)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "deployed versions lookup failed")
		return
	}
	type deployed struct {
		deploymentID uuid.UUID
		deviceID     uuid.UUID
		deviceName   string
		bundleID     string
		version      string
	}
	var deps []deployed
	for rows.Next() {
		var d deployed
		if err := rows.Scan(&d.deploymentID, &d.deviceID, &d.deviceName, &d.bundleID, &d.version); err != nil {
			rows.Close()
			s.writeError(w, http.StatusInternalServerError, "deployed versions scan failed")
			return
		}
		deps = append(deps, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "deployed versions lookup failed")
		return
	}
	if len(deps) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"updates": []any{}})
		return
	}

	bundles := make([]string, 0, len(deps))
	seen := map[string]bool{}
	for _, d := range deps {
		if !seen[d.bundleID] {
			seen[d.bundleID] = true
			bundles = append(bundles, d.bundleID)
		}
	}
	snapRows, err := s.pool.Query(ctx, `
		SELECT bundle_id, name, latest_version, latest_download_url, latest_size, latest_updated_at, source_id
		FROM app_source_listings WHERE bundle_id = ANY($1)`, bundles)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "snapshot lookup failed")
		return
	}
	type snap struct {
		name, version, url, updated, sourceID string
		size                                  int64
	}
	best := map[string]snap{}
	for snapRows.Next() {
		var bundle, name, version, url, updated, sourceID string
		var size int64
		if err := snapRows.Scan(&bundle, &name, &version, &url, &size, &updated, &sourceID); err != nil {
			snapRows.Close()
			s.writeError(w, http.StatusInternalServerError, "snapshot scan failed")
			return
		}
		if cur, ok := best[bundle]; !ok || compareVersions(version, cur.version) > 0 {
			best[bundle] = snap{name, version, url, updated, sourceID, size}
		}
	}
	snapRows.Close()
	if err := snapRows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "snapshot lookup failed")
		return
	}

	sourceNames := map[string]string{}
	for _, src := range s.loadStoreSources() {
		sourceNames[src.ID] = src.Name
	}
	updates := []any{}
	for _, d := range deps {
		sn, ok := best[d.bundleID]
		if !ok || compareVersions(sn.version, d.version) <= 0 {
			continue
		}
		updates = append(updates, map[string]any{
			"kind":                "deployment",
			"deployment_id":       d.deploymentID.String(),
			"device_id":           d.deviceID.String(),
			"device_name":         d.deviceName,
			"bundle_id":           d.bundleID,
			"current_version":     d.version,
			"latest_version":      sn.version,
			"latest_download_url": sn.url,
			"latest_size":         sn.size,
			"latest_updated_at":   sn.updated,
			"source_id":           sn.sourceID,
			"source_name":         sourceNames[sn.sourceID],
			"app_name":            sn.name,
		})
	}
	// Subscriptions: watched bundles that are not necessarily deployed.
	// An entry surfaces when the snapshot is newer than the version seen
	// at subscribe time (or when it first appears, if unknown then).
	subRows, err := s.pool.Query(ctx, `SELECT bundle_id, name, COALESCE(seen_version, '') FROM app_subscriptions`)
	if err == nil {
		for subRows.Next() {
			var bundle, name, seen string
			if err := subRows.Scan(&bundle, &name, &seen); err != nil {
				break
			}
			sn, ok := best[bundle]
			if !ok {
				// Not in the current snapshot set (snapshot query was
				// scoped to deployed bundles); take the best row directly.
				snapRows2, err := s.pool.Query(ctx, `
					SELECT name, latest_version, latest_download_url, latest_size, latest_updated_at, source_id
					FROM app_source_listings WHERE bundle_id = $1`, bundle)
				if err != nil {
					continue
				}
				for snapRows2.Next() {
					var n, v, u, up, sid string
					var sz int64
					if err := snapRows2.Scan(&n, &v, &u, &sz, &up, &sid); err != nil {
						break
					}
					if !ok || compareVersions(v, sn.version) > 0 {
						sn, ok = snap{n, v, u, up, sid, sz}, true
					}
				}
				snapRows2.Close()
				if !ok {
					continue
				}
			}
			if seen != "" && compareVersions(sn.version, seen) <= 0 {
				continue
			}
			if sn.url == "" {
				continue
			}
			updates = append(updates, map[string]any{
				"kind":                "subscription",
				"bundle_id":           bundle,
				"current_version":     seen,
				"latest_version":      sn.version,
				"latest_download_url": sn.url,
				"latest_size":         sn.size,
				"latest_updated_at":   sn.updated,
				"source_id":           sn.sourceID,
				"source_name":         sourceNames[sn.sourceID],
				"app_name":            sn.name,
			})
		}
		subRows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"updates": updates})
}

// handleListSubscriptions returns watched bundle IDs.
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT bundle_id, name, COALESCE(seen_version, ''), created_at
		FROM app_subscriptions ORDER BY created_at`)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "subscriptions lookup failed")
		return
	}
	defer rows.Close()
	subs := []any{}
	for rows.Next() {
		var bundle, name, seen string
		var created time.Time
		if err := rows.Scan(&bundle, &name, &seen, &created); err != nil {
			continue
		}
		subs = append(subs, map[string]any{
			"bundle_id": bundle, "name": name, "seen_version": seen,
			"created_at": created.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

// handleAddSubscription watches a bundle ID. The version seen is the
// current snapshot best (or empty when unknown); only newer snapshots
// surface as updates afterwards.
func (s *Server) handleAddSubscription(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BundleID string `json:"bundle_id"`
		Name     string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.BundleID = strings.TrimSpace(req.BundleID)
	if req.BundleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bundle_id is required"})
		return
	}
	ctx := r.Context()
	var seen, name string
	_ = s.pool.QueryRow(ctx, `
		SELECT latest_version, name FROM app_source_listings
		WHERE bundle_id = $1 ORDER BY seen_at DESC LIMIT 1`,
		req.BundleID).Scan(&seen, &name)
	if req.Name != "" {
		name = req.Name
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO app_subscriptions (bundle_id, name, seen_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (bundle_id) DO UPDATE SET name = EXCLUDED.name`,
		req.BundleID, name, seen); err != nil {
		s.writeError(w, http.StatusInternalServerError, "subscription save failed")
		return
	}
	s.audit.Record(ctx, "admin", "store_subscription.added", audit.WithData(map[string]any{
		"bundle_id": req.BundleID, "seen_version": seen,
	}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"bundle_id": req.BundleID, "name": name, "seen_version": seen,
	})
}

// handleDeleteSubscription stops watching a bundle ID.
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if bundle == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bundle id is required"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM app_subscriptions WHERE bundle_id = $1`, bundle); err != nil {
		s.writeError(w, http.StatusInternalServerError, "subscription delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "bundle_id": bundle})
}

// compareVersions compares dotted version strings segment by segment
// ("1.0.10" > "1.0.9", "1.0" == "1.0.0"). Numeric segments compare
// numerically, others lexically. Returns -1, 0 or 1.
func compareVersions(a, b string) int {
	fa := splitVersion(a)
	fb := splitVersion(b)
	for i := 0; i < len(fa) || i < len(fb); i++ {
		sa, sb := "0", "0"
		if i < len(fa) {
			sa = fa[i]
		}
		if i < len(fb) {
			sb = fb[i]
		}
		if c := compareVersionSegment(sa, sb); c != 0 {
			return c
		}
	}
	return 0
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	})
}

func compareVersionSegment(a, b string) int {
	if isVersionDigits(a) && isVersionDigits(b) {
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

func isVersionDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func normalizeBaseKey(bundleID, name string) string {
	lowID := strings.ToLower(bundleID)
	lowID = strings.TrimSuffix(lowID, ".beta")
	lowID = strings.TrimSuffix(lowID, ".preview")
	lowID = strings.TrimSuffix(lowID, ".nightly")
	lowID = strings.TrimSuffix(lowID, "-beta")
	if lowID != "" {
		return lowID
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func detectChannel(bundleID, version, name, desc string) string {
	combined := strings.ToLower(fmt.Sprintf("%s %s %s %s", bundleID, version, name, desc))
	if strings.Contains(combined, "nightly") {
		return "nightly"
	}
	if strings.Contains(combined, "alpha") {
		return "alpha"
	}
	if strings.Contains(combined, "beta") || strings.Contains(combined, " rc") || strings.Contains(combined, "-rc") || strings.Contains(version, "b") {
		return "beta"
	}
	return "stable"
}

func fetchAltStoreApps(client *http.Client, src StoreSource) []StoreApp {
	url := strings.ReplaceAll(src.URL, " ", "%20")
	if !strings.HasSuffix(url, ".json") && !strings.Contains(url, "apps.json") {
		url = strings.TrimSuffix(url, "/") + "/apps.json"
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Sidey-AltStore-Client/1.0")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if url != src.URL {
			req2, _ := http.NewRequest("GET", src.URL, nil)
			req2.Header.Set("User-Agent", "Sidey-AltStore-Client/1.0")
			resp2, err2 := client.Do(req2)
			if err2 == nil && resp2.StatusCode == http.StatusOK {
				resp = resp2
			} else {
				return nil
			}
		} else {
			return nil
		}
	}
	defer resp.Body.Close()

	var sourceData struct {
		Identifier string `json:"identifier"`
		Name       string `json:"name"`
		Apps       []struct {
			Name                 string `json:"name"`
			BundleIdentifier     string `json:"bundleIdentifier"`
			DeveloperName        string `json:"developerName"`
			Version              string `json:"version"`
			VersionDate          string `json:"versionDate"`
			LocalizedDescription string `json:"localizedDescription"`
			IconURL              string `json:"iconURL"`
			DownloadURL          string `json:"downloadURL"`
			Size                 int64  `json:"size"`
			// Modern per-version history; when present it replaces the
			// legacy top-level version fields above.
			Versions []struct {
				Version              string `json:"version"`
				Date                 string `json:"date"`
				LocalizedDescription string `json:"localizedDescription"`
				DownloadURL          string `json:"downloadURL"`
				Size                 int64  `json:"size"`
				MinOSVersion         string `json:"minOSVersion"`
			} `json:"versions"`
		} `json:"apps"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sourceData); err != nil {
		return nil
	}

	// Pinned-identifier check: a feed that identifies itself as something
	// unexpected is skipped, never merged.
	if src.ExpectedID != "" && sourceData.Identifier != "" && sourceData.Identifier != src.ExpectedID {
		return nil
	}

	sourceTitle := sourceData.Name
	if sourceTitle == "" {
		sourceTitle = src.Name
	}

	appMap := make(map[string]*StoreApp)
	var order []string

	for _, a := range sourceData.Apps {
		type rawVer struct {
			version, date, desc, url string
			size                     int64
		}
		var entries []rawVer
		for _, v := range a.Versions {
			if v.DownloadURL == "" {
				continue
			}
			entries = append(entries, rawVer{v.Version, v.Date, v.LocalizedDescription, v.DownloadURL, v.Size})
		}
		if len(entries) == 0 && a.DownloadURL != "" {
			// Legacy single-version format.
			entries = append(entries, rawVer{a.Version, a.VersionDate, a.LocalizedDescription, a.DownloadURL, a.Size})
		}
		for _, e := range entries {
			// Server-side downloader only speaks https; anything else (http,
			// custom schemes) can never be imported, so don't list it.
			if !strings.HasPrefix(strings.ToLower(e.url), "https://") {
				continue
			}
			baseKey := normalizeBaseKey(a.BundleIdentifier, a.Name)
			channel := detectChannel(a.BundleIdentifier, e.version, a.Name, e.desc)

			ver := AppVersion{
				Version:     e.version,
				Channel:     channel,
				BundleID:    a.BundleIdentifier,
				DownloadURL: e.url,
				Size:        e.size,
				UpdatedDate: e.date,
			}

			if existing, exists := appMap[baseKey]; exists {
				existing.Versions = append(existing.Versions, ver)
				if channel == "stable" {
					existing.Name = a.Name
					if a.IconURL != "" {
						existing.IconURL = a.IconURL
					}
					if a.LocalizedDescription != "" {
						existing.Description = a.LocalizedDescription
					}
				}
			} else {
				cleanName := a.Name
				cleanName = strings.TrimSuffix(cleanName, " (Beta)")
				cleanName = strings.TrimSuffix(cleanName, " Beta")

				app := &StoreApp{
					ID:          "alt-" + baseKey,
					Name:        cleanName,
					Developer:   a.DeveloperName,
					Description: a.LocalizedDescription,
					IconURL:     a.IconURL,
					Source:      sourceTitle,
					SourceType:  "altstore",
					Versions:    []AppVersion{ver},
				}
				appMap[baseKey] = app
				order = append(order, baseKey)
			}
		}
	}

	var results []StoreApp
	for _, k := range order {
		app := appMap[k]
		sort.Slice(app.Versions, func(i, j int) bool {
			if app.Versions[i].Channel == "stable" && app.Versions[j].Channel != "stable" {
				return true
			}
			if app.Versions[j].Channel == "stable" && app.Versions[i].Channel != "stable" {
				return false
			}
			return app.Versions[i].UpdatedDate > app.Versions[j].UpdatedDate
		})

		if len(app.Versions) > 0 {
			primary := app.Versions[0]
			app.BundleID = primary.BundleID
			app.Version = primary.Version
			app.Channel = primary.Channel
			app.DownloadURL = primary.DownloadURL
			app.Size = primary.Size
			app.UpdatedDate = primary.UpdatedDate
		}
		results = append(results, *app)
	}
	return results
}

func fetchGitHubApps(client *http.Client, src StoreSource) []StoreApp {
	repo := src.URL
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases", repo)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Sidey-ControlPlane/1.0")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	defer resp.Body.Close()

	var releases []struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Body        string `json:"body"`
		Prerelease  bool   `json:"prerelease"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			Size               int64  `json:"size"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil || len(releases) == 0 {
		return nil
	}

	appName := src.Name
	if appName == "" {
		parts := strings.Split(repo, "/")
		appName = parts[len(parts)-1]
	}

	var versions []AppVersion
	var description string

	for _, rel := range releases {
		channel := "stable"
		if rel.Prerelease || strings.Contains(strings.ToLower(rel.TagName), "nightly") || strings.Contains(strings.ToLower(rel.TagName), "beta") || strings.Contains(strings.ToLower(rel.TagName), "alpha") {
			if strings.Contains(strings.ToLower(rel.TagName), "nightly") {
				channel = "nightly"
			} else if strings.Contains(strings.ToLower(rel.TagName), "alpha") {
				channel = "alpha"
			} else {
				channel = "beta"
			}
		}

		for _, asset := range rel.Assets {
			if strings.HasSuffix(strings.ToLower(asset.Name), ".ipa") {
				vLabel := rel.TagName
				cleanAssetName := strings.TrimSuffix(asset.Name, ".ipa")
				if !strings.EqualFold(cleanAssetName, appName) && !strings.Contains(vLabel, cleanAssetName) {
					vLabel = fmt.Sprintf("%s (%s)", rel.TagName, cleanAssetName)
				}

				versions = append(versions, AppVersion{
					Version:     vLabel,
					Channel:     channel,
					BundleID:    fmt.Sprintf("com.github.%s", strings.ReplaceAll(repo, "/", ".")),
					DownloadURL: asset.BrowserDownloadURL,
					Size:        asset.Size,
					UpdatedDate: rel.PublishedAt,
				})

				if description == "" && rel.Body != "" {
					description = rel.Body
				}
			}
		}
	}

	if len(versions) == 0 {
		return nil
	}

	// Sort versions: stable first, then newest
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Channel == "stable" && versions[j].Channel != "stable" {
			return true
		}
		if versions[j].Channel == "stable" && versions[i].Channel != "stable" {
			return false
		}
		return versions[i].UpdatedDate > versions[j].UpdatedDate
	})

	if len(versions) > 6 {
		versions = versions[:6]
	}

	primary := versions[0]
	app := StoreApp{
		ID:          fmt.Sprintf("gh-%s", strings.ReplaceAll(repo, "/", "-")),
		Name:        appName,
		Developer:   strings.Split(repo, "/")[0],
		Description: description,
		IconURL:     fmt.Sprintf("https://github.com/%s.png", strings.Split(repo, "/")[0]),
		Source:      "GitHub: " + repo,
		SourceType:  "github",
		Versions:    versions,
		BundleID:    primary.BundleID,
		Version:     primary.Version,
		Channel:     primary.Channel,
		DownloadURL: primary.DownloadURL,
		Size:        primary.Size,
		UpdatedDate: primary.UpdatedDate,
	}

	return []StoreApp{app}
}

type storeInstallRequest struct {
	Name        string    `json:"name"`
	DownloadURL string    `json:"download_url"`
	DeviceID    uuid.UUID `json:"device_id"`
	Mode        string    `json:"mode"` // "livecontainer" or "native"
	AppleID     string    `json:"apple_id"`
}

// handleStoreInstall downloads an IPA from a store link, creates an artifact,
// and routes it to either LiveContainer or Native Apple Signing.
func (s *Server) handleStoreInstall(w http.ResponseWriter, r *http.Request) {
	var req storeInstallRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.DownloadURL == "" || req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "download_url and device_id are required"})
		return
	}
	// Untrusted input: only https downloads are ever fetched server-side.
	if !strings.HasPrefix(strings.ToLower(req.DownloadURL), "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "download_url must be https"})
		return
	}
	if req.Mode == "" {
		req.Mode = "livecontainer"
	}

	// 1. Download remote IPA stream to temp, capped like uploads so a
	// hostile or broken source cannot exhaust the artifact volume.
	client := &http.Client{Timeout: 90 * time.Second}
	dlReq, err := http.NewRequest("GET", req.DownloadURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid download url"})
		return
	}
	dlReq.Header.Set("User-Agent", "Sidey-Store-Downloader/1.0")
	dlResp, err := client.Do(dlReq)
	if err != nil || dlResp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to download IPA from source"})
		return
	}
	defer dlResp.Body.Close()

	sha256Hex, tmp, err := s.artifacts.SaveToTemp(io.LimitReader(dlResp.Body, s.maxArtifactBytes+1))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing download failed")
		return
	}
	defer s.artifacts.DiscardTemp(tmp)
	if fi, statErr := os.Stat(tmp); statErr != nil || fi.Size() > s.maxArtifactBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "downloaded file exceeds size limit"})
		return
	}

	// 2. Inspect downloaded IPA. Anything that fails inspection is
	// rejected: no fabricated metadata, no silent approval.
	meta, metaErr := artifacts.Inspect(tmp)
	if metaErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "downloaded file failed IPA inspection: " + metaErr.Error()})
		return
	}

	// 3. Publish into content-addressed store
	_, err = s.artifacts.Publish(sha256Hex, tmp)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "publishing artifact failed")
		return
	}

	filename := req.Name
	if !strings.HasSuffix(filename, ".ipa") {
		filename = filename + ".ipa"
	}

	// 4. Save artifact in database. Imports land quarantined like uploads
	// and need explicit approval in the dashboard; re-importing known bytes
	// never changes the stored quarantine state.
	var artifactID uuid.UUID
	var quarantineState string
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO artifacts (sha256, filename, bundle_identifier, version, platform, quarantine_state, source)
		VALUES ($1, $2, $3, $4, $5, 'quarantined', 'store')
		ON CONFLICT (sha256) DO UPDATE SET filename = EXCLUDED.filename
		RETURNING id, quarantine_state`,
		sha256Hex, filename, meta.BundleIdentifier, meta.Version, meta.Platform).Scan(&artifactID, &quarantineState)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "artifact database upsert failed")
		return
	}

	// Keep subscriptions in sync: an imported version becomes the seen
	// baseline so it stops surfacing as an update.
	if meta.BundleIdentifier != "" && meta.Version != "" {
		_, _ = s.pool.Exec(r.Context(), `
			UPDATE app_subscriptions SET seen_version = $2 WHERE bundle_id = $1`,
			meta.BundleIdentifier, meta.Version)
	}

	// LiveContainer itself must always be signed natively with Apple developer cert
	if strings.Contains(strings.ToLower(req.Name), "livecontainer") ||
		strings.Contains(strings.ToLower(req.DownloadURL), "livecontainer") ||
		meta.BundleIdentifier == "com.kdt.livecontainer" {
		req.Mode = "native"
	}

	// Fresh imports are quarantined: store now, install only after explicit
	// approval in the dashboard (re-imports of already-approved bytes flow
	// straight through to step 5).
	if quarantineState != "approved" {
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":           "ok",
			"mode":             req.Mode,
			"artifact_id":      artifactID.String(),
			"device_id":        req.DeviceID.String(),
			"quarantine_state": quarantineState,
			"message":          fmt.Sprintf("Downloaded %s into quarantine; approve it in the dashboard to install", req.Name),
		})
		return
	}

	// 5. Route to LiveContainer or Native Deploy
	if req.Mode == "livecontainer" {
		var udid, deviceName, platform string
		_ = s.pool.QueryRow(r.Context(), `SELECT udid, COALESCE(device_name, ''), platform FROM devices WHERE id = $1`, req.DeviceID).Scan(&udid, &deviceName, &platform)

		params := map[string]any{
			"artifact_id": artifactID.String(),
			"udid":        udid,
			"device_name": deviceName,
			"device_type": platform,
			"target":      "livecontainer",
			"bundle_id":   "com.kdt.livecontainer",
		}
		var jobID uuid.UUID
		err = s.pool.QueryRow(r.Context(), `
			INSERT INTO jobs (job_type, device_id, parameters, max_attempts, idempotency_key)
			VALUES ('livecontainer_push', $1, $2, 3, $3)
			RETURNING id`, req.DeviceID, params, uuid.New().String()).Scan(&jobID)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "enqueuing LiveContainer push job failed")
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"status":      "ok",
			"mode":        "livecontainer",
			"artifact_id": artifactID.String(),
			"device_id":   req.DeviceID.String(),
			"job_id":      jobID.String(),
			"message":     fmt.Sprintf("Downloaded %s and queued for wireless LiveContainer install", req.Name),
		})
	} else {
		// Native Sign & Deploy
		var udid, deviceName, platform string
		_ = s.pool.QueryRow(r.Context(), `SELECT udid, COALESCE(device_name, ''), platform FROM devices WHERE id = $1`, req.DeviceID).Scan(&udid, &deviceName, &platform)

		appleID := strings.TrimSpace(req.AppleID)
		if appleID == "" || appleID == "auto" {
			var chosenLabel string
			_ = s.pool.QueryRow(r.Context(), `
				SELECT label FROM apple_accounts
				WHERE auth_state IN ('authenticated', 'authenticating')
				ORDER BY COALESCE(app_id_available_quantity, GREATEST(0, 3 - registered_app_id_count)) DESC, last_auth_at DESC
				LIMIT 1`).Scan(&chosenLabel)
			if chosenLabel != "" {
				appleID = chosenLabel
			}
		}

		params := map[string]any{
			"artifact_id":  artifactID.String(),
			"udid":         udid,
			"device_name":  deviceName,
			"device_type":  platform,
			"machine_name": "isideload-minimal",
			"apple_id":     appleID,
			"source_url":   s.selfSourceURL(r),
		}
		var signJobID uuid.UUID
		err = s.pool.QueryRow(r.Context(), `
			INSERT INTO jobs (job_type, device_id, parameters, max_attempts, idempotency_key)
			VALUES ('sign', $1, $2, 5, $3)
			RETURNING id`, req.DeviceID, params, uuid.New().String()).Scan(&signJobID)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "enqueuing sign job failed")
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"status":      "ok",
			"mode":        "native",
			"artifact_id": artifactID.String(),
			"device_id":   req.DeviceID.String(),
			"apple_id":    appleID,
			"sign_job_id": signJobID.String(),
			"message":     fmt.Sprintf("Downloaded %s and queued for Apple signing & native install", req.Name),
		})
	}
}

// selfSourceURL resolves the Sidey self-hosted App Store source URL for the
// requesting host, embedded into LiveContainer bundles during signing so the
// device can discover the repository without manual setup.
func (s *Server) selfSourceURL(r *http.Request) string {
	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	return fmt.Sprintf("%s://%s/api/v1/store/source.json", proto, r.Host)
}

// handleStoreSourceJSON renders the AltStore / SideStore / LiveContainer compatible repository JSON.
func (s *Server) handleStoreSourceJSON(w http.ResponseWriter, r *http.Request) {
	apps := s.getStoreApps(r.Context())

	host := r.Host
	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", proto, host)

	type AltVersion struct {
		Version              string `json:"version"`
		Date                 string `json:"date"`
		DownloadURL          string `json:"downloadURL"`
		Size                 int64  `json:"size"`
		LocalizedDescription string `json:"localizedDescription,omitempty"`
	}

	type AltApp struct {
		Name                 string         `json:"name"`
		BundleIdentifier     string         `json:"bundleIdentifier"`
		DeveloperName        string         `json:"developerName"`
		Version              string         `json:"version"`
		VersionDate          string         `json:"versionDate"`
		VersionDescription   string         `json:"versionDescription,omitempty"`
		DownloadURL          string         `json:"downloadURL"`
		LocalizedDescription string         `json:"localizedDescription"`
		IconURL              string         `json:"iconURL"`
		TintColor            string         `json:"tintColor,omitempty"`
		Size                 int64          `json:"size"`
		Versions             []AltVersion   `json:"versions,omitempty"`
		AppPermissions       map[string]any `json:"appPermissions,omitempty"`
	}

	type AltSource struct {
		Name        string   `json:"name"`
		Identifier  string   `json:"identifier"`
		Subtitle    string   `json:"subtitle,omitempty"`
		Description string   `json:"description,omitempty"`
		IconURL     string   `json:"iconURL,omitempty"`
		Website     string   `json:"website,omitempty"`
		Apps        []AltApp `json:"apps"`
	}

	var altApps []AltApp
	for _, app := range apps {
		var versions []AltVersion
		for _, v := range app.Versions {
			dlURL := v.DownloadURL
			if strings.HasPrefix(dlURL, "/") {
				dlURL = baseURL + dlURL
			}
			versions = append(versions, AltVersion{
				Version:              v.Version,
				Date:                 v.UpdatedDate,
				DownloadURL:          dlURL,
				Size:                 v.Size,
				LocalizedDescription: fmt.Sprintf("%s %s (%s)", app.Name, v.Version, v.Channel),
			})
		}
		mainDL := app.DownloadURL
		if strings.HasPrefix(mainDL, "/") {
			mainDL = baseURL + mainDL
		}
		icon := app.IconURL
		if strings.HasPrefix(icon, "/") {
			icon = baseURL + icon
		}

		bundleID := app.BundleID
		if bundleID == "" {
			bundleID = "com.sidey.store." + strings.ToLower(strings.ReplaceAll(app.Name, " ", ""))
		}

		altApps = append(altApps, AltApp{
			Name:                 app.Name,
			BundleIdentifier:     bundleID,
			DeveloperName:        app.Developer,
			Version:              app.Version,
			VersionDate:          app.UpdatedDate,
			VersionDescription:   app.Description,
			DownloadURL:          mainDL,
			LocalizedDescription: app.Description,
			IconURL:              icon,
			TintColor:            "388BFD",
			Size:                 app.Size,
			Versions:             versions,
			AppPermissions: map[string]any{
				"entitlements": []string{},
				"privacy":      map[string]any{},
			},
		})
	}

	// Sidey's own library: every approved original IPA is published with a
	// Sidey-hosted download, grouped by bundle with newest first. This is
	// what makes Sidey-managed apps (including the companion app, once its
	// IPA is approved) installable from the self-hosted source.
	type ownEntry struct {
		id, version, date, filename string
	}
	ownByBundle := map[string][]ownEntry{}
	var ownOrder []string
	ownRows, ownErr := s.pool.Query(r.Context(), `
		SELECT id::text, filename, COALESCE(version, ''),
		       bundle_identifier, imported_at
		FROM artifacts
		WHERE quarantine_state = 'approved'
		  AND bundle_identifier IS NOT NULL AND bundle_identifier <> ''
		ORDER BY imported_at DESC`)
	if ownErr == nil {
		for ownRows.Next() {
			var eid, fn, ver, bid string
			var at time.Time
			if err := ownRows.Scan(&eid, &fn, &ver, &bid, &at); err != nil {
				continue
			}
			if _, ok := ownByBundle[bid]; !ok {
				ownOrder = append(ownOrder, bid)
			}
			ownByBundle[bid] = append(ownByBundle[bid], ownEntry{eid, ver, at.Format(time.RFC3339), fn})
		}
		ownRows.Close()
		if err := ownRows.Err(); err != nil {
			s.logger.Warn("self-source artifacts lookup failed", "error", err)
		}
	} else {
		s.logger.Warn("self-source artifacts lookup failed", "error", ownErr)
	}
	for _, bid := range ownOrder {
		list := ownByBundle[bid]
		sort.Slice(list, func(i, j int) bool { return compareVersions(list[i].version, list[j].version) > 0 })
		name := strings.TrimSuffix(list[0].filename, ".ipa")
		if name == "" {
			name = bid
		}
		var versions []AltVersion
		for _, e := range list {
			versions = append(versions, AltVersion{
				Version:              e.version,
				Date:                 e.date,
				DownloadURL:          baseURL + "/api/v1/store/artifacts/" + e.id + "/install.ipa",
				LocalizedDescription: fmt.Sprintf("%s %s (Sidey library)", name, e.version),
			})
		}
		altApps = append(altApps, AltApp{
			Name:                 name,
			BundleIdentifier:     bid,
			DeveloperName:        "Sidey",
			Version:              list[0].version,
			VersionDate:          list[0].date,
			VersionDescription:   "Approved IPA from the Sidey library.",
			DownloadURL:          baseURL + "/api/v1/store/artifacts/" + list[0].id + "/install.ipa",
			LocalizedDescription: "Approved IPA from the Sidey library.",
			TintColor:            "388BFD",
			Versions:             versions,
			AppPermissions: map[string]any{
				"entitlements": []string{},
				"privacy":      map[string]any{},
			},
		})
	}

	source := AltSource{
		Name:        "Sidey App Store",
		Identifier:  "com.sidey.store",
		Subtitle:    "Self-Hosted Sideloading Catalog",
		Description: "Your self-hosted Sidey App Store repository with community apps, games, media, and tools.",
		IconURL:     baseURL + "/icons/icon-192.png",
		Website:     baseURL,
		Apps:        altApps,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(source)
}
