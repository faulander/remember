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
	if err := store.RecordResult(ctx, conflictOp, Result{Conflict: "path_collision"}); err != nil {
		t.Fatal(err)
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
	plan := ApplyPlan{ID: planID, FromCursor: 2, ThroughCursor: 3, Steps: []Change{{Cursor: 3, OperationID: op, ObjectID: obj, Mutation: Create, ObjectType: Folder, Revision: 1, Name: "Folder"}}}
	if err := store.CreateApplyPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	got, err := store.ActiveApplyPlan(ctx)
	if err != nil || got == nil || got.ID != planID || len(got.Steps) != 1 || got.Steps[0].ObjectID != obj {
		t.Fatalf("plan=%#v err=%v", got, err)
	}
}

func TestApplyPlanRejectsCursorGaps(t *testing.T) {
	ctx := context.Background()
	index, _ := localindex.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	defer index.Close()
	store, _ := NewStore(index)
	planID, op1, op2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
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
