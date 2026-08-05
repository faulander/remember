package clientsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/faulander/remember/client/internal/repository"
)

const MaxBlobBytes int64 = 8 * 1024 * 1024

var ErrBlobTooLarge = errors.New("outgoing note exceeds 8 MiB")

// StageNote captures the exact current note bytes under their content hash.
// Later file edits cannot change the bytes referenced by an Outbox operation.
func StageNote(root, relative string, expected [sha256.Size]byte) error {
	if err := repository.EnsurePrivateStagingSupported(); err != nil {
		return err
	}
	content, err := repository.ReadRooted(root, relative, MaxBlobBytes+1)
	if err != nil {
		if strings.Contains(err.Error(), "exceeds configured limit") {
			return ErrBlobTooLarge
		}
		return fmt.Errorf("read outgoing note: %w", err)
	}
	if int64(len(content)) > MaxBlobBytes {
		return ErrBlobTooLarge
	}
	actual := sha256.Sum256(content)
	if actual != expected {
		return errors.New("outgoing note changed during reconciliation")
	}
	if err := repository.EnsureRootedDirectory(root, ".remember/sync", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(root, ".remember/sync/outbox", 0o700); err != nil {
		return err
	}
	target := ".remember/sync/outbox/" + hex.EncodeToString(expected[:])
	if existing, err := repository.ReadRootedPrivate(root, target, MaxBlobBytes+1); err == nil {
		if !bytes.Equal(existing, content) {
			return errors.New("staged outgoing blob is corrupt")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := repository.CreateRootedPrivate(root, target, content); err != nil {
		if !os.IsExist(err) {
			return err
		}
		existing, readErr := repository.ReadRootedPrivate(root, target, MaxBlobBytes+1)
		if readErr != nil || !bytes.Equal(existing, content) {
			return errors.New("concurrent outgoing blob differs")
		}
	}
	return nil
}

// ReadStagedNote reopens and verifies immutable queued bytes immediately
// before an uploader uses them.
func ReadStagedNote(root string, expected [sha256.Size]byte) ([]byte, error) {
	if err := repository.EnsurePrivateStagingSupported(); err != nil {
		return nil, err
	}
	target := ".remember/sync/outbox/" + hex.EncodeToString(expected[:])
	content, err := repository.ReadRootedPrivate(root, target, MaxBlobBytes)
	if err != nil {
		return nil, fmt.Errorf("read staged outgoing blob: %w", err)
	}
	if sha256.Sum256(content) != expected {
		return nil, errors.New("staged outgoing blob hash mismatch")
	}
	return content, nil
}
