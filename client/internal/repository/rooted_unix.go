//go:build darwin || linux

package repository

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Test hooks are package-private and only used to deterministically exercise
// replacement races after the exact pathname object has been staged.
var (
	testHookAfterRootedSaveStage func()
	testHookAfterRootedMoveStage func()
)

func EnsurePrivateStagingSupported() error { return nil }

func ReadRooted(root, relative string, maxBytes int64) ([]byte, error) {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	return readAt(parent, base, maxBytes)
}

// ReadRootedInFolderExpected reads through one retained parent descriptor after
// binding that descriptor to the indexed Folder identity.
func ReadRootedInFolderExpected(root, relative string, device, inode uint64, maxBytes int64) ([]byte, error) {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	if err := verifyFolderDescriptor(parent, device, inode); err != nil {
		return nil, err
	}
	return readAt(parent, base, maxBytes)
}

func ReadRootedPrivate(root, relative string, maxBytes int64) ([]byte, error) {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	return readAtMode(parent, base, maxBytes, 0o600)
}

func CreateRootedDirectory(root, relative string, mode uint32) error {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if err := unix.Mkdirat(parent, base, mode); err != nil {
		return fmt.Errorf("create rooted directory exclusively: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync rooted directory parent: %w", err)
	}
	return nil
}

func EnsureRootedDirectory(root, relative string, mode uint32) error {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if err := unix.Mkdirat(parent, base, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create rooted directory: %w", err)
	}
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("verify rooted directory: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, mode); err != nil {
		return fmt.Errorf("restrict rooted directory: %w", err)
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync rooted directory: %w", err)
	}
	return unix.Fsync(parent)
}

// CreateRootedPrivate publishes immutable technical bytes with mode 0600.
func CreateRootedPrivate(root, relative string, content []byte) error {
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	temp, file, err := createTempAt(parent, ".remember-private-", 0o600)
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		if temp != "" {
			_ = unix.Unlinkat(parent, temp, 0)
		}
	}()
	if err := writeAndSync(file, content); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private staged file: %w", err)
	}
	if err := unix.Linkat(parent, temp, parent, base, 0); err != nil {
		return fmt.Errorf("publish private file exclusively: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync private publication: %w", err)
	}
	if err := unix.Unlinkat(parent, temp, 0); err != nil {
		return fmt.Errorf("remove private staging link: %w", err)
	}
	temp = ""
	return unix.Fsync(parent)
}

// CreateRootedInFolderExpected publishes a note through one retained parent
// descriptor after binding that descriptor to the indexed folder identity.
func CreateRootedInFolderExpected(root, parentRelative, name string, device, inode uint64, content []byte, validate Validator) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return errors.New("invalid rooted child name")
	}
	if validate != nil {
		if err := validate(content); err != nil {
			return fmt.Errorf("validate staged note: %w", err)
		}
	}
	parentOfFolder, folderName, err := openRootedParent(root, parentRelative)
	if err != nil {
		return err
	}
	defer unix.Close(parentOfFolder)
	parent, err := unix.Openat(parentOfFolder, folderName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open expected rooted folder: %w", err)
	}
	defer unix.Close(parent)
	var stat unix.Stat_t
	if err := unix.Fstat(parent, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stat.Dev) != device || uint64(stat.Ino) != inode {
		return errors.New("rooted folder identity changed")
	}
	temp, file, err := createTempAt(parent, ".remember-create-", 0o644)
	if err != nil {
		return err
	}
	defer func() { file.Close(); _ = unix.Unlinkat(parent, temp, 0) }()
	if err := writeAndSync(file, content); err != nil {
		return err
	}
	if err := unix.Linkat(parent, temp, parent, name, 0); err != nil {
		return fmt.Errorf("publish note in expected folder exclusively: %w", err)
	}
	return unix.Fsync(parent)
}

func CreateRooted(root, relative string, content []byte, validate Validator) error {
	if validate != nil {
		if err := validate(content); err != nil {
			return fmt.Errorf("validate staged note: %w", err)
		}
	}
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	temp, file, err := createTempAt(parent, ".remember-create-", 0o644)
	if err != nil {
		return err
	}
	defer func() { file.Close(); _ = unix.Unlinkat(parent, temp, 0) }()
	if err := writeAndSync(file, content); err != nil {
		return err
	}
	if err := unix.Linkat(parent, temp, parent, base, 0); err != nil {
		return fmt.Errorf("publish note exclusively: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync note directory: %w", err)
	}
	return nil
}

// WriteRootedExpected stages the exact current pathname object before checking
// the expected bytes. A replacement created after staging is never removed;
// the displaced version is retained under the hidden staging name.
func WriteRootedExpected(root, relative string, expected, content []byte, validate Validator) error {
	return writeRootedExpected(root, relative, 0, 0, expected, content, validate)
}

