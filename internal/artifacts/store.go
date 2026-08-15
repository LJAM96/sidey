// Package artifacts provides immutable, content-addressed storage for
// uploaded IPAs (Phase E). Files are stored as <dir>/<sha256>.ipa; uploading
// the same bytes twice converges on the same file, so the database can
// dedupe on sha256.
//
// Concurrency with retention: blob deletion in retention/prune.go and blob
// publication here are serialised per content hash with a PostgreSQL
// advisory lock taken by the caller (pg_advisory_xact_lock(hashtext(sha))).
// Callers stream the upload to a temp file (SaveToTemp), then begin a
// transaction, take the advisory lock, publish the temp file into place and
// insert the database row, and commit. A concurrent retention pass holding
// the same lock therefore cannot delete a blob between publication and the
// row commit (and vice versa).
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
// saved but never recorded. Removal should be reference controlled: never
// delete a hash merely because the current operation computed it.
func (s *Store) Remove(sha256 string) {
	os.Remove(s.Path(sha256))
}

// SaveToTemp streams r to a temp file in the store directory and returns the
// content hash plus the temp path. Nothing is visible under the final
// content-addressed name until Publish is called, so a caller can validate
// the bytes and coordinate with retention before publishing.
func (s *Store) SaveToTemp(r io.Reader) (sum string, tmpPath string, err error) {
	if err := s.EnsureDir(); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(r, hash)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("reading upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), tmp.Name(), nil
}

// DiscardTemp removes a temp file created by SaveToTemp.
func (s *Store) DiscardTemp(tmpPath string) {
	if tmpPath == "" {
		return
	}
	os.Remove(tmpPath)
}

// Publish moves a temp file into place named by its content hash. If the
// final file already exists the temp is discarded and created reports false
// (the bytes were already in the content store). Callers hold the advisory
// lock for the hash while publishing and inserting the database row.
func (s *Store) Publish(sum, tmpPath string) (created bool, err error) {
	final := s.Path(sum)
	if _, err := os.Stat(final); err == nil {
		os.Remove(tmpPath)
		return false, nil
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return false, err
	}
	return true, nil
}

// Save streams r to disk, computing the sha256 while it goes. On success the
// file is named by its content hash; a concurrent or repeated upload of the
// same bytes lands on the same file. The caller must still check the
// database for an existing artifact row (the file is content-addressed, the
// row is not). Convenience wrapper: it does not take the advisory lock, so
// prefer SaveToTemp + Publish inside a lock-taking transaction in code that
// races with retention.
func (s *Store) Save(r io.Reader) (sum string, err error) {
	sum, tmp, err := s.SaveToTemp(r)
	if err != nil {
		return "", err
	}
	if _, err := s.Publish(sum, tmp); err != nil {
		return "", err
	}
	return sum, nil
}
