package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

func TestRunInitializesNotesAndFoldersThenIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "Folder/Note.md", []byte("# Note\n"))
	index := openTestIndex(t, ctx, root)

	noteID := uuid.MustParse("018f4c3a-1111-7abc-8123-123456789abc")
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	generator := sequenceGenerator(t, noteID, folderID)
	report, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: generator})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.AssignedNoteIDs != 1 || report.Objects != 2 || len(report.Issues) != 0 {
		t.Errorf("report = %#v", report)
	}

	content, err := os.ReadFile(filepath.Join(root, "Folder", "Note.md"))
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := frontmatter.Inspect(content)
	if err != nil || inspection.NoteID != noteID {
		t.Fatalf("patched note inspection = %#v, error %v", inspection, err)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder := objectAt(t, snapshot, "Folder")
	note := objectAt(t, snapshot, "Folder/Note.md")
	if folder.ID != folderID || note.ID != noteID || note.ParentID != folderID {
		t.Errorf("indexed objects = %#v", snapshot.Objects)
	}

	before := append([]byte(nil), content...)
	_, err = Run(ctx, root, index, Options{NewID: func() (uuid.UUID, error) {
		return uuid.Nil, errors.New("id generator must not be called")
	}})
	if err != nil {
		t.Fatalf("idempotent Run() error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "Folder", "Note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("idempotent reconciliation changed note bytes")
	}
}

func TestRunPreservesNoteIdentityAcrossMove(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	writeTestFile(t, root, "Original.md", noteWithID(id))
	index := openTestIndex(t, ctx, root)
	if _, err := Run(ctx, root, index, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Original.md"), filepath.Join(root, "Moved.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, root, index, Options{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	object := objectAt(t, snapshot, "Moved.md")
	if object.ID.String() != id {
		t.Errorf("moved note ID = %s, want %s", object.ID, id)
	}
}

func TestRunInfersFolderMoveFromUniqueNoteDescendants(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "Old/Note.md", noteWithID("550e8400-e29b-41d4-a716-446655440000"))
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, root, index, Options{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moved := objectAt(t, snapshot, "New")
	if moved.ID != folderID || moved.IdentityState != localindex.IdentityKnown {
		t.Errorf("moved folder = %#v", moved)
	}
	assertNoObjectAt(t, snapshot, "Old")
}

func TestRunDoesNotReuseExactPathAfterRenameAndRecreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "Old/Note.md", noteWithID("550e8400-e29b-41d4-a716-446655440000"))
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Old"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, root, index, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var ambiguous int
	for _, issue := range report.Issues {
		if issue.Code == IssueAmbiguousFolderIdentity {
			ambiguous++
		}
	}
	if ambiguous != 2 {
		t.Fatalf("ambiguous issues = %#v, want Old and New", report.Issues)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := objectAt(t, snapshot, "Old")
	if pending.ID != folderID || pending.IdentityState != localindex.IdentityPending {
		t.Errorf("old identity was attached to recreated folder: %#v", pending)
	}
	assertNoObjectAt(t, snapshot, "New")
}

func TestRunDoesNotChooseBetweenCopiedFolderCandidates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	content := noteWithID("550e8400-e29b-41d4-a716-446655440000")
	writeTestFile(t, root, "Old/Note.md", content)
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "Old")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "CopyA/Note.md", content)
	writeTestFile(t, root, "CopyB/Note.md", content)
	report, err := Run(ctx, root, index, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var ambiguous int
	for _, issue := range report.Issues {
		if issue.Code == IssueAmbiguousFolderIdentity {
			ambiguous++
		}
	}
	if ambiguous != 2 {
		t.Fatalf("ambiguous issues = %#v, want both copies", report.Issues)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := objectAt(t, snapshot, "Old")
	if pending.ID != folderID || pending.IdentityState != localindex.IdentityPending {
		t.Errorf("prior identity not preserved pending: %#v", pending)
	}
	assertNoObjectAt(t, snapshot, "CopyA")
	assertNoObjectAt(t, snapshot, "CopyB")
}

func TestRunPreservesEmptyFolderMoveWithWatcherHint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Old"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, root, index, Options{MoveCandidates: []string{"Old"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moved := objectAt(t, snapshot, "New")
	if moved.ID != folderID || moved.IdentityState != localindex.IdentityKnown {
		t.Errorf("hinted empty folder move = %#v", moved)
	}
	assertNoObjectAt(t, snapshot, "Old")
}

func TestRunPreservesNestedFolderMoveByStructuralSignature(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "Old/Child/Note.md", noteWithID("550e8400-e29b-41d4-a716-446655440000"))
	index := openTestIndex(t, ctx, root)
	parentID := uuid.MustParse("018f4c3a-1111-7abc-8123-123456789abc")
	childID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, parentID, childID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, root, index, Options{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if objectAt(t, snapshot, "New").ID != parentID {
		t.Error("parent folder identity was not preserved")
	}
	if objectAt(t, snapshot, "New/Child").ID != childID {
		t.Error("child folder identity was not preserved")
	}
}

func TestRunKeepsAmbiguousEmptyFolderIdentityPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Old"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, root, index, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 1 || report.Issues[0].Code != IssueAmbiguousFolderIdentity {
		t.Fatalf("issues = %#v, want ambiguous folder identity", report.Issues)
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := objectAt(t, snapshot, "Old")
	if pending.ID != folderID || pending.IdentityState != localindex.IdentityPending {
		t.Errorf("pending folder = %#v", pending)
	}
	assertNoObjectAt(t, snapshot, "New")
}

func openTestIndex(t *testing.T, ctx context.Context, root string) *localindex.Index {
	t.Helper()
	index, err := localindex.Open(ctx, filepath.Join(root, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	return index
}

func sequenceGenerator(t *testing.T, ids ...uuid.UUID) func() (uuid.UUID, error) {
	t.Helper()
	next := 0
	return func() (uuid.UUID, error) {
		if next >= len(ids) {
			t.Fatalf("ID generator called more than %d times", len(ids))
		}
		id := ids[next]
		next++
		return id, nil
	}
}

func noteWithID(id string) []byte {
	return []byte("---\nremember:\n  schema: 1\n  note_id: \"" + id + "\"\n---\nBody\n")
}

func objectAt(t *testing.T, snapshot localindex.Snapshot, relative string) localindex.Object {
	t.Helper()
	for _, object := range snapshot.Objects {
		if object.RelativePath == relative {
			return object
		}
	}
	t.Fatalf("no indexed object at %q in %#v", relative, snapshot.Objects)
	return localindex.Object{}
}

func assertNoObjectAt(t *testing.T, snapshot localindex.Snapshot, relative string) {
	t.Helper()
	for _, object := range snapshot.Objects {
		if object.RelativePath == relative {
			t.Errorf("unexpected indexed object at %q: %#v", relative, object)
		}
	}
}