// WriteRootedInFolderExpected replaces a note only through the exact indexed
// parent Folder descriptor.
func WriteRootedInFolderExpected(root, relative string, device, inode uint64, expected, content []byte, validate Validator) error {
	return writeRootedExpected(root, relative, device, inode, expected, content, validate)
}

func writeRootedExpected(root, relative string, device, inode uint64, expected, content []byte, validate Validator) error {
	if validate != nil {
		if err := validate(content); err != nil {
			return fmt.Errorf("validate staged note: %w", err)
		}
	}
	parent, base, err := openRootedParent(root, relative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if device != 0 || inode != 0 {
		if err := verifyFolderDescriptor(parent, device, inode); err != nil {
			return err
		}
	}
	displaced, err := stageAt(parent, base, ".remember-save-recovery-")
	if err != nil {
		return fmt.Errorf("stage destination before replace: %w", err)
	}
	retained := true
	defer func() {
		if !retained {
			_ = unix.Unlinkat(parent, displaced, 0)
		}
	}()
	if testHookAfterRootedSaveStage != nil {
		testHookAfterRootedSaveStage()
	}
	current, err := readAt(parent, displaced, int64(len(expected))+1)
	if err != nil || !bytes.Equal(current, expected) {
		restored := restoreAt(parent, displaced, base)
		retained = !restored
		return recoveryConflict("destination changed concurrently", displaced, retained)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, displaced, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		restored := restoreAt(parent, displaced, base)
		retained = !restored
		return recoveryConflict("destination is no longer a regular file", displaced, retained)
	}
	temp, file, err := createTempAt(parent, ".remember-write-", uint32(stat.Mode&0o777))
	if err != nil {
		restored := restoreAt(parent, displaced, base)
		retained = !restored
		return err
	}
	defer func() { file.Close(); _ = unix.Unlinkat(parent, temp, 0) }()
	if err := writeAndSync(file, content); err != nil {
		restored := restoreAt(parent, displaced, base)
		retained = !restored
		return err
	}
	if err := unix.Linkat(parent, temp, parent, base, 0); err != nil {
		restored := restoreAt(parent, displaced, base)
		retained = !restored
		if errors.Is(err, unix.EEXIST) {
			return recoveryConflict("destination was recreated concurrently", displaced, retained)
		}
		return fmt.Errorf("publish replacement exclusively: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync destination directory: %w", err)
	}
	if err := unix.Unlinkat(parent, displaced, 0); err != nil {
		return fmt.Errorf("remove displaced destination: %w", err)
	}
	retained = true // already removed; suppress deferred unlink
	return nil
}

// MoveRootedExpected atomically renames the exact source pathname to a hidden
// staging name before validation. It can never unlink a newly recreated source.
func MoveRootedExpected(root, sourceRelative, destinationRelative string, expected []byte) error {
	return moveRootedExpected(root, sourceRelative, destinationRelative, 0, 0, expected)
}

// MoveRootedFromFolderExpected moves a note only through the exact indexed
// source Folder descriptor.
func MoveRootedFromFolderExpected(root, sourceRelative, destinationRelative string, device, inode uint64, expected []byte) error {
	return moveRootedExpected(root, sourceRelative, destinationRelative, device, inode, expected)
}

func moveRootedExpected(root, sourceRelative, destinationRelative string, device, inode uint64, expected []byte) error {
	sourceParent, sourceBase, err := openRootedParent(root, sourceRelative)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	if device != 0 || inode != 0 {
		if err := verifyFolderDescriptor(sourceParent, device, inode); err != nil {
			return err
		}
	}
	destinationParent, destinationBase, err := openRootedParent(root, destinationRelative)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	staged, err := stageAt(sourceParent, sourceBase, ".remember-move-recovery-")
	if errors.Is(err, unix.ENOENT) {
		return recoverStagedMove(sourceParent, destinationParent, destinationBase, expected)
	}
	if err != nil {
		return fmt.Errorf("stage move source: %w", err)
	}
	retained := true
	if testHookAfterRootedMoveStage != nil {
		testHookAfterRootedMoveStage()
	}
	current, readErr := readAt(sourceParent, staged, int64(len(expected))+1)
	if readErr != nil || !bytes.Equal(current, expected) {
		restored := restoreAt(sourceParent, staged, sourceBase)
		retained = !restored
		return recoveryConflict("move source changed concurrently", staged, retained)
	}
	if err := unix.Linkat(sourceParent, staged, destinationParent, destinationBase, 0); err != nil {
		restored := restoreAt(sourceParent, staged, sourceBase)
		retained = !restored
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("publish moved note exclusively: %w", err)
		}
		return fmt.Errorf("publish moved note: %w", err)
	}
	if err := unix.Fsync(destinationParent); err != nil {
		return fmt.Errorf("sync move destination: %w", err)
	}
	if err := unix.Unlinkat(sourceParent, staged, 0); err != nil {
		return fmt.Errorf("remove staged move source: %w", err)
	}
	retained = false
	if sourceParent != destinationParent {
		if err := unix.Fsync(sourceParent); err != nil {
			return fmt.Errorf("sync move source: %w", err)
		}
	}
	return nil
}

