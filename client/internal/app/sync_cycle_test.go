package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/google/uuid"
)

type memorySyncServer struct {
	blobs   map[[32]byte][]byte
	results map[uuid.UUID]clientsync.Result
	states  map[uuid.UUID]clientsync.Change
	changes []clientsync.Change
	pullErr error
	maxPull int
}

type memoryRemote struct{ server *memorySyncServer }

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

func (r *memoryRemote) PutBlob(_ context.Context, h [32]byte, b []byte) error {
	if sha256.Sum256(b) != h {
		return errors.New("hash mismatch")
	}
	r.server.blobs[h] = append([]byte(nil), b...)
	return nil
}
func (r *memoryRemote) ResolveBlob(_ context.Context, h [32]byte) ([]byte, error) {
	b, ok := r.server.blobs[h]
	if !ok {
		return nil, errors.New("missing blob")
	}
	return append([]byte(nil), b...), nil
}
func (s *memorySyncServer) ensureConflictNamespace() {
	if _, exists := s.states[clientsync.ConflictRootID]; exists {
		return
	}
	for _, reserved := range []clientsync.Change{{ObjectID: clientsync.ConflictRootID, ObjectType: clientsync.Folder, Revision: 1, Name: clientsync.ConflictRootName}, {ObjectID: clientsync.ConflictRecoveredID, ObjectType: clientsync.Folder, Revision: 1, ParentID: ptrUUID(clientsync.ConflictRootID), Name: clientsync.ConflictRecoveredName}} {
		reserved.Cursor = uint64(len(s.changes) + 1)
		reserved.OperationID = uuid.Must(uuid.NewV7())
		reserved.Mutation = clientsync.Create
		s.states[reserved.ObjectID] = reserved
		s.changes = append(s.changes, reserved)
	}
}

func (r *memoryRemote) Submit(_ context.Context, m clientsync.Mutation) (clientsync.Result, error) {
	if result, ok := r.server.results[m.OperationID]; ok {
		return result, nil
	}
	state := r.server.states[m.ObjectID]
	if m.Kind == clientsync.Create {
		for id, existing := range r.server.states {
			if id != m.ObjectID && !existing.Deleted && existing.Name == m.Name && ((existing.ParentID == nil && m.ParentID == nil) || (existing.ParentID != nil && m.ParentID != nil && *existing.ParentID == *m.ParentID)) {
				r.server.ensureConflictNamespace()
				result := clientsync.Result{Conflict: "path_collision"}
				r.server.results[m.OperationID] = result
				return result, nil
			}
		}
	}
	if m.Kind != clientsync.Create && state.Revision != m.BaseRevision {
		r.server.ensureConflictNamespace()
		canonical := &clientsync.CanonicalState{ObjectType: state.ObjectType, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name, BlobHash: append([]byte(nil), state.BlobHash...), Deleted: state.Deleted}
		code := "base_revision_mismatch"
		if state.Deleted {
			code = "object_deleted"
		}
		result := clientsync.Result{Conflict: code, Canonical: canonical}
		r.server.results[m.OperationID] = result
		return result, nil
	}
	if m.Kind == clientsync.Create {
		state = clientsync.Change{ObjectID: m.ObjectID, ObjectType: m.ObjectType, Revision: 1, ParentID: m.ParentID, Name: m.Name, BlobHash: append([]byte(nil), m.BlobHash...)}
	} else {
		state.Revision = m.BaseRevision + 1
		switch m.Kind {
		case clientsync.Update:
			state.BlobHash = append([]byte(nil), m.BlobHash...)
		case clientsync.Move:
			state.ParentID, state.Name = m.ParentID, m.Name
		case clientsync.Delete:
			state.Deleted = true
		}
	}
	cursor := uint64(len(r.server.changes) + 1)
	state.Cursor = cursor
	state.OperationID = m.OperationID
	state.Mutation = m.Kind
	r.server.states[m.ObjectID] = state
	r.server.changes = append(r.server.changes, state)
	result := clientsync.Result{Accepted: true, Revision: state.Revision, Cursor: cursor}
	r.server.results[m.OperationID] = result
	return result, nil
}
func (r *memoryRemote) Pull(_ context.Context, after uint64, limit int) (remotehttp.PullPage, error) {
	if r.server.pullErr != nil {
		err := r.server.pullErr
		r.server.pullErr = nil
		return remotehttp.PullPage{}, err
	}
	page := remotehttp.PullPage{NextCursor: after}
	if r.server.maxPull > 0 && limit > r.server.maxPull {
		limit = r.server.maxPull
	}
	for _, change := range r.server.changes {
		if change.Cursor > after && len(page.Changes) < limit {
			page.Changes = append(page.Changes, change)
			page.NextCursor = change.Cursor
		}
	}
	page.HasMore = int(page.NextCursor) < len(r.server.changes)
	return page, nil
}

