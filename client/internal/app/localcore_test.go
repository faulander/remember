package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

func TestExecuteActiveApplyPlanCreateUpdateAndRejectUnsupported(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	createBytes, err := frontmatter.EnsureIdentity([]byte("# Remote\n"), object)
	if err != nil {
		t.Fatal(err)
	}
	createHash := sha256.Sum256(createBytes.Markdown)
	planID, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Remote.md", BlobHash: createHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(_ context.Context, hash [32]byte) ([]byte, error) {
		if hash == createHash {
			return createBytes.Markdown, nil
		}
		return nil, errors.New("unknown blob")
	})
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Remote.md")); err != nil || string(got) != string(createBytes.Markdown) {
		t.Fatalf("created=%q err=%v", got, err)
	}
	if pending, err := store.ListPending(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("remote apply echoed outbox=%#v err=%v", pending, err)
	}

	updateBytes := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + object.String() + "\"\n---\n# Updated\n")
	updateHash := sha256.Sum256(updateBytes)
	updatePlan, updateOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: updatePlan, FromCursor: 1, ThroughCursor: 2, Steps: []clientsync.Change{{Cursor: 2, OperationID: updateOp, ObjectID: object, Mutation: clientsync.Update, ObjectType: clientsync.Note, Revision: 2, Name: "Remote.md", BlobHash: updateHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	resolver = clientsync.BlobResolverFunc(func(_ context.Context, hash [32]byte) ([]byte, error) {
		if hash == updateHash {
			return updateBytes, nil
		}
		return nil, errors.New("unknown blob")
	})
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Remote.md")); err != nil || string(got) != string(updateBytes) {
		t.Fatalf("updated=%q err=%v", got, err)
	}

	unsupportedID, createFolderOp, moveFolderOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folderID := uuid.New()
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: unsupportedID, FromCursor: 2, ThroughCursor: 4, Steps: []clientsync.Change{
		{Cursor: 3, OperationID: createFolderOp, ObjectID: folderID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Folder"},
		{Cursor: 4, OperationID: moveFolderOp, ObjectID: folderID, Mutation: clientsync.Move, ObjectType: clientsync.Folder, Revision: 2, Name: "Moved"},
	}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "Remote.md"))
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); !errors.Is(err, ErrUnsupportedApplyPlan) {
		t.Fatalf("unsupported error=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "Remote.md"))
	if string(before) != string(after) {
		t.Fatal("unsupported plan mutated filesystem")
	}
}

func TestExecuteActiveApplyPlanCreatesNestedFoldersThenNote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	parentID, childID, noteID := uuid.New(), uuid.New(), uuid.New()
	note, _ := frontmatter.EnsureIdentity([]byte("nested\n"), noteID)
	hash := sha256.Sum256(note.Markdown)
	parentOp, childOp, noteOp, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 3, Steps: []clientsync.Change{
		{Cursor: 1, OperationID: parentOp, ObjectID: parentID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Parent"},
		{Cursor: 2, OperationID: childOp, ObjectID: childID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, ParentID: &parentID, Name: "Child"},
		{Cursor: 3, OperationID: noteOp, ObjectID: noteID, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, ParentID: &childID, Name: "N.md", BlobHash: hash[:]},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return note.Markdown, nil })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Parent", "Child", "N.md")); err != nil || string(got) != string(note.Markdown) {
		t.Fatalf("note=%q err=%v", got, err)
	}
	snapshot, err := core.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for id, relative := range map[uuid.UUID]string{parentID: "Parent", childID: "Parent/Child", noteID: "Parent/Child/N.md"} {
		found := false
		for _, object := range snapshot.Objects {
			found = found || object.ID == id && object.RelativePath == relative
		}
		if !found {
			t.Fatalf("missing %s at %s: %#v", id, relative, snapshot.Objects)
		}
	}
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == ".remember-apply-nonce" {
			t.Fatalf("marker remains at %s", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteActiveApplyPlanRejectsUnboundExistingFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	planID, op := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: op, ObjectID: uuid.New(), Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Existing"}}}); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return nil, errors.New("unexpected blob") })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("unbound existing folder accepted")
	}
}

func TestExecuteActiveApplyPlanFolderCreateLosesConcurrentTargetRace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	folderID, planID, op := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: op, ObjectID: folderID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Folder"}}}); err != nil {
		t.Fatal(err)
	}
	testHookBeforeFolderPublication = func() {
		testHookBeforeFolderPublication = nil
		if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookBeforeFolderPublication = nil }()
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return nil, errors.New("unexpected blob") })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("concurrent target accepted")
	}
	entries, err := os.ReadDir(filepath.Join(root, "Folder"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("concurrent target changed: %#v err=%v", entries, err)
	}
}

