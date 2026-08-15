package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bitxeno/atvloadly/internal/manager"
	"github.com/bitxeno/atvloadly/internal/model"
	"github.com/bitxeno/atvloadly/internal/service"
	"github.com/bitxeno/atvloadly/internal/utils"
	"howett.net/plist"
)

// session tracks an interactive plumesign subprocess (login or pairing) so
// the agent can relay the code shown on the TV (or the 2FA code) into the
// running process via pair-code/login-code ops.
type session struct {
	stdin  io.WriteCloser
	done   chan struct{}
	result response
	mu     sync.Mutex
	output strings.Builder
}

// lockedWriter appends captured output under the session lock.
type lockedWriter struct{ s *session }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.output.Write(p)
}

var (
	sessMu sync.Mutex
	sess   *session
)

func newSession() *session {
	s := &session{done: make(chan struct{})}
	sessMu.Lock()
	sess = s
	sessMu.Unlock()
	return s
}

func currentSession() *session {
	sessMu.Lock()
	defer sessMu.Unlock()
	return sess
}

func (s *session) run(name string, args ...string) {
	defer close(s.done)
	cmd := exec.Command(name, args...)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(), "SIDEY_HELPER=1")
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		s.result = response{OK: false, Error: fmt.Sprintf("pipe: %v", err), ErrorStage: "protocol"}
		return
	}
	s.stdin = stdinW
	cmd.Stdin = stdinR
	cmd.Stdout = lockedWriter{s}
	cmd.Stderr = lockedWriter{s}
	err = cmd.Run()
	stdinR.Close()
	if err != nil {
		s.result = response{OK: false, Error: err.Error(), ErrorStage: "protocol"}
		return
	}
	s.result = response{OK: true, Result: map[string]any{"output": s.output.String()}}
}

func (s *session) writeCode(code string) error {
	if s.stdin == nil {
		return fmt.Errorf("session stdin not open")
	}
	_, err := io.WriteString(s.stdin, code+"\n")
	return err
}

// dispatch routes one request to the matching verb handler.
func dispatch(req request) response {
	switch req.Op {
	case "ping":
		return response{OK: true, Result: map[string]any{
			"version":     opVersion,
			"data_dir":    dataDir,
			"plumesign":   findPlumesign(),
			"scripts_dir": scriptsDir,
		}}
	case "login":
		return opLogin(req)
	case "login-code":
		return opCode(req, "login")
	case "login-poll":
		return opPoll(req, "login")
	case "pair":
		return opPair(req)
	case "pair-code":
		return opCode(req, "pair")
	case "pair-poll":
		return opPoll(req, "pair")
	case "deploy":
		return opDeploy(req)
	case "verify":
		return opVerify(req)
	case "inventory":
		return opInventory(req)
	case "uninstall":
		return opUninstall(req)
	case "collect":
		return opCollect(req)
	case "device_info":
		return opDeviceInfo(req)
	case "ipa":
		return opIpa(req)
	default:
		return response{OK: false, Error: "unknown op: " + req.Op, ErrorStage: "protocol"}
	}
}

func findPlumesign() string {
	path, err := exec.LookPath("plumesign")
	if err != nil {
		return ""
	}
	return path
}

// opLogin starts `plumesign account login` for the account. The account
// email and password come from env (SIDEY_APPLE_ID / SIDEY_APPLE_PASSWORD)
// or the request. A 2FA code, when requested by Apple, is supplied by the
// agent via the login-code op.
func opLogin(req request) response {
	account := req.str("account")
	if account == "" {
		account = os.Getenv("SIDEY_APPLE_ID")
	}
	password := req.str("password")
	if password == "" {
		password = os.Getenv("SIDEY_APPLE_PASSWORD")
	}
	if account == "" || password == "" {
		return response{OK: false, Error: "account and password required (request or SIDEY_APPLE_ID/SIDEY_APPLE_PASSWORD)", ErrorStage: "account"}
	}
	if currentSession() != nil {
		return response{OK: false, Error: "another interactive session is running", ErrorStage: "protocol"}
	}
	s := newSession()
	go s.run("plumesign", "account", "login", "-u", account, "-p", password)
	return response{OK: true, Result: map[string]any{"status": "started"}}
}