func TestSyncOnceConvergesTwoRootsForRootNoteCreateUpdate(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	doc, _, err := a.CreateNote(ctx, "N.md", "first\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, err := b.ReadNote(ctx, "N.md")
	if err != nil || bDoc.ID != doc.ID {
		t.Fatalf("b doc=%#v err=%v", bDoc, err)
	}
	if _, _, err := b.SaveNote(ctx, "N.md", bDoc.Revision, "second\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bytesA, errA := os.ReadFile(filepath.Join(rootA, "N.md"))
	bytesB, errB := os.ReadFile(filepath.Join(rootB, "N.md"))
	if errA != nil || errB != nil || string(bytesA) != string(bytesB) {
		t.Fatalf("A=%q B=%q errors=%v/%v", bytesA, bytesB, errA, errB)
	}
	storeA, _ := clientsync.NewStore(a.index)
	storeB, _ := clientsync.NewStore(b.index)
	cursorA, _ := storeA.ConfirmedCursor(ctx)
	cursorB, _ := storeB.ConfirmedCursor(ctx)
	if cursorA != 2 || cursorB != 2 {
		t.Fatalf("cursors=%d/%d", cursorA, cursorB)
	}
}

func TestSyncOnceMaterializesConcurrentNoteUpdate(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	doc, _, err := a.CreateNote(ctx, "N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ := a.ReadNote(ctx, "N.md")
	if _, _, err := a.SaveNote(ctx, "N.md", aDoc.Revision, "canonical\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "N.md")
	if _, _, err := b.SaveNote(ctx, "N.md", bDoc.Revision, "local conflict\n", nil); err != nil {
		t.Fatal(err)
	}
	server.maxPull = 1
	server.pullErr = remotehttp.ErrRetryable
	if err := b.SyncOnce(ctx, remote); !errors.Is(err, remotehttp.ErrRetryable) {
		t.Fatalf("interrupted conflict sync err=%v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md")); len(matches) != 0 {
		t.Fatalf("copy published before canonical pull: %v", matches)
	}
	stagedDoc, _ := b.ReadNote(ctx, "N.md")
	if _, _, err := b.SaveNote(ctx, "N.md", stagedDoc.Revision, "newer must not be lost\n", nil); !errors.Is(err, ErrConflictMaterializationActive) {
		t.Fatalf("edit during materialization err=%v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	canonical, err := b.ReadNote(ctx, "N.md")
	if err != nil || canonical.ID != doc.ID || !strings.Contains(canonical.Body, "canonical") {
		t.Fatalf("canonical=%#v err=%v", canonical, err)
	}
	matches, err := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("conflict copies=%v err=%v", matches, err)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("local conflict")) || !bytes.Contains(copyBytes, []byte("base_revision_mismatch")) {
		t.Fatalf("copy=%q err=%v", copyBytes, err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, exists := server.states[clientsync.ConflictRecoveredID]; !exists {
		t.Fatal("reserved conflict namespace not synchronized")
	}
	if staged, _ := filepath.Glob(filepath.Join(rootB, ".remember", "conflicts", "materializations", "*")); len(staged) != 0 {
		t.Fatalf("completed conflict staging remains: %v", staged)
	}
}

func TestSyncOnceMaterializesEditAgainstRemoteDelete(t *testing.T) {
	t.Run("paged crash resume", func(t *testing.T) { runEditAgainstRemoteDelete(t, 1, true) })
	t.Run("batched crash after reconcile", func(t *testing.T) { runEditAgainstRemoteDelete(t, 0, true) })
}

func runEditAgainstRemoteDelete(t *testing.T, maxPull int, crash bool) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	doc, _, err := a.CreateNote(ctx, "N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ := a.ReadNote(ctx, "N.md")
	if _, _, err := a.MoveNote(ctx, "N.md", "Moved.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ = a.ReadNote(ctx, "Moved.md")
	if _, _, err := a.SaveNote(ctx, "Moved.md", aDoc.Revision, "remote intermediate\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ = a.ReadNote(ctx, "Moved.md")
	if _, err := a.DeleteNote(ctx, "Moved.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "N.md")
	if _, _, err := b.SaveNote(ctx, "N.md", bDoc.Revision, "edited after remote delete\n", nil); err != nil {
		t.Fatal(err)
	}
	server.maxPull = maxPull
	if crash {
		if maxPull == 0 {
			testHookAfterNoteApplyReconcile = func() {
				testHookAfterNoteApplyReconcile = nil
				panic("simulated crash after conflicted delete reconcile")
			}
		} else {
			testHookAfterApplyPublication = func() {
				testHookAfterApplyPublication = nil
				panic("simulated crash after conflicted delete publication")
			}
		}
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatalf("expected simulated crash")
				}
			}()
			if err := b.SyncOnce(ctx, remote); err != nil {
				t.Fatalf("sync before simulated crash: %v", err)
			}
		}()
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		b, _, err = Open(ctx, rootB)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadNote(ctx, "N.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("deleted original remains: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("conflict copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("edited after remote delete")) || !bytes.Contains(copyBytes, []byte("object_deleted")) {
		t.Fatalf("copy=%q err=%v", copyBytes, err)
	}
	trash, _ := filepath.Glob(filepath.Join(rootB, ".remember", "trash", doc.ID.String()+"-*.md"))
	if len(trash) != 1 {
		t.Fatalf("recoverable delete trash=%v", trash)
	}
	trashBytes, _ := os.ReadFile(trash[0])
	if !bytes.Contains(trashBytes, []byte("edited after remote delete")) {
		t.Fatalf("trash lost edit: %q", trashBytes)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	visible, err := frontmatter.Inspect(copyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if state := server.states[visible.NoteID]; state.ObjectType != clientsync.Note || state.Deleted {
		t.Fatalf("conflict copy not synchronized: %#v", state)
	}
}

func TestSyncOnceRebasesLocalDeleteAgainstRemoteEdit(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	doc, _, err := a.CreateNote(ctx, "N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "N.md")
	if _, err := b.DeleteNote(ctx, "N.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	aDoc, _ := a.ReadNote(ctx, "N.md")
	if _, _, err := a.SaveNote(ctx, "N.md", aDoc.Revision, "remote edit to rescue\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	server.maxPull = 1
	testHookBeforeConflictCompletion = func() { testHookBeforeConflictCompletion = nil; panic("simulated crash before delete rebase") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected delete rebase crash")
			}
		}()
		_ = b.SyncOnce(ctx, remote)
	}()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadNote(ctx, "N.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("local delete was undone: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("rescued copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("remote edit to rescue")) {
		t.Fatalf("rescued copy=%q err=%v", copyBytes, err)
	}
	var dependentKind clientsync.MutationKind
	var dependencyObject string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT d.mutation,p.object_id FROM sync_outbox d JOIN sync_outbox_dependencies x ON x.operation_id=d.operation_id JOIN sync_outbox p ON p.operation_id=x.dependency_operation_id WHERE d.object_id=? AND d.mutation='delete' ORDER BY d.sequence DESC LIMIT 1`, doc.ID.String()).Scan(&dependentKind, &dependencyObject)
	}); err != nil || dependentKind != clientsync.Delete || dependencyObject == doc.ID.String() {
		t.Fatalf("rebased dependency kind=%s object=%s err=%v", dependentKind, dependencyObject, err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	state := server.states[doc.ID]
	if !state.Deleted || state.Revision != 3 {
		t.Fatalf("rebased tombstone=%#v", state)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if rescued := server.states[inspection.NoteID]; rescued.Deleted || rescued.ObjectType != clientsync.Note {
		t.Fatalf("rescued remote state=%#v", rescued)
	}
}

func TestSyncOnceMaterializesNoteCreatePathCollision(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	winner, _, err := a.CreateNote(ctx, "Same.md", "remote winner\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	local, _, err := b.CreateNote(ctx, "Same.md", "local colliding note\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	server.pullErr = remotehttp.ErrRetryable
	if err := b.SyncOnce(ctx, remote); !errors.Is(err, remotehttp.ErrRetryable) {
		t.Fatalf("interrupted collision sync=%v", err)
	}
	if _, err := b.ReadNote(ctx, "Same.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("colliding source not evacuated: %v", err)
	}
	if copies, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md")); len(copies) != 0 {
		t.Fatalf("collision copy published before winner pull: %v", copies)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remote); err != nil {
		snapshot, _ := b.index.ReadSnapshot(ctx)
		t.Fatalf("collision resume=%v snapshot=%#v", err, snapshot.Objects)
	}
	winnerDoc, err := b.ReadNote(ctx, "Same.md")
	if err != nil || winnerDoc.ID != winner.ID || !strings.Contains(winnerDoc.Body, "remote winner") {
		t.Fatalf("winner=%#v err=%v", winnerDoc, err)
	}
	copies, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(copies) != 1 {
		t.Fatalf("collision copies=%v", copies)
	}
	copyBytes, _ := os.ReadFile(copies[0])
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil || inspection.NoteID == local.ID || !bytes.Contains(copyBytes, []byte("local colliding note")) {
		t.Fatalf("collision copy=%q inspection=%#v err=%v", copyBytes, inspection, err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if rescued := server.states[inspection.NoteID]; rescued.ObjectType != clientsync.Note || rescued.Deleted {
		t.Fatalf("rescued collision state=%#v", rescued)
	}
}

type cycleRemote struct {
	events    []string
	ambiguous bool
	submitted []uuid.UUID
	pull      remotehttp.PullPage
	blobs     map[[32]byte][]byte
}

func (r *cycleRemote) PutBlob(_ context.Context, h [32]byte, b []byte) error {
	r.events = append(r.events, "put")
	if sha256.Sum256(b) != h {
		return errors.New("bad staged bytes")
	}
	return nil
}
func (r *cycleRemote) Submit(_ context.Context, m clientsync.Mutation) (clientsync.Result, error) {
	r.events = append(r.events, "submit")
	r.submitted = append(r.submitted, m.OperationID)
	if r.ambiguous {
		r.ambiguous = false
		return clientsync.Result{}, remotehttp.ErrRetryable
	}
	return clientsync.Result{Accepted: true, Revision: m.BaseRevision + 1, Cursor: 1}, nil
}
func (r *cycleRemote) Pull(_ context.Context, after uint64, _ int) (remotehttp.PullPage, error) {
	r.events = append(r.events, "pull")
	if len(r.pull.Changes) == 0 {
		return remotehttp.PullPage{NextCursor: after}, nil
	}
	return r.pull, nil
}
func (r *cycleRemote) ResolveBlob(_ context.Context, h [32]byte) ([]byte, error) {
	r.events = append(r.events, "get")
	b, ok := r.blobs[h]
	if !ok {
		return nil, errors.New("missing blob")
	}
	return b, nil
}

func TestSyncOnceConvergesNestedFolderAndNote(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.CreateFolder(ctx, "Folder"); err != nil {
		t.Fatal(err)
	}
	doc, _, err := a.CreateNote(ctx, "Folder/N.md", "nested\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadNote(ctx, "Folder/N.md")
	if err != nil || got.ID != doc.ID {
		t.Fatalf("note=%#v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "Folder", ".remember-apply-nonce")); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}
}

func TestSyncOnceConvergesRootNoteMoveAndDelete(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	doc, _, err := a.CreateNote(ctx, "N.md", "first\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, err := b.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	moved, _, err := b.MoveNote(ctx, "N.md", "Moved.md", bDoc.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if aDoc, err := a.ReadNote(ctx, "Moved.md"); err != nil || aDoc.ID != doc.ID || moved.ID != doc.ID {
		t.Fatalf("moved=%#v a=%#v err=%v", moved, aDoc, err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "N.md")); !os.IsNotExist(err) {
		t.Fatalf("old path=%v", err)
	}
	aDoc, _ := a.ReadNote(ctx, "Moved.md")
	if _, err := a.DeleteNote(ctx, "Moved.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "Moved.md")); !os.IsNotExist(err) {
		t.Fatalf("remote delete=%v", err)
	}
	storeA, _ := clientsync.NewStore(a.index)
	storeB, _ := clientsync.NewStore(b.index)
	cursorA, _ := storeA.ConfirmedCursor(ctx)
	cursorB, _ := storeB.ConfirmedCursor(ctx)
	if cursorA != 3 || cursorB != 3 {
		t.Fatalf("cursors=%d/%d", cursorA, cursorB)
	}
}

func TestSyncOnceRetriesAmbiguousAttemptWithSameOperationAndBlobFirst(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "N.md"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	remote := &cycleRemote{ambiguous: true}
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, remotehttp.ErrRetryable) {
		t.Fatalf("first err=%v", err)
	}
	if len(remote.events) < 2 || remote.events[0] != "put" || remote.events[1] != "submit" {
		t.Fatalf("events=%v", remote.events)
	}
	remote.events = nil
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if len(remote.submitted) != 2 || remote.submitted[0] != remote.submitted[1] {
		t.Fatalf("submitted=%v", remote.submitted)
	}
	if len(remote.events) < 2 || remote.events[0] != "put" || remote.events[1] != "submit" {
		t.Fatalf("retry events=%v", remote.events)
	}
	store, _ := clientsync.NewStore(core.index)
	if ready, err := store.ListReady(ctx, 10); err != nil || len(ready) != 0 {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
}

func TestSyncOnceRejectsFolderUpdateBeforePersistingPlan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	remote := &cycleRemote{pull: remotehttp.PullPage{Changes: []clientsync.Change{{Cursor: 1, Mutation: clientsync.Update, OperationID: uuid.Must(uuid.NewV7()), ObjectID: uuid.New(), ObjectType: clientsync.Folder, Revision: 2, Name: "Moved"}}, NextCursor: 1}}
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnsupportedPullPage) {
		t.Fatalf("error=%v", err)
	}
	store, _ := clientsync.NewStore(core.index)
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestSyncOnceDoesNotAdvanceCursorWhenRemoteApplyFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	object := uuid.New()
	content, _ := frontmatter.EnsureIdentity([]byte("remote\n"), object)
	hash := sha256.Sum256(content.Markdown)
	op := uuid.Must(uuid.NewV7())
	remote := &cycleRemote{pull: remotehttp.PullPage{Changes: []clientsync.Change{{Cursor: 1, Mutation: clientsync.Create, OperationID: op, ObjectID: object, ObjectType: clientsync.Note, Revision: 1, Name: "Remote.md", BlobHash: hash[:]}}, NextCursor: 1}, blobs: map[[32]byte][]byte{hash: []byte("wrong")}}
	if err := core.SyncOnce(ctx, remote); err == nil {
		t.Fatal("invalid remote blob applied")
	}
	store, _ := clientsync.NewStore(core.index)
	if cursor, err := store.ConfirmedCursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active == nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Remote.md")); !os.IsNotExist(err) {
		t.Fatalf("remote file=%v", err)
	}
}