func TestExecuteActiveApplyPlanDoesNotBlessReplacementAfterReconcile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	folderID, planID, op := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: op, ObjectID: folderID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Folder"}}}); err != nil {
		t.Fatal(err)
	}
	testHookAfterFolderReconcile = func() {
		testHookAfterFolderReconcile = nil
		if err := os.Rename(filepath.Join(root, "Folder"), filepath.Join(root, "Published")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(filepath.Join(root, "Folder"), 0o755); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookAfterFolderReconcile = nil }()
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return nil, errors.New("unexpected blob") })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("replacement was blessed")
	}
	snapshot, err := core.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range snapshot.Objects {
		if object.ID == folderID {
			t.Fatalf("replacement retained remote identity: %#v", object)
		}
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
}

func TestExecuteActiveApplyPlanResumesPublishedFolderBeforeReconcile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	folderID, planID, op := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: op, ObjectID: folderID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Folder"}}}); err != nil {
		t.Fatal(err)
	}
	publication, err := core.prepareFolderPublication(ctx, store, planID, 0, folderID, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishRootedFolderPublication(root, publication.StageRelative, publication.TargetRelative, publication.Nonce, publication.Device, publication.Inode); err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return nil, errors.New("unexpected blob") })
	if err := reopened.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Folder", ".remember-apply-nonce")); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}
}

func TestOpenCleansCompletedFolderPublicationMarker(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	folderID, planID, op := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: op, ObjectID: folderID, Mutation: clientsync.Create, ObjectType: clientsync.Folder, Revision: 1, Name: "Folder"}}}); err != nil {
		t.Fatal(err)
	}
	publication, err := core.prepareFolderPublication(ctx, store, planID, 0, folderID, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishRootedFolderPublication(root, publication.StageRelative, "Folder", publication.Nonce, publication.Device, publication.Inode); err != nil {
		t.Fatal(err)
	}
	verify := func() error {
		return repository.VerifyRootedFolderPublication(root, "Folder", publication.Nonce, publication.Device, publication.Inode)
	}
	if _, err := reconcile.Run(ctx, root, core.index, reconcile.Options{TrustedRemoteFolders: map[string]uuid.UUID{"Folder": folderID}, VerifyTrustedRemoteFolders: verify}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFolderStepAppliedAndAuthorizeCleanup(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(filepath.Join(root, "Folder", ".remember-apply-nonce")); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}
}

func TestExecuteActiveApplyPlanMovesAndRecoverablyDeletesNote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	content, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
	hash := sha256.Sum256(content.Markdown)
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return content.Markdown, nil })
	createPlan, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: createPlan, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: hash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	movePlan, moveOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: movePlan, FromCursor: 1, ThroughCursor: 2, Steps: []clientsync.Change{{Cursor: 2, OperationID: moveOp, ObjectID: object, Mutation: clientsync.Move, ObjectType: clientsync.Note, Revision: 2, Name: "Moved.md", BlobHash: hash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Moved.md")); err != nil || string(got) != string(content.Markdown) {
		t.Fatalf("moved=%q err=%v", got, err)
	}
	deletePlan, deleteOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: deletePlan, FromCursor: 2, ThroughCursor: 3, Steps: []clientsync.Change{{Cursor: 3, OperationID: deleteOp, ObjectID: object, Mutation: clientsync.Delete, ObjectType: clientsync.Note, Revision: 3, Name: "Moved.md", BlobHash: hash[:], Deleted: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Moved.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted source=%v", err)
	}
	trash := filepath.Join(root, ".remember", "trash", object.String()+"-"+deleteOp.String()+".md")
	if got, err := os.ReadFile(trash); err != nil || string(got) != string(content.Markdown) {
		t.Fatalf("trash=%q err=%v", got, err)
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 3 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if ready, err := store.ListReady(ctx, 10); err != nil || len(ready) != 0 {
		t.Fatalf("outbox=%#v err=%v", ready, err)
	}
}

func TestExecuteActiveApplyPlanResumesDeleteAfterReconcileBeforeJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	content, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
	hash := sha256.Sum256(content.Markdown)
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return content.Markdown, nil })
	createPlan, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: createPlan, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: hash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	planID, deleteOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	change := clientsync.Change{Cursor: 2, OperationID: deleteOp, ObjectID: object, Mutation: clientsync.Delete, ObjectType: clientsync.Note, Revision: 2, Name: "N.md", BlobHash: hash[:], Deleted: true}
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 1, ThroughCursor: 2, Steps: []clientsync.Change{change}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureRootedDirectory(root, ".remember/trash", 0o700); err != nil {
		t.Fatal(err)
	}
	trash := ".remember/trash/" + object.String() + "-" + deleteOp.String() + ".md"
	if err := repository.MoveRootedExpected(root, "N.md", trash, content.Markdown); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, root, core.index, reconcile.Options{AppliedRemoteDeletes: map[uuid.UUID]bool{object: true}}); err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	reopenedStore, _ := clientsync.NewStore(reopened.index)
	if cursor, err := reopenedStore.ConfirmedCursor(ctx); err != nil || cursor != 2 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(trash))); err != nil || string(got) != string(content.Markdown) {
		t.Fatalf("trash=%q err=%v", got, err)
	}
}

