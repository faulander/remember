package sync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/database"
	"github.com/google/uuid"
)

func TestMutationLifecycleIdempotencyAndPull(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, 1)
	actor := fixture.actors[0]
	// Existing local frontmatter may contain canonical legacy UUIDv4 object IDs;
	// actor and operation identities remain UUIDv7.
	folderID := uuid.New()
	createFolder := mutation(MutationCreate, folderID, ObjectFolder, 0)
	createFolder.Name = "Projekte"
	first, err := actor.Submit(context.Background(), createFolder)
	if err != nil || !first.Accepted || first.Revision != 1 || first.Cursor != 1 {
		t.Fatalf("create folder = %#v, %v", first, err)
	}
	replay, err := actor.Submit(context.Background(), createFolder)
	if err != nil || replay != first {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	changed := createFolder
	changed.Name = "Anders"
	if _, err := actor.Submit(context.Background(), changed); !errors.Is(err, ErrOperationReplayMismatch) {
		t.Fatalf("replay mismatch = %v", err)
	}

	noteID := uuid.New()
	createNote := mutation(MutationCreate, noteID, ObjectNote, 0)
	createNote.ParentID = &folderID
	createNote.Name = "Plan.md"
	createNote.BlobHash = fixture.blob
	created, err := actor.Submit(context.Background(), createNote)
	if err != nil || created.Cursor != 2 {
		t.Fatalf("create note = %#v, %v", created, err)
	}
	update := mutation(MutationUpdate, noteID, ObjectNote, 1)
	update.BlobHash = fixture.blob2
	updated, err := actor.Submit(context.Background(), update)
	if err != nil || updated.Revision != 2 || updated.Cursor != 3 {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	conflicting := mutation(MutationUpdate, noteID, ObjectNote, 99)
	conflicting.BlobHash = fixture.blob
	conflict, err := actor.Submit(context.Background(), conflicting)
	if err != nil || conflict.Conflict != ConflictBaseRevisionMismatch || conflict.Canonical == nil || conflict.Canonical.Revision != 2 || !bytes.Equal(conflict.Canonical.BlobHash, fixture.blob2) {
		t.Fatalf("canonical conflict=%#v err=%v", conflict, err)
	}
	replayedConflict, err := actor.Submit(context.Background(), conflicting)
	if err != nil || replayedConflict.Canonical == nil || replayedConflict.Canonical.Revision != 2 || !bytes.Equal(replayedConflict.Canonical.BlobHash, fixture.blob2) {
		t.Fatalf("replayed canonical conflict=%#v err=%v", replayedConflict, err)
	}
	move := mutation(MutationMove, noteID, ObjectNote, 2)
	move.Name = "Moved.md"
	moved, err := actor.Submit(context.Background(), move)
	if err != nil || moved.Revision != 3 {
		t.Fatalf("move = %#v, %v", moved, err)
	}
	replayedAfterMove, err := actor.Submit(context.Background(), conflicting)
	if err != nil || replayedAfterMove.Canonical == nil || replayedAfterMove.Canonical.Revision != 2 || replayedAfterMove.Canonical.Name != "Plan.md" {
		t.Fatalf("conflict replay drifted=%#v err=%v", replayedAfterMove, err)
	}
	remove := mutation(MutationDelete, noteID, ObjectNote, 3)
	deleted, err := actor.Submit(context.Background(), remove)
	if err != nil || deleted.Revision != 4 {
		t.Fatalf("delete = %#v, %v", deleted, err)
	}

	page, err := actor.Pull(context.Background(), 0, 3)
	if err != nil || len(page.Changes) != 3 || !page.HasMore || page.NextCursor != 3 {
		t.Fatalf("first pull = %#v, %v", page, err)
	}
	middle, err := actor.Pull(context.Background(), page.NextCursor, 3)
	if err != nil || len(middle.Changes) != 3 || !middle.HasMore || middle.Changes[0].ObjectID != ConflictRootID || middle.Changes[1].ObjectID != ConflictRecoveredID {
		t.Fatalf("middle pull = %#v, %v", middle, err)
	}
	last, err := actor.Pull(context.Background(), middle.NextCursor, 3)
	if err != nil || len(last.Changes) != 1 || last.HasMore || !last.Changes[0].Deleted {
		t.Fatalf("last pull = %#v, %v", last, err)
	}
	if len(last.Changes[0].BlobHash) != 32 || !bytes.Equal(last.Changes[0].BlobHash, fixture.blob2) {
		t.Error("pull did not return immutable moved version state")
	}
}

func TestPreserveAndDeleteEmptyFolderIsAtomicAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 2)
	actor := f.actors[0]
	folder := newID(t)
	created := mustAccepted(t, actor, createFolder(folder, "Before", nil))
	moved := mustAccepted(t, actor, func() Mutation {
		m := mutation(MutationMove, folder, ObjectFolder, created.Revision)
		m.Name = "After"
		return m
	}())
	conflictOp := mustNewID()
	deleteRequest := Mutation{OperationID: conflictOp, Kind: MutationDelete, ObjectID: folder, ObjectType: ObjectFolder, BaseRevision: created.Revision}
	conflict, err := actor.Submit(ctx, deleteRequest)
	if err != nil || conflict.Conflict != ConflictBaseRevisionMismatch || conflict.Canonical == nil || conflict.Canonical.Revision != moved.Revision {
		t.Fatalf("conflict=%#v err=%v", conflict, err)
	}
	req := PreserveDeleteFolderRequest{OperationID: mustNewID(), ConflictOperationID: conflictOp, FolderID: folder, ExpectedRevision: moved.Revision}
	result, err := actor.PreserveAndDeleteEmptyFolder(ctx, req)
	if err != nil || result.RecoveredFolderID == uuid.Nil || result.DeletedCursor != result.RecoveredCursor+1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	replay, err := actor.PreserveAndDeleteEmptyFolder(ctx, req)
	if err != nil || replay != result {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	changed := req
	changed.ExpectedRevision++
	if _, err := actor.PreserveAndDeleteEmptyFolder(ctx, changed); !errors.Is(err, ErrOperationReplayMismatch) {
		t.Fatalf("mismatch=%v", err)
	}
	page, err := actor.Pull(ctx, result.RecoveredCursor-1, 10)
	if err != nil || len(page.Changes) != 2 || page.Changes[0].ObjectID != result.RecoveredFolderID || page.Changes[0].ParentID == nil || *page.Changes[0].ParentID != ConflictRecoveredID || page.Changes[1].ObjectID != folder || !page.Changes[1].Deleted {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if _, err := f.actors[1].PreserveAndDeleteEmptyFolder(ctx, req); !errors.Is(err, ErrPreserveDeleteUnavailable) {
		t.Fatalf("tenant accepted=%v", err)
	}
	otherDevice := newID(t)
	if _, err := f.db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms) VALUES(?,?,?,'active',1,1)`, f.users[0][:], otherDevice[:], "Other"); err != nil {
		t.Fatal(err)
	}
	otherActor, err := actor.service.ForActor(f.users[0], otherDevice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherActor.PreserveAndDeleteEmptyFolder(ctx, req); !errors.Is(err, ErrOperationReplayMismatch) {
		t.Fatalf("wrong device replay=%v", err)
	}
}

func TestPreserveAndDeleteEmptyFolderHandlesLongNameAndCollision(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 1)
	actor := f.actors[0]
	folder := newID(t)
	long := strings.Repeat("ä", 90)
	created := mustAccepted(t, actor, createFolder(folder, long, nil))
	moved := mustAccepted(t, actor, func() Mutation {
		m := mutation(MutationMove, folder, ObjectFolder, created.Revision)
		m.Name = long
		return m
	}())
	conflictOp := mustNewID()
	conflict, _ := actor.Submit(ctx, Mutation{OperationID: conflictOp, Kind: MutationDelete, ObjectID: folder, ObjectType: ObjectFolder, BaseRevision: created.Revision})
	if conflict.Canonical == nil {
		t.Fatal("missing conflict")
	}
	req := PreserveDeleteFolderRequest{OperationID: mustNewID(), ConflictOperationID: conflictOp, FolderID: folder, ExpectedRevision: moved.Revision}
	result, err := actor.PreserveAndDeleteEmptyFolder(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, exists, err := loadObject(ctx, tx, f.users[0], result.RecoveredFolderID)
	tx.Rollback()
	if err != nil || !exists || len([]byte(state.Name)) > 255 || state.ParentID == nil || *state.ParentID != ConflictRecoveredID {
		t.Fatalf("state=%#v exists=%t err=%v", state, exists, err)
	}
}

func TestPreserveAndDeleteEmptyFolderRejectsHistoricalChildren(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 1)
	actor := f.actors[0]
	folder, child := newID(t), newID(t)
	created := mustAccepted(t, actor, createFolder(folder, "F", nil))
	mustAccepted(t, actor, createFolder(child, "Child", &folder))
	mustAccepted(t, actor, Mutation{OperationID: mustNewID(), Kind: MutationDelete, ObjectID: child, ObjectType: ObjectFolder, BaseRevision: 1})
	moved := mustAccepted(t, actor, func() Mutation {
		m := mutation(MutationMove, folder, ObjectFolder, created.Revision)
		m.Name = "Moved"
		return m
	}())
	conflictOp := mustNewID()
	conflict, _ := actor.Submit(ctx, Mutation{OperationID: conflictOp, Kind: MutationDelete, ObjectID: folder, ObjectType: ObjectFolder, BaseRevision: created.Revision})
	if conflict.Canonical == nil {
		t.Fatal("missing conflict")
	}
	request := PreserveDeleteFolderRequest{OperationID: mustNewID(), ConflictOperationID: conflictOp, FolderID: folder, ExpectedRevision: moved.Revision}
	if _, err := actor.PreserveAndDeleteEmptyFolder(ctx, request); !errors.Is(err, ErrPreserveDeleteUnavailable) {
		t.Fatalf("historical child accepted=%v", err)
	}
}

func TestPreserveAndDeleteEmptyFolderRejectsChildrenAndStaleRevision(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 1)
	actor := f.actors[0]
	folder := newID(t)
	created := mustAccepted(t, actor, createFolder(folder, "F", nil))
	moved := mustAccepted(t, actor, func() Mutation {
		m := mutation(MutationMove, folder, ObjectFolder, created.Revision)
		m.Name = "Moved"
		return m
	}())
	conflictOp := mustNewID()
	request := Mutation{OperationID: conflictOp, Kind: MutationDelete, ObjectID: folder, ObjectType: ObjectFolder, BaseRevision: created.Revision}
	conflict, err := actor.Submit(ctx, request)
	if err != nil || conflict.Canonical == nil {
		t.Fatal(err)
	}
	mustAccepted(t, actor, createFolder(newID(t), "Child", &folder))
	resolution := PreserveDeleteFolderRequest{OperationID: mustNewID(), ConflictOperationID: conflictOp, FolderID: folder, ExpectedRevision: moved.Revision}
	if _, err := actor.PreserveAndDeleteEmptyFolder(ctx, resolution); !errors.Is(err, ErrPreserveDeleteUnavailable) {
		t.Fatalf("child accepted=%v", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sync_folder_preserve_delete_resolutions WHERE user_id=?`, f.users[0][:]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("resolution count=%d err=%v", count, err)
	}
}

