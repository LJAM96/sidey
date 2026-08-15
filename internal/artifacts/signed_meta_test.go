package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectSignedValid(t *testing.T) {
	expiry := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	data, err := buildSignedIPA("com.example.signedapp", "iPhoneOS", "TEAM12345", "11111111-2222-3333-4444-555555555555", expiry, []string{"00008120-0000000000000001", "00008120-0000000000000002"})
	if err != nil {
		t.Fatalf("buildSignedIPA: %v", err)
	}

	tmp := filepath.Join(t.TempDir(), "signed.ipa")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	meta, err := InspectSigned(tmp)
	if err != nil {
		t.Fatalf("InspectSigned: %v", err)
	}

	if meta.BundleIdentifier != "com.example.signedapp" {
		t.Errorf("BundleIdentifier = %q, want %q", meta.BundleIdentifier, "com.example.signedapp")
	}
	if meta.Platform != "iPhoneOS" || meta.DevicePlatform != PlatformiOS {
		t.Errorf("Platform = %q / %q, want iPhoneOS / ios", meta.Platform, meta.DevicePlatform)
	}
	if meta.TeamID != "TEAM12345" {
		t.Errorf("TeamID = %q, want %q", meta.TeamID, "TEAM12345")
	}
	if meta.ProfileUUID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("ProfileUUID = %q, want %q", meta.ProfileUUID, "11111111-2222-3333-4444-555555555555")
	}
	if meta.ProfileExpiry.UTC().Unix() != expiry.UTC().Unix() {
		t.Errorf("ProfileExpiry = %v, want %v", meta.ProfileExpiry, expiry)
	}
	if len(meta.ProvisionedDevices) != 2 || meta.ProvisionedDevices[0] != "00008120-0000000000000001" {
		t.Errorf("ProvisionedDevices = %v", meta.ProvisionedDevices)
	}
	if meta.AppIdentifier != "TEAM12345.com.example.signedapp" {
		t.Errorf("AppIdentifier = %q", meta.AppIdentifier)
	}
}

func TestInspectSignedRejectsMissingProvision(t *testing.T) {
	// An IPA without embedded.mobileprovision must be rejected by InspectSigned
	data := testIPABytes("com.example.noprov", "iPhoneOS")
	tmp := filepath.Join(t.TempDir(), "noprov.ipa")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := InspectSigned(tmp)
	if !errors.Is(err, ErrMalformedSignedIPA) {
		t.Errorf("expected ErrMalformedSignedIPA, got %v", err)
	}
}

func testIPABytes(bundleID, sdkPlatform string) []byte {
	ipa, _ := buildSignedIPA(bundleID, sdkPlatform, "T", "U", time.Now(), nil)
	return ipa
}

// buildSignedIPA generates a test signed IPA containing both Info.plist and
// embedded.mobileprovision for testing.
func buildSignedIPA(bundleID, sdkPlatform, teamID, profileUUID string, expiry time.Time, provisionedDevices []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	infoPlist := `<?xml version="1.0"?>
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

	// Wrap profile with dummy PKCS#7 envelope prefix and suffix
	fakePKCS7 := append([]byte("PKCS7-HEADER-DER-BYTES\x00\x01\x02"), []byte(profileXml)...)
	fakePKCS7 = append(fakePKCS7, []byte("\x00\x00-SIG-FOOTER")...)

	appDir := "Payload/Test.app"
	files := map[string][]byte{
		path.Join(appDir, "Info.plist"):               []byte(infoPlist),
		path.Join(appDir, "embedded.mobileprovision"): fakePKCS7,
		path.Join(appDir, "Test"):                     []byte("executable-binary-bytes"),
	}

	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
