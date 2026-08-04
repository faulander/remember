package repository

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/google/uuid"
)

func TestWriteAtomicReplacesValidatedContentAndPreservesMode(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "Note.md")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := []byte("new content")
	if err := WriteAtomic(path, content, func(staged []byte) error {
		if !bytes.Equal(staged, content) {
			t.Fatalf("validator received %q, want %q", staged, content)
		}
		return nil
	}); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("destination = %q, want %q", got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("destination mode = %o, want 600", info.Mode().Perm())
	}
	assertNoStagedFiles(t, directory)
}

func TestWriteAtomicExpectedRejectsChangedDestination(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "Note.md")
	expected := []byte("expected")
	external := []byte("external edit")
	if err := os.WriteFile(path, external, 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteAtomicExpected(path, expected, []byte("replacement"), nil)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("WriteAtomicExpected() error = %v, want concurrent modification", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, external) {
		t.Errorf("external edit changed to %q", got)
	}
	assertNoStagedFiles(t, directory)
}

func TestWriteAtomicValidationFailureLeavesOriginal(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "Note.md")
	original := []byte("original")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	validationErr := errors.New("invalid staged bytes")
	err := WriteAtomic(path, []byte("replacement"), func([]byte) error { return validationErr })
	if !errors.Is(err, validationErr) {
		t.Fatalf("WriteAtomic() error = %v, want validation error", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("destination changed to %q, want %q", got, original)
	}
	assertNoStagedFiles(t, directory)
}

func TestEnsureNoteIdentityIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "Note.md")
	original := []byte("# Note\n\nBody\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc")
	first, err := EnsureNoteIdentity(path, id)
	if err != nil {
		t.Fatalf("EnsureNoteIdentity() error = %v", err)
	}
	if !first.Changed {
		t.Error("first identity insertion was not marked changed")
	}
	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	other := uuid.MustParse("018f4c3a-5678-7abc-8123-123456789abc")
	second, err := EnsureNoteIdentity(path, other)
	if err != nil {
		t.Fatalf("second EnsureNoteIdentity() error = %v", err)
	}
	if second.Changed || second.NoteID != id {
		t.Errorf("second result = %#v, want unchanged original ID", second)
	}
	secondBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Error("idempotent call changed note bytes")
	}
	if !bytes.HasSuffix(secondBytes, original) {
		t.Error("identity insertion changed Markdown body")
	}
}

func TestEnsureNoteIdentityRefusesMalformedFrontmatter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "Note.md")
	original := []byte("---\nremember: broken\n---\nBody\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureNoteIdentity(path, uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc"))
	var validationErr *frontmatter.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("EnsureNoteIdentity() error = %v, want frontmatter validation error", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Error("malformed note was modified")
	}
}

func assertNoStagedFiles(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".remember-write-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("staged files remain: %q", matches)
	}
}