func TestExecuteActiveApplyPlanRecoversStagedMoveAndDelete(t *testing.T) {
	ctx := context.Background()
	for _, mutation := range []clientsync.MutationKind{clientsync.Move, clientsync.Delete} {
		t.Run(string(mutation), func(t *testing.T) {
			root := t.TempDir()
			core, _, err := Initialize(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer core.Close()
			store, _ := clientsync.NewStore(core.index)
			object := uuid.New()
			content, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
			hash := sha256.Sum256(content.Markdown)
			resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return content.Markdown, nil })
			createPlan, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: createPlan, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: hash[:]}}}); err != nil {
				t.Fatal(err)
			}
			if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
				t.Fatal(err)
			}
			planID, op := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			name, deleted := "Moved.md", false
			if mutation == clientsync.Delete {
				name, deleted = "N.md", true
			}
			change := clientsync.Change{Cursor: 2, OperationID: op, ObjectID: object, Mutation: mutation, ObjectType: clientsync.Note, Revision: 2, Name: name, BlobHash: hash[:], Deleted: deleted}
			if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 1, ThroughCursor: 2, Steps: []clientsync.Change{change}}); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginApplyPlan(ctx, planID); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(root, "N.md"), filepath.Join(root, ".remember-move-recovery-crash")); err != nil {
				t.Fatal(err)
			}
			if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
				t.Fatal(err)
			}
			if mutation == clientsync.Move {
				if got, err := os.ReadFile(filepath.Join(root, "Moved.md")); err != nil || string(got) != string(content.Markdown) {
					t.Fatalf("moved=%q err=%v", got, err)
				}
			} else {
				trash := filepath.Join(root, ".remember", "trash", object.String()+"-"+op.String()+".md")
				if got, err := os.ReadFile(trash); err != nil || string(got) != string(content.Markdown) {
					t.Fatalf("trash=%q err=%v", got, err)
				}
			}
		})
	}
}

func TestExecuteActiveApplyPlanReusesPathVacatedEarlierInPlan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	firstID, secondID := uuid.New(), uuid.New()
	first, _ := frontmatter.EnsureIdentity([]byte("first\n"), firstID)
	second, _ := frontmatter.EnsureIdentity([]byte("second\n"), secondID)
	firstHash, secondHash := sha256.Sum256(first.Markdown), sha256.Sum256(second.Markdown)
	resolver := clientsync.BlobResolverFunc(func(_ context.Context, hash [32]byte) ([]byte, error) {
		if hash == firstHash {
			return first.Markdown, nil
		}
		if hash == secondHash {
			return second.Markdown, nil
		}
		return nil, errors.New("unknown blob")
	})
	createPlan, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: createPlan, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: firstID, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Note.md", BlobHash: firstHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	planID, deleteOp, secondOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := clientsync.ApplyPlan{ID: planID, FromCursor: 1, ThroughCursor: 3, Steps: []clientsync.Change{
		{Cursor: 2, OperationID: deleteOp, ObjectID: firstID, Mutation: clientsync.Delete, ObjectType: clientsync.Note, Revision: 2, Name: "Note.md", BlobHash: firstHash[:], Deleted: true},
		{Cursor: 3, OperationID: secondOp, ObjectID: secondID, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "note.md", BlobHash: secondHash[:]},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		found = found || entry.Name() == "note.md"
	}
	if !found {
		t.Fatalf("vacated path was not reused: %#v", entries)
	}
}

