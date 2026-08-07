//go:build darwin || linux

package repository

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFolderPublicationBindsNonceInodeAndCleansMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".remember"), 0o700); err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	copy(nonce[:], []byte("01234567890123456789012345678901"))
	stage := ".remember/apply/folders/plan/0"
	device, inode, err := CreateRootedFolderPublication(root, stage, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRootedFolderPublication(root, stage, nonce, device, inode); err != nil {
		t.Fatal(err)
	}
	wrong := nonce
	wrong[0] ^= 1
	if err := VerifyRootedFolderPublication(root, stage, wrong, device, inode); err == nil {
		t.Fatal("wrong nonce accepted")
	}
	if err := PublishRootedFolderPublication(root, stage, "Folder", nonce, device, inode); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRootedFolderPublication(root, "Folder", nonce, device, inode+1); err == nil {
		t.Fatal("wrong inode accepted")
	}
	if err := CleanupRootedFolderPublication(root, "Folder", nonce, device, inode); err != nil {
		t.Fatal(err)
	}
	if err := CleanupRootedFolderPublication(root, "Folder", nonce, device, inode); err != nil {
		t.Fatal("cleanup replay:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Folder", folderNonceMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}
}

func TestMoveRootedFolderExpectedBindsInodeAndResumes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "A", "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "A", "Folder", "N.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	device, inode, err := RootedFolderIdentity(root, "A/Folder")
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveRootedFolderExpected(root, "A/Folder", "B/Moved", device, inode); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRootedFolderIdentity(root, "B/Moved", device, inode); err != nil {
		t.Fatal(err)
	}
	if err := MoveRootedFolderExpected(root, "A/Folder", "B/Moved", device, inode); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "A", "Folder")); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
}

func TestMoveRootedFolderExpectedEmptyDeleteRejectsContent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Folder", "local.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".remember", "trash", "folders"), 0o700); err != nil {
		t.Fatal(err)
	}
	device, inode, err := RootedFolderIdentity(root, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteRootedFolderExpected(root, "Folder", device, inode); err == nil {
		t.Fatal("non-empty folder deleted")
	}
	if err := VerifyRootedFolderIdentity(root, "Folder", device, inode); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRootedFolderExpectedRejectsConcurrentContent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	device, inode, err := RootedFolderIdentity(root, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	testHookBeforeRootedFolderDelete = func() {
		testHookBeforeRootedFolderDelete = nil
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), folderMoveRecoveryPrefix) {
				_ = os.WriteFile(filepath.Join(root, entry.Name(), "late.txt"), []byte("keep"), 0o644)
			}
		}
	}
	defer func() { testHookBeforeRootedFolderDelete = nil }()
	if err := DeleteRootedFolderExpected(root, "Folder", device, inode); err == nil {
		t.Fatal("concurrent folder content was deleted")
	}
	if got, err := os.ReadFile(filepath.Join(root, "Folder", "late.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("late content=%q err=%v", got, err)
	}
}

func TestMoveRootedExpectedRecoversUniqueStagedSource(t *testing.T) {
	root := t.TempDir()
	content := []byte("staged exact")
	staged := filepath.Join(root, ".remember-move-recovery-test")
	if err := os.WriteFile(staged, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MoveRootedExpected(root, "missing.md", "moved.md", content); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "moved.md")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("moved=%q err=%v", got, err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged source remains: %v", err)
	}
}

func TestRootedSavePreservesConcurrentReplacementAndDisplacedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Note.md")
	original := []byte("original")
	external := []byte("external replacement")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	testHookAfterRootedSaveStage = func() {
		if err := os.WriteFile(path, external, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterRootedSaveStage = nil }()
	err := WriteRootedExpected(root, "Note.md", original, []byte("editor save"), nil)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("error = %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, external) {
		t.Fatalf("replacement changed to %q", got)
	}
	recoveries, _ := filepath.Glob(filepath.Join(root, ".remember-save-recovery-*"))
	if len(recoveries) != 1 {
		t.Fatalf("recoveries = %q", recoveries)
	}
	displaced, _ := os.ReadFile(recoveries[0])
	if !bytes.Equal(displaced, original) {
		t.Errorf("displaced = %q", displaced)
	}
}

func TestRootedMoveNeverUnlinksRecreatedSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Source.md")
	original := []byte("original")
	external := []byte("new source")
	if err := os.WriteFile(source, original, 0o644); err != nil {
		t.Fatal(err)
	}
	testHookAfterRootedMoveStage = func() {
		if err := os.WriteFile(source, external, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterRootedMoveStage = nil }()
	if err := MoveRootedExpected(root, "Source.md", "Moved.md", original); err != nil {
		t.Fatal(err)
	}
	gotSource, _ := os.ReadFile(source)
	gotDestination, _ := os.ReadFile(filepath.Join(root, "Moved.md"))
	if !bytes.Equal(gotSource, external) || !bytes.Equal(gotDestination, original) {
		t.Fatalf("source=%q destination=%q", gotSource, gotDestination)
	}
}

func TestRootedSaveParentSwapCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	folder := filepath.Join(root, "Folder")
	held := filepath.Join(root, "Held")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "Note.md"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Note.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookAfterRootedSaveStage = func() {
		if err := os.Rename(folder, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, folder); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterRootedSaveStage = nil }()
	if err := WriteRootedExpected(root, "Folder/Note.md", []byte("original"), []byte("saved"), nil); err != nil {
		t.Fatal(err)
	}
	outsideBytes, _ := os.ReadFile(filepath.Join(outside, "Note.md"))
	heldBytes, _ := os.ReadFile(filepath.Join(held, "Note.md"))
	if string(outsideBytes) != "outside" || string(heldBytes) != "saved" {
		t.Fatalf("outside=%q held=%q", outsideBytes, heldBytes)
	}
}

func TestRootedIdentityAssignmentParentSwapCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	folder := filepath.Join(root, "Folder")
	held := filepath.Join(root, "Held")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "Note.md"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Note.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookAfterRootedSaveStage = func() {
		if err := os.Rename(folder, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, folder); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterRootedSaveStage = nil }()
	id := uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc")
	if _, err := EnsureRootedNoteIdentity(root, "Folder/Note.md", id); err != nil {
		t.Fatal(err)
	}
	outsideBytes, _ := os.ReadFile(filepath.Join(outside, "Note.md"))
	heldBytes, _ := os.ReadFile(filepath.Join(held, "Note.md"))
	if string(outsideBytes) != "outside" || !bytes.Contains(heldBytes, []byte(id.String())) {
		t.Fatalf("outside=%q held=%q", outsideBytes, heldBytes)
	}
}

func TestRootedTrashSwapCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	trash := filepath.Join(root, ".remember", "trash")
	held := filepath.Join(root, ".remember", "trash-held")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Note.md"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookAfterRootedMoveStage = func() {
		if err := os.Rename(trash, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, trash); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookAfterRootedMoveStage = nil }()
	if err := MoveRootedExpected(root, "Note.md", ".remember/trash/Note.md", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "Note.md")); !os.IsNotExist(err) {
		t.Fatalf("outside destination exists: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(held, "Note.md"))
	if err != nil || string(got) != "original" {
		t.Fatalf("held=%q err=%v", got, err)
	}
}
