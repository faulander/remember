package clientsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

type recursiveRecoveryFixture struct {
	rootID, folderAID, folderBID, siblingID, noteID, siblingNoteID uuid.UUID
	rootOp, folderAOp, folderBOp, siblingOp, noteOp, siblingNoteOp uuid.UUID
}

func enqueueRecursiveRecoveryFixture(t *testing.T, store *Store) recursiveRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	f := recursiveRecoveryFixture{
		rootID: uuid.New(), folderAID: uuid.New(), folderBID: uuid.New(), siblingID: uuid.New(), noteID: uuid.New(), siblingNoteID: uuid.New(),
		rootOp: uuid.Must(uuid.NewV7()), folderAOp: uuid.Must(uuid.NewV7()), folderBOp: uuid.Must(uuid.NewV7()), siblingOp: uuid.Must(uuid.NewV7()), noteOp: uuid.Must(uuid.NewV7()), siblingNoteOp: uuid.Must(uuid.NewV7()),
	}
	one, two := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))
	mutations := []Mutation{
		{OperationID: f.rootOp, Kind: Create, ObjectID: f.rootID, ObjectType: Folder, Name: "Root"},
		{OperationID: f.folderAOp, Kind: Create, ObjectID: f.folderAID, ObjectType: Folder, ParentID: &f.rootID, Name: "A", DependencyOperationID: &f.rootOp},
		{OperationID: f.folderBOp, Kind: Create, ObjectID: f.folderBID, ObjectType: Folder, ParentID: &f.folderAID, Name: "B", DependencyOperationID: &f.folderAOp},
		{OperationID: f.noteOp, Kind: Create, ObjectID: f.noteID, ObjectType: Note, ParentID: &f.folderBID, Name: "Note.md", BlobHash: one[:], DependencyOperationID: &f.folderBOp},
		{OperationID: f.siblingOp, Kind: Create, ObjectID: f.siblingID, ObjectType: Folder, ParentID: &f.rootID, Name: "Sibling", DependencyOperationID: &f.rootOp},
		{OperationID: f.siblingNoteOp, Kind: Create, ObjectID: f.siblingNoteID, ObjectType: Note, ParentID: &f.siblingID, Name: "Other.md", BlobHash: two[:], DependencyOperationID: &f.siblingOp},
	}
	if err := store.Enqueue(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRecursiveLocalFolderManifestPersistsStableReplacementDAGAcrossReopen(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "index.db")
	index, err := localindex.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(index)
	if err != nil {
		t.Fatal(err)
	}
	fixture := enqueueRecursiveRecoveryFixture(t, store)
	if err := store.MarkAttempted(ctx, fixture.rootOp); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, fixture.rootOp, Result{Conflict: "path_collision"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.DiscoverRecursiveLocalFolderManifest(ctx, fixture.rootOp, fixture.rootID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest == nil || len(manifest.Members) != 5 {
		t.Fatalf("manifest=%#v", manifest)
	}
	for i := range manifest.Members {
		if manifest.Members[i].ObjectType == Folder {
			manifest.Members[i].Device = uint64(100 + i)
			manifest.Members[i].Inode = uint64(200 + i)
		}
	}
	recoveredID := uuid.New()
	target := ConflictRootName + "/" + ConflictRecoveredName + "/" + ConflictFolderName("Root", fixture.rootOp)
	recovery := ConflictFolderCreateRecovery{OperationID: fixture.rootOp, SourceFolderID: fixture.rootID, RecoveredFolderID: recoveredID, SourceRelative: "Root", TargetRelative: target, Device: 11, Inode: 22, State: "prepared"}
	if err := store.PutConflictFolderCreateRecoveryWithRecursiveManifest(ctx, recovery, *manifest); err != nil {
		t.Fatal(err)
	}
	oldRootReplacement := manifest.NewRootOperationID
	oldReplacements := make(map[uuid.UUID]uuid.UUID, len(manifest.Members))
	for _, member := range manifest.Members {
		oldReplacements[member.ObjectID] = member.NewOperationID
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = localindex.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, err = NewStore(index)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.ConflictRecursiveLocalFolderManifest(ctx, RecursiveFolderCreateRecovery, fixture.rootOp)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil || reloaded.NewRootOperationID != oldRootReplacement || len(reloaded.Members) != len(manifest.Members) {
		t.Fatalf("reloaded=%#v", reloaded)
	}
	for _, member := range reloaded.Members {
		if oldReplacements[member.ObjectID] != member.NewOperationID {
			t.Fatalf("replacement operation changed for %s", member.RelativePath)
		}
	}
	if err := store.MarkConflictFolderCreateMoved(ctx, fixture.rootOp); err != nil {
		t.Fatal(err)
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("moved recursive recovery unresolved=%t err=%v", unresolved, err)
	}
}

func TestRecursiveLocalFolderManifestRejectsAttemptedDescendantAndBaseline(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(context.Context, *localindex.Index, *Store, recursiveRecoveryFixture) error
	}{
		{name: "attempted", mutate: func(ctx context.Context, index *localindex.Index, _ *Store, fixture recursiveRecoveryFixture) error {
			return index.WithTransaction(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='attempted',attempted_at_ms=1 WHERE operation_id=?`, fixture.folderBOp.String())
				return err
			})
		}},
		{name: "baseline", mutate: func(ctx context.Context, index *localindex.Index, _ *Store, fixture recursiveRecoveryFixture) error {
			return index.WithTransaction(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO sync_baselines(object_id,revision) VALUES(?,1)`, fixture.noteID.String())
				return err
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer index.Close()
			store, err := NewStore(index)
			if err != nil {
				t.Fatal(err)
			}
			fixture := enqueueRecursiveRecoveryFixture(t, store)
			err = test.mutate(ctx, index, store, fixture)
			if err != nil {
				t.Fatal(err)
			}
			if manifest, err := store.DiscoverRecursiveLocalFolderManifest(ctx, fixture.rootOp, fixture.rootID); err == nil || manifest != nil {
				t.Fatalf("manifest=%#v err=%v", manifest, err)
			}
		})
	}
}
