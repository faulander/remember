package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/faulander/remember/client/internal/clientsync"
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

func TestRunCapturesDurableCreateAndCompoundMoveUpdateIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID := uuid.New()
	folderID := uuid.MustParse("018f4c3a-2222-7abc-8123-123456789abc")
	writeTestFile(t, root, "Folder/Note.md", noteWithID(noteID.String()))
	index := openTestIndex(t, ctx, root)
	opFolder, _ := uuid.NewV7()
	opNote, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID), NewOperationID: sequenceGenerator(t, opFolder, opNote)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != folderID {
		t.Fatalf("initial parent pending=%#v err=%v", pending, err)
	}
	if _, err := Run(ctx, root, index, Options{NewOperationID: func() (uuid.UUID, error) { t.Fatal("unchanged reconcile generated operation"); return uuid.Nil, nil }}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, pending[0].Mutation.OperationID, clientsync.Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != noteID || pending[0].Mutation.DependencyOperationID == nil {
		t.Fatalf("initial child pending=%#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, pending[0].Mutation.OperationID, clientsync.Result{Accepted: true, Revision: 1, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Folder", "Note.md"), filepath.Join(root, "Moved.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Moved.md"), []byte("---\nremember:\n  schema: 1\n  note_id: \""+noteID.String()+"\"\n---\nChanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moveOp, _ := uuid.NewV7()
	updateOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, moveOp, updateOp)}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.Kind != clientsync.Move || pending[0].Mutation.BaseRevision != 1 {
		t.Fatalf("compound move pending=%#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, moveOp, clientsync.Result{Accepted: true, Revision: 2, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.Kind != clientsync.Update || pending[0].Mutation.BaseRevision != 2 || pending[0].Mutation.DependencyOperationID == nil || *pending[0].Mutation.DependencyOperationID != moveOp {
		t.Fatalf("compound update pending=%#v err=%v", pending, err)
	}
}

func TestRunReappearedPendingNoteContinuesAfterCreateInsteadOfDuplicatingIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID := uuid.New()
	writeTestFile(t, root, "N.md", noteWithID(noteID.String()))
	index := openTestIndex(t, ctx, root)
	createOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, createOp)}); err != nil {
		t.Fatal(err)
	}
	if err := index.ReplaceSnapshot(ctx, localindex.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	moveOp, updateOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, moveOp, updateOp)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != createOp {
		t.Fatalf("reappeared object duplicated or bypassed create: %#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, createOp, clientsync.Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != moveOp || pending[0].Mutation.DependencyOperationID == nil || *pending[0].Mutation.DependencyOperationID != createOp {
		t.Fatalf("reappeared move chain=%#v err=%v", pending, err)
	}
}

func TestRunChildCreatedLaterWaitsForPendingParentCreate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := openTestIndex(t, ctx, root)
	folderID := uuid.MustParse("018f4c3a-4444-7abc-8123-123456789abc")
	folderOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID), NewOperationID: sequenceGenerator(t, folderOp)}); err != nil {
		t.Fatal(err)
	}
	noteID := uuid.New()
	writeTestFile(t, root, "Folder/Later.md", noteWithID(noteID.String()))
	noteOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, noteOp)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != folderOp {
		t.Fatalf("child bypassed pending parent: %#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, folderOp, clientsync.Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != noteOp || pending[0].Mutation.DependencyOperationID == nil || *pending[0].Mutation.DependencyOperationID != folderOp {
		t.Fatalf("child dependency was not persisted: %#v err=%v", pending, err)
	}
}