// opPair starts `plumesign pair` against a device identified by ip:port
// (the agent has already discovered it, e.g. over the tailnet) or by the
// device id in the forked manager.
func opPair(req request) response {
	if currentSession() != nil {
		return response{OK: false, Error: "another interactive session is running", ErrorStage: "protocol"}
	}
	ip := req.str("ip")
	port := req.int("port")
	if ip == "" || port == 0 {
		return response{OK: false, Error: "ip and port required", ErrorStage: "protocol"}
	}
	s := newSession()
	go s.run("plumesign", "pair", "--ip", ip, "--port", fmt.Sprintf("%d", port))
	return response{OK: true, Result: map[string]any{"status": "started", "ip": ip, "port": port}}
}

// opCode relays the code (TV pairing code or 2FA code) into the running
// session of the given kind.
func opCode(req request, kind string) response {
	s := currentSession()
	if s == nil {
		return response{OK: false, Error: "no " + kind + " session running", ErrorStage: "protocol"}
	}
	code := req.str("code")
	if code == "" {
		return response{OK: false, Error: "code required", ErrorStage: "protocol"}
	}
	if err := s.writeCode(code); err != nil {
		return response{OK: false, Error: err.Error(), ErrorStage: "protocol"}
	}
	return response{OK: true, Result: map[string]any{"status": "code_sent", "kind": kind}}
}

// opPoll returns the terminal state of an interactive session once it
// finishes; while running it reports "running" with a partial transcript.
func opPoll(req request, kind string) response {
	s := currentSession()
	if s == nil {
		return response{OK: false, Error: "no " + kind + " session running", ErrorStage: "protocol"}
	}
	timeout := req.duration("timeout", 300*time.Second)
	select {
	case <-s.done:
		sessMu.Lock()
		sess = nil
		sessMu.Unlock()
		return s.result
	case <-time.After(timeout):
		s.mu.Lock()
		partial := s.output.String()
		s.mu.Unlock()
		return response{OK: true, Result: map[string]any{"status": "running", "output": partial}}
	}
}

// opDeploy signs and installs (or refreshes, when refresh=true) the IPA on
// the target device, mirroring the fork's task.tryInstallApp path but
// synchronously. The account must already be logged in (plumesign account
// storage) or provided via env; credentials are never embedded in requests.
// ipaAppInfo reads the bundle identifier and version from the first
// .app/Info.plist in the IPA archive.
func ipaAppInfo(ipaPath string) (bundleID string, version string, err error) {
	zr, err := zip.OpenReader(ipaPath)
	if err != nil {
		return "", "", err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "Info.plist") {
			continue
		}
		if !strings.Contains(f.Name, ".app/") {
			continue
		}
		r, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(r)
		r.Close()
		var info map[string]any
		if _, err := plist.Unmarshal(data, &info); err != nil {
			continue
		}
		bundleID, _ = info["CFBundleIdentifier"].(string)
		version, _ = info["CFBundleShortVersionString"].(string)
		break
	}
	return bundleID, version, nil
}

