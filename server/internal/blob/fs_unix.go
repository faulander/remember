//go:build darwin || linux

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

	"golang.org/x/sys/unix"
)

type securedRoot struct {
	path string
	fd   int
}

func openSecuredRoot(path string) (*securedRoot, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open secured storage root: %w", err)
	}
	return &securedRoot{path: path, fd: fd}, nil
}

func securedRootIdentity(first, second *securedRoot) (same bool, sameFilesystem bool, err error) {
	var one, two unix.Stat_t
	if err := unix.Fstat(first.fd, &one); err != nil {
		return false, false, err
	}
	if err := unix.Fstat(second.fd, &two); err != nil {
		return false, false, err
	}
	return one.Dev == two.Dev && one.Ino == two.Ino, one.Dev == two.Dev, nil
}

func (r *securedRoot) close() error {
	if r == nil || r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}

func syncRootCreation(path string) error {
	current := filepath.Clean(path)
	for {
		fd, err := unix.Open(current, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open directory for durability: %w", err)
		}
		syncErr := unix.Fsync(fd)
		unix.Close(fd)
		if syncErr != nil {
			return fmt.Errorf("sync directory for durability: %w", syncErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func ensureSameFilesystem(first, second string) error {
	var one, two unix.Stat_t
	if err := unix.Stat(first, &one); err != nil {
		return err
	}
	if err := unix.Stat(second, &two); err != nil {
		return err
	}
	if one.Dev != two.Dev {
		return fmt.Errorf("%w: roots must share a filesystem", ErrUnsafeStorage)
	}
	return nil
}

func rootFD(root *securedRoot) (int, error) {
	if root == nil || root.fd < 0 {
		return -1, ErrClosed
	}
	fd, err := unix.Openat(root.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("reopen secured storage root: %w", err)
	}
	return fd, nil
}

func ensureBlobBase(root *securedRoot) error {
	fd, err := rootFD(root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	child, err := ensureDirAt(fd, "sha256")
	if err != nil {
		return err
	}
	unix.Close(child)
	return unix.Fsync(fd)
}

func ensureDirAt(parent int, name string) (int, error) {
	if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open storage directory: %w", err)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("secure storage directory: %w", err)
	}
	if err := unix.Fsync(fd); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("sync storage directory: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("sync storage parent: %w", err)
	}
	return fd, nil
}

func createStage(root *securedRoot) (string, *os.File, error) {
	fd, err := rootFD(root)
	if err != nil {
		return "", nil, err
	}
	defer unix.Close(fd)
	for range 32 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := ".upload-" + hex.EncodeToString(token[:])
		fileFD, err := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, os.NewFile(uintptr(fileFD), name), nil
	}
	return "", nil, errors.New("cannot allocate staging file")
}

func blobParent(root *securedRoot, hash [sha256.Size]byte, create bool) (int, string, error) {
	encoded := hex.EncodeToString(hash[:])
	rootfd, err := rootFD(root)
	if err != nil {
		return -1, "", err
	}
	current := rootfd
	for _, name := range []string{"sha256", encoded[:2], encoded[2:4]} {
		var next int
		if create {
			next, err = ensureDirAt(current, name)
		} else {
			next, err = unix.Openat(current, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(current)
		if err != nil {
			return -1, "", err
		}
		current = next
	}
	return current, encoded, nil
}

func publish(stagingRoot *securedRoot, stage string, blobRoot *securedRoot, hash [sha256.Size]byte, size int64) error {
	stageFD, err := rootFD(stagingRoot)
	if err != nil {
		return err
	}
	defer unix.Close(stageFD)
	parent, name, err := blobParent(blobRoot, hash, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	err = unix.Linkat(stageFD, stage, parent, name, 0)
	if errors.Is(err, unix.EEXIST) {
		content, readErr := readBlob(blobRoot, hash, MaxBlobBytes)
		if readErr != nil {
			return ErrIntegrity
		}
		actual := sha256.Sum256(content)
		if int64(len(content)) != size || !bytes.Equal(actual[:], hash[:]) {
			return ErrIntegrity
		}
	} else if err != nil {
		return fmt.Errorf("publish blob: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync blob directory: %w", err)
	}
	if err := unix.Unlinkat(stageFD, stage, 0); err != nil {
		return fmt.Errorf("remove staged blob: %w", err)
	}
	if err := unix.Fsync(stageFD); err != nil {
		return fmt.Errorf("sync staging directory: %w", err)
	}
	return nil
}

func readBlob(root *securedRoot, hash [sha256.Size]byte, max int64) ([]byte, error) {
	parent, name, err := blobParent(root, hash, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0o777 != 0o600 || stat.Size > max {
		return nil, ErrIntegrity
	}
	content, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(content)) > max {
		return nil, ErrIntegrity
	}
	return content, nil
}

func stagingEntries(root *securedRoot) ([]string, error) {
	fd, err := rootFD(root)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "staging-root")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		token := strings.TrimPrefix(name, ".upload-")
		var stat unix.Stat_t
		if !strings.HasPrefix(name, ".upload-") || !validHex(token, 32) ||
			unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 {
			return nil, ErrUnsafeStorage
		}
		names = append(names, name)
	}
	return names, nil
}

func removeStage(root *securedRoot, name string) error {
	fd, err := rootFD(root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Unlinkat(fd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
