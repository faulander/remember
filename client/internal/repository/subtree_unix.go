//go:build darwin || linux

package repository

import (
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"strings"

	"github.com/faulander/remember/client/internal/naming"
	"golang.org/x/sys/unix"
)

var testHookAfterRootedSubtreeOpen func()

func VerifyRootedSubtreeExpected(root, rootRelative string, rootDevice, rootInode uint64, entries []RootedSubtreeEntry, maxFileBytes int64) error {
	if rootDevice == 0 || rootInode == 0 || maxFileBytes < 0 || maxFileBytes == math.MaxInt64 || naming.ValidateRelativePath(rootRelative) != nil {
		return errors.New("invalid rooted subtree bounds")
	}
	manifest := make(map[string]RootedSubtreeEntry, len(entries))
	children := make(map[string]map[string]RootedSubtreeEntry)
	for _, entry := range entries {
		if entry.Relative == "" || path.Clean(entry.Relative) != entry.Relative || strings.HasPrefix(entry.Relative, "/") {
			return errors.New("invalid rooted subtree path")
		}
		parts := strings.Split(entry.Relative, "/")
		for _, component := range parts {
			if naming.ValidateComponent(component) != nil {
				return errors.New("invalid rooted subtree component")
			}
		}
		if entry.Kind != RootedSubtreeFolder && entry.Kind != RootedSubtreeFile {
			return errors.New("invalid rooted subtree type")
		}
		zeroHash := [sha256.Size]byte{}
		if entry.Kind == RootedSubtreeFolder && (entry.Device == 0 || entry.Inode == 0 || entry.Hash != zeroHash) {
			return errors.New("invalid rooted subtree folder identity")
		}
		if entry.Kind == RootedSubtreeFile && (entry.Device != 0 || entry.Inode != 0) {
			return errors.New("invalid rooted subtree file identity")
		}
		if _, exists := manifest[entry.Relative]; exists {
			return errors.New("duplicate rooted subtree entry")
		}
		manifest[entry.Relative] = entry
		parent := path.Dir(entry.Relative)
		if parent == "." {
			parent = ""
		}
		if children[parent] == nil {
			children[parent] = make(map[string]RootedSubtreeEntry)
		}
		children[parent][path.Base(entry.Relative)] = entry
	}
	for relative := range manifest {
		parent := path.Dir(relative)
		if parent == "." {
			continue
		}
		entry, exists := manifest[parent]
		if !exists || entry.Kind != RootedSubtreeFolder {
			return errors.New("rooted subtree parent is not manifested folder")
		}
	}
	parent, base, err := openRootedParent(root, rootRelative)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	rootFD, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	if err := verifySubtreeFolderIdentity(rootFD, rootDevice, rootInode); err != nil {
		return err
	}
	if testHookAfterRootedSubtreeOpen != nil {
		testHookAfterRootedSubtreeOpen()
	}
	return verifySubtreeDirectory(rootFD, "", children, maxFileBytes)
}

func verifySubtreeDirectory(fd int, relative string, children map[string]map[string]RootedSubtreeEntry, maxFileBytes int64) error {
	expected := children[relative]
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(dup), relative)
	actual, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("rooted subtree entry count mismatch")
	}
	seen := make(map[string]bool, len(actual))
	for _, directoryEntry := range actual {
		entry, exists := expected[directoryEntry.Name()]
		if !exists || seen[directoryEntry.Name()] {
			return errors.New("unexpected rooted subtree entry")
		}
		seen[directoryEntry.Name()] = true
		childFD, err := unix.Openat(fd, directoryEntry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return errors.New("cannot safely open rooted subtree entry")
		}
		var stat unix.Stat_t
		if err := unix.Fstat(childFD, &stat); err != nil {
			unix.Close(childFD)
			return err
		}
		kind := stat.Mode & unix.S_IFMT
		switch entry.Kind {
		case RootedSubtreeFolder:
			if kind != unix.S_IFDIR || uint64(stat.Dev) != entry.Device || uint64(stat.Ino) != entry.Inode {
				unix.Close(childFD)
				return errors.New("rooted subtree folder identity mismatch")
			}
			childRelative := entry.Relative
			err = verifySubtreeDirectory(childFD, childRelative, children, maxFileBytes)
			unix.Close(childFD)
			if err != nil {
				return err
			}
		case RootedSubtreeFile:
			if kind != unix.S_IFREG {
				unix.Close(childFD)
				return errors.New("rooted subtree file type mismatch")
			}
			if stat.Size > maxFileBytes {
				unix.Close(childFD)
				return ErrContentTooLarge
			}
			file := os.NewFile(uintptr(childFD), directoryEntry.Name())
			content, readErr := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
			file.Close()
			if readErr != nil {
				return readErr
			}
			if int64(len(content)) > maxFileBytes {
				return ErrContentTooLarge
			}
			if sha256.Sum256(content) != entry.Hash {
				return errors.New("rooted subtree file hash mismatch")
			}
		}
	}
	if len(seen) != len(expected) {
		return errors.New("rooted subtree manifest incomplete")
	}
	return nil
}

func verifySubtreeFolderIdentity(fd int, device, inode uint64) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stat.Dev) != device || uint64(stat.Ino) != inode {
		return errors.New("rooted subtree root identity mismatch")
	}
	return nil
}
