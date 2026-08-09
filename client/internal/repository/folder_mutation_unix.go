//go:build darwin || linux

package repository

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const folderMoveRecoveryPrefix = ".remember-folder-move-recovery-"

var testHookBeforeRootedFolderDelete func()

// MoveRootedFolderExpected moves only the directory identified by device/inode.
// A crash after source staging is resumed by finding the hidden staged inode.
func MoveRootedFolderExpected(root, sourceRelative, destinationRelative string, device, inode uint64) error {
	return moveRootedFolderExpected(root, sourceRelative, destinationRelative, device, inode, false)
}
func MoveRootedEmptyFolderExpected(root, sourceRelative, destinationRelative string, device, inode uint64) error {
	return moveRootedFolderExpected(root, sourceRelative, destinationRelative, device, inode, true)
}
func moveRootedFolderExpected(root, sourceRelative, destinationRelative string, device, inode uint64, requireEmpty bool) error {
	if err := VerifyRootedFolderIdentity(root, destinationRelative, device, inode); err == nil {
		if !requireEmpty {
			return nil
		}
		if emptyErr := VerifyRootedEmptyFolderIdentity(root, destinationRelative, device, inode); emptyErr == nil {
			return nil
		}
		if restoreErr := moveRootedFolderExpected(root, destinationRelative, sourceRelative, device, inode, false); restoreErr != nil {
			return fmt.Errorf("restore nonempty moved folder: %w", restoreErr)
		}
		return errors.New("folder became nonempty during move")
	}
	sourceParent, sourceBase, err := openRootedParent(root, sourceRelative)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	staged, err := findStagedFolder(sourceParent, device, inode)
	if err != nil {
		return err
	}
	if staged == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		staged = folderMoveRecoveryPrefix + hex.EncodeToString(token[:])
		if err := renameFolderNoReplace(sourceParent, sourceBase, sourceParent, staged); err != nil {
			if errors.Is(err, unix.ENOENT) {
				if err := VerifyRootedFolderIdentity(root, destinationRelative, device, inode); err == nil {
					return nil
				}
			}
			return fmt.Errorf("stage folder move source: %w", err)
		}
		if err := unix.Fsync(sourceParent); err != nil {
			return err
		}
	}
	if err := verifyFolderAt(sourceParent, staged, device, inode); err != nil {
		_ = renameFolderNoReplace(sourceParent, staged, sourceParent, sourceBase)
		return fmt.Errorf("folder move source changed: %w", err)
	}
	if requireEmpty {
		empty, err := folderAtEmpty(sourceParent, staged, device, inode)
		if err != nil || !empty {
			_ = renameFolderNoReplace(sourceParent, staged, sourceParent, sourceBase)
			if err != nil {
				return err
			}
			return errors.New("folder is not empty")
		}
	}
	destinationParent, destinationBase, err := openRootedParent(root, destinationRelative)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	if err := renameFolderNoReplace(sourceParent, staged, destinationParent, destinationBase); err != nil {
		return fmt.Errorf("publish folder move exclusively: %w", err)
	}
	if err := unix.Fsync(destinationParent); err != nil {
		return err
	}
	if sourceParent != destinationParent {
		if err := unix.Fsync(sourceParent); err != nil {
			return err
		}
	}
	if err := VerifyRootedFolderIdentity(root, destinationRelative, device, inode); err != nil {
		return err
	}
	if requireEmpty {
		if err := VerifyRootedEmptyFolderIdentity(root, destinationRelative, device, inode); err != nil {
			if restoreErr := moveRootedFolderExpected(root, destinationRelative, sourceRelative, device, inode, false); restoreErr != nil {
				return fmt.Errorf("restore folder after concurrent content: %v / %w", err, restoreErr)
			}
			return err
		}
	}
	return nil
}

func folderAtEmpty(parent int, name string, device, inode uint64) (bool, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	if uint64(stat.Dev) != device || stat.Ino != inode {
		return false, errors.New("empty folder identity mismatch")
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return false, err
	}
	directory := os.NewFile(uintptr(dup), name)
	entries, readErr := directory.ReadDir(1)
	directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, readErr
	}
	return len(entries) == 0, nil
}

// DeleteRootedFolderExpected atomically removes only an empty, inode-bound
// directory. POSIX rmdir performs the final emptiness check atomically.
func DeleteRootedFolderExpected(root, sourceRelative string, device, inode uint64) error {
	sourceParent, sourceBase, err := openRootedParent(root, sourceRelative)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	staged, err := findStagedFolder(sourceParent, device, inode)
	if err != nil {
		return err
	}
	if staged == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		staged = folderMoveRecoveryPrefix + hex.EncodeToString(token[:])
		if err := renameFolderNoReplace(sourceParent, sourceBase, sourceParent, staged); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return fmt.Errorf("stage folder delete source: %w", err)
		}
		if err := unix.Fsync(sourceParent); err != nil {
			return err
		}
	}
	if err := verifyFolderAt(sourceParent, staged, device, inode); err != nil {
		_ = renameFolderNoReplace(sourceParent, staged, sourceParent, sourceBase)
		return fmt.Errorf("folder delete source changed: %w", err)
	}
	if testHookBeforeRootedFolderDelete != nil {
		testHookBeforeRootedFolderDelete()
	}
	if err := unix.Unlinkat(sourceParent, staged, unix.AT_REMOVEDIR); err != nil {
		_ = renameFolderNoReplace(sourceParent, staged, sourceParent, sourceBase)
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return errors.New("remote folder delete requires an empty folder")
		}
		return fmt.Errorf("remove empty folder: %w", err)
	}
	return unix.Fsync(sourceParent)
}

func findStagedFolder(parent int, device, inode uint64) (string, error) {
	dup, err := unix.Dup(parent)
	if err != nil {
		return "", err
	}
	directory := os.NewFile(uintptr(dup), "folder-move-recovery")
	entries, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), folderMoveRecoveryPrefix) {
			continue
		}
		if verifyFolderAt(parent, entry.Name(), device, inode) != nil {
			continue
		}
		if found != "" {
			return "", errors.New("ambiguous staged folder move recovery")
		}
		found = entry.Name()
	}
	return found, nil
}

func verifyFolderAt(parent int, name string, device, inode uint64) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if uint64(stat.Dev) != device || stat.Ino != inode {
		return errors.New("folder inode mismatch")
	}
	return nil
}
