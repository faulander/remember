package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/faulander/remember/client/internal/repository"
)

func TestNoteLifecycleReconcilesImmediatelyAndPreservesIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	created, report, err := core.CreateNote(ctx, "Note.md", "# Note\n", []string{" Work ", "work", "Café"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID.Version() != 7 || len(created.Revision) != 64 || len(created.Tags) != 2 || report.AssignedNoteIDs != 0 {
		t.Fatalf("created note = %#v, report=%#v", created, report)
	}
	assertSnapshotPath(t, core, "Note.md", true)

	saved, _, err := core.SaveNote(ctx, "Note.md", created.Revision, "# Changed\n", []string{"personal"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != created.ID || saved.Revision == created.Revision || saved.Body != "# Changed\n" {
		t.Errorf("saved note = %#v", saved)
	}
	moved, _, err := core.MoveNote(ctx, "Note.md", "Folder/Renamed.md", saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != created.ID || moved.RelativePath != "Folder/Renamed.md" {
		t.Errorf("moved note = %#v", moved)
	}
	assertSnapshotPath(t, core, "Note.md", false)
	assertSnapshotPath(t, core, "Folder/Renamed.md", true)

	if _, err := core.DeleteNote(ctx, moved.RelativePath, moved.Revision); err != nil {
		t.Fatal(err)
	}
	assertSnapshotPath(t, core, moved.RelativePath, false)
	trashEntries, err := os.ReadDir(filepath.Join(root, ".remember", "trash"))
	if err != nil || len(trashEntries) != 1 {
		t.Fatalf("trash entries=%d err=%v", len(trashEntries), err)
	}
	trashed, err := os.ReadFile(filepath.Join(root, ".remember", "trash", trashEntries[0].Name()))
	if err != nil || !strings.Contains(string(trashed), created.ID.String()) {
		t.Error("recoverable trash did not retain note identity")
	}
}

func TestCreateFolderReconcilesAndDoesNotClobber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	if _, err := core.CreateFolder(ctx, "Parent/Child"); err != nil {
		t.Fatal(err)
	}
	assertSnapshotPath(t, core, "Parent/Child", true)
	info, err := os.Lstat(filepath.Join(root, "Parent", "Child"))
	if err != nil || !info.IsDir() {
		t.Fatalf("created folder info=%v err=%v", info, err)
	}
	if _, err := core.CreateFolder(ctx, "parent/child"); !errors.Is(err, ErrDestinationUsed) {
		t.Errorf("case-fold folder collision error = %v", err)
	}
	if _, err := core.CreateFolder(ctx, "../Escape"); !errors.Is(err, ErrInvalidFolderPath) {
		t.Errorf("folder traversal error = %v", err)
	}
}

func TestCreateFolderRemainsVisibleInRecoveryMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".remember", "index.db")); err != nil {
		t.Fatal(err)
	}
	core, _, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	if _, err := core.CreateFolder(ctx, "CreatedDuringRecovery"); err != nil {
		t.Fatal(err)
	}
	assertSnapshotPath(t, core, "CreatedDuringRecovery", true)
}

func TestNoteOptimisticConflictAndNoClobber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	first, _, err := core.CreateNote(ctx, "First.md", "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := core.CreateNote(ctx, "Second.md", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.CreateNote(ctx, "second.MD", "clobber", nil); !errors.Is(err, ErrDestinationUsed) {
		t.Errorf("case-fold create collision error = %v", err)
	}
	if _, _, err := core.MoveNote(ctx, "First.md", "Second.md", first.Revision); !errors.Is(err, ErrDestinationUsed) {
		t.Errorf("move clobber error = %v", err)
	}
	content, _ := os.ReadFile(filepath.Join(root, "Second.md"))
	if !strings.Contains(string(content), second.ID.String()) {
		t.Error("destination was clobbered")
	}
	if err := os.WriteFile(filepath.Join(root, "First.md"), []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.SaveNote(ctx, "First.md", first.Revision, "overwrite", nil); !errors.Is(err, repository.ErrConcurrentModification) {
		t.Errorf("save conflict error = %v", err)
	}
	if _, _, err := core.MoveNote(ctx, "First.md", "Moved.md", first.Revision); !errors.Is(err, repository.ErrConcurrentModification) {
		t.Errorf("move conflict error = %v", err)
	}
	if _, err := core.DeleteNote(ctx, "First.md", first.Revision); !errors.Is(err, repository.ErrConcurrentModification) {
		t.Errorf("delete conflict error = %v", err)
	}
	content, _ = os.ReadFile(filepath.Join(root, "First.md"))
	if string(content) != "external" {
		t.Error("external edit was overwritten")
	}
}

func TestNotePathsRejectTraversalAbsoluteAndSymlinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	for _, relative := range []string{"../escape.md", "/absolute.md", ".remember/secret.md", "bad\\path.md", "not.txt"} {
		if _, _, err := core.CreateNote(ctx, relative, "body", nil); !errors.Is(err, ErrInvalidNotePath) {
			t.Errorf("CreateNote(%q) error = %v", relative, err)
		}
	}
	safe, _, err := core.CreateNote(ctx, "SafeBeforeLinks.md", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.SaveNote(ctx, "../SafeBeforeLinks.md", safe.Revision, "changed", nil); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("save traversal error = %v", err)
	}
	if _, _, err := core.MoveNote(ctx, safe.RelativePath, "../Moved.md", safe.Revision); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("move traversal error = %v", err)
	}
	if _, err := core.DeleteNote(ctx, "/absolute.md", safe.Revision); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("delete absolute error = %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "Link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.CreateNote(ctx, "Link/Escape.md", "body", nil); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("symlink parent error = %v", err)
	}
	outsideNote := filepath.Join(outside, "Outside.md")
	if err := os.WriteFile(outsideNote, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideNote, filepath.Join(root, "Target.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := core.ReadNote(ctx, "Target.md"); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("symlink target error = %v", err)
	}
	created, _, err := core.CreateNote(ctx, "Safe.md", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".remember", "trash")); err != nil {
		t.Fatal(err)
	}
	if _, err := core.DeleteNote(ctx, created.RelativePath, created.Revision); !errors.Is(err, ErrInvalidNotePath) {
		t.Errorf("symlink trash error = %v", err)
	}
}

func TestCreateAndSaveRejectOversizedNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, _, err := core.CreateNote(ctx, "TooLarge.md", strings.Repeat("x", MaxNoteBytes+1), nil); !errors.Is(err, ErrNoteTooLarge) {
		t.Errorf("oversized create error = %v", err)
	}
	created, _, err := core.CreateNote(ctx, "Small.md", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.SaveNote(ctx, created.RelativePath, created.Revision, strings.Repeat("x", MaxNoteBytes+1), nil); !errors.Is(err, ErrNoteTooLarge) {
		t.Errorf("oversized save error = %v", err)
	}
}

func TestReadRejectsOversizedNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Large.md"), make([]byte, MaxNoteBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := core.ReadNote(ctx, "Large.md"); !errors.Is(err, ErrNoteTooLarge) {
		t.Errorf("oversized read error = %v", err)
	}
}

func assertSnapshotPath(t *testing.T, core *LocalCore, relative string, want bool) {
	t.Helper()
	snapshot, err := core.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, object := range snapshot.Objects {
		if object.RelativePath == relative {
			found = true
		}
	}
	if found != want {
		t.Errorf("snapshot path %q found=%t want=%t", relative, found, want)
	}
}
