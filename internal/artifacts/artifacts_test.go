package artifacts

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testInfoPlist = `<?xml version="1.0"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>com.example.Test</string>
  <key>CFBundleShortVersionString</key><string>1.2.3</string>
  <key>CFBundleVersion</key><string>45</string>
  <key>MinimumOSVersion</key><string>15.0</string>
  <key>CFBundleSupportedPlatforms</key><array><string>iPhoneOS</string></array>
</dict></plist>`

// buildIPA creates a zip with the given entries (valid IPA layouts include
// Payload/Test.app/Info.plist).
func buildIPA(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
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

func writeIPA(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspect(t *testing.T) {
	path := writeIPA(t, "test.ipa", buildIPA(t, map[string]string{
		"Payload/Test.app/Info.plist":                  testInfoPlist,
		"Payload/Test.app/Test":                        "binary",
		"Payload/Test.app/PlugIns/Widget.appex/Widget": "extension",
	}))

	meta, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.BundleIdentifier != "com.example.Test" {
		t.Errorf("bundle id = %q", meta.BundleIdentifier)
	}
	if meta.Version != "1.2.3" || meta.BuildNumber != "45" {
		t.Errorf("version = %q/%q", meta.Version, meta.BuildNumber)
	}
	if meta.MinOSVersion != "15.0" {
		t.Errorf("min os = %q", meta.MinOSVersion)
	}
	if meta.Platform != "iPhoneOS" {
		t.Errorf("platform = %q", meta.Platform)
	}
	if len(meta.Extensions) != 1 || meta.Extensions[0] != "Widget.appex" {
		t.Errorf("extensions = %v", meta.Extensions)
	}
}

func TestInspectRejectsMalformed(t *testing.T) {
	// Not a zip.
	notZip := writeIPA(t, "bad.ipa", []byte("this is not a zip"))
	if _, err := Inspect(notZip); !errors.Is(err, ErrMalformed) {
		t.Errorf("non-zip: err = %v", err)
	}

	// Zip without an Info.plist.
	noPlist := writeIPA(t, "noplist.ipa", buildIPA(t, map[string]string{
		"Payload/Test.app/Test": "binary",
	}))
	if _, err := Inspect(noPlist); !errors.Is(err, ErrMalformed) {
		t.Errorf("missing Info.plist: err = %v", err)
	}

	// Unparsable plist.
	badPlist := writeIPA(t, "badplist.ipa", buildIPA(t, map[string]string{
		"Payload/Test.app/Info.plist": "<plist><dict>",
	}))
	if _, err := Inspect(badPlist); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad plist: err = %v", err)
	}
}

func TestStoreContentAddressed(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	content := []byte("same bytes twice")

	sum1, err := store.Save(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := store.Save(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if sum1 != sum2 {
		t.Errorf("hashes differ: %s vs %s", sum1, sum2)
	}
	if !store.Exists(sum1) {
		t.Error("stored file missing")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Two uploads of the same bytes must converge on a single blob (plus no
	// leftover temp files).
	if len(entries) != 1 {
		t.Errorf("expected 1 stored file, got %d", len(entries))
	}
	if entries[0].Name() != sum1+".ipa" {
		t.Errorf("stored name = %q", entries[0].Name())
	}
}