func opDeploy(req request) response {
	udid := req.str("udid")
	ipaPath := req.str("ipa_path")
	account := req.str("account")
	refresh := req.bool("refresh")
	customName := req.str("custom_name")
	if udid == "" || ipaPath == "" {
		return response{OK: false, Error: "udid and ipa_path required", ErrorStage: "protocol"}
	}
	if account == "" {
		account = os.Getenv("SIDEY_APPLE_ID")
	}
	if account == "" {
		return response{OK: false, Error: "account required (request or SIDEY_APPLE_ID)", ErrorStage: "account"}
	}

	// When a scripts dir is configured, delegate the install to the proven
	// pmd3 flow (scripts/tvos-install.sh) instead of the fork's plumesign
	// sign-rsd path, which fails pair-verify for pmd3 pair records on tvOS
	// 27 (Phase G status 2026-08-09).
	if scriptsDir != "" {
		return delegatedDeploy(req, udid, ipaPath, account, refresh)
	}

	// Ensure the device is known to the forked manager. With an explicit
	// ip/port the agent can deploy without mDNS discovery (tailnet path).
	dev := model.Device{
		ID:   utils.Md5(udid),
		UDID: udid,
		Name: req.str("name"),
	}
	if dev.Name == "" {
		dev.Name = "Apple TV"
	}
	dev.IP = req.str("ip")
	dev.Port = uint16(req.int("port"))
	dev.Connection = model.DeviceConnectionRemote
	dev.Status = model.Paired
	manager.RegisterDevice(dev)

	installMgr := manager.NewInstallManager()
	defer installMgr.Close()
	err := installMgr.TryStart(context.Background(), manager.InstallOptions{
		UDID:             udid,
		Account:          account,
		IP:               dev.IP,
		Port:             dev.Port,
		IpaPath:          ipaPath,
		CustomName:       customName,
		RemoveExtensions: req.bool("remove_extensions"),
		RefreshMode:      refresh,
	})
	if err != nil {
		stage := "installation"
		if installMgr.IsAccountInvalid() {
			stage = "signing"
		}
		return response{OK: false, Error: installMgr.ErrorLog() + " " + err.Error(), ErrorStage: stage, PlumesignStderr: installMgr.OutputLog()}
	}
	if !installMgr.IsSuccess() {
		return response{OK: false, Error: "install failed with unknown error: " + installMgr.OutputLog(), ErrorStage: "installation", PlumesignStderr: installMgr.OutputLog()}
	}
	profile := installMgr.ProvisioningProfile

	// Bundle id + version come from the IPA itself, not the request.
	bundleID := req.str("bundle_identifier")
	version := req.str("version")
	if pulledBundle, pulledVersion, pullErr := ipaAppInfo(ipaPath); pullErr == nil {
		if bundleID == "" {
			bundleID = pulledBundle
		}
		if version == "" {
			version = pulledVersion
		}
	}

	// Record the installed app so inventory/verify can report centrally.
	appRec := model.InstalledApp{
		IpaName:          req.str("ipa_name"),
		IpaPath:          ipaPath,
		Device:           dev.Name,
		DeviceClass:      string(model.DeviceClassAppleTV),
		UDID:             udid,
		Account:          account,
		BundleIdentifier: bundleID,
		Version:          version,
		CustomName:       customName,
		Enabled:          true,
	}
	now := time.Now()
	expiry := now.AddDate(0, 0, 7)
	if profile != nil {
		expiry = profile.ExpirationDate.Local()
	}
	appRec.InstalledDate = &now
	appRec.RefreshedDate = &now
	appRec.ExpirationDate = &expiry
	appRec.RefreshedResult = true
	saved, err := service.SaveApp(appRec)
	if err != nil {
		return response{OK: false, Error: "install succeeded but record failed: " + err.Error(), ErrorStage: "installation"}
	}

	return response{OK: true, Result: map[string]any{
		"udid": udid, "bundle_identifier": appRec.BundleIdentifier,
		"version": appRec.Version, "expiration_date": expiry.Format(time.RFC3339),
		"record_id": saved.ID, "output": installMgr.OutputLog(),
	}}
}

// delegatedDeploy runs scripts/tvos-install.sh (proven pmd3 flow) and
// records the outcome centrally. Returns a response with the same shape as
// opDeploy. The script resolves the signed IPA + provisioning profile paths
// and echoes "SIDEY_*=" lines for the helper to record.
func delegatedDeploy(req request, udid, ipaPath, account string, refresh bool) response {
	script := filepath.Join(scriptsDir, "tvos-install.sh")
	if _, err := os.Stat(script); err != nil {
		return response{OK: false, Error: "SIDEY_TVOS_SCRIPTS_DIR set but tvos-install.sh missing: " + script, ErrorStage: "installation"}
	}

	// The pair record identifier for the tunnel (differs from the device
	// UDID on the Apple TV); when unset the script falls back to the UDID.
	identifier := req.str("identifier")
	if identifier == "" {
		identifier = udid
	}

	args := []string{script, ipaPath}
	if refresh {
		args = append(args, "--refresh")
	}
	cmd := exec.Command("bash", args...)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(),
		"DEVICE_UDID="+udid,
		"DEVICE_IDENTIFIER="+identifier,
		"DEVICE_IP="+req.str("ip"),
		"DEVICE_PORT="+fmt.Sprintf("%d", req.int("port")),
		"SIDEY_APPLE_ID="+account,
		"SIDEY_TVOS_DATA_DIR="+dataDir,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := out.String()

	if err != nil {
		stage := "installation"
		if strings.Contains(output, "register device") || strings.Contains(output, "not found") {
			stage = "signing"
		}
		return response{OK: false, Error: output + " " + err.Error(), ErrorStage: stage, PlumesignStderr: output}
	}

	bundleID := req.str("bundle_identifier")
	version := req.str("version")
	signedIPA := ""
	provision := ""
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "SIDEY_BUNDLE_ID="):
			bundleID = strings.TrimPrefix(line, "SIDEY_BUNDLE_ID=")
		case strings.HasPrefix(line, "SIDEY_VERSION="):
			version = strings.TrimPrefix(line, "SIDEY_VERSION=")
		case strings.HasPrefix(line, "SIDEY_SIGNED_IPA="):
			signedIPA = strings.TrimPrefix(line, "SIDEY_SIGNED_IPA=")
		case strings.HasPrefix(line, "SIDEY_PROVISION="):
			provision = strings.TrimPrefix(line, "SIDEY_PROVISION=")
		}
	}
	if bundleID == "" {
		bundleID, version, _ = ipaAppInfo(ipaPath)
	}

	appRec := model.InstalledApp{
		IpaName:          req.str("ipa_name"),
		IpaPath:          signedIPA,
		Device:           req.str("name"),
		DeviceClass:      string(model.DeviceClassAppleTV),
		UDID:             udid,
		Account:          account,
		BundleIdentifier: bundleID,
		Version:          version,
		CustomName:       req.str("custom_name"),
		Enabled:          true,
	}
	if appRec.Device == "" {
		appRec.Device = "Apple TV"
	}
	now := time.Now()
	expiry := now.AddDate(0, 0, 7)
	if profile, perr := model.ParseMobileProvisioningProfileFile(provision); perr == nil {
		expiry = profile.ExpirationDate.Local()
	}
	appRec.InstalledDate = &now
	appRec.RefreshedDate = &now
	appRec.ExpirationDate = &expiry
	appRec.RefreshedResult = true
	saved, err := service.SaveApp(appRec)
	if err != nil {
		return response{OK: false, Error: "install succeeded but record failed: " + err.Error(), ErrorStage: "installation"}
	}

	return response{OK: true, Result: map[string]any{
		"udid": udid, "bundle_identifier": appRec.BundleIdentifier,
		"version": appRec.Version, "expiration_date": expiry.Format(time.RFC3339),
		"record_id": saved.ID, "output": output,
	}}
}