func TestExecuteActiveApplyPlanResumesPublishedLaterMove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	content, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
	hash := sha256.Sum256(content.Markdown)
	planID, createOp, moveOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 2, Steps: []clientsync.Change{
		{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: hash[:]},
		{Cursor: 2, OperationID: moveOp, ObjectID: object, Mutation: clientsync.Move, ObjectType: clientsync.Note, Revision: 2, Name: "Moved.md", BlobHash: hash[:]},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Moved.md"), content.Markdown, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, root, core.index, reconcile.Options{AppliedRemoteNotes: map[uuid.UUID][32]byte{object: hash}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return content.Markdown, nil })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 2 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
}

func TestExecuteActiveApplyPlanRejectsBlobMismatchBeforeFilesystemMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	expected := sha256.Sum256([]byte("expected"))
	planID, operationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: operationID, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Mismatch.md", BlobHash: expected[:]}}}); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return []byte("different"), nil })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("blob mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "Mismatch.md")); !os.IsNotExist(err) {
		t.Fatalf("mismatch mutated filesystem: %v", err)
	}
	active, err := store.ActiveApplyPlan(ctx)
	if err != nil || active == nil || active.Status != "prepared" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestExecuteActiveApplyPlanResumesCreatePublishedBeforeJournalMark(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	patched, err := frontmatter.EnsureIdentity([]byte("resume\n"), object)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(patched.Markdown)
	planID, operationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: operationID, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Resume.md", BlobHash: hash[:]}}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after durable filesystem publication but before reconcile
	// and the apply-step journal transition.
	if err := os.WriteFile(filepath.Join(root, "Resume.md"), patched.Markdown, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return patched.Markdown, nil })
	if err := reopened.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	reopenedStore, _ := clientsync.NewStore(reopened.index)
	if cursor, err := reopenedStore.ConfirmedCursor(ctx); err != nil || cursor != 1 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if active, err := reopenedStore.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestExecuteActiveApplyPlanPreservesOfflineEditAfterBeginCrash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	initial, _ := frontmatter.EnsureIdentity([]byte("initial\n"), object)
	initialHash := sha256.Sum256(initial.Markdown)
	createPlan, createOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: createPlan, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: createOp, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: initialHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return initial.Markdown, nil })); err != nil {
		t.Fatal(err)
	}
	remote := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + object.String() + "\"\n---\nremote\n")
	remoteHash := sha256.Sum256(remote)
	updatePlan, updateOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: updatePlan, FromCursor: 1, ThroughCursor: 2, Steps: []clientsync.Change{{Cursor: 2, OperationID: updateOp, ObjectID: object, Mutation: clientsync.Update, ObjectType: clientsync.Note, Revision: 2, Name: "N.md", BlobHash: remoteHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, updatePlan); err != nil {
		t.Fatal(err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	local := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + object.String() + "\"\n---\nlocal offline\n")
	if err := os.WriteFile(filepath.Join(root, "N.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, _, err := reopened.CreateNote(ctx, "Blocked.md", "blocked", nil); !errors.Is(err, ErrApplyPlanActive) {
		t.Fatalf("mutation during active plan error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Blocked.md")); !os.IsNotExist(err) {
		t.Fatalf("blocked mutation changed filesystem: %v", err)
	}
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return remote, nil })
	if err := reopened.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("offline edit was overwritten")
	}
	if got, err := os.ReadFile(filepath.Join(root, "N.md")); err != nil || string(got) != string(local) {
		t.Fatalf("local bytes=%q err=%v", got, err)
	}
	if _, err := reopened.Reconcile(ctx); !errors.Is(err, ErrApplyPlanActive) {
		t.Fatalf("reconcile during active plan error=%v", err)
	}
	reopenedStore, _ := clientsync.NewStore(reopened.index)
	if active, err := reopenedStore.ActiveApplyPlan(ctx); err != nil || active == nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestExecuteActiveApplyPlanRejectsIdentitylessBlobAndPortableCollision(t *testing.T) {
	for _, test := range []struct {
		name       string
		localName  string
		remoteName string
		identity   bool
	}{
		{name: "identityless", remoteName: "Remote.md"},
		{name: "portable collision", localName: "note.md", remoteName: "Note.md", identity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if test.localName != "" {
				if err := os.WriteFile(filepath.Join(root, test.localName), []byte("local\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			core, _, err := Initialize(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer core.Close()
			store, _ := clientsync.NewStore(core.index)
			object := uuid.New()
			blob := []byte("identityless\n")
			if test.identity {
				patched, patchErr := frontmatter.EnsureIdentity([]byte("remote\n"), object)
				if patchErr != nil {
					t.Fatal(patchErr)
				}
				blob = patched.Markdown
			}
			hash := sha256.Sum256(blob)
			planID, operationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: operationID, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: test.remoteName, BlobHash: hash[:]}}}); err != nil {
				t.Fatal(err)
			}
			resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return blob, nil })
			if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
				t.Fatal("unsafe remote blob accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == test.remoteName {
					t.Fatal("remote create mutated disk")
				}
			}
		})
	}
}

