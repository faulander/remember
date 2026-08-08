//go:build darwin || linux

package repository

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const maxConflictStageBytes = 8 * 1024 * 1024

var testHookBeforeConflictCleanupUnlink func()

func validConflictMaterializationStage(relative string) bool {
	parts := strings.Split(relative, "/")
	return len(parts) == 4 && parts[0] == ".remember" && parts[1] == "conflicts" && parts[2] == "materializations" && strings.HasSuffix(parts[3], ".md") && len(strings.TrimSuffix(parts[3], ".md")) == 36
}

// RemoveRootedConflictStageExpected removes only exact immutable technical
// conflict-copy bytes. Rename-before-validate and a final inode check preserve
// both the original pathname and a concurrently replaced cleanup pathname.
func RemoveRootedConflictStageExpected(root, relative string, expected [32]byte) error {
	if !validConflictMaterializationStage(relative) {
		return errors.New("invalid conflict materialization stage")
	}
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	cleanup := base + ".cleanup"
	fd, err := unix.Openat(parent, cleanup, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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
		fd, err = unix.Openat(parent, cleanup, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&0o777 != 0o600 {
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
	if sha256.Sum256(content) != expected {
		return errors.New("conflict cleanup staging hash mismatch")
	}
	if source, sourceErr := readAtMode(parent, base, maxConflictStageBytes, 0o600); sourceErr == nil && bytes.Equal(source, content) {
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
	if err := unix.Unlinkat(parent, cleanup, 0); err != nil {
		return err
	}
	return unix.Fsync(parent)
}
