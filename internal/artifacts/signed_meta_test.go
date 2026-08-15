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
	data, err := BuildSignedIPA("com.example.signedapp", "iPhoneOS", "TEAM12345", "11111111-2222-3333-4444-555555555555", expiry, []string{"00008120-0000000000000001", "00008120-0000000000000002"})
	if err != nil {
		t.Fatalf("BuildSignedIPA: %v", err)
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
	ipa, _ := BuildSignedIPA(bundleID, sdkPlatform, "T", "U", time.Now(), nil)
	return ipa
}
