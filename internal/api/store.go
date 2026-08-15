package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
}

type StoreApp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BundleID    string `json:"bundle_id"`
	Version     string `json:"version"`
	Developer   string `json:"developer"`
	Description string `json:"description"`
	IconURL     string `json:"icon_url"`
	DownloadURL string `json:"download_url"`
	Source      string `json:"source"`
	SourceType  string `json:"source_type"`
	Size        int64  `json:"size"`
	UpdatedDate string `json:"updated_date"`
}

var defaultSources = []StoreSource{
	{
		ID:        "src-altstore-official",
		Type:      "altstore",
		URL:       "https://apps.altstore.io",
		Name:      "AltStore Official",
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
			sources = append(sources, custom...)
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
	} else if req.Name == "" {
		req.Name = req.URL
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
	path := "/var/lib/sidey/store_sources.json"
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &custom)
	}
	var filtered []StoreSource
	for _, src := range custom {
		if src.ID != id {
			filtered = append(filtered, src)
		}
	}
	_ = s.saveCustomSources(filtered)

	globalStoreCache.Lock()
	globalStoreCache.apps = nil
	globalStoreCache.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

// handleListStoreApps fetches and returns aggregated apps from all sources.
func (s *Server) handleListStoreApps(w http.ResponseWriter, r *http.Request) {
	globalStoreCache.RLock()
	if len(globalStoreCache.apps) > 0 && time.Since(globalStoreCache.updatedAt) < 10*time.Minute {
		cached := globalStoreCache.apps
		globalStoreCache.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"apps": cached, "cached": true})
		return
	}
	globalStoreCache.RUnlock()

	sources := s.loadStoreSources()
	var allApps []StoreApp
	var mu sync.Mutex
	var wg sync.WaitGroup

	client := &http.Client{Timeout: 8 * time.Second}

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

	writeJSON(w, http.StatusOK, map[string]any{"apps": allApps, "cached": false})
}

func fetchAltStoreApps(client *http.Client, src StoreSource) []StoreApp {
	url := src.URL
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
		Name string `json:"name"`
		Apps []struct {
			Name                 string `json:"name"`
			BundleIdentifier     string `json:"bundleIdentifier"`
			DeveloperName        string `json:"developerName"`
			Version              string `json:"version"`
			VersionDate          string `json:"versionDate"`
			LocalizedDescription string `json:"localizedDescription"`
			IconURL              string `json:"iconURL"`
			DownloadURL          string `json:"downloadURL"`
			Size                 int64  `json:"size"`
		} `json:"apps"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sourceData); err != nil {
		return nil
	}

	var results []StoreApp
	sourceTitle := sourceData.Name
	if sourceTitle == "" {
		sourceTitle = src.Name
	}

	for _, a := range sourceData.Apps {
		if a.DownloadURL == "" {
			continue
		}
		results = append(results, StoreApp{
			ID:          "alt-" + a.BundleIdentifier,
			Name:        a.Name,
			BundleID:    a.BundleIdentifier,
			Version:     a.Version,
			Developer:   a.DeveloperName,
			Description: a.LocalizedDescription,
			IconURL:     a.IconURL,
			DownloadURL: a.DownloadURL,
			Source:      sourceTitle,
			SourceType:  "altstore",
			Size:        a.Size,
			UpdatedDate: a.VersionDate,
		})
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

	var results []StoreApp
	for _, rel := range releases {
		for _, asset := range rel.Assets {
			if strings.HasSuffix(strings.ToLower(asset.Name), ".ipa") {
				appName := src.Name
				if appName == "" {
					parts := strings.Split(repo, "/")
					appName = parts[len(parts)-1]
				}
				results = append(results, StoreApp{
					ID:          fmt.Sprintf("gh-%s-%s", repo, asset.Name),
					Name:        fmt.Sprintf("%s (%s)", appName, strings.TrimSuffix(asset.Name, ".ipa")),
					BundleID:    fmt.Sprintf("com.github.%s", strings.ReplaceAll(repo, "/", ".")),
					Version:     rel.TagName,
					Developer:   strings.Split(repo, "/")[0],
					Description: rel.Body,
					IconURL:     fmt.Sprintf("https://github.com/%s.png", strings.Split(repo, "/")[0]),
					DownloadURL: asset.BrowserDownloadURL,
					Source:      "GitHub: " + repo,
					SourceType:  "github",
					Size:        asset.Size,
					UpdatedDate: rel.PublishedAt,
				})
				break
			}
		}
		if len(results) > 0 {
			break
		}
	}
	return results
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
	if req.Mode == "" {
		req.Mode = "livecontainer"
	}

	// 1. Download remote IPA stream to temp
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

	sha256Hex, tmp, err := s.artifacts.SaveToTemp(dlResp.Body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing download failed")
		return
	}
	defer s.artifacts.DiscardTemp(tmp)

	// 2. Inspect downloaded IPA
	meta, metaErr := artifacts.Inspect(tmp)
	if metaErr != nil {
		meta = &artifacts.Metadata{
			BundleIdentifier: "com.sidey.storeapp",
			Version:          "1.0",
			DevicePlatform:   "ios",
			Platform:         "iPhoneOS",
		}
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

	// 4. Save artifact in database
	var artifactID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO artifacts (sha256, filename, bundle_identifier, version, platform, quarantine_state, state_changed_at)
		VALUES ($1, $2, $3, $4, $5, 'approved', now())
		ON CONFLICT (sha256) DO UPDATE SET
			quarantine_state = 'approved',
			state_changed_at = now()
		RETURNING id`,
		sha256Hex, filename, meta.BundleIdentifier, meta.Version, meta.Platform).Scan(&artifactID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "artifact database upsert failed")
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
			INSERT INTO jobs (job_type, device_id, parameters, max_attempts)
			VALUES ('livecontainer_push', $1, $2, 3)
			RETURNING id`, req.DeviceID, params).Scan(&jobID)
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
				ORDER BY registered_app_id_count ASC, last_auth_at DESC
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
		}
		var signJobID uuid.UUID
		err = s.pool.QueryRow(r.Context(), `
			INSERT INTO jobs (job_type, device_id, parameters, max_attempts)
			VALUES ('sign', $1, $2, 5)
			RETURNING id`, req.DeviceID, params).Scan(&signJobID)
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
