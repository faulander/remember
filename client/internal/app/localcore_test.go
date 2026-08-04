package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
)

func TestInitializeOpenAndReconcileExternalChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Note.md"), []byte("# Note\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	core, report, err := Initialize(ctx, root)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if report.AssignedNoteIDs != 1 || report.Objects != 2 {
		t.Errorf("initial report = %#v", report)
	}
	content, err := os.ReadFile(filepath.Join(root, "Note.md"))
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := frontmatter.Inspect(content)
	if err != nil || !inspection.HasRemember {
		t.Fatalf("initialized note inspection = %#v, error %v", inspection, err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, report, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer reopened.Close()
	if report.AssignedNoteIDs != 0 || report.Objects != 2 {
		t.Errorf("open report = %#v", report)
	}

	if err := os.Rename(filepath.Join(root, "Note.md"), filepath.Join(root, "Moved.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moved := findObject(snapshot, "Moved.md")
	if moved == nil || moved.ID != inspection.NoteID {
		t.Errorf("moved note not preserved: %#v", snapshot.Objects)
	}
}

func TestStartWatchingConvergesAfterExternalCreate(t *testing.T) {
	root := t.TempDir()
	core, _, err := Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := core.StartWatching(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Consume the mandatory startup reconciliation.
	select {
	case update := <-updates:
		if update.Err != nil {
			t.Fatal(update.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup reconciliation timed out")
	}

	if err := os.WriteFile(filepath.Join(root, "External.md"), []byte("Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatal("updates closed before external note converged")
			}
			if update.Err != nil {
				t.Fatal(update.Err)
			}
			snapshot, err := core.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if object := findObject(snapshot, "External.md"); object != nil {
				if object.Type != localindex.ObjectNote {
					t.Errorf("external object type = %q", object.Type)
				}
				return
			}
		case <-deadline:
			t.Fatal("external note did not converge")
		}
	}
}

func TestWatcherPreservesEmptyFolderIdentityOnRename(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Old"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	before, err := core.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := findObject(before, "Old")
	if old == nil {
		t.Fatal("Old folder not indexed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := core.StartWatching(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err != nil {
			t.Fatal(update.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup reconciliation timed out")
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatal("updates closed before folder move converged")
			}
			if update.Err != nil {
				t.Fatal(update.Err)
			}
			snapshot, err := core.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if moved := findObject(snapshot, "New"); moved != nil {
				if moved.ID != old.ID {
					t.Fatalf("moved folder ID = %s, want %s", moved.ID, old.ID)
				}
				return
			}
		case <-deadline:
			t.Fatal("empty folder rename did not preserve identity")
		}
	}
}

func TestOpenReconstructsNotesAndReportsFoldersAfterIndexLoss(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Folder", "Note.md"), []byte("Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "Folder", "Note.md"))
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := frontmatter.Inspect(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, ".remember", "index.db"+suffix)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	recovered, report, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("Open() after index loss error = %v", err)
	}
	var folderIssue bool
	for _, issue := range report.Issues {
		if issue.Code == "ambiguous_folder_identity" && issue.RelativePath == "Folder" {
			folderIssue = true
		}
	}
	if !folderIssue {
		t.Errorf("recovery issues = %#v, want folder identity issue", report.Issues)
	}
	snapshot, err := recovered.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	note := findObject(snapshot, "Folder/Note.md")
	if note == nil || note.ID != inspection.NoteID {
		t.Errorf("note identity not reconstructed: %#v", snapshot.Objects)
	}
	if folder := findObject(snapshot, "Folder"); folder != nil {
		t.Errorf("folder identity was guessed during recovery: %#v", *folder)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, secondReport, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("second recovery Open() error = %v", err)
	}
	defer reopened.Close()
	folderIssue = false
	for _, issue := range secondReport.Issues {
		if issue.Code == "ambiguous_folder_identity" && issue.RelativePath == "Folder" {
			folderIssue = true
		}
	}
	if !folderIssue {
		t.Errorf("second recovery issues = %#v, recovery mode was not durable", secondReport.Issues)
	}
	secondSnapshot, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if folder := findObject(secondSnapshot, "Folder"); folder != nil {
		t.Errorf("folder identity was guessed after recovery reopen: %#v", *folder)
	}
}

func TestClosedCoreCannotReconcileOrWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	core, _, err := Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "AfterClose.md")
	original := []byte("Body\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Reconcile(context.Background()); !errors.Is(err, ErrCoreClosed) {
		t.Fatalf("Reconcile() error = %v, want closed", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Error("closed core modified a note")
	}
}

func TestInitializationGuards(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, _, err := Open(context.Background(), root); !errors.Is(err, ErrNotInitialized) {
		t.Errorf("Open() error = %v, want not initialized", err)
	}
	core, _, err := Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, _, err := Initialize(context.Background(), root); !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("second Initialize() error = %v, want already initialized", err)
	}
	if _, _, err := Open(context.Background(), root); !errors.Is(err, ErrRootInUse) {
		t.Errorf("concurrent Open() error = %v, want root in use", err)
	}
}

func findObject(snapshot localindex.Snapshot, relative string) *localindex.Object {
	for i := range snapshot.Objects {
		if snapshot.Objects[i].RelativePath == relative {
			return &snapshot.Objects[i]
		}
	}
	return nil
}
