//go:build windows

package blob

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type securedRoot struct{ path string }

func openSecuredRoot(path string) (*securedRoot, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrUnsafeStorage
	}
	return &securedRoot{path: path}, nil
}
func securedRootIdentity(first, second *securedRoot) (same bool, sameFilesystem bool, err error) {
	one, err := os.Stat(first.path)
	if err != nil {
		return false, false, err
	}
	two, err := os.Stat(second.path)
	if err != nil {
		return false, false, err
	}
	return os.SameFile(one, two), strings.EqualFold(filepath.VolumeName(first.path), filepath.VolumeName(second.path)), nil
}
func (r *securedRoot) close() error { return nil }

// The production server runs on Linux. Windows support keeps the package
// buildable for development tools; directory-entry durability is not claimed.
func syncRootCreation(string) error { return nil }

func ensureSameFilesystem(first, second string) error {
	if !strings.EqualFold(filepath.VolumeName(first), filepath.VolumeName(second)) {
		return fmt.Errorf("%w: roots must share a filesystem", ErrUnsafeStorage)
	}
	return nil
}
func ensureBlobBase(root *securedRoot) error {
	return ensureRealDirectory(filepath.Join(root.path, "sha256"))
}
func ensureRealDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeStorage
	}
	return nil
}
func createStage(root *securedRoot) (string, *os.File, error) {
	for range 32 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := ".upload-" + hex.EncodeToString(token[:])
		file, err := os.OpenFile(filepath.Join(root.path, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, file, nil
	}
	return "", nil, errors.New("cannot allocate staging file")
}
func blobPath(root *securedRoot, hash [sha256.Size]byte, create bool) (string, error) {
	encoded := hex.EncodeToString(hash[:])
	current := root.path
	for _, name := range []string{"sha256", encoded[:2], encoded[2:4]} {
		current = filepath.Join(current, name)
		if create {
			if err := ensureRealDirectory(current); err != nil {
				return "", err
			}
		} else {
			info, err := os.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", ErrUnsafeStorage
			}
		}
	}
	return filepath.Join(current, encoded), nil
}
func publish(stagingRoot *securedRoot, stage string, blobRoot *securedRoot, hash [sha256.Size]byte, size int64) error {
	target, err := blobPath(blobRoot, hash, true)
	if err != nil {
		return err
	}
	err = os.Link(filepath.Join(stagingRoot.path, stage), target)
	if os.IsExist(err) {
		content, readErr := readBlob(blobRoot, hash, MaxBlobBytes)
		actual := sha256.Sum256(content)
		if readErr != nil || int64(len(content)) != size || !bytes.Equal(actual[:], hash[:]) {
			return ErrIntegrity
		}
	} else if err != nil {
		return fmt.Errorf("publish blob: %w", err)
	}
	return removeStage(stagingRoot, stage)
}
func readBlob(root *securedRoot, hash [sha256.Size]byte, max int64) ([]byte, error) {
	path, err := blobPath(root, hash, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > max {
		return nil, ErrIntegrity
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(content)) > max {
		return nil, ErrIntegrity
	}
	return content, nil
}
func stagingEntries(root *securedRoot) ([]string, error) {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		token := strings.TrimPrefix(name, ".upload-")
		info, err := os.Lstat(filepath.Join(root.path, name))
		if err != nil || !strings.HasPrefix(name, ".upload-") || !validHex(token, 32) ||
			info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, ErrUnsafeStorage
		}
		names = append(names, name)
	}
	return names, nil
}

func removeStage(root *securedRoot, name string) error {
	return os.Remove(filepath.Join(root.path, name))
}