func TestConflictLazilyProvisionsReservedNamespace(t *testing.T) {
	fixture := newFixture(t, 1)
	actor := fixture.actors[0]
	missing := mutation(MutationUpdate, uuid.New(), ObjectNote, 1)
	missing.BlobHash = fixture.blob
	result, err := actor.Submit(context.Background(), missing)
	if err != nil || result.Conflict != ConflictObjectMissing {
		t.Fatalf("conflict=%#v err=%v", result, err)
	}
	page, err := actor.Pull(context.Background(), 0, 10)
	if err != nil || len(page.Changes) != 2 || page.Changes[0].ObjectID != ConflictRootID || page.Changes[1].ObjectID != ConflictRecoveredID {
		t.Fatalf("reserved changes=%#v err=%v", page, err)
	}
	copyID := uuid.New()
	copyMutation := mutation(MutationCreate, copyID, ObjectNote, 0)
	copyMutation.ParentID, copyMutation.Name, copyMutation.BlobHash = ptrID(ConflictRecoveredID), "Copy.md", fixture.blob
	if accepted, err := actor.Submit(context.Background(), copyMutation); err != nil || !accepted.Accepted || accepted.Cursor != 3 {
		t.Fatalf("conflict copy=%#v err=%v", accepted, err)
	}
	for _, id := range []uuid.UUID{ConflictRootID, ConflictRecoveredID} {
		remove := mutation(MutationDelete, id, ObjectFolder, 1)
		if _, err := actor.Submit(context.Background(), remove); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("reserved folder mutation %s err=%v", id, err)
		}
	}
}

