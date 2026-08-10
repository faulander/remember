package clientsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

func TestOutboxUUIDv4CoalescingResultsAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	index, err := localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(index)
	store.clock = func() time.Time { return time.Unix(10, 0) }
	object := uuid.New()
	op1, _ := uuid.NewV7()
	op2, _ := uuid.NewV7()
	hash := sha256.Sum256([]byte("one"))
	first := Mutation{OperationID: op1, Kind: Create, ObjectID: object, ObjectType: Note, Name: "One.md", BlobHash: hash[:]}
	if err := store.Enqueue(ctx, []Mutation{first}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.OperationID = op2
	second.Name = "Two.md"
	if err := store.Enqueue(ctx, []Mutation{second}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != op2 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if err := store.MarkAttempted(ctx, op2); err != nil {
		t.Fatal(err)
	}
	op3, _ := uuid.NewV7()
	thirdHash := sha256.Sum256([]byte("three"))
	third := Mutation{OperationID: op3, Kind: Update, ObjectID: object, ObjectType: Note, BaseRevision: 1, BlobHash: thirdHash[:], DependencyOperationID: &op2}
	if err := store.Enqueue(ctx, []Mutation{third}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("dependent mutation became eligible before predecessor: %#v err=%v", pending, err)
	}
	if err := store.RecordResult(ctx, op2, Result{Accepted: true, Revision: 1, Cursor: 7}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.OperationID != op3 {
		t.Fatalf("accepted predecessor did not unlock dependent: %#v err=%v", pending, err)
	}
	rev, found, err := store.Baseline(ctx, object)
	if err != nil || !found || rev != 1 {
		t.Fatalf("baseline=%d/%t %v", rev, found, err)
	}
	if err := store.SetConfirmedCursor(ctx, 7); err != nil {
		t.Fatal(err)
	}
	index.Close()
	index, err = localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ = NewStore(index)
	cursor, err := store.ConfirmedCursor(ctx)
	if err != nil || cursor != 7 {
		t.Fatalf("cursor=%d %v", cursor, err)
	}
}

func TestEquivalentNoteMoveResolutionGuardsAndUnlocksDependency(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	setup := func(name, canonical string, parent, canonicalParent *uuid.UUID) (uuid.UUID, uuid.UUID, uuid.UUID) {
		object := uuid.New()
		create, _ := uuid.NewV7()
		hash := sha256.Sum256([]byte(name))
		if err := store.Enqueue(ctx, []Mutation{{OperationID: create, Kind: Create, ObjectID: object, ObjectType: Note, Name: "N.md", BlobHash: hash[:]}}); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkAttempted(ctx, create); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordResult(ctx, create, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
			t.Fatal(err)
		}
		move, _ := uuid.NewV7()
		if err := store.Enqueue(ctx, []Mutation{{OperationID: move, Kind: Move, ObjectID: object, ObjectType: Note, BaseRevision: 1, ParentID: parent, Name: name}}); err != nil {
			t.Fatal(err)
		}
		update, _ := uuid.NewV7()
		dependentHash := sha256.Sum256([]byte("dependent"))
		if err := store.Enqueue(ctx, []Mutation{{OperationID: update, Kind: Update, ObjectID: object, ObjectType: Note, BaseRevision: 2, BlobHash: dependentHash[:], DependencyOperationID: &move}}); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkAttempted(ctx, move); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordResult(ctx, move, Result{Conflict: "base_revision_mismatch", Canonical: &CanonicalState{ObjectType: Note, Revision: 2, ParentID: canonicalParent, Name: canonical, BlobHash: hash[:]}}); err != nil {
			t.Fatal(err)
		}
		return object, move, update
	}
	_, move, update := setup("Same.md", "Same.md", nil, nil)
	if err := store.ResolveEquivalentNoteMove(ctx, move); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ListReady(ctx, 10)
	if err != nil || len(ready) != 1 || ready[0].Mutation.OperationID != update {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	_, divergent, _ := setup("Local.md", "Remote.md", nil, nil)
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) VALUES(?,'note_move_equivalent',1)`, divergent.String())
		return err
	}); err == nil {
		t.Fatal("SQL guard accepted divergent note move")
	}
	parent, otherParent := uuid.New(), uuid.New()
	_, mismatchedParent, _ := setup("SameNested.md", "SameNested.md", &parent, &otherParent)
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) VALUES(?,'note_move_equivalent',1)`, mismatchedParent.String())
		return err
	}); err == nil {
		t.Fatal("SQL guard accepted mismatched-parent equivalent move")
	}
	_, nonroot, nonrootUpdate := setup("SameNested2.md", "SameNested2.md", &parent, &parent)
	if err := store.ResolveEquivalentNoteMove(ctx, nonroot); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ListReady(ctx, 10)
	found := false
	for _, item := range ready {
		found = found || item.Mutation.OperationID == nonrootUpdate
	}
	if err != nil || !found {
		t.Fatalf("non-root dependency not ready: %#v err=%v", ready, err)
	}
}