func RootedStagedMoveExists(root, sourceRelative string, expected []byte) (bool, error) {
	parent, _, err := openRootedParent(root, sourceRelative)
	if err != nil {
		return false, err
	}
	defer unix.Close(parent)
	candidate, err := findStagedMove(parent, expected)
	return candidate != "", err
}

func findStagedMove(sourceParent int, expected []byte) (string, error) {
	dup, err := unix.Dup(sourceParent)
	if err != nil {
		return "", err
	}
	directory := os.NewFile(uintptr(dup), "move-recovery")
	entries, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return "", err
	}
	var candidate string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".remember-move-recovery-") {
			continue
		}
		content, readErr := readAt(sourceParent, entry.Name(), int64(len(expected))+1)
		if readErr == nil && bytes.Equal(content, expected) {
			if candidate != "" {
				return "", errors.New("ambiguous staged move recovery")
			}
			candidate = entry.Name()
		}
	}
	return candidate, nil
}

func recoverStagedMove(sourceParent, destinationParent int, destinationBase string, expected []byte) error {
	candidate, err := findStagedMove(sourceParent, expected)
	if err != nil {
		return err
	}
	if candidate == "" {
		return unix.ENOENT
	}
	if err := unix.Linkat(sourceParent, candidate, destinationParent, destinationBase, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			current, readErr := readAt(destinationParent, destinationBase, int64(len(expected))+1)
			if readErr == nil && bytes.Equal(current, expected) {
				if err := unix.Fsync(destinationParent); err != nil {
					return err
				}
				if err := unix.Unlinkat(sourceParent, candidate, 0); err != nil {
					return err
				}
				return unix.Fsync(sourceParent)
			}
		}
		return err
	}
	if err := unix.Fsync(destinationParent); err != nil {
		return err
	}
	if err := unix.Unlinkat(sourceParent, candidate, 0); err != nil {
		return err
	}
	return unix.Fsync(sourceParent)
}

func verifyFolderDescriptor(fd int, device, inode uint64) error {
	if device == 0 || inode == 0 {
		return errors.New("invalid rooted folder identity")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stat.Dev) != device || uint64(stat.Ino) != inode {
		return errors.New("rooted folder identity changed")
	}
	return nil
}

func openRootedParent(root, relative string) (int, string, error) {
	parts := strings.Split(relative, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return -1, "", errors.New("invalid rooted path")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open managed root: %w", err)
	}
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return -1, "", errors.New("invalid rooted path")
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			return -1, "", fmt.Errorf("open rooted parent: %w", openErr)
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func readAt(parent int, name string, maxBytes int64) ([]byte, error) {
	return readAtMode(parent, name, maxBytes, 0)
}

func readAtMode(parent int, name string, maxBytes int64, requiredMode uint32) ([]byte, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("rooted object is not a regular file")
	}
	if requiredMode != 0 && uint32(stat.Mode)&0o777 != requiredMode {
		return nil, errors.New("rooted private file has unsafe permissions")
	}
	if maxBytes >= 0 && stat.Size > maxBytes {
		return nil, ErrContentTooLarge
	}
	reader := io.Reader(file)
	if maxBytes >= 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maxBytes >= 0 && int64(len(content)) > maxBytes {
		return nil, ErrContentTooLarge
	}
	return content, nil
}

func createTempAt(parent int, prefix string, mode uint32) (string, *os.File, error) {
	for range 32 {
		name, err := randomName(prefix)
		if err != nil {
			return "", nil, err
		}
		fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("cannot allocate unique staged name")
}

func stageAt(parent int, source, prefix string) (string, error) {
	name, placeholder, err := createTempAt(parent, prefix, 0o600)
	if err != nil {
		return "", err
	}
	if err := placeholder.Close(); err != nil {
		return "", err
	}
	if err := unix.Renameat(parent, source, parent, name); err != nil {
		_ = unix.Unlinkat(parent, name, 0)
		return "", err
	}
	return name, nil
}

func restoreAt(parent int, staged, destination string) bool {
	if err := unix.Linkat(parent, staged, parent, destination, 0); err != nil {
		return false
	}
	_ = unix.Fsync(parent)
	_ = unix.Unlinkat(parent, staged, 0)
	return true
}

func writeAndSync(file *os.File, content []byte) error {
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write staged file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync staged file: %w", err)
	}
	return nil
}

func randomName(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func recoveryConflict(detail, staged string, retained bool) error {
	if retained {
		return fmt.Errorf("%w: %s; displaced bytes retained as %s", ErrConcurrentModification, detail, staged)
	}
	return fmt.Errorf("%w: %s", ErrConcurrentModification, detail)
}