func TestExecuteActiveApplyPlanResumesLaterPublishedStepForSameObject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	created, _ := frontmatter.EnsureIdentity([]byte("created\n"), object)
	updated := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + object.String() + "\"\n---\nupdated\n")
	createHash, updateHash := sha256.Sum256(created.Markdown), sha256.Sum256(updated)
	planID, op1, op2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 2, Steps: []clientsync.Change{
		{Cursor: 1, OperationID: op1, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "N.md", BlobHash: createHash[:]},
		{Cursor: 2, OperationID: op2, ObjectID: object, Mutation: clientsync.Update, ObjectType: clientsync.Note, Revision: 2, Name: "N.md", BlobHash: updateHash[:]},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "N.md"), updated, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, root, core.index, reconcile.Options{AppliedRemoteNotes: map[uuid.UUID][32]byte{object: updateHash}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(_ context.Context, hash [32]byte) ([]byte, error) {
		if hash == createHash {
			return created.Markdown, nil
		}
		if hash == updateHash {
			return updated, nil
		}
		return nil, errors.New("unknown blob")
	})
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err != nil {
		t.Fatal(err)
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 2 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "N.md")); err != nil || string(got) != string(updated) {
		t.Fatalf("content=%q err=%v", got, err)
	}
}

func TestExecuteActiveApplyPlanRejectsInternalCollisionBeforeWrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	firstID, secondID := uuid.New(), uuid.New()
	first, _ := frontmatter.EnsureIdentity([]byte("first\n"), firstID)
	second, _ := frontmatter.EnsureIdentity([]byte("second\n"), secondID)
	firstHash, secondHash := sha256.Sum256(first.Markdown), sha256.Sum256(second.Markdown)
	planID, op1, op2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 2, Steps: []clientsync.Change{
		{Cursor: 1, OperationID: op1, ObjectID: firstID, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "note.md", BlobHash: firstHash[:]},
		{Cursor: 2, OperationID: op2, ObjectID: secondID, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Note.md", BlobHash: secondHash[:]},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	resolver := clientsync.BlobResolverFunc(func(_ context.Context, hash [32]byte) ([]byte, error) {
		if hash == firstHash {
			return first.Markdown, nil
		}
		if hash == secondHash {
			return second.Markdown, nil
		}
		return nil, errors.New("unknown blob")
	})
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("internal path collision accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "note.md" || entry.Name() == "Note.md" {
			t.Fatalf("partial plan publication: %s", entry.Name())
		}
	}
}

func TestExecuteActiveApplyPlanDetectsEditDuringSuppressedReconcileGap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ := clientsync.NewStore(core.index)
	object := uuid.New()
	remote, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
	hash := sha256.Sum256(remote.Markdown)
	planID, operationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []clientsync.Change{{Cursor: 1, OperationID: operationID, ObjectID: object, Mutation: clientsync.Create, ObjectType: clientsync.Note, Revision: 1, Name: "Race.md", BlobHash: hash[:]}}}); err != nil {
		t.Fatal(err)
	}
	local := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + object.String() + "\"\n---\nlocal race\n")
	testHookAfterApplyPublication = func() {
		testHookAfterApplyPublication = nil
		if err := os.WriteFile(filepath.Join(root, "Race.md"), local, 0o644); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookAfterApplyPublication = nil }()
	resolver := clientsync.BlobResolverFunc(func(context.Context, [32]byte) ([]byte, error) { return remote.Markdown, nil })
	if err := core.ExecuteActiveApplyPlan(ctx, resolver); err == nil {
		t.Fatal("concurrent local edit was absorbed")
	}
	if got, err := os.ReadFile(filepath.Join(root, "Race.md")); err != nil || string(got) != string(local) {
		t.Fatalf("local bytes=%q err=%v", got, err)
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active == nil || active.Status != "applying" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	if pending, err := store.ListPending(ctx, 10); err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != object {
		t.Fatalf("concurrent local intent=%#v err=%v", pending, err)
	}
}

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
	store, err := clientsync.NewStore(recovered.index)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("recovery enqueued guessed sync intents: %#v err=%v", pending, err)
	}
	if err := recovered.PrepareSyncBootstrap(ctx); err == nil {
		t.Fatal("recovery mode allowed sync bootstrap")
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
