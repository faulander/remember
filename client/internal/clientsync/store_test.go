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
