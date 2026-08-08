//go:build windows

package repository

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Sync staging remains disabled on Windows until every path component can be
// retained by handle and checked for reparse-point replacement.
func EnsurePrivateStagingSupported() error {
	return errors.New("private sync staging is not yet supported on Windows")
}

func RootedStagedMoveExists(string, string, []byte) (bool, error) {
	return false, errors.New("handle-safe move recovery is not yet supported on Windows")
}

func ReadRootedPrivate(string, string, int64) ([]byte, error) {
	return nil, errors.New("private sync staging is not yet supported on Windows")
}
func RemoveRootedConflictStageExpected(string, string, [32]byte) error {
	return errors.New("private sync staging is not yet supported on Windows")
}

func ReadRooted(root, relative string, maxBytes int64) ([]byte, error) {
	return readBoundedFile(filepath.Join(root, filepath.FromSlash(relative)), maxBytes)
}

func CreateRootedDirectory(root, relative string, mode uint32) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.Mkdir(path, os.FileMode(mode)); err != nil {
		return fmt.Errorf("create rooted directory exclusively: %w", err)
	}
	return nil
}

func EnsureRootedDirectory(root, relative string, mode uint32) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.Mkdir(path, os.FileMode(mode)); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("rooted directory is not a real directory")
	}
	return nil
}

func CreateRootedPrivate(root, relative string, content []byte) error {
	return CreateExclusivePrivate(filepath.Join(root, filepath.FromSlash(relative)), content)
}

func CreateRooted(root, relative string, content []byte, validate Validator) error {
	return CreateExclusive(filepath.Join(root, filepath.FromSlash(relative)), content, validate)
}

func WriteRootedExpected(root, relative string, expected, content []byte, validate Validator) error {
	return WriteAtomicExpected(filepath.Join(root, filepath.FromSlash(relative)), expected, content, validate)
}

func MoveRootedExpected(root, sourceRelative, destinationRelative string, expected []byte) error {
	source := filepath.Join(root, filepath.FromSlash(sourceRelative))
	destination := filepath.Join(root, filepath.FromSlash(destinationRelative))
	if err := MoveExclusiveExpected(source, destination, expected); err != nil {
		return fmt.Errorf("move rooted note: %w", err)
	}
	return nil
}