func TestReservedNamespaceFailsClosedOnMissingHistory(t *testing.T) {
	fixture := newFixture(t, 1)
	actor := fixture.actors[0]
	first := mutation(MutationUpdate, uuid.New(), ObjectNote, 1)
	first.BlobHash = fixture.blob
	if _, err := actor.Submit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`DROP TRIGGER sync_change_log_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`DELETE FROM sync_change_log WHERE user_id=? AND object_id=?`, fixture.users[0][:], ConflictRootID[:]); err != nil {
		t.Fatal(err)
	}
	second := mutation(MutationUpdate, uuid.New(), ObjectNote, 1)
	second.BlobHash = fixture.blob
	if _, err := actor.Submit(context.Background(), second); err == nil || !strings.Contains(err.Error(), "history is corrupt") {
		t.Fatalf("corrupt reserved history accepted: %v", err)
	}
}

func TestAllPersistedConflictPaths(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	actor := f.actors[0]
	ctx := context.Background()
	folderA := newID(t)
	mustAccepted(t, actor, createFolder(folderA, "A", nil))
	folderB := newID(t)
	mustAccepted(t, actor, createFolder(folderB, "B", &folderA))
	note := newID(t)
	mustAccepted(t, actor, createNote(note, "One.md", &folderA, f.blob))
	other := newID(t)
	mustAccepted(t, actor, createNote(other, "Other.md", &folderA, f.blob))

	cases := []struct {
		name    string
		request Mutation
		code    ConflictCode
	}{
		{"object exists", createFolderWithOperation(folderA, "Again", nil), ConflictObjectExists},
		{"object missing", updateNote(newID(t), 1, f.blob2), ConflictObjectMissing},
		{"base mismatch", updateNote(note, 99, f.blob2), ConflictBaseRevisionMismatch},
		{"parent unavailable", createFolder(newID(t), "Child", ptrID(newID(t))), ConflictParentUnavailable},
		{"path collision", createNote(newID(t), "One.md", &folderA, f.blob), ConflictPathCollision},
		{"folder not empty", deleteObject(folderA, ObjectFolder, 1), ConflictFolderNotEmpty},
		{"folder cycle", moveObject(folderA, ObjectFolder, 1, "A", &folderB), ConflictFolderCycle},
		{"type mismatch", updateNote(folderB, 1, f.blob2), ConflictTypeMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := actor.Submit(ctx, tc.request)
			if err != nil || result.Accepted || result.Conflict != tc.code {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if tc.code == ConflictTypeMismatch {
				if result.Canonical == nil || result.Canonical.ObjectType != ObjectFolder || result.Canonical.Revision != 1 || result.Canonical.Name != "B" {
					t.Fatalf("type mismatch canonical=%#v", result.Canonical)
				}
				replayed, replayErr := actor.Submit(ctx, tc.request)
				if replayErr != nil || replayed.Canonical == nil || replayed.Canonical.ObjectType != result.Canonical.ObjectType || replayed.Canonical.Revision != result.Canonical.Revision || replayed.Canonical.Name != result.Canonical.Name || replayed.Canonical.Deleted != result.Canonical.Deleted || ((replayed.Canonical.ParentID == nil) != (result.Canonical.ParentID == nil)) || (replayed.Canonical.ParentID != nil && *replayed.Canonical.ParentID != *result.Canonical.ParentID) || len(replayed.Canonical.BlobHash) != 0 || replayed.Conflict != ConflictTypeMismatch {
					t.Fatalf("type mismatch replay=%#v err=%v", replayed, replayErr)
				}
			}
		})
	}
	deleted := mustAccepted(t, actor, deleteObject(note, ObjectNote, 1))
	result, err := actor.Submit(ctx, updateNote(note, deleted.Revision, f.blob2))
	if err != nil || result.Conflict != ConflictObjectDeleted {
		t.Fatalf("deleted conflict=%#v %v", result, err)
	}
	var conflicts, changes int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_operations WHERE result='conflict'").Scan(&conflicts); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_change_log").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if conflicts != 9 || changes != 7 {
		t.Errorf("conflicts=%d changes=%d", conflicts, changes)
	}
	var proposed string
	if err := f.db.QueryRow("SELECT proposed_name FROM sync_operations WHERE conflict_code='path_collision'").Scan(&proposed); err != nil || proposed != "One.md" {
		t.Errorf("proposed intent=%q %v", proposed, err)
	}
}

func TestBlobActorValidationAndInvalidInputsDoNotPersist(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	ctx := context.Background()
	actor := f.actors[0]
	request := createNote(newID(t), "Missing.md", nil, bytes.Repeat([]byte{99}, 32))
	if _, err := actor.Submit(ctx, request); !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("blob error=%v", err)
	}
	bad := createFolder(newID(t), "CON", nil)
	if _, err := actor.Submit(ctx, bad); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("name error=%v", err)
	}
	if _, err := actor.Pull(ctx, 0, 501); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("limit error=%v", err)
	}
	if _, err := f.db.Exec(`UPDATE devices SET status='revoked',revoked_at_ms=created_at_ms,updated_at_ms=created_at_ms
		WHERE user_id=? AND id=?`, f.users[0][:], f.devices[0][:]); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.Submit(ctx, createFolder(newID(t), "Nope", nil)); !errors.Is(err, ErrInactiveActor) {
		t.Fatalf("actor error=%v", err)
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_operations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("operations=%d err=%v", count, err)
	}
}

func TestCreateRequiresZeroBaseAndInvalidRequestDoesNotPersist(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	request := createFolder(newID(t), "InvalidBase", nil)
	request.BaseRevision = 7
	if _, err := f.actors[0].Submit(context.Background(), request); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nonzero create base error = %v", err)
	}
	move := moveObject(newID(t), ObjectFolder, 0, "InvalidMove", nil)
	if _, err := f.actors[0].Submit(context.Background(), move); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero-base move error = %v", err)
	}
	move.BaseRevision = 1
	move.BlobHash = clone(f.blob)
	if _, err := f.actors[0].Submit(context.Background(), move); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blob-bearing move error = %v", err)
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_operations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid create persisted %d operations: %v", count, err)
	}
}

func TestBlobEntitlementIsTenantBound(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	if _, err := f.db.Exec("DELETE FROM user_content_blobs WHERE user_id=? AND hash=?", f.users[1][:], f.blob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.actors[0].Submit(context.Background(), createNote(newID(t), "Owned.md", nil, f.blob)); err != nil {
		t.Fatalf("entitled tenant rejected: %v", err)
	}
	if _, err := f.actors[1].Submit(context.Background(), createNote(newID(t), "Foreign.md", nil, f.blob)); !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("unentitled tenant blob error = %v", err)
	}
}

func TestPortableFullPathReservedRootsAndMovedDescendants(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	actor := f.actors[0]
	if _, err := actor.Submit(context.Background(), createFolder(newID(t), "_Konflikte", nil)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reserved root error = %v", err)
	}

	var deepParent *uuid.UUID
	for i := 0; i < 3; i++ {
		id := newID(t)
		mustAccepted(t, actor, createFolder(id, strings.Repeat(string(rune('a'+i)), 170), deepParent))
		deepParent = &id
	}
	movedID := newID(t)
	mustAccepted(t, actor, createFolder(movedID, "MoveMe", nil))
	childID := newID(t)
	mustAccepted(t, actor, createFolder(childID, strings.Repeat("z", 180), &movedID))
	move := moveObject(movedID, ObjectFolder, 1, strings.Repeat("m", 100), deepParent)
	if _, err := actor.Submit(context.Background(), move); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized moved subtree error = %v", err)
	}
	tooDeepParentID := newID(t)
	mustAccepted(t, actor, createFolder(tooDeepParentID, strings.Repeat("p", 100), deepParent))
	if _, err := actor.Submit(context.Background(), createFolder(newID(t), strings.Repeat("q", 180), &tooDeepParentID)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized create path error = %v", err)
	}
}

func TestTenantIsolationSameObjectIDAndIndependentCursors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	shared := newID(t)
	one := mustAccepted(t, f.actors[0], createFolder(shared, "One", nil))
	two := mustAccepted(t, f.actors[1], createFolder(shared, "Two", nil))
	if one.Cursor != 1 || two.Cursor != 1 {
		t.Fatalf("cursors %d %d", one.Cursor, two.Cursor)
	}
	for i, actor := range f.actors {
		page, err := actor.Pull(context.Background(), 0, 0)
		if err != nil || len(page.Changes) != 1 || page.Changes[0].Name == []string{"One", "Two"}[1-i] {
			t.Fatalf("tenant %d page=%#v err=%v", i, page, err)
		}
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_objects WHERE object_id=?", shared[:]).Scan(&count); err != nil || count != 2 {
		t.Fatalf("shared objects=%d %v", count, err)
	}
}

func TestConcurrentIdenticalSubmitProducesOneMutation(t *testing.T) {
	f := newFixture(t, 1)
	request := createFolder(newID(t), "Concurrent", nil)
	actor := f.actors[0]
	var wg sync.WaitGroup
	results := make(chan SubmitResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := actor.Submit(context.Background(), request); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if !result.Accepted || result.Cursor != 1 {
			t.Errorf("result=%#v", result)
		}
	}
	var operations, changes int
	f.db.QueryRow("SELECT COUNT(*) FROM sync_operations").Scan(&operations)
	f.db.QueryRow("SELECT COUNT(*) FROM sync_change_log").Scan(&changes)
	if operations != 1 || changes != 1 {
		t.Errorf("operations=%d changes=%d", operations, changes)
	}
}

func TestConcurrentDifferentSubmitsConditionOnRevision(t *testing.T) {
	f := newFixture(t, 1)
	id := newID(t)
	mustAccepted(t, f.actors[0], createNote(id, "Race.md", nil, f.blob))
	requests := []Mutation{updateNote(id, 1, f.blob2), updateNote(id, 1, f.blob)}
	results := make(chan SubmitResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, request := range requests {
		wg.Add(1)
		go func(request Mutation) {
			defer wg.Done()
			result, err := f.actors[0].Submit(context.Background(), request)
			results <- result
			errs <- err
		}(request)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var accepted, conflicted int
	for result := range results {
		if result.Accepted {
			accepted++
		}
		if result.Conflict == ConflictBaseRevisionMismatch {
			conflicted++
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Errorf("accepted=%d conflicted=%d", accepted, conflicted)
	}
}

func TestSubmitFailureRollsBackObjectAndCursor(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_test_operation BEFORE INSERT ON sync_operations
		WHEN NEW.proposed_name = 'Rollback' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.actors[0].Submit(context.Background(), createFolder(newID(t), "Rollback", nil)); err == nil {
		t.Fatal("injected failure accepted")
	}
	var objects, cursors int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM sync_objects").Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT COUNT(*) FROM user_cursor_counters").Scan(&cursors); err != nil {
		t.Fatal(err)
	}
	if objects != 0 || cursors != 0 {
		t.Errorf("rollback objects=%d counters=%d", objects, cursors)
	}
}

func TestAccountDeletionCascadesSyncHistoryButDirectHistoryDeleteFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	mustAccepted(t, f.actors[0], createFolder(newID(t), "History", nil))
	if _, err := f.db.Exec("DELETE FROM sync_change_log WHERE user_id=?", f.users[0][:]); err == nil {
		t.Fatal("direct change-log deletion succeeded")
	}
	if _, err := f.db.Exec("DELETE FROM users WHERE id=?", f.users[0][:]); err != nil {
		t.Fatalf("account purge failed: %v", err)
	}
	for _, table := range []string{"devices", "user_content_blobs", "sync_objects", "sync_operations", "sync_object_versions", "sync_change_log"} {
		var count int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s rows after account purge = %d, error %v", table, count, err)
		}
	}
}

func TestBlobLookupDatabaseFailureIsNotMasked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	if _, err := f.db.Exec("ALTER TABLE user_content_blobs RENAME TO unavailable_blob_entitlements"); err != nil {
		t.Fatal(err)
	}
	_, err := f.actors[0].Submit(context.Background(), createNote(newID(t), "Failure.md", nil, f.blob))
	if err == nil || errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("database failure was masked as blob unavailability: %v", err)
	}
}

func TestImmutableSchemaAndBlobConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	mustAccepted(t, f.actors[0], createFolder(newID(t), "Immutable", nil))
	if _, err := f.db.Exec("UPDATE sync_object_versions SET name='changed'"); err == nil {
		t.Error("immutable version updated")
	}
	if _, err := f.db.Exec("INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms) VALUES('12345678901234567890123456789012',1,1,1)"); err == nil {
		t.Error("TEXT blob hash accepted")
	}
}

func TestPortableNamingConformanceVectors(t *testing.T) {
	t.Parallel()
	valid := []string{"Note.md", "Café", "CONifer", "計画"}
	for _, name := range valid {
		if _, _, err := normalizeName(name); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
	}
	invalid := []string{"", "CON", "aux.txt", "bad/name", "trail.", "bad\x00name"}
	for _, name := range invalid {
		if _, _, err := normalizeName(name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid %q: %v", name, err)
		}
	}
	_, one, _ := normalizeName("Straße")
	_, two, _ := normalizeName("STRASSE")
	if one != two {
		t.Errorf("fold mismatch %q %q", one, two)
	}
}

type fixture struct {
	db             *sql.DB
	users, devices []uuid.UUID
	actors         []*ActorService
	blob, blob2    []byte
}

func newFixture(t *testing.T, count int) *fixture {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "sync.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, fixedClock{time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, blob: bytes.Repeat([]byte{1}, 32), blob2: bytes.Repeat([]byte{2}, 32)}
	for _, blob := range [][]byte{f.blob, f.blob2} {
		if _, err := db.Exec("INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms) VALUES(?,1,1,1)", blob); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		user, device := newID(t), newID(t)
		if _, err := db.Exec("INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms) VALUES(?,?,?,?,1,'active',1)", user[:], user.String()+"@example.com", user.String()+"@example.com", "hash"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms) VALUES(?,?,?,'active',1,1)", user[:], device[:], "Device"); err != nil {
			t.Fatal(err)
		}
		for _, blob := range [][]byte{f.blob, f.blob2} {
			if _, err := db.Exec("INSERT INTO user_content_blobs(user_id,hash,entitled_at_ms) VALUES(?,?,1)", user[:], blob); err != nil {
				t.Fatal(err)
			}
		}
		actor, err := service.ForActor(user, device)
		if err != nil {
			t.Fatal(err)
		}
		f.users = append(f.users, user)
		f.devices = append(f.devices, device)
		f.actors = append(f.actors, actor)
	}
	return f
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }
func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func ptrID(id uuid.UUID) *uuid.UUID { return &id }
func mutation(kind MutationKind, id uuid.UUID, typ ObjectType, base uint64) Mutation {
	return Mutation{OperationID: mustNewID(), Kind: kind, ObjectID: id, ObjectType: typ, BaseRevision: base}
}
func mustNewID() uuid.UUID { id, _ := uuid.NewV7(); return id }
func createFolder(id uuid.UUID, name string, parent *uuid.UUID) Mutation {
	m := mutation(MutationCreate, id, ObjectFolder, 0)
	m.Name = name
	m.ParentID = parent
	return m
}
func createFolderWithOperation(id uuid.UUID, name string, parent *uuid.UUID) Mutation {
	return createFolder(id, name, parent)
}
func createNote(id uuid.UUID, name string, parent *uuid.UUID, blob []byte) Mutation {
	m := mutation(MutationCreate, id, ObjectNote, 0)
	m.Name = name
	m.ParentID = parent
	m.BlobHash = clone(blob)
	return m
}
func updateNote(id uuid.UUID, base uint64, blob []byte) Mutation {
	m := mutation(MutationUpdate, id, ObjectNote, base)
	m.BlobHash = clone(blob)
	return m
}
func moveObject(id uuid.UUID, typ ObjectType, base uint64, name string, parent *uuid.UUID) Mutation {
	m := mutation(MutationMove, id, typ, base)
	m.Name = name
	m.ParentID = parent
	return m
}
func deleteObject(id uuid.UUID, typ ObjectType, base uint64) Mutation {
	return mutation(MutationDelete, id, typ, base)
}
func mustAccepted(t *testing.T, actor *ActorService, m Mutation) SubmitResult {
	t.Helper()
	r, e := actor.Submit(context.Background(), m)
	if e != nil || !r.Accepted {
		t.Fatalf("submit=%#v err=%v", r, e)
	}
	return r
}
