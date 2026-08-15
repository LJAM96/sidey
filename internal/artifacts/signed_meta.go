package artifacts

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"time"

	plist "howett.net/plist"
)

// SignedMetadata holds the verified metadata extracted directly from a signed
// IPA archive, its Info.plist and its embedded.mobileprovision.
type SignedMetadata struct {
	BundleIdentifier     string
	Version              string
	BuildNumber          string
	Platform             string
	DevicePlatform       string
	ProfileUUID          string
	ProfileName          string
	TeamID               string
	TeamName             string
	ProfileExpiry        time.Time
	AppIdentifier        string
	ProvisionedDevices   []string
	ProvisionsAllDevices bool
	Entitlements         map[string]any
}

var (
	// ErrMalformedSignedIPA is returned when a signed IPA is not a valid zip,
	// lacks Payload/*.app/Info.plist, lacks embedded.mobileprovision, or contains
	// unparseable plists.
	ErrMalformedSignedIPA = errors.New("malformed signed IPA")

	provisionPattern = regexp.MustCompile(`^Payload/[^/]+\.app/embedded\.mobileprovision$`)
)

// InspectSigned inspects a signed IPA on disk, parsing both its Info.plist and
// embedded.mobileprovision profile without relying on client-supplied claims.
func InspectSigned(ipaPath string) (*SignedMetadata, error) {
	zr, err := zip.OpenReader(ipaPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedSignedIPA, err)
	}
	defer zr.Close()

	meta := &SignedMetadata{}
	plistFound := false
	provisionFound := false
	var infoPlistBytes []byte
	var provisionBytes []byte

	for _, f := range zr.File {
		if infoPlistPattern.MatchString(f.Name) {
			if f.UncompressedSize64 > maxMetadataEntrySize {
				return nil, fmt.Errorf("%w: %s is oversized", ErrMalformedSignedIPA, f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformedSignedIPA, f.Name, err)
			}
			raw, err := io.ReadAll(io.LimitReader(rc, maxMetadataEntrySize+1))
			rc.Close()
			if err != nil || len(raw) > maxMetadataEntrySize {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformedSignedIPA, f.Name, err)
			}
			infoPlistBytes = raw
			plistFound = true
		} else if provisionPattern.MatchString(f.Name) {
			if f.UncompressedSize64 > maxMetadataEntrySize {
				return nil, fmt.Errorf("%w: %s is oversized", ErrMalformedSignedIPA, f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformedSignedIPA, f.Name, err)
			}
			raw, err := io.ReadAll(io.LimitReader(rc, maxMetadataEntrySize+1))
			rc.Close()
			if err != nil || len(raw) > maxMetadataEntrySize {
				return nil, fmt.Errorf("%w: reading %s: %v", ErrMalformedSignedIPA, f.Name, err)
			}
			provisionBytes = raw
			provisionFound = true
		}
	}

	if !plistFound {
		return nil, fmt.Errorf("%w: no Payload/*.app/Info.plist", ErrMalformedSignedIPA)
	}
	if !provisionFound {
		return nil, fmt.Errorf("%w: no Payload/*.app/embedded.mobileprovision", ErrMalformedSignedIPA)
	}

	// 1. Parse Info.plist
	var info map[string]any
	if _, err := plist.Unmarshal(infoPlistBytes, &info); err != nil {
		return nil, fmt.Errorf("%w: unmarshaling Info.plist: %v", ErrMalformedSignedIPA, err)
	}
	meta.BundleIdentifier = plistString(info, "CFBundleIdentifier")
	meta.Version = plistString(info, "CFBundleShortVersionString")
	meta.BuildNumber = plistString(info, "CFBundleVersion")
	if platforms, ok := info["CFBundleSupportedPlatforms"].([]any); ok && len(platforms) > 0 {
		meta.Platform = fmt.Sprintf("%v", platforms[0])
	}
	meta.DevicePlatform = sdkPlatformToDevice[meta.Platform]
	if meta.BundleIdentifier == "" {
		return nil, fmt.Errorf("%w: Info.plist lacks CFBundleIdentifier", ErrMalformedSignedIPA)
	}

	// 2. Parse embedded.mobileprovision (PKCS#7 signed XML plist)
	profilePlist, err := ExtractProfilePlist(provisionBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parsing embedded.mobileprovision: %v", ErrMalformedSignedIPA, err)
	}

	var profile map[string]any
	if _, err := plist.Unmarshal(profilePlist, &profile); err != nil {
		return nil, fmt.Errorf("%w: unmarshaling profile plist: %v", ErrMalformedSignedIPA, err)
	}

	meta.ProfileUUID = plistString(profile, "UUID")
	meta.ProfileName = plistString(profile, "Name")
	meta.TeamName = plistString(profile, "TeamName")

	if expiry, ok := profile["ExpirationDate"].(time.Time); ok {
		meta.ProfileExpiry = expiry
	}
	if teams, ok := profile["TeamIdentifier"].([]any); ok && len(teams) > 0 {
		meta.TeamID = fmt.Sprintf("%v", teams[0])
	}
	if allDevs, ok := profile["ProvisionsAllDevices"].(bool); ok {
		meta.ProvisionsAllDevices = allDevs
	}
	if devs, ok := profile["ProvisionedDevices"].([]any); ok {
		for _, d := range devs {
			if s := fmt.Sprintf("%v", d); s != "" {
				meta.ProvisionedDevices = append(meta.ProvisionedDevices, s)
			}
		}
	}
	if ent, ok := profile["Entitlements"].(map[string]any); ok {
		meta.Entitlements = ent
		meta.AppIdentifier = plistString(ent, "application-identifier")
	}

	return meta, nil
}

// ExtractProfilePlist extracts the raw XML plist embedded within a PKCS#7
// encoded .mobileprovision byte slice.
func ExtractProfilePlist(data []byte) ([]byte, error) {
	start := bytes.Index(data, []byte("<?xml"))
	if start == -1 {
		return nil, errors.New("missing XML plist start marker")
	}
	endMarker := []byte("</plist>")
	endRel := bytes.Index(data[start:], endMarker)
	if endRel == -1 {
		return nil, errors.New("missing XML plist end marker")
	}
	return data[start : start+endRel+len(endMarker)], nil
}