func TestAttemptedOperationRemainsReadyWithStableAttemptTimestamp(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	now := time.Unix(20, 0)
	store.clock = func() time.Time { return now }
	op, _ := uuid.NewV7()
	if err := store.Enqueue(ctx, []Mutation{{OperationID: op, Kind: Create, ObjectID: uuid.New(), ObjectType: Folder, Name: "F"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttempted(ctx, op); err != nil {
		t.Fatal(err)
	}
	store.clock = func() time.Time { return now.Add(time.Hour) }
	if err := store.MarkAttempted(ctx, op); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.ListPending(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	ready, err := store.ListReady(ctx, 10)
	if err != nil || len(ready) != 1 || ready[0].Mutation.OperationID != op || ready[0].Status != "attempted" {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	var attempted int64
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT attempted_at_ms FROM sync_outbox WHERE operation_id=?`, op.String()).Scan(&attempted)
	}); err != nil {
		t.Fatal(err)
	}
	if attempted != now.UnixMilli() {
		t.Fatalf("attempted_at=%d", attempted)
	}
}

func TestAcceptedFolderIntentIsExactAndImmutable(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	folder := uuid.New()
	createOp, moveOp := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.Enqueue(ctx, []Mutation{{OperationID: createOp, Kind: Create, ObjectID: folder, ObjectType: Folder, Name: "Folder"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttempted(ctx, createOp); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, createOp, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, []Mutation{{OperationID: moveOp, Kind: Move, ObjectID: folder, ObjectType: Folder, BaseRevision: 1, Name: "Moved", FolderSourceRelative: "Folder", FolderDevice: 11, FolderInode: 22}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttempted(ctx, moveOp); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, moveOp, Result{Accepted: true, Revision: 2, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	change := Change{OperationID: moveOp, ObjectID: folder, ObjectType: Folder, Mutation: Move, Revision: 2, Cursor: 2}
	intent, err := store.AcceptedFolderIntent(ctx, change)
	if err != nil || intent == nil || intent.SourceRelative != "Folder" || intent.Device != 11 || intent.Inode != 22 {
		t.Fatalf("intent=%#v err=%v", intent, err)
	}
	change.Cursor = 3
	if intent, err := store.AcceptedFolderIntent(ctx, change); err != nil || intent != nil {
		t.Fatalf("mismatched cursor intent=%#v err=%v", intent, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_outbox_folder_intents SET inode=23 WHERE operation_id=?`, moveOp.String())
		return err
	}); err == nil {
		t.Fatal("folder intent was mutable")
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM sync_outbox_folder_intents WHERE operation_id=?`, moveOp.String())
		return err
	}); err == nil {
		t.Fatal("folder intent was deletable")
	}
}

func TestConflictAndReplayMismatchPersistence(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	object := uuid.New()
	conflictOp, _ := uuid.NewV7()
	replayOp, _ := uuid.NewV7()
	for _, m := range []Mutation{{OperationID: conflictOp, Kind: Create, ObjectID: object, ObjectType: Folder, Name: "A"}, {OperationID: replayOp, Kind: Create, ObjectID: uuid.New(), ObjectType: Folder, Name: "B"}} {
		if err := store.Enqueue(ctx, []Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}
	canonical := &CanonicalState{ObjectType: Folder, Revision: 3, Name: "Canonical"}
	if err := store.RecordResult(ctx, conflictOp, Result{Conflict: "path_collision", Canonical: canonical}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CanonicalConflictState(ctx, conflictOp); err != nil || got == nil || got.ObjectType != Folder || got.Revision != 3 || got.Name != "Canonical" {
		t.Fatalf("canonical=%#v err=%v", got, err)
	}
	if err := store.RecordReplayMismatch(ctx, replayOp); err != nil {
		t.Fatal(err)
	}
	var statuses []string
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT status FROM sync_outbox ORDER BY sequence`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			statuses = append(statuses, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != "conflict" || statuses[1] != "replay_mismatch" {
		t.Fatalf("statuses=%v", statuses)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_outbox SET name='tampered' WHERE operation_id=?`, conflictOp.String())
		return err
	}); err == nil {
		t.Fatal("immutable outbox payload was mutated")
	}
}

func TestConflictMaterializationTransitionsUnblockIntent(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	createOp := uuid.Must(uuid.NewV7())
	object := uuid.New()
	hash := sha256.Sum256([]byte("x"))
	if err := store.Enqueue(ctx, []Mutation{{OperationID: createOp, Kind: Create, ObjectID: object, ObjectType: Note, Name: "N.md", BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, createOp, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	op := uuid.Must(uuid.NewV7())
	if err := store.Enqueue(ctx, []Mutation{{OperationID: op, Kind: Update, ObjectID: object, ObjectType: Note, BaseRevision: 1, BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	dependentOp := uuid.Must(uuid.NewV7())
	dependency := op
	if err := store.Enqueue(ctx, []Mutation{{OperationID: dependentOp, Kind: Create, ObjectID: uuid.New(), ObjectType: Folder, Name: "Dependent", DependencyOperationID: &dependency}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, op, Result{Conflict: "base_revision_mismatch", Canonical: &CanonicalState{ObjectType: Note, Revision: 2, Name: "N.md", BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	copyID := uuid.Must(uuid.NewV7())
	materialized := sha256.Sum256([]byte("copy"))
	m := ConflictMaterialization{OperationID: op, SourceObjectID: object, ConflictNoteID: copyID, OriginalRelative: "N.md", TargetRelative: "_Konflikte/Wiederhergestellt/" + ConflictFileName("N.md", op), SourceHash: hash, MaterializedHash: materialized, StagedRelative: ".remember/conflicts/materializations/" + op.String() + ".md", State: "prepared"}
	if err := store.PutConflictMaterialization(ctx, m); err != nil {
		t.Fatal(err)
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || !unresolved {
		t.Fatalf("prepared unresolved=%t err=%v", unresolved, err)
	}
	if err := store.MarkConflictCopyStaged(ctx, op); err != nil {
		t.Fatal(err)
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("staged unresolved=%t err=%v", unresolved, err)
	}
	var dependentStatus string
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT status FROM sync_outbox WHERE operation_id=?`, dependentOp.String()).Scan(&dependentStatus)
	}); err != nil || dependentStatus != "superseded" {
		t.Fatalf("dependent status=%q err=%v", dependentStatus, err)
	}
	if staged, err := store.StagedConflictMaterializations(ctx); err != nil || len(staged) != 1 || staged[0].ConflictNoteID != copyID {
		t.Fatalf("staged=%#v err=%v", staged, err)
	}
	if err := store.MarkConflictCopyPublished(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteConflictMaterialization(ctx, op); err != nil {
		t.Fatal(err)
	}
	got, err := store.ConflictMaterialization(ctx, op)
	if err != nil || got == nil || got.State != "completed" {
		t.Fatalf("materialization=%#v err=%v", got, err)
	}
	if cleanups, err := store.CompletedConflictCleanups(ctx); err != nil || len(cleanups) != 1 {
		t.Fatalf("cleanups=%#v err=%v", cleanups, err)
	}
	if err := store.MarkConflictMaterializationCleaned(ctx, op); err != nil {
		t.Fatal(err)
	}
	if cleanups, err := store.CompletedConflictCleanups(ctx); err != nil || len(cleanups) != 0 {
		t.Fatalf("cleaned remains=%#v err=%v", cleanups, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conflict_materializations SET cleaned_at_ms=NULL WHERE operation_id=?`, op.String())
		return err
	}); err == nil {
		t.Fatal("cleanup completion moved backwards")
	}
}

func TestMutationShapesDependenciesAndCursorAreFailClosed(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	object := uuid.New()
	op, _ := uuid.NewV7()
	hash := sha256.Sum256([]byte("content"))
	invalid := []Mutation{
		{OperationID: op, Kind: Delete, ObjectID: object, ObjectType: Folder},
		{OperationID: op, Kind: Create, ObjectID: object, ObjectType: Folder, Name: "Folder", BlobHash: hash[:]},
		{OperationID: op, Kind: Update, ObjectID: object, ObjectType: Note, BaseRevision: 1},
		{OperationID: op, Kind: Move, ObjectID: object, ObjectType: Note, BaseRevision: 1, Name: ""},
	}
	for _, mutation := range invalid {
		if err := store.Enqueue(ctx, []Mutation{mutation}); err == nil {
			t.Fatalf("invalid mutation accepted: %#v", mutation)
		}
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,name,blob_hash,status,created_at_ms)
			VALUES(?, 'delete', ?, 'folder', 0, 'bad', ?, 'pending', 1)`, op.String(), object.String(), hash[:])
		return err
	}); err == nil {
		t.Fatal("SQL schema accepted an unsendable mutation")
	}
	create, update := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.Enqueue(ctx, []Mutation{{OperationID: create, Kind: Create, ObjectID: object, ObjectType: Note, Name: "N.md", BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, []Mutation{{OperationID: update, Kind: Update, ObjectID: object, ObjectType: Note, BaseRevision: 1, BlobHash: hash[:], DependencyOperationID: &create}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttempted(ctx, update); err == nil {
		t.Fatal("dependent operation was attempted before its prerequisite")
	}
	if err := store.RecordResult(ctx, update, Result{Accepted: true, Revision: 2, Cursor: 2}); err == nil {
		t.Fatal("dependent result was accepted before its prerequisite")
	}
	if err := store.RecordResult(ctx, create, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, update, Result{Accepted: true, Revision: 2, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfirmedCursor(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfirmedCursor(ctx, 8); err == nil {
		t.Fatal("confirmed cursor moved backwards")
	}
}

func TestCaptureProjectionAndSnapshotCommitExcludeConcurrentResultTransitions(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	object := uuid.New()
	op, _ := uuid.NewV7()
	if err := store.Enqueue(ctx, []Mutation{{OperationID: op, Kind: Create, ObjectID: object, ObjectType: Folder, Name: "Folder"}}); err != nil {
		t.Fatal(err)
	}
	projectionRead := make(chan struct{})
	release := make(chan struct{})
	captureDone := make(chan error, 1)
	go func() {
		captureDone <- store.CaptureSnapshot(ctx, &localindex.Snapshot{}, false, func(tx *sql.Tx) ([]Mutation, []uuid.UUID, error) {
			if _, _, _, err := store.ProjectionTx(ctx, tx, object); err != nil {
				return nil, nil, err
			}
			close(projectionRead)
			<-release
			return nil, nil, nil
		})
	}()
	<-projectionRead
	attemptDone := make(chan error, 1)
	go func() { attemptDone <- store.MarkAttempted(ctx, op) }()
	select {
	case err := <-attemptDone:
		t.Fatalf("attempt interleaved with capture transaction: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-captureDone; err != nil {
		t.Fatal(err)
	}
	if err := <-attemptDone; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAndOutboxRollbackTogether(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	a := uuid.New()
	initial := localindex.Snapshot{Objects: []localindex.Object{{ID: a, Type: localindex.ObjectFolder, RelativePath: "A", CollisionPath: "a", IdentityState: localindex.IdentityKnown}}}
	if err := index.ReplaceSnapshot(ctx, initial); err != nil {
		t.Fatal(err)
	}
	b := uuid.New()
	next := localindex.Snapshot{Objects: []localindex.Object{{ID: b, Type: localindex.ObjectFolder, RelativePath: "B", CollisionPath: "b", IdentityState: localindex.IdentityKnown}}}
	bad := Mutation{OperationID: uuid.New(), Kind: Create, ObjectID: b, ObjectType: Folder, Name: "B"}
	if err := store.ReplaceSnapshotAndEnqueue(ctx, next, []Mutation{bad}, nil); err == nil {
		t.Fatal("invalid operation accepted")
	}
	got, err := index.ReadSnapshot(ctx)
	if err != nil || len(got.Objects) != 1 || got.Objects[0].ID != a {
		t.Fatalf("rollback snapshot=%#v err=%v", got, err)
	}
}

func TestExplicitBootstrapStagesAndQueuesParentBeforeChild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".remember"), 0o700); err != nil {
		t.Fatal(err)
	}
	index, err := localindex.Open(ctx, filepath.Join(root, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	folder := uuid.New()
	note := uuid.New()
	content := []byte("bootstrap exact")
	hash := sha256.Sum256(content)
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Folder", "N.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := localindex.Snapshot{Objects: []localindex.Object{{ID: folder, Type: localindex.ObjectFolder, RelativePath: "Folder", CollisionPath: "folder", IdentityState: localindex.IdentityKnown}, {ID: note, Type: localindex.ObjectNote, RelativePath: "Folder/N.md", CollisionPath: "folder/n.md", ParentID: folder, ContentHash: hash[:], IdentityState: localindex.IdentityKnown}}}
	if err := index.ReplaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	setBootstrap(t, index)
	if err := PrepareBootstrap(ctx, root, index, nil); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(index)
	required, _ := store.BootstrapRequired(ctx)
	pending, err := store.ListPending(ctx, 10)
	if err != nil || required || len(pending) != 1 || pending[0].Mutation.ObjectID != folder {
		t.Fatalf("required=%t parent pending=%#v err=%v", required, pending, err)
	}
	if err := store.RecordResult(ctx, pending[0].Mutation.OperationID, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Mutation.ObjectID != note || pending[0].Mutation.DependencyOperationID == nil {
		t.Fatalf("child pending=%#v err=%v", pending, err)
	}
}

func TestApplyPlanPersistence(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, _ := uuid.NewV7()
	op, _ := uuid.NewV7()
	obj := uuid.New()
	if err := store.SetConfirmedCursor(ctx, 2); err != nil {
		t.Fatal(err)
	}
	plan := ApplyPlan{ID: planID, FromCursor: 2, ThroughCursor: 3, Steps: []Change{{Cursor: 3, OperationID: op, ObjectID: obj, Mutation: Create, ObjectType: Folder, Revision: 1, Name: "Folder"}}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	got, err := store.ActiveApplyPlan(ctx)
	if err != nil || got == nil || got.ID != planID || len(got.Steps) != 1 || got.Steps[0].ObjectID != obj || got.Steps[0].State != "pending" {
		t.Fatalf("plan=%#v err=%v", got, err)
	}
}

func TestApplyPlanTransitionsCompleteAtomically(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, op := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	object := uuid.New()
	plan := ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []Change{{Cursor: 1, OperationID: op, ObjectID: object, Mutation: Create, ObjectType: Note, Revision: 1, Name: "N.md", BlobHash: make([]byte, 32)}}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteApplyPlan(ctx, planID); err == nil {
		t.Fatal("prepared plan completed")
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal("begin replay failed:", err)
	}
	if err := store.CompleteApplyPlan(ctx, planID); err == nil {
		t.Fatal("pending plan completed")
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err != nil {
		t.Fatal("step replay failed:", err)
	}
	if err := store.CompleteApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 1 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if revision, found, err := store.Baseline(ctx, object); err != nil || !found || revision != 1 {
		t.Fatalf("baseline=%d/%t err=%v", revision, found, err)
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestFolderPublicationTransitionIsAtomicAndImmutable(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, op, folderID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.New()
	plan := ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 1, Steps: []Change{{Cursor: 1, OperationID: op, ObjectID: folderID, Mutation: Create, ObjectType: Folder, Revision: 1, Name: "Folder"}}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	nonce[0] = 7
	publication := FolderPublication{PlanID: planID, StepIndex: 0, FolderID: folderID, TargetRelative: "Folder", StageRelative: ".remember/apply/folders/" + planID.String() + "/0", Nonce: nonce, Device: 11, Inode: 12}
	if err := store.PutFolderPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFolderPublication(ctx, publication); err == nil {
		t.Fatal("duplicate publication accepted")
	}
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err == nil {
		t.Fatal("folder step used non-atomic generic transition")
	}
	if err := store.MarkFolderStepAppliedAndAuthorizeCleanup(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.FolderPublication(ctx, planID, 0)
	if err != nil || got == nil || !got.CleanupAuthorized || got.Nonce != nonce || got.Device != 11 || got.Inode != 12 {
		t.Fatalf("publication=%#v err=%v", got, err)
	}
	active, err := store.ActiveApplyPlan(ctx)
	if err != nil || active == nil || active.Steps[0].State != "applied" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE apply_folder_publications SET inode=99 WHERE plan_id=?`, planID.String())
		return err
	}); err == nil {
		t.Fatal("publication identity mutated")
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE apply_folder_publications SET cleanup_authorized=0 WHERE plan_id=?`, planID.String())
		return err
	}); err == nil {
		t.Fatal("cleanup authorization moved backwards")
	}
	if err := store.CompleteApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.CompletedFolderPublications(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("completed publications=%#v err=%v", pending, err)
	}
	if err := store.MarkFolderPublicationCleaned(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.CompletedFolderPublications(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("cleaned publication remains=%#v err=%v", pending, err)
	}
}

func TestFolderMutationBindingIsImmutable(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, createOp, moveOp, folderID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.New()
	if err := store.CreateApplyPlan(ctx, ApplyPlan{ID: planID, FromCursor: 0, ThroughCursor: 2, Steps: []Change{
		{Cursor: 1, OperationID: createOp, ObjectID: folderID, Mutation: Create, ObjectType: Folder, Revision: 1, Name: "Folder"},
		{Cursor: 2, OperationID: moveOp, ObjectID: folderID, Mutation: Move, ObjectType: Folder, Revision: 2, Name: "Moved"},
	}}); err != nil {
		t.Fatal(err)
	}
	binding := FolderMutation{PlanID: planID, StepIndex: 1, FolderID: folderID, Mutation: Move, SourceRelative: "Folder", TargetRelative: "Moved", Device: 11, Inode: 12}
	if err := store.PutFolderMutation(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFolderMutation(ctx, binding); err == nil {
		t.Fatal("duplicate folder mutation accepted")
	}
	got, err := store.FolderMutation(ctx, planID, 1)
	if err != nil || got == nil || *got != binding {
		t.Fatalf("binding=%#v err=%v", got, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE apply_folder_mutations SET inode=99 WHERE plan_id=?`, planID.String())
		return err
	}); err == nil {
		t.Fatal("folder mutation binding mutated")
	}
}

func inboxFolderChange(t *testing.T, cursor, revision uint64, object uuid.UUID, mutation MutationKind, name string) Change {
	t.Helper()
	op, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return Change{Cursor: cursor, OperationID: op, ObjectID: object, Mutation: mutation, ObjectType: Folder, Revision: revision, Name: name, Deleted: mutation == Delete}
}

func TestInboxPageIngestIsAtomicReplaySafeAndPersistent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inbox.db")
	index, err := localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(index)
	object1, object2 := uuid.New(), uuid.New()
	first := inboxFolderChange(t, 1, 1, object1, Create, "One")
	second := inboxFolderChange(t, 2, 1, object2, Create, "Two")
	invalid := second
	invalid.Name = ""
	if err := store.IngestPullPage(ctx, 0, 2, []Change{first, invalid}); err == nil {
		t.Fatal("invalid page accepted")
	}
	if cursor, err := store.DownloadedCursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if _, found, err := store.InboxChange(ctx, 1); err != nil || found {
		t.Fatalf("partial page persisted found=%t err=%v", found, err)
	}
	if err := store.IngestPullPage(ctx, 0, 2, []Change{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := store.IngestPullPage(ctx, 0, 2, []Change{first, second}); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	mismatch := second
	mismatch.Name = "Other"
	if err := store.IngestPullPage(ctx, 0, 2, []Change{first, mismatch}); err == nil {
		t.Fatal("mismatched replay accepted")
	}
	if cursor, err := store.DownloadedCursor(ctx); err != nil || cursor != 2 {
		t.Fatalf("mismatch changed cursor=%d err=%v", cursor, err)
	}
	if stored, found, err := store.InboxChange(ctx, 2); err != nil || !found || stored.Change.Name != second.Name {
		t.Fatalf("mismatch changed row=%#v found=%t err=%v", stored, found, err)
	}
	if err := store.IngestPullPage(ctx, 1, 3, []Change{second, inboxFolderChange(t, 3, 1, uuid.New(), Create, "Three")}); err == nil {
		t.Fatal("overlapping page accepted")
	}
	gap1 := inboxFolderChange(t, 4, 1, uuid.New(), Create, "Gap")
	gap2 := inboxFolderChange(t, 5, 1, uuid.New(), Create, "Gap2")
	if err := store.IngestPullPage(ctx, 2, 4, []Change{gap1, gap2}); err == nil {
		t.Fatal("cursor gap accepted")
	}
	duplicate := inboxFolderChange(t, 3, 1, uuid.New(), Create, "Duplicate")
	duplicate.OperationID = first.OperationID
	if err := store.IngestPullPage(ctx, 2, 3, []Change{duplicate}); err == nil {
		t.Fatal("duplicate operation id accepted")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ = NewStore(index)
	if cursor, err := store.DownloadedCursor(ctx); err != nil || cursor != 2 {
		t.Fatalf("reopened cursor=%d err=%v", cursor, err)
	}
	pending, err := store.ListPendingInbox(ctx, 10)
	if err != nil || len(pending) != 2 || pending[0].Change.OperationID != first.OperationID || pending[1].Change.OperationID != second.OperationID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func TestInboxRejectsStrictlyInvalidChangeShapes(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	valid := inboxFolderChange(t, 1, 1, uuid.New(), Create, "Folder")
	cases := []Change{valid, valid, valid, valid, valid, valid}
	cases[0].OperationID = uuid.New()
	cases[1].Revision = 2
	cases[2].Deleted = true
	cases[3].BlobHash = make([]byte, sha256.Size)
	cases[4].State = "pending"
	cases[5].BlobHash = []byte{}
	for i, change := range cases {
		if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err == nil {
			t.Fatalf("invalid shape %d accepted", i)
		}
	}
	if cursor, err := store.DownloadedCursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	parent := uuid.New()
	hash := sha256.Sum256([]byte("note"))
	operation, _ := uuid.NewV7()
	note := Change{Cursor: 1, OperationID: operation, ObjectID: uuid.New(), Mutation: Create, ObjectType: Note, Revision: 1, ParentID: &parent, Name: "Note.md", BlobHash: hash[:]}
	if err := store.IngestPullPage(ctx, 0, 1, []Change{note}); err != nil {
		t.Fatalf("valid note rejected: %v", err)
	}
	stored, found, err := store.InboxChange(ctx, 1)
	if err != nil || !found || !sameChangePayload(stored.Change, note) {
		t.Fatalf("stored note=%#v found=%t err=%v", stored, found, err)
	}
}

func TestInboxStateFrontierAndSameObjectOrdering(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	x, y := uuid.New(), uuid.New()
	first := inboxFolderChange(t, 1, 1, x, Create, "X")
	second := inboxFolderChange(t, 2, 1, y, Create, "Y")
	third := inboxFolderChange(t, 3, 2, x, Move, "X2")
	if err := store.IngestPullPage(ctx, 0, 3, []Change{first, second, third}); err != nil {
		t.Fatal(err)
	}
	if earlier, err := store.HasEarlierPendingInboxForObject(ctx, x, 3); err != nil || !earlier {
		t.Fatalf("earlier=%t err=%v", earlier, err)
	}
	if item, found, err := store.PendingInboxChange(ctx, 2); err != nil || !found || item.Change.OperationID != second.OperationID {
		t.Fatalf("pending item=%#v found=%t err=%v", item, found, err)
	}
	if err := store.MarkInboxApplying(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.PendingInboxChange(ctx, 2); err != nil || found {
		t.Fatalf("applying row returned pending found=%t err=%v", found, err)
	}
	if err := store.MarkInboxApplied(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if frontier, err := store.AdvanceConfirmedInboxCursor(ctx); err != nil || frontier != 0 {
		t.Fatalf("frontier=%d err=%v", frontier, err)
	}
	if err := store.MarkInboxApplied(ctx, 1); err == nil {
		t.Fatal("pending skipped directly to applied")
	}
	if err := store.MarkInboxApplying(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if earlier, err := store.HasEarlierPendingInboxForObject(ctx, x, 3); err != nil || !earlier {
		t.Fatalf("applying earlier=%t err=%v", earlier, err)
	}
	if frontier, err := store.AdvanceConfirmedInboxCursor(ctx); err != nil || frontier != 0 {
		t.Fatalf("applying frontier=%d err=%v", frontier, err)
	}
	if err := store.MarkInboxApplied(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if frontier, err := store.AdvanceConfirmedInboxCursor(ctx); err != nil || frontier != 2 {
		t.Fatalf("frontier=%d err=%v", frontier, err)
	}
	if earlier, err := store.HasEarlierPendingInboxForObject(ctx, x, 3); err != nil || earlier {
		t.Fatalf("applied earlier=%t err=%v", earlier, err)
	}
	if err := store.MarkInboxApplying(ctx, 1); err == nil {
		t.Fatal("applied row moved backwards")
	}
	if err := store.MarkInboxApplying(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInboxApplied(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if frontier, err := store.AdvanceConfirmedInboxCursor(ctx); err != nil || frontier != 3 {
		t.Fatalf("final frontier=%d err=%v", frontier, err)
	}
	item, found, err := store.InboxChange(ctx, 3)
	if err != nil || !found || item.State != "applied" || item.ApplyingAt == nil || item.AppliedAt == nil {
		t.Fatalf("item=%#v found=%t err=%v", item, found, err)
	}
}

func TestInboxSQLGuardsPayloadStateAndDelete(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	change := inboxFolderChange(t, 1, 1, uuid.New(), Create, "Folder")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT OR REPLACE INTO sync_inbox_changes(cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,state,ingested_at_ms) SELECT cursor,'01900000-0000-7000-8000-000000000001',object_id,mutation,object_type,revision,parent_id,'Replaced',blob_hash,deleted,state,ingested_at_ms FROM sync_inbox_changes WHERE cursor=1`,
		`UPDATE sync_inbox_changes SET name='Changed' WHERE cursor=1`,
		`UPDATE sync_inbox_changes SET state='applied',applying_at_ms=ingested_at_ms,applied_at_ms=ingested_at_ms WHERE cursor=1`,
		`DELETE FROM sync_inbox_changes WHERE cursor=1`,
	} {
		if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(statement); return err }); err == nil {
			t.Fatalf("SQL guard accepted %q", statement)
		}
	}
	if item, found, err := store.InboxChange(ctx, 1); err != nil || !found || item.Change.Name != "Folder" || item.State != "pending" {
		t.Fatalf("item=%#v found=%t err=%v", item, found, err)
	}
}

func TestReconcileInboxAppliedThroughConfirmedIsPartialIdempotentAndResumable(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	first := inboxFolderChange(t, 1, 1, uuid.New(), Create, "One")
	second := inboxFolderChange(t, 2, 1, uuid.New(), Create, "Two")
	if err := store.IngestPullPage(ctx, 0, 2, []Change{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInboxApplying(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfirmedCursor(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	one, _, err := store.InboxChange(ctx, 1)
	if err != nil || one.State != "applied" || one.ApplyingAt == nil || one.AppliedAt == nil {
		t.Fatalf("one=%#v err=%v", one, err)
	}
	two, _, err := store.InboxChange(ctx, 2)
	if err != nil || two.State != "pending" {
		t.Fatalf("two=%#v err=%v", two, err)
	}
	if err := store.SetConfirmedCursor(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
		t.Fatal(err)
	}
	two, _, err = store.InboxChange(ctx, 2)
	if err != nil || two.State != "applied" || two.ApplyingAt == nil || two.AppliedAt == nil {
		t.Fatalf("two=%#v err=%v", two, err)
	}
}

func TestReconcileInboxAppliedThroughConfirmedSeedsLegacyCursorWithoutRows(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	if err := store.SetConfirmedCursor(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
		t.Fatal(err)
	}
	if downloaded, err := store.DownloadedCursor(ctx); err != nil || downloaded != 42 {
		t.Fatalf("downloaded=%d err=%v", downloaded, err)
	}
}

func TestReconcileInboxAppliedThroughConfirmedRejectsMissingDownloadedRow(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	changes := []Change{inboxFolderChange(t, 1, 1, uuid.New(), Create, "One"), inboxFolderChange(t, 2, 1, uuid.New(), Create, "Two")}
	if err := store.IngestPullPage(ctx, 0, 2, changes); err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TRIGGER sync_inbox_no_delete`); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM sync_inbox_changes WHERE cursor=2`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err == nil {
		t.Fatal("missing downloaded row accepted")
	}
}

func TestInboxScanAndFrontierRejectCorruptStoredPayload(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	change := inboxFolderChange(t, 1, 1, uuid.New(), Create, "Folder")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TRIGGER sync_inbox_payload_immutable`); err != nil {
			return err
		}
		if _, err := tx.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sync_inbox_changes SET mutation='update' WHERE cursor=1`); err != nil {
			return err
		}
		_, err := tx.Exec(`PRAGMA ignore_check_constraints=OFF`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InboxChange(ctx, 1); err == nil {
		t.Fatal("corrupt inbox payload scanned")
	}
	if _, err := store.AdvanceConfirmedInboxCursor(ctx); err == nil {
		t.Fatal("corrupt inbox advanced frontier")
	}
}

func inboxNoteChange(t *testing.T, cursor, revision uint64, object uuid.UUID, mutation MutationKind, parent *uuid.UUID, name string) Change {
	t.Helper()
	op, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", name, cursor)))
	return Change{Cursor: cursor, OperationID: op, ObjectID: object, Mutation: mutation, ObjectType: Note, Revision: revision, ParentID: parent, Name: name, BlobHash: hash[:], Deleted: mutation == Delete}
}

func putInboxBaseline(t *testing.T, index *localindex.Index, object uuid.UUID, revision uint64) {
	t.Helper()
	if err := index.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_baselines(object_id,revision) VALUES(?,?)`, object.String(), revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func applyInboxPlan(t *testing.T, store *Store, planID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := store.BeginApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, planID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteApplyPlan(ctx, planID); err != nil {
		t.Fatal(err)
	}
}

func TestInboxApplyPlanCompletesOutOfOrderWithoutSkippingFrontier(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	x, y := uuid.New(), uuid.New()
	changes := []Change{inboxNoteChange(t, 1, 2, x, Update, nil, "X.md"), inboxNoteChange(t, 2, 2, y, Delete, nil, "Y.md")}
	if err := store.IngestPullPage(ctx, 0, 2, changes); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, x, 1)
	putInboxBaseline(t, index, y, 1)
	yPlan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 2, yPlan); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveApplyPlan(ctx)
	if err != nil || active == nil || len(active.Steps) != 1 {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	copied := active.Steps[0]
	copied.State = ""
	if !sameChangePayload(copied, changes[1]) {
		t.Fatalf("copied=%#v want=%#v", copied, changes[1])
	}
	applyInboxPlan(t, store, yPlan)
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != 0 {
		t.Fatalf("confirmed skipped X: %d", confirmed)
	}
	if rev, found, err := store.Baseline(ctx, y); err != nil || !found || rev != 2 {
		t.Fatalf("Y baseline=%d/%t err=%v", rev, found, err)
	}
	xPlan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 1, xPlan); err != nil {
		t.Fatal(err)
	}
	applyInboxPlan(t, store, xPlan)
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != 2 {
		t.Fatalf("confirmed=%d", confirmed)
	}
	for cursor := uint64(1); cursor <= 2; cursor++ {
		item, found, err := store.InboxChange(ctx, cursor)
		if err != nil || !found || item.State != "applied" {
			t.Fatalf("cursor %d item=%#v found=%t err=%v", cursor, item, found, err)
		}
	}
}

func TestInboxApplyPlanRejectsAnotherActivePlan(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	id := uuid.New()
	change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, id, 1)
	legacy := ApplyPlan{ID: uuid.Must(uuid.NewV7()), FromCursor: 0, ThroughCursor: 1, Steps: []Change{change}}
	if err := store.CreateApplyPlan(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInboxApplyPlan(ctx, 1, uuid.Must(uuid.NewV7())); err == nil {
		t.Fatal("second active plan accepted")
	}
}

func TestInboxApplyPlanBlocksSameObjectOvertake(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "inbox-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	object := uuid.New()
	changes := []Change{inboxNoteChange(t, 1, 2, object, Update, nil, "N.md"), inboxNoteChange(t, 2, 3, object, Update, nil, "N.md")}
	if err := store.IngestPullPage(ctx, 0, 2, changes); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, object, 2)
	plan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 2, plan); err == nil {
		t.Fatal("overtake behind pending predecessor accepted")
	}
	if err := store.MarkInboxApplying(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInboxApplyPlan(ctx, 2, plan); err == nil {
		t.Fatal("overtake behind applying predecessor accepted")
	}
}

func TestInboxApplyPlanCompletionRequiresUnchangedPredecessor(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	id := uuid.New()
	change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, id, 1)
	plan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplyStepApplied(ctx, plan, 0); err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_baselines SET revision=3 WHERE object_id=?`, id.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteApplyPlan(ctx, plan); err == nil {
		t.Fatal("changed predecessor accepted")
	}
	item, _, _ := store.InboxChange(ctx, 1)
	if item.State != "applying" {
		t.Fatalf("inbox=%s", item.State)
	}
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != 0 {
		t.Fatalf("confirmed=%d", confirmed)
	}
}

func TestInboxApplyPlanResumesAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inbox-plan.db")
	index, err := localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(index)
	object := uuid.New()
	change := inboxNoteChange(t, 1, 2, object, Update, nil, "N.md")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, object, 1)
	plan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = localindex.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ = NewStore(index)
	active, err := store.ActiveApplyPlan(ctx)
	if err != nil || active == nil || active.ID != plan || active.Status != "applying" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	if err := store.BeginApplyPlan(ctx, plan); err != nil {
		t.Fatalf("idempotent begin: %v", err)
	}
	if err := store.MarkApplyStepApplied(ctx, plan, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != 1 {
		t.Fatalf("confirmed=%d", confirmed)
	}
}

func TestInboxApplyPlanRejectsIneligibleShapesAndMissingPredecessor(t *testing.T) {
	cases := []struct {
		name     string
		change   func(*testing.T, uuid.UUID) Change
		baseline bool
	}{
		{"create", func(t *testing.T, id uuid.UUID) Change { return inboxNoteChange(t, 1, 1, id, Create, nil, "N.md") }, true},
		{"move", func(t *testing.T, id uuid.UUID) Change { return inboxNoteChange(t, 1, 2, id, Move, nil, "N.md") }, true},
		{"nested", func(t *testing.T, id uuid.UUID) Change {
			parent := uuid.New()
			return inboxNoteChange(t, 1, 2, id, Update, &parent, "N.md")
		}, true},
		{"folder", func(t *testing.T, id uuid.UUID) Change { return inboxFolderChange(t, 1, 2, id, Update, "Folder") }, true},
		{"missing baseline", func(t *testing.T, id uuid.UUID) Change { return inboxNoteChange(t, 1, 2, id, Update, nil, "N.md") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer index.Close()
			store, _ := NewStore(index)
			id := uuid.New()
			change := tc.change(t, id)
			if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
				t.Fatal(err)
			}
			if tc.baseline {
				putInboxBaseline(t, index, id, change.Revision-1)
			}
			if err := store.CreateInboxApplyPlan(ctx, 1, uuid.Must(uuid.NewV7())); err == nil {
				t.Fatal("ineligible change accepted")
			}
		})
	}
}

func TestInboxApplyPlanSQLGuardsExactPayloadAndLink(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	id := uuid.New()
	change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
	if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
		t.Fatal(err)
	}
	putInboxBaseline(t, index, id, 1)
	spoofPlan := uuid.Must(uuid.NewV7())
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO apply_plans(plan_id,from_cursor,through_cursor,status,created_at_ms) VALUES(?,0,1,'prepared',0)`, spoofPlan.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) VALUES(?,0,1,?,?,'update','note',2,NULL,'Spoof.md',?,'pending')`, spoofPlan.String(), change.OperationID.String(), id.String(), change.BlobHash); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sync_inbox_apply_plans(plan_id,cursor) VALUES(?,1)`, spoofPlan.String())
		return err
	}); err == nil {
		t.Fatal("mismatched link inserted")
	}
	plan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInboxApplying(ctx, 1); err == nil {
		t.Fatal("linked inbox bypassed plan begin")
	}
	statements := []string{`UPDATE apply_plans SET status='completed',completed_at_ms=1 WHERE plan_id='` + plan.String() + `'`, `UPDATE apply_steps SET state='applied' WHERE plan_id='` + plan.String() + `'`, `INSERT OR REPLACE INTO sync_inbox_apply_plans(plan_id,cursor) VALUES('` + plan.String() + `',1)`, `UPDATE apply_plans SET through_cursor=2 WHERE plan_id='` + plan.String() + `'`, `INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) SELECT plan_id,1,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state FROM apply_steps WHERE plan_id='` + plan.String() + `'`, `UPDATE apply_steps SET name='Spoof.md' WHERE plan_id='` + plan.String() + `'`, `UPDATE sync_inbox_apply_plans SET cursor=2 WHERE plan_id='` + plan.String() + `'`, `DELETE FROM sync_inbox_apply_plans WHERE plan_id='` + plan.String() + `'`, `DELETE FROM apply_steps WHERE plan_id='` + plan.String() + `'`}
	for _, statement := range statements {
		if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(statement); return err }); err == nil {
			t.Fatalf("SQL spoof accepted: %s", statement)
		}
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_inbox_changes SET state='applying',applying_at_ms=ingested_at_ms WHERE cursor=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginApplyPlan(ctx, plan); err != nil {
		t.Fatalf("partial begin recovery: %v", err)
	}
	if err := store.MarkApplyStepApplied(ctx, plan, 0); err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_inbox_changes SET state='applied',applied_at_ms=applying_at_ms WHERE cursor=1`)
		return err
	}); err == nil {
		t.Fatal("inbox completed before baseline")
	}
	if err := store.CompleteApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonPreparedInboxPlanRequiresPristineState(t *testing.T) {
	t.Run("pristine", func(t *testing.T) {
		ctx := context.Background()
		index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		store, _ := NewStore(index)
		id := uuid.New()
		change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
		if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
			t.Fatal(err)
		}
		putInboxBaseline(t, index, id, 1)
		plan := uuid.Must(uuid.NewV7())
		if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
			t.Fatal(err)
		}
		if err := store.AbandonPreparedInboxPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
			t.Fatalf("active=%#v err=%v", active, err)
		}
		item, _, _ := store.InboxChange(ctx, 1)
		if item.State != "pending" {
			t.Fatalf("inbox=%s", item.State)
		}
		if err := store.RetryAbandonedInboxPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		active, err := store.ActiveApplyPlan(ctx)
		if err != nil || active == nil || active.ID != plan || active.Status != "prepared" {
			t.Fatalf("retried active=%#v err=%v", active, err)
		}
	})
	t.Run("applying", func(t *testing.T) {
		ctx := context.Background()
		index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		store, _ := NewStore(index)
		id := uuid.New()
		change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
		if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
			t.Fatal(err)
		}
		putInboxBaseline(t, index, id, 1)
		plan := uuid.Must(uuid.NewV7())
		if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
			t.Fatal(err)
		}
		if err := store.BeginApplyPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		if err := store.AbandonPreparedInboxPlan(ctx, plan); err == nil {
			t.Fatal("applying plan abandoned")
		}
	})
	t.Run("side journal", func(t *testing.T) {
		ctx := context.Background()
		index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		store, _ := NewStore(index)
		id := uuid.New()
		change := inboxNoteChange(t, 1, 2, id, Update, nil, "N.md")
		if err := store.IngestPullPage(ctx, 0, 1, []Change{change}); err != nil {
			t.Fatal(err)
		}
		putInboxBaseline(t, index, id, 1)
		plan := uuid.Must(uuid.NewV7())
		if err := store.CreateInboxApplyPlan(ctx, 1, plan); err != nil {
			t.Fatal(err)
		}
		if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO apply_folder_mutations(plan_id,step_index,folder_id,mutation_kind,source_relative,target_relative,device,inode) VALUES(?,0,?,'delete','X','X',1,1)`, plan.String(), id.String())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.AbandonPreparedInboxPlan(ctx, plan); err == nil {
			t.Fatal("plan with side journal abandoned")
		}
	})
}

func TestApplyPlanRejectsCursorGaps(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, op1, op2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := store.SetConfirmedCursor(ctx, 2); err != nil {
		t.Fatal(err)
	}
	plan := ApplyPlan{ID: planID, FromCursor: 2, ThroughCursor: 5, Steps: []Change{
		{Cursor: 3, OperationID: op1, ObjectID: uuid.New(), Mutation: Create, ObjectType: Folder, Revision: 1, Name: "A"},
		{Cursor: 5, OperationID: op2, ObjectID: uuid.New(), Mutation: Create, ObjectType: Folder, Revision: 1, Name: "B"},
	}}
	if err := store.CreateApplyPlan(ctx, plan); err == nil {
		t.Fatal("apply plan accepted a cursor gap")
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("failed plan was partially persisted: %#v err=%v", active, err)
	}
}

func TestStageNoteExactBytesDedupeSizeAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink and mode checks")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".remember"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("exact outgoing bytes")
	if err := os.WriteFile(filepath.Join(root, "N.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	wrong := sha256.Sum256([]byte("wrong"))
	if err := StageNote(root, "N.md", wrong); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".remember", "sync", "outbox", fmtHash(wrong))); !os.IsNotExist(err) {
		t.Fatalf("mismatch was staged: %v", err)
	}
	if err := StageNote(root, "N.md", hash); err != nil {
		t.Fatal(err)
	}
	staged, err := ReadStagedNote(root, hash)
	if err != nil || !bytes.Equal(staged, content) {
		t.Fatalf("staged read=%q err=%v", staged, err)
	}
	if err := StageNote(root, "N.md", hash); err != nil {
		t.Fatalf("dedupe failed: %v", err)
	}
	stagedPath := filepath.Join(root, ".remember", "sync", "outbox", fmtHash(hash))
	if err := os.Chmod(stagedPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStagedNote(root, hash); err == nil {
		t.Fatal("unsafe staged permissions accepted")
	}
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "N.md"), []byte("later edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err = os.ReadFile(filepath.Join(root, ".remember", "sync", "outbox", fmtHash(hash)))
	if err != nil || !bytes.Equal(staged, content) {
		t.Fatalf("staged=%q err=%v", staged, err)
	}
	info, _ := os.Stat(filepath.Join(root, ".remember", "sync", "outbox", fmtHash(hash)))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if err := os.Remove(filepath.Join(root, "N.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "N.md")); err != nil {
		t.Fatal(err)
	}
	if err := StageNote(root, "N.md", hash); err == nil {
		t.Fatal("symlink source accepted")
	}
	large := filepath.Join(root, "Large.md")
	if err := os.Remove(filepath.Join(root, "N.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, make([]byte, MaxBlobBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	largeHash := sha256.Sum256(make([]byte, MaxBlobBytes+1))
	if err := StageNote(root, "Large.md", largeHash); !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("large error=%v", err)
	}
}
func TestIndependentInboxCandidatesExcludeUnresolvedIntentsBeforeLimit(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	const blocked = 1000
	changes := make([]Change, 0, blocked+1)
	ids := make([]uuid.UUID, 0, blocked+1)
	for cursor := 1; cursor <= blocked+1; cursor++ {
		id := uuid.New()
		ids = append(ids, id)
		changes = append(changes, inboxNoteChange(t, uint64(cursor), 2, id, Update, nil, "N.md"))
	}
	if err := store.IngestPullPage(ctx, 0, blocked+1, changes); err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_baselines(object_id,revision) VALUES(?,1)`, id.String()); err != nil {
				return err
			}
			if i < blocked {
				hash := make([]byte, 32)
				hash[0] = byte(i)
				if _, err := tx.ExecContext(ctx, `INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,name,blob_hash,status,created_at_ms) VALUES(?,'update',?,'note',1,'',?,'pending',1)`, uuid.Must(uuid.NewV7()).String(), id.String(), hash); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInboxApplyPlan(ctx, 1, uuid.Must(uuid.NewV7())); err == nil {
		t.Fatal("linked plan accepted unresolved local intent")
	}
	items, err := store.ListIndependentInboxCandidates(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Change.ObjectID != ids[blocked] {
		t.Fatalf("candidates=%d", len(items))
	}
	plan := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, items[0].Change.Cursor, plan); err != nil {
		t.Fatal(err)
	}
}

func fmtHash(hash [32]byte) string { return fmt.Sprintf("%x", hash[:]) }

func setBootstrap(t *testing.T, index *localindex.Index) {
	t.Helper()
	if err := index.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_state(key,value) VALUES('bootstrap_required','1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