// delegatedUninstall removes an app on the Apple TV itself over the RSD
// tunnel via scripts/tvos-uninstall.sh (mirror of the proven deploy path).
// The central install record is removed by the caller regardless.
func delegatedUninstall(req request, udid, bundleID string) response {
	script := filepath.Join(scriptsDir, "tvos-uninstall.sh")
	if _, err := os.Stat(script); err != nil {
		return response{OK: false, Error: "SIDEY_TVOS_SCRIPTS_DIR set but tvos-uninstall.sh missing: " + script, ErrorStage: "installation"}
	}

	identifier := req.str("identifier")
	if identifier == "" {
		identifier = udid
	}

	cmd := exec.Command("bash", script, bundleID)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(),
		"DEVICE_UDID="+udid,
		"DEVICE_IDENTIFIER="+identifier,
		"DEVICE_IP="+req.str("ip"),
		"DEVICE_PORT="+fmt.Sprintf("%d", req.int("port")),
		"SIDEY_TVOS_DATA_DIR="+dataDir,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := out.String()

	if err != nil {
		return response{OK: false, Error: output + " " + err.Error(), ErrorStage: "installation", PlumesignStderr: output}
	}
	if strings.Contains(output, "STILL PRESENT") {
		return response{OK: false, Error: output, ErrorStage: "installation", PlumesignStderr: output}
	}
	return response{OK: true, Result: map[string]any{"bundle_identifier": bundleID, "output": output}}
}

// opVerify re-checks post-install state: the recorded install must exist.
func opVerify(req request) response {
	udid := req.str("udid")
	bundleID := req.str("bundle_identifier")
	if udid == "" || bundleID == "" {
		return response{OK: false, Error: "udid and bundle_identifier required", ErrorStage: "protocol"}
	}
	apps, err := service.GetAppList()
	if err != nil {
		return response{OK: false, Error: err.Error(), ErrorStage: "installation"}
	}
	var found *model.InstalledApp
	for i := range apps {
		if apps[i].UDID == udid && apps[i].BundleIdentifier == bundleID {
			found = &apps[i]
			break
		}
	}
	if found == nil {
		return response{OK: false, Error: "no install record for " + bundleID, ErrorStage: "installation"}
	}
	if found.ExpirationDate == nil || found.ExpirationDate.Before(time.Now()) {
		return response{OK: false, Error: "provisioning expired", ErrorStage: "signing"}
	}

	// When the device endpoint is supplied, verify does NOT pass on the
	// worker's own record alone: it demands the device actually respond over
	// the RSD tunnel (AFC probe). A recorded install with an unreachable
	// device is a failed verify, not a success.
	if ip := req.str("ip"); ip != "" && req.int("port") != 0 {
		cmd := exec.Command("plumesign", "check", "afc", "--ip", ip, "--port", fmt.Sprintf("%d", req.int("port")), "--udid", udid)
		cmd.Dir = dataDir
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "SUCCESS") {
			return response{OK: false, Error: "device unreachable: AFC probe failed", ErrorStage: "installation"}
		}
	}

	// Installed-version cross-check: when the caller knows the version it
	// deployed, the recorded install must match it.
	if want := req.str("version"); want != "" && found.Version != "" && found.Version != want {
		return response{OK: false, Error: "installed version mismatch: recorded " +
			found.Version + " != expected " + want, ErrorStage: "installation"}
	}

	return response{OK: true, Result: map[string]any{
		"udid": udid, "bundle_identifier": bundleID,
		"version": found.Version, "expiration_date": found.ExpirationDate.Format(time.RFC3339),
		"installation_verified": true,
	}}
}

