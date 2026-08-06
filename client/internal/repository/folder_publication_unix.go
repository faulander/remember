//go:build darwin || linux

package repository

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const folderNonceMarker = ".remember-apply-nonce"

func validFolderStage(relative string) bool {
	parts := strings.Split(relative, "/")
	if len(parts) != 5 || parts[0] != ".remember" || parts[1] != "apply" || parts[2] != "folders" {
		return false
	}
	for _, part := range parts[3:] {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func CreateRootedFolderPublication(root, stageRelative string, nonce [32]byte) (uint64, uint64, error) {
	if !validFolderStage(stageRelative) {
		return 0, 0, errors.New("invalid folder publication stage")
	}
	parts := strings.Split(stageRelative, "/")
	for i := 2; i < len(parts); i++ {
		if err := EnsureRootedDirectory(root, strings.Join(parts[:i], "/"), 0o700); err != nil {
			return 0, 0, err
		}
	}
	if err := CreateRootedDirectory(root, stageRelative, 0o700); err != nil {
		return 0, 0, err
	}
	parent, base, err := openRootedParent(root, stageRelative)
	if err != nil {
		return 0, 0, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, 0, err
	}
	defer unix.Close(fd)
	marker, err := unix.Openat(fd, folderNonceMarker, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return 0, 0, err
	}
	file := os.NewFile(uintptr(marker), folderNonceMarker)
	if _, err := file.Write(nonce[:]); err != nil {
		file.Close()
		return 0, 0, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, 0, err
	}
	if err := file.Close(); err != nil {
		return 0, 0, err
	}
	if err := unix.Fsync(fd); err != nil {
		return 0, 0, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, 0, err
	}
	if uint64(stat.Dev) > math.MaxInt64 || stat.Ino > math.MaxInt64 {
		return 0, 0, errors.New("folder publication identity exceeds SQLite range")
	}
	return uint64(stat.Dev), stat.Ino, nil
}

func VerifyRootedFolderPublication(root, relative string, nonce [32]byte, device, inode uint64) error {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if uint64(stat.Dev) != device || stat.Ino != inode {
		return errors.New("folder publication inode mismatch")
	}
	content, err := readAtMode(fd, folderNonceMarker, 32, 0o600)
	if err != nil || !bytes.Equal(content, nonce[:]) {
		return errors.New("folder publication nonce mismatch")
	}
	return nil
}

func PublishRootedFolderPublication(root, stageRelative, targetRelative string, nonce [32]byte, device, inode uint64) error {
	if err := VerifyRootedFolderPublication(root, stageRelative, nonce, device, inode); err != nil {
		return err
	}
	sourceParent, sourceBase, err := openRootedParent(root, stageRelative)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	targetParent, targetBase, err := openRootedParent(root, targetRelative)
	if err != nil {
		return err
	}
	defer unix.Close(targetParent)
	if err := renameFolderNoReplace(sourceParent, sourceBase, targetParent, targetBase); err != nil {
		return fmt.Errorf("publish folder exclusively: %w", err)
	}
	if err := unix.Fsync(targetParent); err != nil {
		return err
	}
	if sourceParent != targetParent {
		if err := unix.Fsync(sourceParent); err != nil {
			return err
		}
	}
	return VerifyRootedFolderPublication(root, targetRelative, nonce, device, inode)
}

func CleanupRootedFolderPublication(root, targetRelative string, nonce [32]byte, device, inode uint64) error {
	parent, base, err := openRootedParent(root, targetRelative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if uint64(stat.Dev) != device || stat.Ino != inode {
		return errors.New("folder cleanup inode mismatch")
	}
	content, err := readAtMode(fd, folderNonceMarker, 32, 0o600)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil || !bytes.Equal(content, nonce[:]) {
		return errors.New("folder cleanup nonce mismatch")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parent, base, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != stat.Dev || current.Ino != stat.Ino {
		return errors.New("folder cleanup target changed concurrently")
	}
	if err := unix.Unlinkat(fd, folderNonceMarker, 0); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Fstatat(parent, base, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != stat.Dev || current.Ino != stat.Ino {
		return errors.New("folder cleanup target changed concurrently")
	}
	return nil
}

func RemoveRootedFolderPublicationStage(root, stageRelative string) error {
	if !validFolderStage(stageRelative) {
		return errors.New("invalid folder publication stage")
	}
	parent, base, err := openRootedParent(root, stageRelative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		unix.Close(fd)
		return err
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	directory := os.NewFile(uintptr(dup), base)
	entries, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		unix.Close(fd)
		return err
	}
	for _, entry := range entries {
		if entry.Name() != folderNonceMarker {
			unix.Close(fd)
			return errors.New("folder stage contains unexpected entry")
		}
	}
	if err := unix.Unlinkat(fd, folderNonceMarker, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		unix.Close(fd)
		return err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parent, base, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != opened.Dev || current.Ino != opened.Ino {
		unix.Close(fd)
		return errors.New("folder stage changed concurrently")
	}
	unix.Close(fd)
	if err := unix.Unlinkat(parent, base, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(parent)
}
