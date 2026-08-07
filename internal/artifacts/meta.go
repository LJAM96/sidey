package artifacts

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	plist "howett.net/plist"
)

// Metadata is the subset of IPA metadata Phase E extracts for the artifact
// record. Entitlements require reading the code signature, which is deferred.
type Metadata struct {
	BundleIdentifier string
	Version          string
	BuildNumber      string
	MinOSVersion     string
	Platform         string
	Extensions       []string
}

// ErrMalformed marks archives that are not a valid IPA (not a zip, no
// Payload/*.app/Info.plist, or an unreadable plist).
var ErrMalformed = errors.New("malformed IPA")

var infoPlistPattern = regexp.MustCompile(`^Payload/[^/]+\.app/Info\.plist$`)

// Inspect reads an IPA from disk and extracts its metadata.
func Inspect(ipaPath string) (*Metadata, error) {
	zr, err := zip.OpenReader(ipaPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	defer zr.Close()

	meta := &Metadata{}
	appPrefix := ""
	plistFound := false
	for _, f := range zr.File {
		if infoPlistPattern.MatchString(f.Name) {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformed, f.Name, err)
			}
			raw, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformed, f.Name, err)
			}
			var info map[string]any
			if _, err := plist.Unmarshal(raw, &info); err != nil {
				return nil, fmt.Errorf("%w: Info.plist: %v", ErrMalformed, err)
			}
			meta.BundleIdentifier = plistString(info, "CFBundleIdentifier")
			meta.Version = plistString(info, "CFBundleShortVersionString")
			meta.BuildNumber = plistString(info, "CFBundleVersion")
			meta.MinOSVersion = plistString(info, "MinimumOSVersion")
			if platforms, ok := info["CFBundleSupportedPlatforms"].([]any); ok && len(platforms) > 0 {
				meta.Platform = fmt.Sprintf("%v", platforms[0])
			}
			plistFound = true
			appPrefix = path.Dir(f.Name)
			break
		}
	}
	if !plistFound {
		return nil, fmt.Errorf("%w: no Payload/*.app/Info.plist", ErrMalformed)
	}
	if meta.BundleIdentifier == "" {
		return nil, fmt.Errorf("%w: Info.plist lacks CFBundleIdentifier", ErrMalformed)
	}

	// App extensions live in <app>/PlugIns/*.appex. Every entry under that
	// directory (including the files inside each extension) names the
	// extension; collect unique names.
	plugIns := appPrefix + "/PlugIns/"
	seen := map[string]bool{}
	for _, f := range zr.File {
		rel := strings.TrimPrefix(f.Name, plugIns)
		if rel == f.Name {
			continue
		}
		if idx := strings.Index(rel, ".appex"); idx > 0 {
			ext := rel[:idx+len(".appex")]
			if !seen[ext] {
				seen[ext] = true
				meta.Extensions = append(meta.Extensions, ext)
			}
		}
	}
	return meta, nil
}

func plistString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
