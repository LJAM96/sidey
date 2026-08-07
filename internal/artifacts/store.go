// Package artifacts provides immutable, content-addressed storage for
// uploaded IPAs (Phase E). Files are stored as <dir>/<sha256>.ipa; uploading
// the same bytes twice converges on the same file, so the database can
// dedupe on sha256.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// EnsureDir creates the storage directory if missing (first run as the
// container user).
func (s *Store) EnsureDir() error {
	return os.MkdirAll(s.dir, 0o755)
}

// Path returns the on-disk location for a sha256.
func (s *Store) Path(sha256 string) string {
	return filepath.Join(s.dir, sha256+".ipa")
}

func (s *Store) Exists(sha256 string) bool {
	_, err := os.Stat(s.Path(sha256))
	return err == nil
}

// Remove deletes a stored file. Callers use it to clean up a file that was
// saved but never recorded (e.g. a failed signed-artifact upload).
func (s *Store) Remove(sha256 string) {
	os.Remove(s.Path(sha256))
}

// Save streams r to disk, computing the sha256 while it goes. On success the
// file is named by its content hash; a concurrent or repeated upload of the
// same bytes lands on the same file. The caller must still check the
// database for an existing artifact row (the file is content-addressed, the
// row is not).
func (s *Store) Save(r io.Reader) (sum string, err error) {
	if err := s.EnsureDir(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(r, hash)); err != nil {
		return "", fmt.Errorf("reading upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	sum = hex.EncodeToString(hash.Sum(nil))
	final := s.Path(sum)
	if _, err := os.Stat(final); err == nil {
		// Already stored: drop the duplicate copy.
		os.Remove(tmpName)
	} else {
		if err := os.Rename(tmpName, final); err != nil {
			return "", err
		}
	}
	return sum, nil
}