func TestRunMoveWaitsForPendingDestinationParent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID, keepID := uuid.New(), uuid.New()
	folderA := uuid.MustParse("018f4c3a-5555-7abc-8123-123456789abc")
	folderB := uuid.MustParse("018f4c3a-6666-7abc-8123-123456789abc")
	writeTestFile(t, root, "A/N.md", noteWithID(noteID.String()))
	writeTestFile(t, root, "A/Keep.md", noteWithID(keepID.String()))
	index := openTestIndex(t, ctx, root)
	createA, createKeep, createNote := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderA), NewOperationID: sequenceGenerator(t, createA, createKeep, createNote)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != createA {
		t.Fatalf("initial parent queue=%#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, createA, clientsync.Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("initial note queue=%#v err=%v", pending, err)
	}
	for offset, item := range pending {
		if err := store.RecordResult(ctx, item.Mutation.OperationID, clientsync.Result{Accepted: true, Revision: 1, Cursor: uint64(offset + 2)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	createB, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{TrustedNewFolders: []string{"B"}, NewID: sequenceGenerator(t, folderB), NewOperationID: sequenceGenerator(t, createB)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "A", "N.md"), filepath.Join(root, "B", "N.md")); err != nil {
		t.Fatal(err)
	}
	moveNote, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{MoveCandidates: []string{"A/N.md", "B/N.md"}, NewOperationID: sequenceGenerator(t, moveNote)}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundParent, foundMove := false, false
	for _, item := range pending {
		foundParent = foundParent || item.Mutation.OperationID == createB
		foundMove = foundMove || item.Mutation.OperationID == moveNote
	}
	if !foundParent || foundMove {
		t.Fatalf("move bypassed destination create: %#v", pending)
	}
	if err := store.RecordResult(ctx, createB, clientsync.Result{Accepted: true, Revision: 1, Cursor: 4}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundMove = false
	for _, item := range pending {
		foundMove = foundMove || item.Mutation.OperationID == moveNote
	}
	if !foundMove {
		t.Fatalf("move did not unlock after destination create: %#v", pending)
	}
}

func TestRunChainsChangeAfterAttemptedCreate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID := uuid.New()
	writeTestFile(t, root, "Note.md", noteWithID(noteID.String()))
	index := openTestIndex(t, ctx, root)
	createOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, createOp)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	if err := store.MarkAttempted(ctx, createOp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Note.md"), []byte("---\nremember:\n  schema: 1\n  note_id: \""+noteID.String()+"\"\n---\nChanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updateOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, updateOp)}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("dependent update became eligible before create result: %#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, createOp, clientsync.Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.Kind != clientsync.Update || pending[0].Mutation.BaseRevision != 1 || pending[0].Mutation.DependencyOperationID == nil || *pending[0].Mutation.DependencyOperationID != createOp {
		t.Fatalf("follow-up=%#v err=%v", pending, err)
	}
}

func TestRunCapturesDeletesChildBeforeParent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID := uuid.New()
	folderID := uuid.MustParse("018f4c3a-3333-7abc-8123-123456789abc")
	writeTestFile(t, root, "Folder/Note.md", noteWithID(noteID.String()))
	index := openTestIndex(t, ctx, root)
	createFolderOp, _ := uuid.NewV7()
	createNoteOp, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{AllowInitialFolderIDs: true, NewID: sequenceGenerator(t, folderID), NewOperationID: sequenceGenerator(t, createFolderOp, createNoteOp)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	for cursor := uint64(1); cursor <= 2; cursor++ {
		pending, err := store.ListPending(ctx, 10)
		if err != nil || len(pending) != 1 {
			t.Fatalf("initial dependency queue=%#v err=%v", pending, err)
		}
		if err := store.RecordResult(ctx, pending[0].Mutation.OperationID, clientsync.Result{Accepted: true, Revision: 1, Cursor: cursor}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, "Folder", "Note.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Folder")); err != nil {
		t.Fatal(err)
	}
	deleteNote, _ := uuid.NewV7()
	deleteFolder, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{NewOperationID: sequenceGenerator(t, deleteNote, deleteFolder)}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.Kind != clientsync.Delete || pending[0].Mutation.ObjectID != noteID {
		t.Fatalf("child delete=%#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, pending[0].Mutation.OperationID, clientsync.Result{Accepted: true, Revision: 2, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != folderID {
		t.Fatalf("parent delete=%#v err=%v", pending, err)
	}
}

func TestRunSuppressesOnlyExactAppliedRemoteNote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remoteID, localID := uuid.New(), uuid.New()
	remote := noteWithID(remoteID.String())
	local := append(noteWithID(localID.String()), []byte("local change\n")...)
	writeTestFile(t, root, "Remote.md", remote)
	writeTestFile(t, root, "Local.md", local)
	index := openTestIndex(t, ctx, root)
	remoteHash, localHash := sha256.Sum256(remote), sha256.Sum256(local)
	previous := localindex.Snapshot{Objects: []localindex.Object{
		{ID: remoteID, Type: localindex.ObjectNote, RelativePath: "Remote.md", CollisionPath: "remote.md", ContentHash: make([]byte, 32), IdentityState: localindex.IdentityKnown},
		{ID: localID, Type: localindex.ObjectNote, RelativePath: "Local.md", CollisionPath: "local.md", ContentHash: make([]byte, 32), IdentityState: localindex.IdentityKnown},
	}}
	if err := index.ReplaceSnapshot(ctx, previous); err != nil {
		t.Fatal(err)
	}
	op, _ := uuid.NewV7()
	if _, err := Run(ctx, root, index, Options{AppliedRemoteNotes: map[uuid.UUID][32]byte{remoteID: remoteHash}, NewOperationID: sequenceGenerator(t, op)}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != localID || !bytes.Equal(pending[0].Mutation.BlobHash, localHash[:]) {
		t.Fatalf("selective capture=%#v err=%v", pending, err)
	}
}

func TestRunKeepsOversizedSyncIssueUntilContentChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	noteID := uuid.New()
	content := append([]byte(noteWithID(noteID.String())), bytes.Repeat([]byte("x"), int(clientsync.MaxBlobBytes))...)
	writeTestFile(t, root, "Large.md", content)
	index := openTestIndex(t, ctx, root)
	for attempt := 0; attempt < 2; attempt++ {
		report, err := Run(ctx, root, index, Options{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, issue := range report.Issues {
			if issue.Code == IssueCode("sync_blob_too_large") {
				found = true
			}
		}
		if !found {
			t.Fatalf("attempt %d lost oversized sync issue: %#v", attempt, report.Issues)
		}
	}
	store, _ := clientsync.NewStore(index)
	if pending, err := store.ListPending(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("oversized note queued=%#v err=%v", pending, err)
	}
}

func TestRunRejectsUnverifiedTrustedRemoteFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Remote"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := openTestIndex(t, ctx, root)
	if _, err := Run(ctx, root, index, Options{TrustedRemoteFolders: map[string]uuid.UUID{"Remote": uuid.New()}}); err == nil {
		t.Fatal("path-only remote folder trust accepted")
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
