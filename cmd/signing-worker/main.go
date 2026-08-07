// signing-worker is the Phase F headless signing service.
//
// It enrols as a control-plane agent, claims "sign" jobs (job_type=sign),
// downloads the approved source IPA, signs it with isideload's signonly
// binary (reusing the account's certificate identity by machine name) and
// uploads the signed derivative back to the control plane.
//
// The isideload state (private key + anisette state) is stored envelope
// encrypted on disk: the plaintext only ever exists in a memory-backed
// runtime directory while a signing job runs. The key-encryption key (KEK)
// comes from /run/secrets/apple_credential_key (32 bytes, hex encoded).
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	claimPollSeconds    = 30
	heartbeatSeconds    = 60
	jobTimeout          = 900 * time.Second
	maxErrorDetails     = 2000
	defaultMachineName  = "isideload-minimal"
	defaultStateRuntime = "/run/sidey/signing-state"
	defaultStateVolume  = "/var/lib/sidey/signing-worker"
	defaultControlPlane = "http://127.0.0.1:8080"
	defaultAnisetteURL  = "http://127.0.0.1:6970"
)

type config struct {
	controlPlane  string
	agentStateDir string
	importDir     string
	stateRuntime  string
	signonlyBin   string
	anisetteURL   string
	appleID       string
	applePassword string
	enrolToken    string
	codeFile      string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := config{
		controlPlane:  envOr("SIDEY_CONTROL_PLANE_URL", defaultControlPlane),
		agentStateDir: envOr("SIDEY_AGENT_STATE_DIR", defaultStateVolume),
		importDir:     os.Getenv("SIDEY_IMPORT_STATE_DIR"),
		stateRuntime:  envOr("SIDEY_STATE_RUNTIME_DIR", defaultStateRuntime),
		signonlyBin:   envOr("SIDEY_SIGNONLY_BIN", "/usr/local/bin/signonly"),
		anisetteURL:   envOr("SIDEY_ANISETTE_URL", defaultAnisetteURL),
		appleID:       os.Getenv("SIDEY_APPLE_ID"),
		applePassword: os.Getenv("SIDEY_APPLE_MAIN_PASSWORD"),
		enrolToken:    os.Getenv("SIDEY_ENROLMENT_TOKEN"),
		codeFile:      envOr("SIDEY_2FA_CODE_FILE", "/tmp/opencode/2fa-code.txt"),
	}
	// Optional credentials: env vars take precedence; fall back to the
	// compose-mounted secret, then a credentials file in the state dir.
	// Lines are SIDEY_APPLE_ID=... / SIDEY_APPLE_MAIN_PASSWORD=...
	if cfg.appleID == "" || cfg.applePassword == "" {
		readCredsFile("/run/secrets/apple_credentials", &cfg)
	}
	loadCredsFile(cfg.agentStateDir, &cfg)

	if err := os.MkdirAll(cfg.agentStateDir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}

	agentKey, agentID, err := ensureAgent(cfg, "signing-worker")
	if err != nil {
		log.Fatalf("agent setup: %v", err)
	}

	// One-time state import: seed the encrypted volume from plaintext
	// isideload state (preserves the existing certificate identity).
	if cfg.importDir != "" {
		if err := importState(cfg.importDir, cfg.agentStateDir); err != nil {
			log.Fatalf("state import: %v", err)
		}
	}

	go serveHealthz()
	go func() {
		for {
			heartbeat(cfg, agentKey)
			time.Sleep(heartbeatSeconds * time.Second)
		}
	}()

	log.Printf("signing-worker %s ready (polling every %ds)", agentID, claimPollSeconds)
	for {
		claimAndRun(cfg, agentKey)
		time.Sleep(claimPollSeconds * time.Second)
	}
}

// ---------------------------------------------------------------------------
// Agent lifecycle

func loadCredsFile(stateDir string, cfg *config) {
	data, err := os.ReadFile(filepath.Join(stateDir, "credentials"))
	if err != nil {
		return
	}
	parseCreds(data, cfg)
}

