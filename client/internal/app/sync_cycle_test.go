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
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/client/internal/repository"
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
	if m.Kind != clientsync.Create && state.Revision == 0 {
		r.server.ensureConflictNamespace()
		result := clientsync.Result{Conflict: "object_missing"}
		r.server.results[m.OperationID] = result
		return result, nil
	}
	if (m.Kind == clientsync.Create || m.Kind == clientsync.Move) && m.ParentID != nil {
		parent, exists := r.server.states[*m.ParentID]
		if !exists || parent.Deleted || parent.ObjectType != clientsync.Folder {
			r.server.ensureConflictNamespace()
			result := clientsync.Result{Conflict: "parent_unavailable"}
			if m.Kind == clientsync.Move {
				result.Canonical = &clientsync.CanonicalState{ObjectType: state.ObjectType, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name, BlobHash: append([]byte(nil), state.BlobHash...), Deleted: state.Deleted}
			}
			r.server.results[m.OperationID] = result
			return result, nil
		}
	}
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
	if m.Kind != clientsync.Create && state.ObjectType != m.ObjectType {
		r.server.ensureConflictNamespace()
		canonical := &clientsync.CanonicalState{ObjectType: state.ObjectType, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name, BlobHash: append([]byte(nil), state.BlobHash...), Deleted: state.Deleted}
		result := clientsync.Result{Conflict: "type_mismatch", Canonical: canonical}
		r.server.results[m.OperationID] = result
		return result, nil
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
	if m.Kind == clientsync.Move && state.ObjectType == clientsync.Folder && m.ParentID != nil {
		cursor := m.ParentID
		for depth := 0; cursor != nil && depth < 1000; depth++ {
			if *cursor == m.ObjectID {
				r.server.ensureConflictNamespace()
				canonical := &clientsync.CanonicalState{ObjectType: clientsync.Folder, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name}
				result := clientsync.Result{Conflict: "folder_cycle", Canonical: canonical}
				r.server.results[m.OperationID] = result
				return result, nil
			}
			parent, exists := r.server.states[*cursor]
			if !exists || parent.Deleted {
				break
			}
			cursor = parent.ParentID
		}
	}
	if m.Kind == clientsync.Delete && state.ObjectType == clientsync.Folder {
		for _, child := range r.server.states {
			if child.ParentID != nil && *child.ParentID == m.ObjectID && !child.Deleted {
				r.server.ensureConflictNamespace()
				canonical := &clientsync.CanonicalState{ObjectType: clientsync.Folder, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name}
				result := clientsync.Result{Conflict: "folder_not_empty", Canonical: canonical}
				r.server.results[m.OperationID] = result
				return result, nil
			}
		}
	}
	if m.Kind == clientsync.Move {
		for id, existing := range r.server.states {
			if id != m.ObjectID && !existing.Deleted && existing.Name == m.Name && ((existing.ParentID == nil && m.ParentID == nil) || (existing.ParentID != nil && m.ParentID != nil && *existing.ParentID == *m.ParentID)) {
				r.server.ensureConflictNamespace()
				canonical := &clientsync.CanonicalState{ObjectType: state.ObjectType, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name, BlobHash: append([]byte(nil), state.BlobHash...), Deleted: state.Deleted}
				result := clientsync.Result{Conflict: "path_collision", Canonical: canonical}
				r.server.results[m.OperationID] = result
				return result, nil
			}
		}
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

func TestSyncOnceFailsClosedForNoteToFolderTypeMismatch(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	created, _, err := core.CreateNote(ctx, "N.md", "original\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	note, err := core.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.SaveNote(ctx, "N.md", note.Revision, "local bytes survive\n", nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	state := server.states[created.ID]
	state.ObjectType = clientsync.Folder
	state.Name = "CanonicalFolder"
	state.BlobHash = nil
	server.states[created.ID] = state
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("type mismatch sync err=%v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "N.md"))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("note bytes changed=%t err=%v", bytes.Equal(after, before), err)
	}
	snapshot, err := core.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, object := range snapshot.Objects {
		if object.ID == created.ID {
			found = object.Type == localindex.ObjectNote && object.RelativePath == "N.md"
		}
	}
	if !found {
		t.Fatal("note identity changed during type mismatch")
	}
	store, _ := clientsync.NewStore(core.index)
	conflicts, err := store.ListConflicts(ctx, 10)
	if err != nil || len(conflicts) != 1 || conflicts[0].Code != "type_mismatch" || conflicts[0].Canonical == nil || conflicts[0].Canonical.ObjectType != clientsync.Folder {
		t.Fatalf("type mismatch conflict=%#v err=%v", conflicts, err)
	}
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("replayed type mismatch err=%v", err)
	}
	again, _ := os.ReadFile(filepath.Join(root, "N.md"))
	if !bytes.Equal(again, before) {
		t.Fatal("replayed type mismatch changed note")
	}
}

func TestSyncOnceFailsClosedForFolderToNoteTypeMismatch(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := core.CreateFolder(ctx, "F"); err != nil {
		t.Fatal(err)
	}
	folderSnapshot, err := core.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var folderID uuid.UUID
	for _, object := range folderSnapshot.Objects {
		if object.Type == localindex.ObjectFolder && object.RelativePath == "F" {
			folderID = object.ID
		}
	}
	if folderID == uuid.Nil {
		t.Fatal("folder id missing")
	}
	if _, _, err := core.CreateNote(ctx, "F/Child.md", "child survives\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	device, inode, err := repository.RootedFolderIdentity(root, "F")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "F"), filepath.Join(root, "Moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	childBefore, err := os.ReadFile(filepath.Join(root, "Moved", "Child.md"))
	if err != nil {
		t.Fatal(err)
	}
	canonicalBytes := []byte("canonical note bytes")
	canonicalHash := sha256.Sum256(canonicalBytes)
	server.blobs[canonicalHash] = canonicalBytes
	state := server.states[folderID]
	state.ObjectType = clientsync.Note
	state.Name = "Canonical.md"
	state.BlobHash = canonicalHash[:]
	server.states[folderID] = state
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("folder type mismatch sync err=%v", err)
	}
	if err := repository.VerifyRootedFolderIdentity(root, "Moved", device, inode); err != nil {
		t.Fatal(err)
	}
	childAfter, err := os.ReadFile(filepath.Join(root, "Moved", "Child.md"))
	if err != nil || !bytes.Equal(childAfter, childBefore) {
		t.Fatalf("folder child changed=%t err=%v", bytes.Equal(childAfter, childBefore), err)
	}
	if _, err := os.Stat(filepath.Join(root, "Canonical.md")); !os.IsNotExist(err) {
		t.Fatalf("canonical note applied across unresolved mismatch: %v", err)
	}
	store, _ := clientsync.NewStore(core.index)
	conflicts, err := store.ListConflicts(ctx, 10)
	if err != nil || len(conflicts) != 1 || conflicts[0].Code != "type_mismatch" || conflicts[0].Canonical == nil || conflicts[0].Canonical.ObjectType != clientsync.Note {
		t.Fatalf("folder mismatch conflict=%#v err=%v", conflicts, err)
	}
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
	if staged, _ := filepath.Glob(filepath.Join(rootB, ".remember", "conflicts", "materializations", "*")); len(staged) > 1 {
		t.Fatalf("unexpected conflict staging artifacts: %v", staged)
	} else if len(staged) == 1 {
		info, err := os.Stat(staged[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 || !strings.HasSuffix(staged[0], ".cleanup") {
			t.Fatalf("completed conflict bytes remain: %v", staged)
		}
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

func TestSyncOnceMaterializesNoteMovePathCollision(t *testing.T) {
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
	source, _, err := a.CreateNote(ctx, "Original.md", "canonical source\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	winner, _, err := a.CreateNote(ctx, "Taken.md", "path winner\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "Original.md")
	moved, _, err := b.MoveNote(ctx, "Original.md", "Taken.md", bDoc.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Taken.md", moved.Revision, "local moved edit\n", nil); err != nil {
		t.Fatal(err)
	}
	server.pullErr = remotehttp.ErrRetryable
	if err := b.SyncOnce(ctx, remote); !errors.Is(err, remotehttp.ErrRetryable) {
		store, _ := clientsync.NewStore(b.index)
		pending, _ := store.ListPending(ctx, 10)
		conflicts, _ := store.ListConflicts(ctx, 10)
		t.Fatalf("interrupted move collision=%v pending=%#v conflicts=%#v", err, pending, conflicts)
	}
	canonical, err := b.ReadNote(ctx, "Original.md")
	if err != nil || canonical.ID != source.ID || !strings.Contains(canonical.Body, "canonical source") {
		t.Fatalf("restored canonical=%#v err=%v", canonical, err)
	}
	if _, err := b.ReadNote(ctx, "Taken.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("attempted target not evacuated: %v", err)
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
	taken, err := b.ReadNote(ctx, "Taken.md")
	if err != nil || taken.ID != winner.ID {
		t.Fatalf("path winner=%#v err=%v", taken, err)
	}
	copies, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(copies) != 1 {
		t.Fatalf("move collision copies=%v", copies)
	}
	copyBytes, _ := os.ReadFile(copies[0])
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil || inspection.NoteID == source.ID || !bytes.Contains(copyBytes, []byte("local moved edit")) {
		t.Fatalf("move collision copy=%q inspection=%#v err=%v", copyBytes, inspection, err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if rescued := server.states[inspection.NoteID]; rescued.ObjectType != clientsync.Note || rescued.Deleted {
		t.Fatalf("remote rescued move=%#v", rescued)
	}
}

func TestSyncOnceMaterializesUpdateForMissingRemoteNote(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	doc, _, err := core.CreateNote(ctx, "Missing.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	delete(server.states, doc.ID)
	current, _ := core.ReadNote(ctx, "Missing.md")
	if _, _, err := core.SaveNote(ctx, "Missing.md", current.Revision, "local orphan edit\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictEvacuation = func() { testHookAfterConflictEvacuation = nil; panic("simulated crash after orphan evacuation") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected orphan evacuation crash")
			}
		}()
		_ = core.SyncOnce(ctx, remote)
	}()
	if _, err := core.ReadNote(ctx, "Missing.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("orphan source not evacuated: %v", err)
	}
	if copies, _ := filepath.Glob(filepath.Join(root, "_Konflikte", "Wiederhergestellt", "*.md")); len(copies) != 0 {
		t.Fatalf("orphan copy published before reserved pull: %v", copies)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core, _, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	copies, _ := filepath.Glob(filepath.Join(root, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(copies) != 1 {
		t.Fatalf("orphan copies=%v", copies)
	}
	content, _ := os.ReadFile(copies[0])
	inspection, err := frontmatter.Inspect(content)
	if err != nil || inspection.NoteID == doc.ID || !bytes.Contains(content, []byte("local orphan edit")) || !bytes.Contains(content, []byte("object_missing")) {
		t.Fatalf("orphan copy=%q inspection=%#v err=%v", content, inspection, err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if rescued := server.states[inspection.NoteID]; rescued.ObjectType != clientsync.Note || rescued.Deleted {
		t.Fatalf("remote orphan rescue=%#v", rescued)
	}
}

func TestSyncOnceMaterializesMoveForMissingRemoteNote(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	doc, _, err := core.CreateNote(ctx, "Original.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	delete(server.states, doc.ID)
	current, _ := core.ReadNote(ctx, "Original.md")
	moved, _, err := core.MoveNote(ctx, "Original.md", "Moved.md", current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.SaveNote(ctx, "Moved.md", moved.Revision, "moved orphan edit\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := core.ReadNote(ctx, "Moved.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("orphan move remains: %v", err)
	}
	if _, err := core.ReadNote(ctx, "Original.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("missing source was recreated: %v", err)
	}
	copies, _ := filepath.Glob(filepath.Join(root, "_Konflikte", "Wiederhergestellt", "*.md"))
	if len(copies) != 1 {
		t.Fatalf("move orphan copies=%v", copies)
	}
	if technical, _ := filepath.Glob(filepath.Join(root, ".remember", "trash", "conflicts", "*")); len(technical) > 1 {
		t.Fatalf("unexpected evacuation artifacts: %v", technical)
	} else if len(technical) == 1 {
		info, err := os.Stat(technical[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 || !strings.HasSuffix(technical[0], ".cleanup") {
			t.Fatalf("evacuation bytes remain: %v size=%v", technical, info.Size())
		}
	}
	content, _ := os.ReadFile(copies[0])
	inspection, err := frontmatter.Inspect(content)
	if err != nil || inspection.NoteID == doc.ID || !bytes.Contains(content, []byte("moved orphan edit")) || !bytes.Contains(content, []byte("object_missing")) {
		t.Fatalf("move orphan copy=%q inspection=%#v err=%v", content, inspection, err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if rescued := server.states[inspection.NoteID]; rescued.ObjectType != clientsync.Note || rescued.Deleted {
		t.Fatalf("remote move orphan=%#v", rescued)
	}
}

func TestSyncOnceTreatsMissingRemoteDeleteAsSatisfied(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	core, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	doc, _, err := core.CreateNote(ctx, "Gone.md", "body\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	delete(server.states, doc.ID)
	current, _ := core.ReadNote(ctx, "Gone.md")
	if _, err := core.DeleteNote(ctx, "Gone.md", current.Revision); err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("missing delete unresolved=%t err=%v", unresolved, err)
	}
	var conflict, resolution, rawOperation string
	if err := core.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT o.operation_id,o.conflict_code,r.resolution FROM sync_outbox o JOIN sync_conflict_resolutions r ON r.operation_id=o.operation_id WHERE o.object_id=? AND o.mutation='delete'`, doc.ID.String()).Scan(&rawOperation, &conflict, &resolution)
	}); err != nil || conflict != "object_missing" || resolution != "already_deleted" {
		t.Fatalf("missing delete history=%s/%s err=%v", conflict, resolution, err)
	}
	operationID := uuid.MustParse(rawOperation)
	if err := store.ResolveMissingDelete(ctx, operationID); err != nil {
		t.Fatalf("idempotent missing delete resolution: %v", err)
	}
	if err := store.ResolveMissingDelete(ctx, uuid.Must(uuid.NewV7())); err == nil {
		t.Fatal("unauthorized missing delete resolution accepted")
	}
}

func TestSyncOnceRecoversEmptyFolderCreatePathCollision(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	aSnapshot, _ := a.index.ReadSnapshot(ctx)
	var winnerID uuid.UUID
	for _, object := range aSnapshot.Objects {
		if object.Type == localindex.ObjectFolder && object.RelativePath == "Same" {
			winnerID = object.ID
		}
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderCreateMove = func() { testHookAfterConflictFolderCreateMove = nil; panic("simulated folder create recovery crash") }
	defer func() { testHookAfterConflictFolderCreateMove = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected folder create recovery crash")
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
	for i := 0; i < 3; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := b.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var original, recovered localindex.Object
	for _, object := range snapshot.Objects {
		if object.RelativePath == "Same" {
			original = object
		}
		if strings.HasPrefix(object.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Same (Konflikt - ") {
			recovered = object
		}
	}
	if original.ID != winnerID || recovered.ID == uuid.Nil || recovered.ID == winnerID || recovered.Type != localindex.ObjectFolder {
		t.Fatalf("folder collision original=%#v recovered=%#v", original, recovered)
	}
	state, ok := server.states[recovered.ID]
	if !ok || state.ParentID == nil || *state.ParentID != clientsync.ConflictRecoveredID || state.ObjectType != clientsync.Folder {
		t.Fatalf("recovered folder not synchronized: %#v", state)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("folder create collision unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aRecovered := false
	aFinal, _ := a.index.ReadSnapshot(ctx)
	for _, object := range aFinal.Objects {
		if object.ID == recovered.ID {
			aRecovered = true
		}
	}
	if !aRecovered {
		t.Fatal("recovered folder did not converge to other client")
	}
}

func TestSyncOnceRejectsNonemptyFolderCreatePathCollision(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	a, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rootB := t.TempDir()
	b, _, err := Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.CreateNote(ctx, "Same/Child.md", "must remain\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil || !strings.Contains(err.Error(), "subtree recovery") {
		t.Fatalf("nonempty folder collision err=%v", err)
	}
	note, err := b.ReadNote(ctx, "Same/Child.md")
	if err != nil || !strings.Contains(note.Body, "must remain") {
		t.Fatalf("nonempty collision child=%#v err=%v", note, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName)); err != nil {
		t.Fatalf("conflict namespace missing: %v", err)
	}
}

func TestSyncOnceRevertsFolderMovePathCollisionAndKeepsChildEdit(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Source"); err != nil {
		t.Fatal(err)
	}
	note, _, err := a.CreateNote(ctx, "Source/N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "Winner"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "Source"), filepath.Join(rootB, "Winner")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := b.ReadNote(ctx, "Winner/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Winner/N.md", doc.Revision, "edited after move\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderMoveReconcile = func() {
		testHookAfterConflictFolderMoveReconcile = nil
		panic("simulated folder move revert reconcile crash")
	}
	defer func() { testHookAfterConflictFolderMoveReconcile = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected folder move revert reconcile crash")
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
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	restored, err := b.ReadNote(ctx, "Source/N.md")
	if err != nil || restored.ID != note.ID || !strings.Contains(restored.Body, "edited after move") {
		t.Fatalf("restored child=%#v err=%v", restored, err)
	}
	if info, err := os.Stat(filepath.Join(rootB, "Winner")); err != nil || !info.IsDir() {
		t.Fatalf("remote winner folder=%v err=%v", info, err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("folder move conflict unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	remoteEdit, err := a.ReadNote(ctx, "Source/N.md")
	if err != nil || !strings.Contains(remoteEdit.Body, "edited after move") {
		t.Fatalf("remote child edit=%#v err=%v", remoteEdit, err)
	}
}

func TestSyncOnceResolvesEquivalentFolderMoveRevisionConflictAndKeepsChildEdit(t *testing.T) {
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
	for _, folder := range []string{"F", "Remote"} {
		if _, err := a.CreateFolder(ctx, folder); err != nil {
			t.Fatal(err)
		}
	}
	note, _, err := a.CreateNote(ctx, "F/N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Remote", "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Remote", "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := b.ReadNote(ctx, "Remote/F/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Remote/F/N.md", doc.Revision, "edited after concurrent folder moves\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	restored, err := b.ReadNote(ctx, "Remote/F/N.md")
	if err != nil || restored.ID != note.ID || !strings.Contains(restored.Body, "edited after concurrent") {
		t.Fatalf("revised folder child=%#v err=%v", restored, err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("folder revision conflict unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	remoteEdit, err := a.ReadNote(ctx, "Remote/F/N.md")
	if err != nil || !strings.Contains(remoteEdit.Body, "edited after concurrent") {
		t.Fatalf("concurrent folder edit not synchronized=%#v err=%v", remoteEdit, err)
	}
}

func TestSyncOnceRejectsDivergentFolderMoveRevisionConflict(t *testing.T) {
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
	for _, folder := range []string{"F", "Remote", "Local"} {
		if _, err := a.CreateFolder(ctx, folder); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.CreateNote(ctx, "F/N.md", "base\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Remote", "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local", "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil || !strings.Contains(err.Error(), "canonical state mismatch") {
		t.Fatalf("divergent folder move err=%v", err)
	}
	if _, err := b.ReadNote(ctx, "Local/F/N.md"); err != nil {
		t.Fatalf("divergent local tree changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "Remote", "F")); !os.IsNotExist(err) {
		t.Fatalf("remote target was guessed: %v", err)
	}
}

func TestSyncOnceRevertsFolderCycleAndKeepsDescendantEdits(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "F"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "X"); err != nil {
		t.Fatal(err)
	}
	fNote, _, err := a.CreateNote(ctx, "F/F.md", "f base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateNote(ctx, "X/X.md", "x base\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootA, "X"), filepath.Join(rootA, "F", "X")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "X", "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := b.ReadNote(ctx, "X/F/F.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "X/F/F.md", doc.Revision, "f edited after cycle move\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	restored, err := b.ReadNote(ctx, "F/F.md")
	if err != nil || restored.ID != fNote.ID || !strings.Contains(restored.Body, "edited after cycle") {
		t.Fatalf("cycle child=%#v err=%v", restored, err)
	}
	if nested, err := b.ReadNote(ctx, "F/X/X.md"); err != nil || !strings.Contains(nested.Body, "x base") {
		t.Fatalf("remote descendant=%#v err=%v", nested, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "X")); !os.IsNotExist(err) {
		t.Fatalf("stale root X remains: %v", err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("folder cycle unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	remoteEdit, err := a.ReadNote(ctx, "F/F.md")
	if err != nil || !strings.Contains(remoteEdit.Body, "edited after cycle") {
		t.Fatalf("cycle edit not synchronized=%#v err=%v", remoteEdit, err)
	}
}

func TestSyncOnceRescuesNoteCreateUnderUnavailableParent(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Gone"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	var goneID uuid.UUID
	for id, state := range server.states {
		if state.ObjectType == clientsync.Folder && state.Name == "Gone" {
			goneID = id
		}
	}
	if goneID == uuid.Nil {
		t.Fatal("remote folder missing")
	}
	if result, err := remote.Submit(ctx, clientsync.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: clientsync.Delete, ObjectID: goneID, ObjectType: clientsync.Folder, BaseRevision: 1}); err != nil || !result.Accepted {
		t.Fatalf("remote folder delete=%#v err=%v", result, err)
	}
	local, _, err := a.CreateNote(ctx, "Gone/Local.md", "must survive\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictEvacuation = func() { testHookAfterConflictEvacuation = nil; panic("simulated unavailable-parent evacuation crash") }
	defer func() { testHookAfterConflictEvacuation = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected evacuation crash")
			}
		}()
		if err := a.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("sync before unavailable-parent crash: %v", err)
		}
	}()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, _, err = Open(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "Gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted parent remains: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootA, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Local (Konflikt *).md"))
	if len(matches) != 1 {
		t.Fatalf("conflict copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("must survive")) {
		t.Fatalf("rescued bytes=%q err=%v", copyBytes, err)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil || inspection.NoteID == local.ID {
		t.Fatalf("rescued identity=%v err=%v", inspection.NoteID, err)
	}
	state, ok := server.states[inspection.NoteID]
	if !ok || state.ParentID == nil || *state.ParentID != clientsync.ConflictRecoveredID {
		t.Fatalf("rescued note not synchronized: %#v", state)
	}
	store, _ := clientsync.NewStore(a.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("parent conflict unresolved=%t err=%v", unresolved, err)
	}
}

func TestSyncOnceRescuesNoteMoveToUnavailableParent(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Source"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "Gone"); err != nil {
		t.Fatal(err)
	}
	note, _, err := a.CreateNote(ctx, "Source/N.md", "original\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	var goneID uuid.UUID
	for id, state := range server.states {
		if state.ObjectType == clientsync.Folder && state.Name == "Gone" {
			goneID = id
		}
	}
	if goneID == uuid.Nil {
		t.Fatal("remote folder missing")
	}
	if result, err := remote.Submit(ctx, clientsync.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: clientsync.Delete, ObjectID: goneID, ObjectType: clientsync.Folder, BaseRevision: 1}); err != nil || !result.Accepted {
		t.Fatalf("remote folder delete=%#v err=%v", result, err)
	}
	doc, err := a.ReadNote(ctx, "Source/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "Source/N.md", "Gone/N.md", doc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	canonical, err := a.ReadNote(ctx, "Source/N.md")
	if err != nil || canonical.ID != note.ID || !strings.Contains(canonical.Body, "original") {
		t.Fatalf("canonical note=%#v err=%v", canonical, err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "Gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted target parent remains: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootA, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "N (Konflikt *).md"))
	if len(matches) != 1 {
		t.Fatalf("move conflict copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("original")) {
		t.Fatalf("move rescue=%q err=%v", copyBytes, err)
	}
	store, _ := clientsync.NewStore(a.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("move parent conflict unresolved=%t err=%v", unresolved, err)
	}
}

func TestSyncOnceTreatsMissingRemoteFolderDeleteAsSatisfied(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := core.CreateFolder(ctx, "GoneFolder"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := core.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var folderID uuid.UUID
	for _, object := range snapshot.Objects {
		if object.Type == "folder" && object.RelativePath == "GoneFolder" {
			folderID = object.ID
		}
	}
	if folderID == uuid.Nil {
		t.Fatal("folder identity unavailable")
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	delete(server.states, folderID)
	if err := os.Remove(filepath.Join(root, "GoneFolder")); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("missing folder delete unresolved=%t err=%v", unresolved, err)
	}
	var resolution string
	if err := core.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT r.resolution FROM sync_outbox o JOIN sync_conflict_resolutions r ON r.operation_id=o.operation_id WHERE o.object_id=? AND o.object_type='folder' AND o.mutation='delete'`, folderID.String()).Scan(&resolution)
	}); err != nil || resolution != "already_deleted" {
		t.Fatalf("folder delete resolution=%s err=%v", resolution, err)
	}
}

func TestSyncOnceRestoresFolderRejectedAsNotEmpty(t *testing.T) {
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	child, _, err := a.CreateNote(ctx, "Folder/Remote.md", "remote child\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := b.index.ReadSnapshot(ctx)
	var folderID uuid.UUID
	for _, object := range snapshot.Objects {
		if object.Type == "folder" && object.RelativePath == "Folder" {
			folderID = object.ID
		}
	}
	if err := os.Remove(filepath.Join(rootB, "Folder")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderPublication = func() { testHookAfterConflictFolderPublication = nil; panic("simulated folder restoration crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected folder restoration crash")
			}
		}()
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("sync before folder restoration crash: %v", err)
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
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadNote(ctx, "Folder/Remote.md")
	if err != nil || got.ID != child.ID || !strings.Contains(got.Body, "remote child") {
		t.Fatalf("restored folder child=%#v err=%v", got, err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("folder-not-empty unresolved=%t err=%v", unresolved, err)
	}
	var resolution string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT r.resolution FROM sync_outbox o JOIN sync_conflict_resolutions r ON r.operation_id=o.operation_id WHERE o.object_id=? AND o.conflict_code='folder_not_empty'`, folderID.String()).Scan(&resolution)
	}); err != nil || resolution != "folder_not_empty_preserved" {
		t.Fatalf("folder restoration resolution=%s err=%v", resolution, err)
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conflict_folder_restorations SET state='prepared'`)
		return err
	}); err == nil {
		t.Fatal("folder restoration moved backwards")
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM conflict_folder_restorations`); return err }); err == nil {
		t.Fatal("folder restoration history was deleted")
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
