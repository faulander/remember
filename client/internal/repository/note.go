package repository

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

// EnsureNoteIdentity inserts candidate into a note that has no Remember
// metadata. Existing identities remain byte-exact and malformed metadata is
// never overwritten. Callers that operate inside a managed root should use
// EnsureRootedNoteIdentity so every path component stays descriptor-anchored.
func EnsureNoteIdentity(path string, candidate uuid.UUID) (frontmatter.PatchResult, error) {
	if err := naming.ValidateComponent(filepath.Base(path)); err != nil {
		return frontmatter.PatchResult{}, fmt.Errorf("validate note name: %w", err)
	}
	if !bytes.EqualFold([]byte(filepath.Ext(path)), []byte(".md")) {
		return frontmatter.PatchResult{}, fmt.Errorf("note does not have .md extension")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return frontmatter.PatchResult{}, fmt.Errorf("read note: %w", err)
	}
	result, err := frontmatter.EnsureIdentity(original, candidate)
	if err != nil {
		return frontmatter.PatchResult{}, err
	}
	if !result.Changed {
		return result, nil
	}

	err = WriteAtomicExpected(path, original, result.Markdown, func(staged []byte) error {
		inspection, err := frontmatter.Inspect(staged)
		if err != nil {
			return err
		}
		if inspection.NoteID != result.NoteID {
			return fmt.Errorf("staged note identity is %s, want %s", inspection.NoteID, result.NoteID)
		}
		return nil
	})
	if err != nil {
		return frontmatter.PatchResult{}, err
	}
	return result, nil
}

// EnsureRootedNoteIdentity is the managed-root variant used by reconciliation.
// On Darwin and Linux, reads and replacement stay anchored beneath root even
// if another process swaps a parent directory for a symlink during the write.
func EnsureRootedNoteIdentity(root, relative string, candidate uuid.UUID) (frontmatter.PatchResult, error) {
	if err := naming.ValidateRelativePath(relative); err != nil {
		return frontmatter.PatchResult{}, fmt.Errorf("validate note path: %w", err)
	}
	if !bytes.EqualFold([]byte(filepath.Ext(relative)), []byte(".md")) {
		return frontmatter.PatchResult{}, fmt.Errorf("note does not have .md extension")
	}
	original, err := ReadRooted(root, relative, -1)
	if err != nil {
		return frontmatter.PatchResult{}, fmt.Errorf("read rooted note: %w", err)
	}
	result, err := frontmatter.EnsureIdentity(original, candidate)
	if err != nil {
		return frontmatter.PatchResult{}, err
	}
	if !result.Changed {
		return result, nil
	}
	if err := WriteRootedExpected(root, relative, original, result.Markdown, func(staged []byte) error {
		inspection, err := frontmatter.Inspect(staged)
		if err != nil {
			return err
		}
		if inspection.NoteID != result.NoteID {
			return fmt.Errorf("staged note identity is %s, want %s", inspection.NoteID, result.NoteID)
		}
		return nil
	}); err != nil {
		return frontmatter.PatchResult{}, err
	}
	return result, nil
}