// readCredsFile reads credentials from an explicit file path (e.g. the
// compose-mounted /run/secrets/apple_credentials).
func readCredsFile(path string, cfg *config) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	parseCreds(data, cfg)
}

func parseCreds(data []byte, cfg *config) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SIDEY_APPLE_ID=") {
			if cfg.appleID == "" {
				cfg.appleID = strings.TrimPrefix(line, "SIDEY_APPLE_ID=")
			}
		}
		if strings.HasPrefix(line, "SIDEY_APPLE_MAIN_PASSWORD=") {
			if cfg.applePassword == "" {
				cfg.applePassword = strings.TrimPrefix(line, "SIDEY_APPLE_MAIN_PASSWORD=")
			}
		}
	}
}

func ensureAgent(cfg config, name string) (key, id string, err error) {
	keyFile := filepath.Join(cfg.agentStateDir, "api_key")
	idFile := filepath.Join(cfg.agentStateDir, "agent_id")
	if k, err := os.ReadFile(keyFile); err == nil {
		if i, err := os.ReadFile(idFile); err == nil {
			return string(k), string(i), nil
		}
	}
	if cfg.enrolToken == "" {
		return "", "", errors.New("no agent key and no SIDEY_ENROLMENT_TOKEN")
	}
	body := map[string]any{
		"name":             name,
		"operating_system": "linux",
		"architecture":     runtime.GOARCH,
		"software_version": "phase-f",
		"tailnet_identity": "signing-worker",
		"capabilities":     map[string]any{"signing": true},
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", cfg.controlPlane+"/api/v1/agents/enrol", bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.enrolToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		AgentID string `json:"agent_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("enrol response: %w", err)
	}
	if out.APIKey == "" {
		return "", "", errors.New("enrol returned no api_key")
	}
	if err := os.WriteFile(keyFile, []byte(out.APIKey), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(idFile, []byte(out.AgentID), 0o600); err != nil {
		return "", "", err
	}
	log.Printf("enrolled as agent %s", out.AgentID)
	return out.APIKey, out.AgentID, nil
}

func heartbeat(cfg config, agentKey string) {
	body, _ := json.Marshal(map[string]any{"capabilities": map[string]any{"signing": true}})
	req, err := http.NewRequest("POST", cfg.controlPlane+"/api/v1/agents/me/heartbeat", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------------------
// Job loop

type signParams struct {
	ArtifactID  string `json:"artifact_id"`
	MachineName string `json:"machine_name"`
	DeviceUdid  string `json:"udid"`
	DeviceName  string `json:"device_name"`
	DeviceType  string `json:"device_type"`
}

func claimAndRun(cfg config, agentKey string) {
	body, _ := json.Marshal(map[string]any{
		"job_types": []string{"sign"},
		"limit":     1,
	})
	req, err := http.NewRequest("POST", cfg.controlPlane+"/api/v1/jobs/claim", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("claim failed: %v", err)
		return
	}
	defer resp.Body.Close()
	var out struct {
		Jobs []struct {
			ID         string          `json:"id"`
			DeviceID   *string         `json:"device_id"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return
	}
	for _, j := range out.Jobs {
		runJob(cfg, agentKey, j.ID, j.DeviceID, j.Parameters)
	}
}

func postJobStatus(cfg config, agentKey, jobID, state string, progress *int, category, details string, result any) {
	reqBody := map[string]any{"state": state}
	if progress != nil {
		reqBody["progress"] = *progress
	}
	if category != "" {
		reqBody["error_category"] = category
	}
	if details != "" {
		reqBody["error_details"] = details
	}
	if result != nil {
		reqBody["result"] = result
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", cfg.controlPlane+"/api/v1/jobs/"+jobID+"/status", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("job %s status update failed: %v", jobID, err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func runJob(cfg config, agentKey, jobID string, deviceID *string, rawParams json.RawMessage) {
	progress := 5
	postJobStatus(cfg, agentKey, jobID, "in_progress", &progress, "", "", nil)

	var params signParams
	if err := json.Unmarshal(rawParams, &params); err != nil || params.ArtifactID == "" {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other",
			"sign job parameters missing artifact_id", nil)
		return
	}
	if params.DeviceUdid == "" {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other",
			"sign job parameters missing udid (no target device)", nil)
		return
	}
	machineName := params.MachineName
	if machineName == "" {
		machineName = defaultMachineName
	}

	stopHeartbeat := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(heartbeatSeconds * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				postJobStatus(cfg, agentKey, jobID, "in_progress", nil, "", "", nil)
			case <-stopHeartbeat:
				return
			}
		}
	}()

	workDir, err := os.MkdirTemp("", "sign-*")
	if err != nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other", err.Error(), nil)
		return
	}
	defer os.RemoveAll(workDir)

	// 1. Download the approved source IPA.
	sourceIPA := filepath.Join(workDir, "source.ipa")
	if err := downloadArtifact(cfg, agentKey, params.ArtifactID, sourceIPA); err != nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other", "artifact download failed: "+err.Error(), nil)
		close(stopHeartbeat)
		wg.Wait()
		return
	}
	progress = 15
	postJobStatus(cfg, agentKey, jobID, "in_progress", &progress, "", "", nil)

	// 2. Decrypt isideload state into the memory-backed runtime dir.
	if err := os.MkdirAll(cfg.stateRuntime, 0o700); err != nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other", err.Error(), nil)
		close(stopHeartbeat)
		wg.Wait()
		return
	}
	if err := decryptState(cfg.agentStateDir, cfg.stateRuntime); err != nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "certificate", "state decrypt failed: "+err.Error(), nil)
		close(stopHeartbeat)
		wg.Wait()
		return
	}

	// 3. Sign with signonly.
	signedIPA := filepath.Join(workDir, "signed.ipa")
	signResult, signErr := runSignonly(cfg, workDir, sourceIPA, signedIPA, machineName, params.DeviceUdid, params.DeviceName, params.DeviceType)
	if signErr != nil {
		category := "other"
		details := signErr.Error()
		if signResult != nil {
			if signResult.Category != "" {
				category = signResult.Category
			}
			if signResult.Error != "" {
				details = signResult.Error
			}
		}
		if len(details) > maxErrorDetails {
			details = details[:maxErrorDetails]
		}
		postJobStatus(cfg, agentKey, jobID, "failed", nil, category, details, nil)
		// State may have been updated (anisette) even on failure.
		encryptState(cfg.stateRuntime, cfg.agentStateDir)
		close(stopHeartbeat)
		wg.Wait()
		return
	}
	progress = 85
	postJobStatus(cfg, agentKey, jobID, "in_progress", &progress, "", "", nil)

	// 4. Upload the signed IPA.
	if deviceID == nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other", "sign job has no device_id", nil)
		encryptState(cfg.stateRuntime, cfg.agentStateDir)
		close(stopHeartbeat)
		wg.Wait()
		return
	}
	signedID, err := uploadSignedIPA(cfg, agentKey, jobID, params, *deviceID, signResult, signedIPA)
	if err != nil {
		postJobStatus(cfg, agentKey, jobID, "failed", nil, "other", "signed ipa upload failed: "+err.Error(), nil)
		encryptState(cfg.stateRuntime, cfg.agentStateDir)
		close(stopHeartbeat)
		wg.Wait()
		return
	}

	// 5. Re-encrypt the (possibly updated) state back to the volume.
	if err := encryptState(cfg.stateRuntime, cfg.agentStateDir); err != nil {
		log.Printf("state re-encrypt failed: %v", err)
	}

	result := map[string]any{
		"signed_artifact_id":       signedID,
		"signed_ipa_sha256":        signResult.SignedIPASha256,
		"profile_expiry_at":        signResult.ProfileExpiryAt,
		"cert_serial":              signResult.CertSerial,
		"team_id":                  signResult.TeamID,
		"bundle_identifier":        signResult.BundleIdentifier,
		"signed_bundle_identifier": signResult.SignedBundleIdentifier,
		"version":                  signResult.Version,
		"device_count":             signResult.DeviceCount,
		"app_id_count":             signResult.AppIDCount,
	}
	postJobStatus(cfg, agentKey, jobID, "completed", nil, "", "", result)
	close(stopHeartbeat)
	wg.Wait()
	log.Printf("job %s: signed %s (sha256 %s, cert %s)", jobID, params.ArtifactID, signResult.SignedIPASha256, signResult.CertSerial)
}

func downloadArtifact(cfg config, agentKey, artifactID, dest string) error {
	req, err := http.NewRequest("GET",
		cfg.controlPlane+"/api/v1/agents/artifacts/"+artifactID+"/download", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+agentKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// ---------------------------------------------------------------------------
// signonly execution

type signonlyResult struct {
	Status                 string `json:"status"`
	Category               string `json:"category"`
	Error                  string `json:"error"`
	SignedIPASha256        string `json:"signed_ipa_sha256"`
	BundleIdentifier       string `json:"bundle_identifier"`
	SignedBundleIdentifier string `json:"signed_bundle_identifier"`
	Version                string `json:"version"`
	ProfileExpiryAt        string `json:"profile_expiry_at"`
	CertSerial             string `json:"cert_serial"`
	TeamID                 string `json:"team_id"`
	DeviceCount            int    `json:"device_count"`
	AppIDCount             int    `json:"app_id_count"`
}

func runSignonly(cfg config, workDir, sourceIPA, signedIPA, machineName, deviceUDID, deviceName, deviceType string) (*signonlyResult, error) {
	if cfg.appleID == "" || cfg.applePassword == "" {
		return nil, errors.New("Apple credentials not configured (SIDEY_APPLE_ID / SIDEY_APPLE_MAIN_PASSWORD)")
	}
	if deviceUDID == "" {
		return nil, errors.New("no device UDID: refusing to sign without a target device")
	}
	cmd := exec.Command(cfg.signonlyBin, cfg.appleID, cfg.applePassword, sourceIPA, signedIPA)
	cmd.Env = append(os.Environ(),
		"SIDEY_ISIDELOAD_STATE="+cfg.stateRuntime,
		"ANISETTE_URL="+cfg.anisetteURL,
		"MACHINE_NAME="+machineName,
		"DEVICE_UDID="+deviceUDID,
		"DEVICE_NAME="+deviceName,
		"DEVICE_TYPE="+deviceType,
		"SIGNONLY_2FA_CODE_FILE="+cfg.codeFile,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			res := parseSignonly(stdout.String())
			if res == nil {
				res = &signonlyResult{Category: "other", Error: strings.TrimSpace(stderr.String())}
			}
			if res.Error == "" {
				res.Error = err.Error()
			}
			return res, fmt.Errorf("signonly failed: %s", res.Error)
		}
	case <-time.After(jobTimeout):
		cmd.Process.Kill()
		return &signonlyResult{Category: "timeout"}, errors.New("signonly timed out")
	}

	res := parseSignonly(stdout.String())
	if res == nil {
		return nil, errors.New("signonly produced no JSON result")
	}
	if res.Status != "ok" {
		return res, fmt.Errorf("signonly error: %s", res.Error)
	}
	return res, nil
}

func parseSignonly(stdout string) *signonlyResult {
	var res signonlyResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		return nil
	}
	return &res
}

// ---------------------------------------------------------------------------
// Envelope encryption

func kekFromFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(data))
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("KEK must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("KEK must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

var kekCache struct {
	sync.Once
	key []byte
	err error
}

func kek() ([]byte, error) {
	kekCache.Do(func() {
		// Secret file mounted by compose; fall back to env for local testing.
		if key, err := kekFromFile("/run/secrets/apple_credential_key"); err == nil {
			kekCache.key = key
			return
		}
		if raw := os.Getenv("SIDEY_KEK"); raw != "" {
			if key, err := hex.DecodeString(strings.TrimSpace(raw)); err == nil && len(key) == 32 {
				kekCache.key = key
				return
			}
		}
		kekCache.err = errors.New("no KEK available (mount /run/secrets/apple_credential_key or set SIDEY_KEK)")
	})
	return kekCache.key, kekCache.err
}

func encryptFile(src, dst string) error {
	key, err := kek()
	if err != nil {
		return err
	}
	plain, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, append(nonce, sealed...), 0o600)
}

func decryptFile(src, dst string) error {
	key, err := kek()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(data) < gcm.NonceSize() {
		return errors.New("encrypted file too short")
	}
	nonce, sealed := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return fmt.Errorf("decrypt failed (wrong KEK?): %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, plain, 0o600)
}

// importState copies plaintext isideload state (e.g. the existing host
// /var/lib/sidey/isideload) into the encrypted volume. Only done once.
func importState(srcDir, stateDir string) error {
	if _, err := os.Stat(filepath.Join(stateDir, "state")); err == nil {
		log.Print("state already imported; skipping")
		return nil
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		rel := e.Name()
		dest := filepath.Join(stateDir, "state", rel)
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(srcDir, rel))
			if err != nil {
				return err
			}
			for _, f := range sub {
				src := filepath.Join(srcDir, rel, f.Name())
				if !f.IsDir() {
					if err := encryptFile(src, filepath.Join(dest, f.Name())); err != nil {
						return err
					}
					count++
				}
			}
		} else {
			if err := encryptFile(filepath.Join(srcDir, rel), dest); err != nil {
				return err
			}
			count++
		}
	}
	log.Printf("imported %d state files from %s", count, srcDir)
	return nil
}

// decryptState mirrors the encrypted volume state into the runtime dir.
func decryptState(stateDir, runtimeDir string) error {
	base := filepath.Join(stateDir, "state")
	if _, err := os.Stat(base); err != nil {
		return nil // nothing imported yet: signonly will create fresh state
	}
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		return decryptFile(path, filepath.Join(runtimeDir, rel))
	})
	return err
}

// encryptState mirrors the runtime dir state back into the encrypted volume.
func encryptState(runtimeDir, stateDir string) error {
	if _, err := os.Stat(runtimeDir); err != nil {
		return nil
	}
	base := filepath.Join(stateDir, "state")
	return filepath.Walk(runtimeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(runtimeDir, path)
		if err != nil {
			return err
		}
		return encryptFile(path, filepath.Join(base, rel))
	})
}

// ---------------------------------------------------------------------------
// Upload

func uploadSignedIPA(cfg config, agentKey, jobID string, params signParams, deviceID string, res *signonlyResult, signedIPA string) (string, error) {
	file, err := os.Open(signedIPA)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if fw, err := w.CreateFormFile("ipa", "signed.ipa"); err != nil {
		return "", err
	} else if _, err := io.Copy(fw, file); err != nil {
		return "", err
	}
	fields := map[string]string{
		"job_id":                   jobID,
		"source_artifact_id":       params.ArtifactID,
		"device_id":                deviceID,
		"account_email":            cfg.appleID,
		"team_id":                  res.TeamID,
		"cert_serial":              res.CertSerial,
		"profile_expiry_at":        res.ProfileExpiryAt,
		"signed_bundle_identifier": res.SignedBundleIdentifier,
		"signed_ipa_sha256":        res.SignedIPASha256,
		"device_count":             fmt.Sprintf("%d", res.DeviceCount),
		"app_id_count":             fmt.Sprintf("%d", res.AppIDCount),
	}
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := w.WriteField(k, v); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", cfg.controlPlane+"/api/v1/signed-artifacts", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+agentKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ---------------------------------------------------------------------------
// Health

func serveHealthz() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	port := envOr("PORT", "8090")
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Printf("healthz server: %v", err)
	}
}
