// Package repository performs durable local note file operations.
package repository

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Validator checks staged bytes before they replace the destination.
type Validator func([]byte) error

// ErrConcurrentModification means the destination changed after it was read.
var ErrConcurrentModification = errors.New("destination changed concurrently")

// WriteAtomic writes and validates a same-directory temporary file before an
// atomic rename. Failures before rename leave the destination unchanged.
func WriteAtomic(path string, content []byte, validate Validator) error {
	return writeAtomic(path, nil, content, validate)
}

// WriteAtomicExpected additionally verifies the destination still contains
// expected immediately before replacement. It is used for read-modify-write
// operations so an observed external edit is never knowingly overwritten.
func WriteAtomicExpected(path string, expected, content []byte, validate Validator) error {
	return writeAtomic(path, expected, content, validate)
}

func writeAtomic(path string, expected, content []byte, validate Validator) (err error) {
	directory := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat destination: %w", statErr)
	}

	temporary, err := os.CreateTemp(directory, ".remember-write-*")
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		temporary.Close()
		if removeErr := os.Remove(temporaryPath); err == nil && removeErr != nil && !os.IsNotExist(removeErr) {
			err = fmt.Errorf("remove staged file: %w", removeErr)
		}
	}()

	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set staged file mode: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write staged file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync staged file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staged file: %w", err)
	}

	staged, err := os.ReadFile(temporaryPath)
	if err != nil {
		return fmt.Errorf("read staged file: %w", err)
	}
	if validate != nil {
		if err := validate(staged); err != nil {
			return fmt.Errorf("validate staged file: %w", err)
		}
	}
	if expected == nil {
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("replace destination: %w", err)
		}
		if err := syncDirectory(directory); err != nil {
			return fmt.Errorf("sync destination directory: %w", err)
		}
		return nil
	}

	placeholder, err := os.CreateTemp(directory, ".remember-save-recovery-*")
	if err != nil {
		return fmt.Errorf("reserve displaced destination: %w", err)
	}
	displaced := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return err
	}
	if err := os.Remove(displaced); err != nil {
		return fmt.Errorf("remove displaced placeholder: %w", err)
	}
	if err := os.Rename(path, displaced); err != nil {
		_ = os.Remove(displaced)
		return fmt.Errorf("stage destination before replace: %w", err)
	}
	restore := func() bool {
		if err := os.Link(displaced, path); err != nil {
			return false
		}
		_ = syncDirectory(directory)
		_ = os.Remove(displaced)
		return true
	}
	current, readErr := os.ReadFile(displaced)
	info, statErr := os.Lstat(displaced)
	if readErr != nil || statErr != nil || !info.Mode().IsRegular() || !bytes.Equal(current, expected) {
		retained := !restore()
		return recoveryConflictGeneric("destination changed concurrently", displaced, retained)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		retained := !restore()
		if os.IsExist(err) {
			return recoveryConflictGeneric("destination was recreated concurrently", displaced, retained)
		}
		return fmt.Errorf("publish replacement exclusively: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync destination directory: %w", err)
	}
	if err := os.Remove(displaced); err != nil {
		return fmt.Errorf("remove displaced destination: %w", err)
	}
	return nil
}
