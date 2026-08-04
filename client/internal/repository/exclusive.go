package repository

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrContentTooLarge = errors.New("content exceeds configured limit")

// CreateExclusive publishes complete staged bytes without replacing an
// existing destination. A crash can leave the staged file, but never a partial
// destination.
func CreateExclusive(path string, content []byte, validate Validator) (err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".remember-create-*")
	if err != nil {
		return fmt.Errorf("create staged note: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { temporary.Close(); _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set staged note mode: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write staged note: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync staged note: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staged note: %w", err)
	}
	if validate != nil {
		if err := validate(content); err != nil {
			return fmt.Errorf("validate staged note: %w", err)
		}
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return fmt.Errorf("publish note exclusively: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync note directory: %w", err)
	}
	return nil
}

// MoveExclusiveExpected first renames the exact source pathname to a hidden
// staging name. Validation and publication operate on that captured object, so
// a source recreated concurrently is never unlinked.
func MoveExclusiveExpected(source, destination string, expected []byte) error {
	sourceDirectory := filepath.Dir(source)
	placeholder, err := os.CreateTemp(sourceDirectory, ".remember-move-recovery-*")
	if err != nil {
		return fmt.Errorf("reserve move staging: %w", err)
	}
	staged := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return err
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("remove move staging placeholder: %w", err)
	}
	if err := os.Rename(source, staged); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("stage move source: %w", err)
	}
	restore := func() bool {
		if err := os.Link(staged, source); err != nil {
			return false
		}
		_ = syncDirectory(sourceDirectory)
		_ = os.Remove(staged)
		return true
	}
	if expected != nil {
		captured, readErr := os.ReadFile(staged)
		info, statErr := os.Lstat(staged)
		if readErr != nil || statErr != nil || !info.Mode().IsRegular() || !bytes.Equal(captured, expected) {
			retained := !restore()
			return recoveryConflictGeneric("move source changed concurrently", staged, retained)
		}
	}
	if err := os.Link(staged, destination); err != nil {
		_ = restore()
		return fmt.Errorf("publish moved note exclusively: %w", err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("sync moved note destination: %w", err)
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("remove staged move source: %w", err)
	}
	if sourceDirectory != filepath.Dir(destination) {
		if err := syncDirectory(sourceDirectory); err != nil {
			return fmt.Errorf("sync moved note source: %w", err)
		}
	}
	return nil
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("object is not a regular file")
	}
	if maxBytes >= 0 && info.Size() > maxBytes {
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

func recoveryConflictGeneric(detail, staged string, retained bool) error {
	if retained {
		return fmt.Errorf("%w: %s; displaced bytes retained as %s", ErrConcurrentModification, detail, filepath.Base(staged))
	}
	return fmt.Errorf("%w: %s", ErrConcurrentModification, detail)
}