// opInventory lists the installed apps recorded in the worker DB.
func opInventory(req request) response {
	apps, err := service.GetAppList()
	if err != nil {
		return response{OK: false, Error: err.Error(), ErrorStage: "installation"}
	}
	type item struct {
		UDID           string `json:"udid"`
		BundleID       string `json:"bundle_identifier"`
		Version        string `json:"version"`
		Device         string `json:"device"`
		ExpirationDate string `json:"expiration_date"`
		InstalledAt    string `json:"installed_at"`
	}
	items := make([]item, 0, len(apps))
	for _, a := range apps {
		exp := ""
		if a.ExpirationDate != nil {
			exp = a.ExpirationDate.Format(time.RFC3339)
		}
		inst := ""
		if a.InstalledDate != nil {
			inst = a.InstalledDate.Format(time.RFC3339)
		}
		items = append(items, item{UDID: a.UDID, BundleID: a.BundleIdentifier, Version: a.Version, Device: a.Device, ExpirationDate: exp, InstalledAt: inst})
	}
	return response{OK: true, Result: map[string]any{"apps": items, "count": len(items)}}
}

// opUninstall removes the recorded app entry (the fork's plumesign path has
// no uninstall verb; the record removal lets the control plane drop the
// inventory row).
//
// When a scripts dir is configured this also removes the app on the device
// itself over the RSD tunnel (scripts/tvos-uninstall.sh), mirroring the
// proven deploy path; the central install record is removed either way.
func opUninstall(req request) response {
	udid := req.str("udid")
	bundleID := req.str("bundle_identifier")
	if udid == "" || bundleID == "" {
		return response{OK: false, Error: "udid and bundle_identifier required", ErrorStage: "protocol"}
	}

	if scriptsDir != "" {
		if resp := delegatedUninstall(req, udid, bundleID); !resp.OK {
			return resp
		}
	}

	apps, err := service.GetAppList()
	if err != nil {
		return response{OK: false, Error: err.Error(), ErrorStage: "installation"}
	}
	removed := 0
	for _, a := range apps {
		if a.UDID == udid && a.BundleIdentifier == bundleID {
			if ok, err := service.DeleteApp(a.ID); err == nil && ok {
				removed++
			}
		}
	}
	if removed == 0 {
		return response{OK: false, Error: "no install record for " + bundleID, ErrorStage: "installation"}
	}
	return response{OK: true, Result: map[string]any{"removed": removed}}
}

// opCollect dumps diagnostics for the proof harness: helper version, data
// dir layout, plumesign presence and the recorded inventory.
func opCollect(req request) response {
	entries := map[string]any{"version": opVersion}
	dirs, err := os.ReadDir(dataDir)
	if err == nil {
		names := []string{}
		for _, d := range dirs {
			names = append(names, d.Name())
		}
		entries["data_dir"] = names
	}
	entries["plumesign"] = findPlumesign()
	inv := opInventory(req)
	entries["inventory"] = inv.Result
	return response{OK: true, Result: entries}
}

// opDeviceInfo returns the control plane device record fields the helper
// knows about (registration data, product info when lockdown connected).
func opDeviceInfo(req request) response {
	udid := req.str("udid")
	if udid == "" {
		return response{OK: false, Error: "udid required", ErrorStage: "protocol"}
	}
	dev, found := manager.GetDeviceByUDID(udid)
	if !found {
		return response{OK: false, Error: "device not registered: " + udid, ErrorStage: "pairing"}
	}
	return response{OK: true, Result: map[string]any{
		"udid": dev.UDID, "name": dev.Name, "ip": dev.IP, "port": dev.Port,
		"product_version": dev.ProductVersion, "product_type": dev.ProductType,
		"device_class": dev.DeviceClass, "status": dev.Status,
	}}
}

