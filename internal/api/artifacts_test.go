package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testIPA builds a minimal valid IPA payload for upload tests.
func testIPA(t *testing.T, bundleID string) []byte {
	t.Helper()
	return testIPAPlatform(t, bundleID, "iPhoneOS")
}

// testIPAPlatform builds an IPA declaring the given Apple SDK platform
// (e.g. iPhoneOS, AppleTVOS).
func testIPAPlatform(t *testing.T, bundleID, sdkPlatform string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	plist := `<?xml version="1.0"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>` + bundleID + `</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>MinimumOSVersion</key><string>14.0</string>
  <key>CFBundleSupportedPlatforms</key><array><string>` + sdkPlatform + `</string></array>
</dict></plist>`
	for name, content := range map[string]string{
		"Payload/Test.app/Info.plist": plist,
		"Payload/Test.app/Test":       "binary",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testSignedIPA builds a minimal valid signed IPA containing both Info.plist
// and embedded.mobileprovision for signing tests.
func testSignedIPA(t *testing.T, bundleID, teamID, profileUUID string, expiry time.Time, provisionedDevices []string) []byte {
	t.Helper()
	return testSignedIPAPlatform(t, bundleID, "iPhoneOS", teamID, profileUUID, expiry, provisionedDevices)
}

// testSignedIPAPlatform builds a signed IPA declaring the given Apple SDK platform.
func testSignedIPAPlatform(t *testing.T, bundleID, sdkPlatform, teamID, profileUUID string, expiry time.Time, provisionedDevices []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	plist := `<?xml version="1.0"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>` + bundleID + `</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>MinimumOSVersion</key><string>14.0</string>
  <key>CFBundleSupportedPlatforms</key><array><string>` + sdkPlatform + `</string></array>
</dict></plist>`

	var devList string
	for _, udid := range provisionedDevices {
		devList += "    <string>" + udid + "</string>\n"
	}

	expiryStr := expiry.UTC().Format(time.RFC3339)
	profileXml := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>AppIDName</key><string>Sidey App</string>
  <key>Name</key><string>Sidey Development Profile</string>
  <key>UUID</key><string>` + profileUUID + `</string>
  <key>TeamName</key><string>Sidey Team</string>
  <key>TeamIdentifier</key><array><string>` + teamID + `</string></array>
  <key>ExpirationDate</key><date>` + expiryStr + `</date>
  <key>ProvisionedDevices</key><array>
` + devList + `  </array>
  <key>Entitlements</key><dict>
    <key>application-identifier</key><string>` + teamID + "." + bundleID + `</string>
    <key>get-task-allow</key><true/>
  </dict>
</dict>
</plist>`

	fakePKCS7 := append([]byte("PKCS7-HEADER-DER-BYTES\x00\x01\x02"), []byte(profileXml)...)
	fakePKCS7 = append(fakePKCS7, []byte("\x00\x00-SIG-FOOTER")...)

	for name, content := range map[string][]byte{
		"Payload/Test.app/Info.plist":               []byte(plist),
		"Payload/Test.app/embedded.mobileprovision": fakePKCS7,
		"Payload/Test.app/Test":                     []byte("binary"),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadIPA(t *testing.T, data []byte) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST",
		httpServer.URL+"/api/v1/artifacts?filename=Test.ipa",
		bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return res, parsed
}

// TestArtifactUploadDedupeAndState covers the Phase E flows: upload extracts
// metadata, duplicate bytes converge on the same artifact, the quarantine
// workflow applies, and download returns the original bytes.
func TestArtifactUploadDedupeAndState(t *testing.T) {
	truncate(t)
	ipa := testIPA(t, "com.example.Test")

	res, body := uploadIPA(t, ipa)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", res.StatusCode, body)
	}
	id, _ := body["id"].(string)
	uploadedSHA, _ := body["sha256"].(string)
	if id == "" || uploadedSHA == "" {
		t.Fatal("no artifact id/sha256")
	}
	if body["bundle_identifier"] != "com.example.Test" {
		t.Errorf("bundle id = %v", body["bundle_identifier"])
	}
	if body["version"] != "1.0.0" || body["quarantine_state"] != "quarantined" {
		t.Errorf("meta = %v %v", body["version"], body["quarantine_state"])
	}

	// Same bytes again: same artifact, existing=true, same state.
	res, body = uploadIPA(t, ipa)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-upload: %d %v", res.StatusCode, body)
	}
	if body["existing"] != true || body["id"] != id {
		t.Errorf("dedupe = %v id %v", body["existing"], body["id"])
	}

	// Malformed upload is rejected and does not create a row.
	res, body = uploadIPA(t, []byte("not a zip"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed: %d %v", res.StatusCode, body)
	}

	// Quarantine workflow.
	res, body = doJSON(t, "PATCH", "/api/v1/artifacts/"+id, adminKey, map[string]any{"state": "approved"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %v", res.StatusCode, body)
	}
	res, body = doJSON(t, "PATCH", "/api/v1/artifacts/"+id, adminKey, map[string]any{"state": "bogus"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: %d %v", res.StatusCode, body)
	}
	res, _ = doJSON(t, "PATCH", "/api/v1/artifacts/"+uuid.Nil.String(), adminKey, map[string]any{"state": "approved"})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing artifact: %d", res.StatusCode)
	}

	// Download round-trips the exact bytes.
	req, err := http.NewRequest("GET", httpServer.URL+"/api/v1/artifacts/"+id+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	dl, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	got, err := io.ReadAll(dl.Body)
	if err != nil {
		t.Fatal(err)
	}
	if dl.StatusCode != http.StatusOK || !bytes.Equal(got, ipa) {
		t.Errorf("download mismatch: %d len=%d", dl.StatusCode, len(got))
	}

	// Dashboard lists the artifact with the approved state.
	res, body = doJSON(t, "GET", "/api/v1/dashboard/artifacts", adminKey, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d %v", res.StatusCode, body)
	}
	sha256 := uploadedSHA
	found := false
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		if row["sha256"] == sha256 {
			found = row["quarantine_state"] == "approved"
		}
	}
	if !found {
		t.Error("dashboard does not show the approved artifact")
	}
}
