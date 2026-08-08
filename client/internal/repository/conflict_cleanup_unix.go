//go:build darwin || linux

package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const maxConflictStageBytes = 8 * 1024 * 1024

var testHookBeforeConflictCleanupUnlink func()

func validConflictTechnicalNote(relative string) bool {
	parts := strings.Split(relative, "/")
	if len(parts) != 4 || parts[0] != ".remember" || !strings.HasSuffix(parts[3], ".md") || len(strings.TrimSuffix(parts[3], ".md")) != 36 {
		return false
	}
	return parts[1] == "conflicts" && parts[2] == "materializations" || parts[1] == "trash" && parts[2] == "conflicts"
}

// RemoveRootedConflictStageExpected removes only exact immutable technical
// conflict-copy bytes. Rename-before-validate and a final inode check preserve
// both the original pathname and a concurrently replaced cleanup pathname.
func RemoveRootedConflictStageExpected(root, relative string, expected [32]byte) error {
	if !strings.HasPrefix(relative, ".remember/conflicts/materializations/") || !validConflictTechnicalNote(relative) {
		return errors.New("invalid conflict materialization stage")
	}
	return removeRootedTechnicalExpected(root, relative, expected, 0o600, ".cleanup")
}
func RemoveRootedConflictEvacuationExpected(root, relative string, expected [32]byte) error {
	if !strings.HasPrefix(relative, ".remember/trash/conflicts/") || !validConflictTechnicalNote(relative) {
		return errors.New("invalid conflict evacuation")
	}
	return removeRootedTechnicalExpected(root, relative, expected, 0o644, ".cleanup")
}
func RemoveRootedOutboxBlobExpected(root, relative string, expected [32]byte, throughSequence int64) error {
	parts := strings.Split(relative, "/")
	if len(parts) != 4 || parts[0] != ".remember" || parts[1] != "sync" || parts[2] != "outbox" || len(parts[3]) != 64 || throughSequence <= 0 {
		return errors.New("invalid outbox blob cleanup")
	}
	decoded, err := hex.DecodeString(parts[3])
	if err != nil || parts[3] != strings.ToLower(parts[3]) || !bytes.Equal(decoded, expected[:]) {
		return errors.New("outbox blob cleanup hash path mismatch")
	}
	return removeRootedTechnicalExpected(root, relative, expected, 0o600, fmt.Sprintf(".cleanup-%d", throughSequence))
}
func removeRootedTechnicalExpected(root, relative string, expected [32]byte, expectedMode uint32, cleanupSuffix string) error {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	cleanup := base + cleanupSuffix
	fd, err := unix.Openat(parent, cleanup, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := renameFolderNoReplace(parent, base, parent, cleanup); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return fmt.Errorf("stage conflict cleanup: %w", err)
		}
		if err := unix.Fsync(parent); err != nil {
			return err
		}
		fd, err = unix.Openat(parent, cleanup, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	mode := opened.Mode & 0o777
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || uint32(mode) != expectedMode {
		return errors.New("invalid conflict cleanup staging mode")
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(dup), cleanup)
	content, readErr := io.ReadAll(io.LimitReader(file, maxConflictStageBytes+1))
	file.Close()
	if readErr != nil || len(content) > maxConflictStageBytes {
		return errors.New("read conflict cleanup staging")
	}
	if len(content) == 0 {
		if _, sourceErr := readAt(parent, base, 1); !errors.Is(sourceErr, unix.ENOENT) {
			return errors.New("zeroed conflict cleanup has source replacement")
		}
		var current unix.Stat_t
		if err := unix.Fstatat(parent, cleanup, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != opened.Dev || current.Ino != opened.Ino {
			return errors.New("zeroed conflict cleanup changed concurrently")
		}
		if err := unix.Fsync(fd); err != nil {
			return err
		}
		return unix.Fsync(parent)
	}
	if sha256.Sum256(content) != expected {
		return errors.New("conflict cleanup staging hash mismatch")
	}
	if source, sourceErr := readAt(parent, base, maxConflictStageBytes); sourceErr == nil && bytes.Equal(source, content) {
		return errors.New("ambiguous duplicate conflict cleanup staging")
	} else if sourceErr != nil && !errors.Is(sourceErr, unix.ENOENT) {
		return sourceErr
	}
	if testHookBeforeConflictCleanupUnlink != nil {
		testHookBeforeConflictCleanupUnlink()
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parent, cleanup, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != opened.Dev || current.Ino != opened.Ino {
		return errors.New("conflict cleanup staging changed concurrently")
	}
	// Erase through the validated open descriptor. Unlike pathname unlink this
	// cannot delete a replacement swapped in after the final inode check.
	if err := unix.Ftruncate(fd, 0); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	return unix.Fsync(parent)
}