// opIpa inspects a local IPA's embedded provisioning profile and Info.plist
// without installing anything (viewing: bundle id, version, profile expiry,
// team, allowed devices).
func opIpa(req request) response {
	ipaPath := req.str("ipa_path")
	if ipaPath == "" {
		return response{OK: false, Error: "ipa_path required", ErrorStage: "protocol"}
	}
	zr, err := zip.OpenReader(ipaPath)
	if err != nil {
		return response{OK: false, Error: "open ipa: " + err.Error(), ErrorStage: "protocol"}
	}
	defer zr.Close()

	var profileBytes []byte
	var infoPlistBytes []byte
	var appDir string
	entries := []string{}
	for _, f := range zr.File {
		entries = append(entries, f.Name)
		if appDir == "" && strings.HasPrefix(f.Name, "Payload/") && strings.HasSuffix(f.Name, ".app/") {
			appDir = strings.TrimSuffix(f.Name, "/")
		}
	}
	if appDir == "" {
		for _, f := range zr.File {
			idx := strings.Index(f.Name, ".app/")
			if idx > 0 {
				appDir = f.Name[:idx+4]
				break
			}
		}
	}
	if appDir == "" {
		return response{OK: false, Error: "no .app found in ipa", ErrorStage: "protocol"}
	}
	for _, f := range zr.File {
		switch {
		case strings.HasSuffix(f.Name, "embedded.mobileprovision"):
			r, err := f.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(r)
			r.Close()
			profileBytes = append(profileBytes, data...)
		case strings.HasSuffix(f.Name, "Info.plist"):
			r, err := f.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(r)
			r.Close()
			if infoPlistBytes == nil {
				infoPlistBytes = data
			}
		}
	}

	result := map[string]any{
		"app_dir": appDir, "entries": entries,
		"bundle_identifier": "", "version": "",
		"app_id": "", "team_id": "", "expiration": "", "allowed_devices": "",
		"embedded_profile": profileBytes != nil,
	}

	// Info.plist: bundle id + version.
	if infoPlistBytes != nil {
		var info map[string]any
		if _, err := plist.Unmarshal(infoPlistBytes, &info); err == nil {
			if v, ok := info["CFBundleIdentifier"].(string); ok {
				result["bundle_identifier"] = v
			}
			if v, ok := info["CFBundleShortVersionString"].(string); ok {
				result["version"] = v
			}
		}
	}

	// embedded.mobileprovision: profile details.
	if profileBytes != nil {
		var prof map[string]any
		idx := bytes.Index(profileBytes, []byte("<?xml"))
		if idx < 0 {
			idx = 0
		}
		if _, err := plist.Unmarshal(profileBytes[idx:], &prof); err == nil {
			if v, ok := prof["ExpirationDate"]; ok {
				result["expiration"] = v
			}
			if v, ok := prof["TeamIdentifier"]; ok {
				if arr, ok := v.([]any); ok && len(arr) > 0 {
					result["team_id"] = arr[0]
				}
			}
			if v, ok := prof["ProvisionedDevices"]; ok {
				result["allowed_devices"] = v
			}
			if ent, ok := prof["Entitlements"].(map[string]any); ok {
				if v, ok := ent["application-identifier"]; ok {
					result["app_id"] = v
				}
			}
		}
	}
	return response{OK: true, Result: result}
}

// request helpers decode typed fields from the request data map.
func (r request) str(key string) string {
	if v, ok := r.Data[key].(string); ok {
		return v
	}
	return ""
}

func (r request) int(key string) int {
	switch v := r.Data[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func (r request) bool(key string) bool {
	v, _ := r.Data[key].(bool)
	return v
}

func (r request) duration(key string, def time.Duration) time.Duration {
	switch v := r.Data[key].(type) {
	case float64:
		return time.Duration(v) * time.Second
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func parseRequest(line []byte) (request, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return request{}, fmt.Errorf("invalid request json: %w", err)
	}
	req := request{ID: asString(raw["id"]), Op: asString(raw["op"])}
	if req.Op == "" {
		return request{}, fmt.Errorf("op is required")
	}
	_ = json.Unmarshal(raw["data"], &req.Data)
	return req, nil
}

// asString decodes a quoted JSON string raw message.
func asString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func write(w io.Writer, resp response) {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(response{OK: false, Error: "response marshal failed", ErrorStage: "protocol"})
	}
	w.Write(append(data, '\n'))
}
