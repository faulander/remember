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
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

type memorySyncServer struct {
	blobs      map[[32]byte][]byte
	results    map[uuid.UUID]clientsync.Result
	states     map[uuid.UUID]clientsync.Change
	changes    []clientsync.Change
	pullErr    error
	maxPull    int
	pullAfters []uint64
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
	if m.Kind == clientsync.Create && state.Revision != 0 {
		r.server.ensureConflictNamespace()
		canonical := &clientsync.CanonicalState{ObjectType: state.ObjectType, Revision: state.Revision, ParentID: state.ParentID, Name: state.Name, BlobHash: append([]byte(nil), state.BlobHash...), Deleted: state.Deleted}
		result := clientsync.Result{Conflict: "object_exists", Canonical: canonical}
		r.server.results[m.OperationID] = result
		return result, nil
	}
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
func (r *memoryRemote) PreserveAndDeleteEmptyFolder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64) (remotehttp.PreserveDeleteFolderResult, error) {
	return remotehttp.PreserveDeleteFolderResult{}, errors.New("unsupported test resolution")
}
func (r *memoryRemote) Pull(_ context.Context, after uint64, limit int) (remotehttp.PullPage, error) {
	r.server.pullAfters = append(r.server.pullAfters, after)
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

func TestSyncOnceFailsClosedForCreateObjectExists(t *testing.T) {
	t.Run("note", func(t *testing.T) {
		ctx := context.Background()
		server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
		remote := &memoryRemote{server: server}
		root := t.TempDir()
		core, _, err := Initialize(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer core.Close()
		note, _, err := core.CreateNote(ctx, "Local.md", "local collision bytes\n", nil)
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(root, "Local.md"))
		if err != nil {
			t.Fatal(err)
		}
		server.states[note.ID] = clientsync.Change{ObjectID: note.ID, ObjectType: clientsync.Folder, Revision: 7, Name: "CanonicalFolder"}
		if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
			t.Fatalf("object-exists note sync=%v", err)
		}
		after, err := os.ReadFile(filepath.Join(root, "Local.md"))
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("object-exists note changed=%t err=%v", bytes.Equal(after, before), err)
		}
		if state := server.states[note.ID]; state.ObjectType != clientsync.Folder || state.Revision != 7 || state.Name != "CanonicalFolder" {
			t.Fatalf("canonical object overwritten: %#v", state)
		}
		assertSingleFailClosedConflict(t, ctx, core, note.ID, "object_exists", clientsync.Folder)
		if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
			t.Fatalf("object-exists note replay=%v", err)
		}
	})
	t.Run("folder subtree", func(t *testing.T) {
		ctx := context.Background()
		server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
		remote := &memoryRemote{server: server}
		root := t.TempDir()
		core, _, err := Initialize(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer core.Close()
		if _, err := core.CreateFolder(ctx, "Local"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.CreateNote(ctx, "Local/Child.md", "subtree survives\n", nil); err != nil {
			t.Fatal(err)
		}
		snapshot, err := core.index.ReadSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var folderID uuid.UUID
		for _, object := range snapshot.Objects {
			if object.Type == localindex.ObjectFolder && object.RelativePath == "Local" {
				folderID = object.ID
			}
		}
		if folderID == uuid.Nil {
			t.Fatal("local folder id missing")
		}
		device, inode, err := repository.RootedFolderIdentity(root, "Local")
		if err != nil {
			t.Fatal(err)
		}
		childBefore, err := os.ReadFile(filepath.Join(root, "Local", "Child.md"))
		if err != nil {
			t.Fatal(err)
		}
		server.states[folderID] = clientsync.Change{ObjectID: folderID, ObjectType: clientsync.Folder, Revision: 3, Name: "Canonical"}
		if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
			t.Fatalf("object-exists folder sync=%v", err)
		}
		if err := repository.VerifyRootedFolderIdentity(root, "Local", device, inode); err != nil {
			t.Fatal(err)
		}
		childAfter, err := os.ReadFile(filepath.Join(root, "Local", "Child.md"))
		if err != nil || !bytes.Equal(childAfter, childBefore) {
			t.Fatalf("object-exists subtree changed=%t err=%v", bytes.Equal(childAfter, childBefore), err)
		}
		assertSingleFailClosedConflict(t, ctx, core, folderID, "object_exists", clientsync.Folder)
		if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
			t.Fatalf("object-exists folder replay=%v", err)
		}
	})
}

func assertSingleFailClosedConflict(t *testing.T, ctx context.Context, core *LocalCore, objectID uuid.UUID, code string, canonicalType clientsync.ObjectType) {
	t.Helper()
	store, _ := clientsync.NewStore(core.index)
	conflicts, err := store.ListConflicts(ctx, 10)
	if err != nil || len(conflicts) != 1 || conflicts[0].Outbox.Mutation.ObjectID != objectID || conflicts[0].Code != code || conflicts[0].Canonical == nil || conflicts[0].Canonical.ObjectType != canonicalType {
		t.Fatalf("fail-closed conflicts=%#v err=%v", conflicts, err)
	}
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

func TestSyncOnceResolvesEquivalentRootNoteMoves(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "N.md", "base equivalent move\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for _, core := range []*LocalCore{a, b} {
		current, err := core.ReadNote(ctx, "N.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.MoveNote(ctx, "N.md", "Same.md", current.Revision); err != nil {
			t.Fatal(err)
		}
	}
	local, err := b.ReadNote(ctx, "Same.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Same.md", local.Revision, "dependent edit survives equivalent move\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadNote(ctx, "Same.md")
	if err != nil || got.ID != doc.ID || !strings.Contains(got.Body, "dependent edit survives") {
		t.Fatalf("equivalent local=%#v err=%v", got, err)
	}
	var resolution string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT r.resolution FROM sync_conflict_resolutions r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE o.object_id=? AND o.mutation='move' AND o.status='conflict'`, doc.ID.String()).Scan(&resolution)
	}); err != nil || resolution != "note_move_equivalent" {
		t.Fatalf("resolution=%q err=%v", resolution, err)
	}
	if copies, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md")); len(copies) != 0 {
		t.Fatalf("equivalent move created copies=%v", copies)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "Same.md")
	if err != nil || aDoc.ID != doc.ID || !strings.Contains(aDoc.Body, "dependent edit survives") {
		t.Fatalf("A convergence=%#v err=%v", aDoc, err)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	cDoc, err := c.ReadNote(ctx, "Same.md")
	if err != nil || cDoc.ID != doc.ID || !strings.Contains(cDoc.Body, "dependent edit survives") {
		t.Fatalf("cold C=%#v err=%v", cDoc, err)
	}
}

func TestSyncOnceResolvesEquivalentNonRootNoteMoves(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "F/N.md", "nested equivalent base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for _, core := range []*LocalCore{a, b} {
		current, err := core.ReadNote(ctx, "F/N.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.MoveNote(ctx, "F/N.md", "F/Same.md", current.Revision); err != nil {
			t.Fatal(err)
		}
	}
	local, err := b.ReadNote(ctx, "F/Same.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "F/Same.md", local.Revision, "nested dependent edit survives\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for label, core := range map[string]*LocalCore{"A": a, "B": b} {
		got, err := core.ReadNote(ctx, "F/Same.md")
		if err != nil || got.ID != doc.ID || !strings.Contains(got.Body, "nested dependent edit survives") {
			t.Fatalf("%s nested convergence=%#v err=%v", label, got, err)
		}
	}
	var resolution string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT r.resolution FROM sync_conflict_resolutions r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE o.object_id=? AND o.mutation='move' AND o.status='conflict'`, doc.ID.String()).Scan(&resolution)
	}); err != nil || resolution != "note_move_equivalent" {
		t.Fatalf("nested resolution=%q err=%v", resolution, err)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadNote(ctx, "F/Same.md")
	if err != nil || got.ID != doc.ID || !strings.Contains(got.Body, "nested dependent edit survives") {
		t.Fatalf("cold C nested=%#v err=%v", got, err)
	}
}

func TestSyncOnceRejectsEquivalentNonRootNoteMoveWithParentIntent(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "F/N.md", "must remain\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for _, core := range []*LocalCore{a, b} {
		current, err := core.ReadNote(ctx, "F/N.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.MoveNote(ctx, "F/N.md", "F/Same.md", current.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "LocalF")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("equivalent nested move resolved across parent intent")
	}
	got, err := b.ReadNote(ctx, "LocalF/Same.md")
	if err != nil || got.ID != doc.ID || !strings.Contains(got.Body, "must remain") {
		t.Fatalf("local bytes changed=%#v err=%v", got, err)
	}
	var count int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_conflict_resolutions r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE o.object_id=? AND r.resolution='note_move_equivalent'`, doc.ID.String()).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("unsafe resolution count=%d err=%v", count, err)
	}
}

func TestSyncOnceMaterializesDivergentRootNoteMoves(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "N.md", "base move bytes\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "N.md", "Remote.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, err := b.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.MoveNote(ctx, "N.md", "Local.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	local, err := b.ReadNote(ctx, "Local.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Local.md", local.Revision, "edited after divergent move\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictEvacuation = func() { testHookAfterConflictEvacuation = nil; panic("simulated divergent note move evacuation crash") }
	defer func() { testHookAfterConflictEvacuation = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected divergent move crash")
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
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	canonical, err := b.ReadNote(ctx, "Remote.md")
	if err != nil || canonical.ID != doc.ID || !strings.Contains(canonical.Body, "base move bytes") {
		t.Fatalf("divergent move canonical=%#v err=%v", canonical, err)
	}
	if _, err := b.ReadNote(ctx, "Local.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("losing move remains at target: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("divergent move copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("edited after divergent move")) || !bytes.Contains(copyBytes, []byte("base_revision_mismatch")) {
		t.Fatalf("divergent move copy=%q err=%v", copyBytes, err)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.NoteID == doc.ID {
		t.Fatal("divergent move copy reused canonical id")
	}
	if state := server.states[doc.ID]; state.Name != "Remote.md" || state.Deleted || state.Revision != 2 {
		t.Fatalf("divergent canonical state=%#v", state)
	}
	if rescued := server.states[inspection.NoteID]; rescued.Deleted || rescued.ObjectType != clientsync.Note {
		t.Fatalf("divergent rescued state=%#v", rescued)
	}
	evacuations, _ := filepath.Glob(filepath.Join(rootB, ".remember", "trash", "conflicts", "*.md"))
	if len(evacuations) != 0 {
		t.Fatalf("divergent evacuation remains=%v", evacuations)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aCopies, _ := filepath.Glob(filepath.Join(rootA, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(aCopies) != 1 {
		t.Fatalf("divergent move did not converge=%v", aCopies)
	}
}

func TestSyncOnceMaterializesDivergentNestedNoteMoves(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "F/N.md", "nested move base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "F/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "F/N.md", "F/Remote.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, err := b.ReadNote(ctx, "F/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.MoveNote(ctx, "F/N.md", "F/Local.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	local, err := b.ReadNote(ctx, "F/Local.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "F/Local.md", local.Revision, "nested losing edit survives\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictEvacuation = func() {
		testHookAfterConflictEvacuation = nil
		panic("simulated nested divergent move evacuation crash")
	}
	defer func() { testHookAfterConflictEvacuation = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected nested divergent move crash")
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
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	canonical, err := b.ReadNote(ctx, "F/Remote.md")
	if err != nil || canonical.ID != doc.ID || canonical.Body != "nested move base\n" {
		t.Fatalf("nested canonical=%#v err=%v", canonical, err)
	}
	if _, err := b.ReadNote(ctx, "F/Local.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("nested losing target remains: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("nested copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("nested losing edit survives")) || !bytes.Contains(copyBytes, []byte("base_revision_mismatch")) {
		t.Fatalf("nested copy=%q err=%v", copyBytes, err)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil || inspection.NoteID == doc.ID {
		t.Fatalf("nested copy identity=%#v err=%v", inspection, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for label, core := range map[string]*LocalCore{"A": a, "B": b, "C": c} {
		got, err := core.ReadNote(ctx, "F/Remote.md")
		if err != nil || got.ID != doc.ID || got.Body != "nested move base\n" {
			t.Fatalf("%s canonical=%#v err=%v", label, got, err)
		}
		copies, _ := filepath.Glob(filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
		if len(copies) != 1 {
			t.Fatalf("%s copies=%v", label, copies)
		}
		content, err := os.ReadFile(copies[0])
		copyInspection, inspectErr := frontmatter.Inspect(content)
		if err != nil || inspectErr != nil || copyInspection.NoteID != inspection.NoteID || !bytes.Contains(content, []byte("nested losing edit survives")) {
			t.Fatalf("%s copy=%q identity=%#v err=%v inspect=%v", label, content, copyInspection, err, inspectErr)
		}
	}
}

func TestSyncOnceRejectsDivergentNestedMoveWithParentIntent(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "F/N.md", "must remain unchanged\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ := a.ReadNote(ctx, "F/N.md")
	if _, _, err := a.MoveNote(ctx, "F/N.md", "F/Remote.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "F/N.md")
	if _, _, err := b.MoveNote(ctx, "F/N.md", "F/Local.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "LocalF")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("nested divergent move crossed unresolved parent intent")
	}
	got, err := b.ReadNote(ctx, "LocalF/Local.md")
	if err != nil || got.ID != doc.ID || got.Body != "must remain unchanged\n" {
		t.Fatalf("local note=%#v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, clientsync.ConflictRootName)); !os.IsNotExist(err) {
		t.Fatalf("conflict namespace mutated on rejected recovery: %v", err)
	}
}

func TestSyncOnceRejectsDivergentNestedMoveAcrossParents(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "G"); err != nil {
		t.Fatal(err)
	}
	doc, _, err := a.CreateNote(ctx, "F/N.md", "different parents remain\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, _ := a.ReadNote(ctx, "F/N.md")
	if _, _, err := a.MoveNote(ctx, "F/N.md", "G/Remote.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, _ := b.ReadNote(ctx, "F/N.md")
	if _, _, err := b.MoveNote(ctx, "F/N.md", "F/Local.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("different-parent divergent move resolved")
	}
	got, err := b.ReadNote(ctx, "F/Local.md")
	if err != nil || got.ID != doc.ID || got.Body != "different parents remain\n" {
		t.Fatalf("different-parent local=%#v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, clientsync.ConflictRootName)); !os.IsNotExist(err) {
		t.Fatalf("different-parent conflict namespace mutated: %v", err)
	}
}

func TestSyncOnceRejectsNonAdvancingRootMoveConflict(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	doc, _, err := core.CreateNote(ctx, "N.md", "must not evacuate\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	current, err := core.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.MoveNote(ctx, "N.md", "Local.md", current.Revision); err != nil {
		t.Fatal(err)
	}
	var rawOperation string
	var baseRevision uint64
	if err := core.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT operation_id,base_revision FROM sync_outbox WHERE object_id=? AND mutation='move' AND status='pending' ORDER BY sequence DESC LIMIT 1`, doc.ID.String()).Scan(&rawOperation, &baseRevision)
	}); err != nil {
		t.Fatal(err)
	}
	operationID := uuid.MustParse(rawOperation)
	state := server.states[doc.ID]
	server.results[operationID] = clientsync.Result{Conflict: "base_revision_mismatch", Canonical: &clientsync.CanonicalState{ObjectType: clientsync.Note, Revision: baseRevision, Name: "Remote.md", BlobHash: append([]byte(nil), state.BlobHash...)}}
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("nonadvancing move conflict err=%v", err)
	}
	local, err := core.ReadNote(ctx, "Local.md")
	if err != nil || local.ID != doc.ID || !strings.Contains(local.Body, "must not evacuate") {
		t.Fatalf("nonadvancing local=%#v err=%v", local, err)
	}
	if _, err := core.ReadNote(ctx, "Remote.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("stale canonical was applied: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, ".remember", "trash", "conflicts", "*.md")); len(matches) != 0 {
		t.Fatalf("nonadvancing conflict evacuated=%v", matches)
	}
}

func TestSyncOnceRecoversLocalMoveAgainstRemoteDelete(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "N.md", "move survives delete\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteNote(ctx, "N.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	bDoc, err := b.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.MoveNote(ctx, "N.md", "Locally Moved.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictEvacuation = func() { testHookAfterConflictEvacuation = nil; panic("simulated move-delete evacuation crash") }
	defer func() { testHookAfterConflictEvacuation = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected move-delete crash")
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
	for _, relative := range []string{"N.md", "Locally Moved.md"} {
		if _, err := b.ReadNote(ctx, relative); !errors.Is(err, ErrNoteNotFound) {
			t.Fatalf("deleted identity visible at %s: %v", relative, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("move-delete copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("move survives delete")) || !bytes.Contains(copyBytes, []byte("object_deleted")) {
		t.Fatalf("move-delete copy=%q err=%v", copyBytes, err)
	}
	evacuations, _ := filepath.Glob(filepath.Join(rootB, ".remember", "trash", "conflicts", "*.md"))
	if len(evacuations) != 0 {
		t.Fatalf("completed move-delete evacuation remains: %v", evacuations)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.NoteID == doc.ID {
		t.Fatal("recovered move reused tombstoned id")
	}
	if state := server.states[inspection.NoteID]; state.ObjectType != clientsync.Note || state.Deleted {
		t.Fatalf("recovered move state=%#v", state)
	}
	if state := server.states[doc.ID]; !state.Deleted {
		t.Fatalf("canonical delete lost: %#v", state)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aMatches, _ := filepath.Glob(filepath.Join(rootA, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(aMatches) != 1 {
		t.Fatalf("recovered move did not converge: %v", aMatches)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadNote(ctx, "Locally Moved.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("cold client revived moved identity: %v", err)
	}
	cMatches, _ := filepath.Glob(filepath.Join(rootC, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(cMatches) != 1 {
		t.Fatalf("cold move-delete copies=%v", cMatches)
	}
}

func TestSyncOnceRebasesLocalDeleteAgainstRemoteMove(t *testing.T) {
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
	doc, _, err := a.CreateNote(ctx, "N.md", "remote move survives as copy\n", nil)
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
	if _, err := b.DeleteNote(ctx, "N.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "N.md", "Remotely Moved.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	testHookBeforeConflictCompletion = func() { testHookBeforeConflictCompletion = nil; panic("simulated delete-move rebase crash") }
	defer func() { testHookBeforeConflictCompletion = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected delete-move crash")
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
	for _, relative := range []string{"N.md", "Remotely Moved.md"} {
		if _, err := b.ReadNote(ctx, relative); !errors.Is(err, ErrNoteNotFound) {
			t.Fatalf("local delete undone at %s: %v", relative, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("delete-move copies=%v", matches)
	}
	copyBytes, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(copyBytes, []byte("remote move survives as copy")) || !bytes.Contains(copyBytes, []byte("base_revision_mismatch")) {
		t.Fatalf("delete-move copy=%q err=%v", copyBytes, err)
	}
	inspection, err := frontmatter.Inspect(copyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.NoteID == doc.ID {
		t.Fatal("delete-move copy reused deleted id")
	}
	if state := server.states[doc.ID]; !state.Deleted || state.Revision != 3 {
		t.Fatalf("rebased move tombstone=%#v", state)
	}
	if rescued := server.states[inspection.NoteID]; rescued.Deleted || rescued.ObjectType != clientsync.Note {
		t.Fatalf("rescued move state=%#v", rescued)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReadNote(ctx, "Remotely Moved.md"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("remote mover retained original: %v", err)
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

func TestSyncOnceTreatsAlreadyDeletedObjectsAsSatisfied(t *testing.T) {
	t.Run("note", func(t *testing.T) {
		ctx := context.Background()
		server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
		remote := &memoryRemote{server: server}
		a, _, err := Initialize(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		b, _, err := Initialize(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		doc, _, err := a.CreateNote(ctx, "N.md", "body\n", nil)
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
		if _, err := a.DeleteNote(ctx, "N.md", aDoc.Revision); err != nil {
			t.Fatal(err)
		}
		if err := a.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		bDoc, _ := b.ReadNote(ctx, "N.md")
		if _, err := b.DeleteNote(ctx, "N.md", bDoc.Revision); err != nil {
			t.Fatal(err)
		}
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		assertAlreadyDeletedResolution(t, ctx, b, doc.ID)
	})
	t.Run("folder", func(t *testing.T) {
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
		snapshot, _ := a.index.ReadSnapshot(ctx)
		var folderID uuid.UUID
		for _, object := range snapshot.Objects {
			if object.RelativePath == "F" && object.Type == localindex.ObjectFolder {
				folderID = object.ID
			}
		}
		if err := a.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(rootA, "F")); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if err := a.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(rootB, "F")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatal(err)
		}
		assertAlreadyDeletedResolution(t, ctx, b, folderID)
	})
}

func assertAlreadyDeletedResolution(t *testing.T, ctx context.Context, core *LocalCore, objectID uuid.UUID) {
	t.Helper()
	store, _ := clientsync.NewStore(core.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("already-deleted unresolved=%t err=%v", unresolved, err)
	}
	var conflict, resolution string
	if err := core.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT o.conflict_code,r.resolution FROM sync_outbox o JOIN sync_conflict_resolutions r ON r.operation_id=o.operation_id WHERE o.object_id=? AND o.mutation='delete' ORDER BY o.sequence DESC LIMIT 1`, objectID.String()).Scan(&conflict, &resolution)
	}); err != nil || conflict != "object_deleted" || resolution != "already_deleted" {
		t.Fatalf("already-deleted history=%s/%s err=%v", conflict, resolution, err)
	}
}

func TestSyncOnceRecoversDirectNoteFolderCreatePathCollision(t *testing.T) {
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Same/Child.md", "direct child survives\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(filepath.Join(rootB, "Same", "Child.md"))
	if err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderCreateReconcile = func() {
		testHookAfterConflictFolderCreateReconcile = nil
		panic("direct-note recovery reconcile crash")
	}
	defer func() { testHookAfterConflictFolderCreateReconcile = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected direct-note recovery crash")
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
	for i := 0; i < 4; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	snapshot, err := b.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var recovered localindex.Object
	var recoveredNote localindex.Object
	for _, object := range snapshot.Objects {
		if strings.HasPrefix(object.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Same (Konflikt - ") && object.Type == localindex.ObjectFolder {
			recovered = object
		}
		if object.ID == note.ID {
			recoveredNote = object
		}
	}
	if recovered.ID == uuid.Nil || recovered.ID == note.ID || recoveredNote.ID != note.ID || recoveredNote.ParentID != recovered.ID {
		t.Fatalf("direct-note recovery folder=%#v note=%#v", recovered, recoveredNote)
	}
	content, err := os.ReadFile(filepath.Join(rootB, filepath.FromSlash(recoveredNote.RelativePath)))
	if err != nil || !bytes.Equal(content, beforeBytes) {
		t.Fatalf("direct-note bytes changed=%t err=%v", bytes.Equal(content, beforeBytes), err)
	}
	var dependencies int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_outbox note JOIN sync_outbox_dependencies d ON d.operation_id=note.operation_id JOIN sync_outbox folder ON folder.operation_id=d.dependency_operation_id WHERE note.object_id=? AND note.mutation='create' AND note.parent_id=? AND folder.object_id=? AND folder.mutation='create'`, note.ID.String(), recovered.ID.String(), recovered.ID.String()).Scan(&dependencies)
	}); err != nil || dependencies != 1 {
		t.Fatalf("replacement dependency=%d err=%v", dependencies, err)
	}
	folderState, noteState := server.states[recovered.ID], server.states[note.ID]
	if folderState.ParentID == nil || *folderState.ParentID != clientsync.ConflictRecoveredID || noteState.ParentID == nil || *noteState.ParentID != recovered.ID || noteState.Deleted {
		t.Fatalf("direct-note server folder=%#v note=%#v", folderState, noteState)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aBytes, err := os.ReadFile(filepath.Join(rootA, filepath.FromSlash(recoveredNote.RelativePath)))
	if err != nil || !bytes.Contains(aBytes, []byte("direct child survives")) {
		t.Fatalf("direct-note convergence=%q err=%v", aBytes, err)
	}
	aInspection, err := frontmatter.Inspect(aBytes)
	if err != nil || aInspection.NoteID != note.ID {
		t.Fatalf("direct-note converged identity=%#v err=%v", aInspection, err)
	}
}

func TestSyncOnceRecoversUpdatedDirectNoteFolderCreatePathCollision(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Edited"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Edited"); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Edited/Child.md", "v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := b.ReadNote(ctx, "Edited/Child.md")
	doc, _, err = b.SaveNote(ctx, "Edited/Child.md", doc.Revision, "v1\n", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err = b.SaveNote(ctx, "Edited/Child.md", doc.Revision, "v2 final\n", []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	finalBytes, err := os.ReadFile(filepath.Join(rootB, "Edited", "Child.md"))
	if err != nil {
		t.Fatal(err)
	}
	finalHash := sha256.Sum256(finalBytes)
	testHookAfterConflictFolderCreateReconcile = func() { testHookAfterConflictFolderCreateReconcile = nil; panic("updated direct-note recovery crash") }
	defer func() { testHookAfterConflictFolderCreateReconcile = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected updated recovery crash")
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
	for i := 0; i < 4; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	snap, err := b.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var recoveredFolder, recoveredNote localindex.Object
	for _, object := range snap.Objects {
		if object.Type == localindex.ObjectFolder && strings.HasPrefix(object.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Edited (Konflikt - ") {
			recoveredFolder = object
		}
		if object.ID == note.ID {
			recoveredNote = object
		}
	}
	if recoveredFolder.ID == uuid.Nil || recoveredNote.ID != note.ID || recoveredNote.ParentID != recoveredFolder.ID || !bytes.Equal(recoveredNote.ContentHash, finalHash[:]) {
		t.Fatalf("updated recovery folder=%#v note=%#v", recoveredFolder, recoveredNote)
	}
	got, err := os.ReadFile(filepath.Join(rootB, filepath.FromSlash(recoveredNote.RelativePath)))
	if err != nil || !bytes.Equal(got, finalBytes) {
		t.Fatalf("final bytes changed=%t err=%v", bytes.Equal(got, finalBytes), err)
	}
	var oldCreates, oldUpdates, replacements int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT SUM(CASE WHEN mutation='create' AND status='superseded' THEN 1 ELSE 0 END),SUM(CASE WHEN mutation='update' AND status='superseded' THEN 1 ELSE 0 END),SUM(CASE WHEN mutation='create' AND status='accepted' AND parent_id=? AND blob_hash=? THEN 1 ELSE 0 END) FROM sync_outbox WHERE object_id=?`, recoveredFolder.ID.String(), finalHash[:], note.ID.String()).Scan(&oldCreates, &oldUpdates, &replacements)
	}); err != nil || oldCreates != 1 || oldUpdates != 2 || replacements != 1 {
		t.Fatalf("old creates=%d updates=%d replacements=%d err=%v", oldCreates, oldUpdates, replacements, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for label, core := range map[string]*LocalCore{"A": a, "B": b, "C": c} {
		content, err := os.ReadFile(filepath.Join(core.Root(), filepath.FromSlash(recoveredNote.RelativePath)))
		if err != nil || !bytes.Equal(content, finalBytes) {
			t.Fatalf("%s final bytes changed=%t err=%v", label, bytes.Equal(content, finalBytes), err)
		}
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != note.ID {
			t.Fatalf("%s identity=%#v err=%v", label, inspection, err)
		}
	}
}

func TestSyncOnceRejectsAttemptedDirectNoteUpdateFolderRecovery(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	a, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := t.TempDir()
	b, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.CreateFolder(ctx, "Attempted"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Attempted"); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Attempted/N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := b.ReadNote(ctx, "Attempted/N.md")
	if _, _, err := b.SaveNote(ctx, "Attempted/N.md", doc.Revision, "attempted final\n", nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "Attempted", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_outbox SET status='attempted',attempted_at_ms=1 WHERE object_id=? AND mutation='update' AND status='pending'`, note.ID.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("attempted update recovered")
	}
	after, err := os.ReadFile(filepath.Join(root, "Attempted", "N.md"))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("attempted bytes changed=%t err=%v", bytes.Equal(after, before), err)
	}
	targets, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Attempted (Konflikt - *)"))
	if len(targets) != 0 {
		t.Fatalf("attempted update moved source: %v", targets)
	}
}

func TestSyncOnceRejectsDependentAddedAfterDirectNoteRecoveryPrepare(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	a, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := t.TempDir()
	b, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.CreateFolder(ctx, "LateDependent"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "LateDependent"); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "LateDependent/N.md", "v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := b.ReadNote(ctx, "LateDependent/N.md")
	if _, _, err := b.SaveNote(ctx, "LateDependent/N.md", doc.Revision, "final preserved\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderCreateReconcile = func() { testHookAfterConflictFolderCreateReconcile = nil; panic("prepared recovery") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected prepared crash")
			}
		}()
		_ = b.SyncOnce(ctx, remote)
	}()
	var finalOperation string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT operation_id FROM sync_outbox WHERE object_id=? AND mutation='update' ORDER BY sequence DESC LIMIT 1`, note.ID.String()).Scan(&finalOperation)
	}); err != nil {
		t.Fatal(err)
	}
	extra, _ := uuid.NewV7()
	h := sha256.Sum256([]byte("late dependent"))
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',2,NULL,'',?,?,'pending',1)`, extra.String(), note.ID.String(), h[:], finalOperation); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, extra.String(), finalOperation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("post-prepare dependent accepted")
	}
	var resolutions int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_conflict_resolutions r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE o.object_type='folder' AND r.resolution='folder_create_collision_recovered'`).Scan(&resolutions)
	}); err != nil || resolutions != 0 {
		t.Fatalf("unsafe resolution=%d err=%v", resolutions, err)
	}
	matches, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "LateDependent (Konflikt - *)", "N.md"))
	if len(matches) != 1 {
		t.Fatalf("prepared bytes not preserved: %v", matches)
	}
	content, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Contains(content, []byte("final preserved")) {
		t.Fatalf("preserved content=%q err=%v", content, err)
	}
}

func TestSyncOnceRejectsNonlinearDirectNoteFolderRecovery(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, context.Context, *LocalCore, string, NoteDocument)
	}{
		{"move", func(t *testing.T, ctx context.Context, c *LocalCore, root string, n NoteDocument) {
			if _, _, err := c.MoveNote(ctx, "Unsafe/N.md", "Unsafe/Moved.md", n.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{"delete", func(t *testing.T, ctx context.Context, c *LocalCore, root string, n NoteDocument) {
			if _, err := c.DeleteNote(ctx, "Unsafe/N.md", n.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{"branch", func(t *testing.T, ctx context.Context, c *LocalCore, root string, n NoteDocument) {
			if _, _, err := c.SaveNote(ctx, "Unsafe/N.md", n.Revision, "linear first\n", nil); err != nil {
				t.Fatal(err)
			}
			var createRaw string
			if err := c.index.WithTransaction(ctx, func(tx *sql.Tx) error {
				return tx.QueryRow(`SELECT operation_id FROM sync_outbox WHERE object_id=? AND mutation='create'`, n.ID.String()).Scan(&createRaw)
			}); err != nil {
				t.Fatal(err)
			}
			createID, err := uuid.Parse(createRaw)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256([]byte("branch"))
			op, _ := uuid.NewV7()
			if err := c.index.WithTransaction(ctx, func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',1,NULL,'',?,?,'pending',1)`, op.String(), n.ID.String(), h[:], createID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(`INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, op.String(), createID.String())
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"disconnected same-object update", func(t *testing.T, ctx context.Context, c *LocalCore, root string, n NoteDocument) {
			if _, _, err := c.SaveNote(ctx, "Unsafe/N.md", n.Revision, "linear final\n", nil); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256([]byte("disconnected"))
			op, _ := uuid.NewV7()
			if err := c.index.WithTransaction(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',99,NULL,'',?,NULL,'pending',1)`, op.String(), n.ID.String(), h[:])
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"unexpected entry", func(t *testing.T, ctx context.Context, c *LocalCore, root string, n NoteDocument) {
			if err := os.WriteFile(filepath.Join(root, "Unsafe", "extra.bin"), []byte("unexpected"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
			remote := &memoryRemote{server: server}
			a, _, err := Initialize(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			root := t.TempDir()
			b, _, err := Initialize(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if _, err := a.CreateFolder(ctx, "Unsafe"); err != nil {
				t.Fatal(err)
			}
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if _, err := b.CreateFolder(ctx, "Unsafe"); err != nil {
				t.Fatal(err)
			}
			created, _, err := b.CreateNote(ctx, "Unsafe/N.md", "preserve me\n", nil)
			if err != nil {
				t.Fatal(err)
			}
			note, err := b.ReadNote(ctx, "Unsafe/N.md")
			if err != nil {
				t.Fatal(err)
			}
			if note.ID != created.ID {
				t.Fatal("note identity changed")
			}
			tc.mutate(t, ctx, b, root, note)
			if err := b.SyncOnce(ctx, remote); err == nil {
				t.Fatal("nonlinear recovery accepted")
			}
			targets, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Unsafe (Konflikt - *)"))
			if len(targets) != 0 {
				t.Fatalf("unsafe subtree moved: %v", targets)
			}
			var superseded int
			if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
				return tx.QueryRow(`SELECT COUNT(*) FROM sync_outbox WHERE object_id=? AND mutation='update' AND status='superseded'`, created.ID.String()).Scan(&superseded)
			}); err != nil || superseded != 0 {
				t.Fatalf("partial update supersede=%d err=%v", superseded, err)
			}
		})
	}
}

func TestSyncOnceRestoresChangedDirectNoteRecoveryTarget(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	a, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := t.TempDir()
	b, _, err := Initialize(ctx, root)
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
	if _, _, err := b.CreateNote(ctx, "Same/Child.md", "preserved\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderCreateMove = func() {
		testHookAfterConflictFolderCreateMove = nil
		targets, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Same (Konflikt - *)"))
		if len(targets) != 1 {
			panic("recovery target unavailable")
		}
		if err := os.WriteFile(filepath.Join(targets[0], "unexpected.txt"), []byte("concurrent bytes"), 0o600); err != nil {
			panic(err)
		}
		panic("simulated changed recovery target crash")
	}
	defer func() { testHookAfterConflictFolderCreateMove = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected changed-target crash")
			}
		}()
		_ = b.SyncOnce(ctx, remote)
	}()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = Open(ctx, root)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		if b != nil {
			_ = b.Close()
		}
		t.Fatalf("changed target open err=%v", err)
	}
	child, err := os.ReadFile(filepath.Join(root, "Same", "Child.md"))
	if err != nil || !bytes.Contains(child, []byte("preserved")) {
		t.Fatalf("restored child=%q err=%v", child, err)
	}
	unexpected, err := os.ReadFile(filepath.Join(root, "Same", "unexpected.txt"))
	if err != nil || string(unexpected) != "concurrent bytes" {
		t.Fatalf("restored concurrent bytes=%q err=%v", unexpected, err)
	}
	targets, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Same (Konflikt - *)"))
	if len(targets) != 0 {
		t.Fatalf("changed recovery target not restored: %v", targets)
	}
}

func TestSyncOnceRejectsUnsupportedNonemptyFolderCreateRecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T, *LocalCore)
	}{{"nested", func(t *testing.T, c *LocalCore) {
		if _, err := c.CreateFolder(context.Background(), "Same/Nested"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.CreateNote(context.Background(), "Same/Nested/N.md", "nested\n", nil); err != nil {
			t.Fatal(err)
		}
	}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
			remote := &memoryRemote{server: server}
			a, _, err := Initialize(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			root := t.TempDir()
			b, _, err := Initialize(ctx, root)
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
			tc.build(t, b)
			before, err := os.ReadDir(filepath.Join(root, "Same"))
			if err != nil {
				t.Fatal(err)
			}
			if err := b.SyncOnce(ctx, remote); err == nil {
				t.Fatal("unsupported subtree recovered")
			}
			after, err := os.ReadDir(filepath.Join(root, "Same"))
			if err != nil || len(after) != len(before) {
				t.Fatalf("unsupported subtree changed before=%d after=%d err=%v", len(before), len(after), err)
			}
			if _, err := os.Stat(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName)); err != nil {
				t.Fatalf("conflict namespace unavailable: %v", err)
			}
		})
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

func TestSyncOnceRecoversDirectNoteFolderCreateFromUnavailableParent(t *testing.T) {
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
	if _, err := a.CreateFolder(ctx, "Parent"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootA, "Parent")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Parent/Local"); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Parent/Local/N.md", "parent recovery v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := b.ReadNote(ctx, "Parent/Local/N.md")
	doc, _, err = b.SaveNote(ctx, "Parent/Local/N.md", doc.Revision, "parent recovery v1\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err = b.SaveNote(ctx, "Parent/Local/N.md", doc.Revision, "parent recovery final\n", []string{"final"})
	if err != nil {
		t.Fatal(err)
	}
	finalBytes, err := os.ReadFile(filepath.Join(rootB, "Parent", "Local", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	finalHash := sha256.Sum256(finalBytes)
	for i := 0; i < 4; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootB, "Parent")); !os.IsNotExist(err) {
		t.Fatalf("deleted parent remains: %v", err)
	}
	snapshot, err := b.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var recovered, recoveredNote localindex.Object
	for _, object := range snapshot.Objects {
		if strings.HasPrefix(object.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Local (Konflikt - ") && object.Type == localindex.ObjectFolder {
			recovered = object
		}
		if object.ID == note.ID {
			recoveredNote = object
		}
	}
	if recovered.ID == uuid.Nil || recovered.Type != localindex.ObjectFolder || recoveredNote.ID != note.ID || recoveredNote.ParentID != recovered.ID {
		t.Fatalf("unavailable-parent recovery=%#v note=%#v", recovered, recoveredNote)
	}
	state, ok := server.states[recovered.ID]
	noteState := server.states[note.ID]
	if !ok || state.ParentID == nil || *state.ParentID != clientsync.ConflictRecoveredID || state.ObjectType != clientsync.Folder || noteState.ParentID == nil || *noteState.ParentID != recovered.ID || !bytes.Equal(noteState.BlobHash, finalHash[:]) {
		t.Fatalf("unavailable-parent server recovery=%#v note=%#v", state, noteState)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("unavailable-parent create unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	aSnapshot, err := a.index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundFolder, foundNote := false, false
	for _, object := range aSnapshot.Objects {
		foundFolder = foundFolder || object.ID == recovered.ID
		foundNote = foundNote || object.ID == note.ID
	}
	if !foundFolder || !foundNote {
		t.Fatalf("unavailable-parent subtree did not converge folder=%t note=%t", foundFolder, foundNote)
	}
	aBytes, err := os.ReadFile(filepath.Join(rootA, filepath.FromSlash(recoveredNote.RelativePath)))
	if err != nil || !bytes.Equal(aBytes, finalBytes) {
		t.Fatalf("unavailable-parent final bytes changed=%t err=%v", bytes.Equal(aBytes, finalBytes), err)
	}
}

func TestSyncOnceRecoversEmptyFolderMoveAgainstRemoteDelete(t *testing.T) {
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
	snapshot, _ := a.index.ReadSnapshot(ctx)
	var folderID uuid.UUID
	for _, object := range snapshot.Objects {
		if object.Type == localindex.ObjectFolder && object.RelativePath == "F" {
			folderID = object.ID
		}
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootA, "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	var moveCount int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_outbox WHERE object_id=? AND mutation='move' AND status='pending'`, folderID.String()).Scan(&moveCount)
	}); err != nil || moveCount != 1 {
		t.Fatalf("folder move outbox=%d err=%v", moveCount, err)
	}
	device, inode, err := repository.RootedFolderIdentity(rootB, "Local")
	if err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderMoveDeleteReconcile = func() {
		testHookAfterConflictFolderMoveDeleteReconcile = nil
		panic("folder move/delete recovery crash")
	}
	defer func() { testHookAfterConflictFolderMoveDeleteReconcile = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected move/delete recovery crash")
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
	for i := 0; i < 4; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootB, "Local")); !os.IsNotExist(err) {
		t.Fatalf("losing move remains: %v", err)
	}
	final, _ := b.index.ReadSnapshot(ctx)
	var recovered localindex.Object
	for _, object := range final.Objects {
		if object.Type == localindex.ObjectFolder && strings.HasPrefix(object.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Local (Konflikt - ") {
			recovered = object
		}
	}
	if recovered.ID == uuid.Nil || recovered.ID == folderID || recovered.FolderDevice != device || recovered.FolderInode != inode {
		t.Fatalf("move/delete recovery=%#v", recovered)
	}
	state := server.states[folderID]
	if !state.Deleted {
		t.Fatalf("original folder not tombstoned: %#v", state)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("move/delete recovery unresolved=%t err=%v", unresolved, err)
	}
	var resolution string
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT resolution FROM sync_conflict_resolutions WHERE operation_id=(SELECT operation_id FROM sync_outbox WHERE object_id=? AND conflict_code='object_deleted' ORDER BY sequence LIMIT 1)`, folderID.String()).Scan(&resolution)
	}); err != nil || resolution != "folder_move_deleted_recovered" {
		t.Fatalf("move/delete resolution=%s err=%v", resolution, err)
	}
	recoveredState := server.states[recovered.ID]
	if recoveredState.ParentID == nil || *recoveredState.ParentID != clientsync.ConflictRecoveredID || recoveredState.Deleted {
		t.Fatalf("recovered folder server=%#v", recoveredState)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	rootC := t.TempDir()
	c, _, err := Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for label, core := range map[string]*LocalCore{"A": a, "C": c} {
		snap, _ := core.index.ReadSnapshot(ctx)
		found := false
		for _, object := range snap.Objects {
			found = found || object.ID == recovered.ID
		}
		if !found {
			t.Fatalf("%s missed recovered empty folder", label)
		}
	}
}

func TestSyncOnceRecoversDirectNotesInFolderMoveAgainstRemoteDelete(t *testing.T) {
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	snap, _ := a.index.ReadSnapshot(ctx)
	var folderID uuid.UUID
	for _, o := range snap.Objects {
		if o.RelativePath == "F" && o.Type == localindex.ObjectFolder {
			folderID = o.ID
		}
	}
	if err := os.Remove(filepath.Join(rootA, "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Local/N.md", "v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := b.ReadNote(ctx, "Local/N.md")
	doc, _, err = b.SaveNote(ctx, "Local/N.md", doc.Revision, "v1\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = b.SaveNote(ctx, "Local/N.md", doc.Revision, "final moved subtree\n", []string{"kept"})
	if err != nil {
		t.Fatal(err)
	}
	finalBytes, err := os.ReadFile(filepath.Join(rootB, "Local", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	finalHash := sha256.Sum256(finalBytes)
	testHookAfterConflictFolderMoveDeleteReconcile = func() { testHookAfterConflictFolderMoveDeleteReconcile = nil; panic("direct-note move/delete crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected recovery crash")
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
	for i := 0; i < 5; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	final, _ := b.index.ReadSnapshot(ctx)
	var recoveredFolder, recoveredNote localindex.Object
	for _, o := range final.Objects {
		if o.Type == localindex.ObjectFolder && strings.HasPrefix(o.RelativePath, clientsync.ConflictRootName+"/"+clientsync.ConflictRecoveredName+"/Local (Konflikt - ") {
			recoveredFolder = o
		}
		if o.ID == note.ID {
			recoveredNote = o
		}
	}
	if recoveredFolder.ID == uuid.Nil || recoveredFolder.ID == folderID || recoveredNote.ParentID != recoveredFolder.ID || !bytes.Equal(recoveredNote.ContentHash, finalHash[:]) {
		t.Fatalf("folder=%#v note=%#v", recoveredFolder, recoveredNote)
	}
	got, err := os.ReadFile(filepath.Join(rootB, filepath.FromSlash(recoveredNote.RelativePath)))
	if err != nil || !bytes.Equal(got, finalBytes) {
		t.Fatalf("final bytes changed err=%v", err)
	}
	if !server.states[folderID].Deleted {
		t.Fatal("original folder tombstone lost")
	}
	noteState := server.states[note.ID]
	if noteState.ParentID == nil || *noteState.ParentID != recoveredFolder.ID || !bytes.Equal(noteState.BlobHash, finalHash[:]) {
		t.Fatalf("server note=%#v", noteState)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	c, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for label, core := range map[string]*LocalCore{"A": a, "B": b, "C": c} {
		content, err := os.ReadFile(filepath.Join(core.Root(), filepath.FromSlash(recoveredNote.RelativePath)))
		if err != nil || !bytes.Equal(content, finalBytes) {
			t.Fatalf("%s bytes err=%v", label, err)
		}
	}
}

func TestSyncOnceRejectsNonemptyFolderMoveAgainstRemoteDelete(t *testing.T) {
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootA, "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateFolder(ctx, "Local/Nested"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.CreateNote(ctx, "Local/Nested/N.md", "must remain\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("nested moved folder recovered")
	}
	content, err := os.ReadFile(filepath.Join(rootB, "Local", "Nested", "N.md"))
	if err != nil || !bytes.Contains(content, []byte("must remain")) {
		t.Fatalf("nonempty move bytes=%q err=%v", content, err)
	}
}

func TestSyncOnceRejectsUnsafeDirectNotesInFolderMoveDeleteRecovery(t *testing.T) {
	cases := []string{"attempted", "branch", "disconnected", "unexpected"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
			remote := &memoryRemote{server: server}
			a, _, err := Initialize(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			root := t.TempDir()
			b, _, err := Initialize(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if _, err := a.CreateFolder(ctx, "F"); err != nil {
				t.Fatal(err)
			}
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := b.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(a.Root(), "F")); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(root, "F"), filepath.Join(root, "Local")); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcile.Run(ctx, root, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
				t.Fatal(err)
			}
			note, _, err := b.CreateNote(ctx, "Local/N.md", "preserve\n", nil)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "attempted":
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE sync_outbox SET status='attempted',attempted_at_ms=1 WHERE object_id=? AND mutation='create'`, note.ID.String())
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "branch":
				doc, _ := b.ReadNote(ctx, "Local/N.md")
				if _, _, err := b.SaveNote(ctx, "Local/N.md", doc.Revision, "linear\n", nil); err != nil {
					t.Fatal(err)
				}
				var create string
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					return tx.QueryRow(`SELECT operation_id FROM sync_outbox WHERE object_id=? AND mutation='create'`, note.ID.String()).Scan(&create)
				}); err != nil {
					t.Fatal(err)
				}
				op, _ := uuid.NewV7()
				h := sha256.Sum256([]byte("branch"))
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					if _, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',1,NULL,'',?,?,'pending',1)`, op.String(), note.ID.String(), h[:], create); err != nil {
						return err
					}
					_, err := tx.Exec(`INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, op.String(), create)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "disconnected":
				doc, _ := b.ReadNote(ctx, "Local/N.md")
				if _, _, err := b.SaveNote(ctx, "Local/N.md", doc.Revision, "linear\n", nil); err != nil {
					t.Fatal(err)
				}
				op, _ := uuid.NewV7()
				h := sha256.Sum256([]byte("disconnected"))
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					_, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',99,NULL,'',?,NULL,'pending',1)`, op.String(), note.ID.String(), h[:])
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "unexpected":
				if err := os.WriteFile(filepath.Join(root, "Local", "extra.bin"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.SyncOnce(ctx, remote); err == nil {
				t.Fatal("unsafe subtree recovered")
			}
			content, err := os.ReadFile(filepath.Join(root, "Local", "N.md"))
			inspection, inspectErr := frontmatter.Inspect(content)
			if err != nil || inspectErr != nil || inspection.NoteID != note.ID {
				t.Fatalf("bytes=%q err=%v inspect=%v", content, err, inspectErr)
			}
			targets, _ := filepath.Glob(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Local (Konflikt - *)"))
			if len(targets) != 0 {
				t.Fatalf("unsafe subtree moved: %v", targets)
			}
		})
	}
}

func TestSyncOnceRejectsLateDependentDuringFolderMoveDeleteRecovery(t *testing.T) {
	ctx := context.Background()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	a, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := t.TempDir()
	b, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.CreateFolder(ctx, "F"); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Root(), "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "F"), filepath.Join(root, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, root, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderMoveDeleteReconcile = func() {
		testHookAfterConflictFolderMoveDeleteReconcile = nil
		var move, folder string
		if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT operation_id,object_id FROM sync_outbox WHERE mutation='move' AND object_type='folder' AND status='conflict' ORDER BY sequence DESC LIMIT 1`).Scan(&move, &folder)
		}); err != nil {
			panic(err)
		}
		op, _ := uuid.NewV7()
		note := uuid.New()
		h := sha256.Sum256([]byte("late"))
		if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'create',?,'note',0,?,'Late.md',?,?,'pending',1)`, op.String(), note.String(), folder, h[:], move); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, op.String(), move)
			return err
		}); err != nil {
			panic(err)
		}
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("late dependent crossed manifest guard")
	}
	var state string
	var resolutions int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT state FROM conflict_folder_move_delete_recoveries LIMIT 1`).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_conflict_resolutions WHERE resolution='folder_move_deleted_recovered'`).Scan(&resolutions)
	}); err != nil || state != "prepared" || resolutions != 0 {
		t.Fatalf("state=%s resolutions=%d err=%v", state, resolutions, err)
	}
}

func TestSyncOnceRejectsLaterOperationDuringFolderMoveDeleteRecovery(t *testing.T) {
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
	snapshot, _ := a.index.ReadSnapshot(ctx)
	var folderID uuid.UUID
	for _, object := range snapshot.Objects {
		if object.RelativePath == "F" && object.Type == localindex.ObjectFolder {
			folderID = object.ID
		}
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootA, "F")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	device, inode, err := repository.RootedFolderIdentity(rootB, "Local")
	if err != nil {
		t.Fatal(err)
	}
	later := uuid.Must(uuid.NewV7())
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'move',?,'folder',1,NULL,'Later',NULL,NULL,'pending',1)`, later.String(), folderID.String())
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO sync_outbox_folder_intents(operation_id,folder_id,mutation_kind,source_relative,device,inode) VALUES(?,?,'move','Local',?,?)`, later.String(), folderID.String(), device, inode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err == nil {
		t.Fatal("later same-folder operation accepted")
	}
	if err := repository.VerifyRootedEmptyFolderIdentity(rootB, "Local", device, inode); err != nil {
		t.Fatal(err)
	}
	targets, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "Local (Konflikt - *)"))
	if len(targets) != 0 {
		t.Fatalf("later-operation recovery moved folder: %v", targets)
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

func TestSyncOnceRevertsFolderMoveFromDeletedParentAndKeepsChildEdit(t *testing.T) {
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
	for _, folder := range []string{"Source", "Target"} {
		if _, err := a.CreateFolder(ctx, folder); err != nil {
			t.Fatal(err)
		}
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
	if err := os.Remove(filepath.Join(rootA, "Target")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "Source"), filepath.Join(rootB, "Target", "Source")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := b.ReadNote(ctx, "Target/Source/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Target/Source/N.md", doc.Revision, "edited under stale parent\n", nil); err != nil {
		t.Fatal(err)
	}
	testHookAfterConflictFolderMoveRevert = func() {
		testHookAfterConflictFolderMoveRevert = nil
		panic("simulated unavailable-parent folder revert crash")
	}
	defer func() { testHookAfterConflictFolderMoveRevert = nil }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected unavailable-parent revert crash")
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
	restored, err := b.ReadNote(ctx, "Source/N.md")
	if err != nil || restored.ID != note.ID || !strings.Contains(restored.Body, "edited under stale parent") {
		t.Fatalf("restored stale-parent child=%#v err=%v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "Target")); !os.IsNotExist(err) {
		t.Fatalf("deleted parent remains: %v", err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("parent move conflict unresolved=%t err=%v", unresolved, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	remoteEdit, err := a.ReadNote(ctx, "Source/N.md")
	if err != nil || !strings.Contains(remoteEdit.Body, "edited under stale parent") {
		t.Fatalf("remote stale-parent child=%#v err=%v", remoteEdit, err)
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

func TestSyncOnceRecoversEmptyDivergentRootFolderMoves(t *testing.T) {
	ctx := context.Background()
	folderIDAt := func(core *LocalCore, relative string) uuid.UUID {
		t.Helper()
		snapshot, err := core.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range snapshot.Objects {
			if object.Type == localindex.ObjectFolder && object.RelativePath == relative {
				return object.ID
			}
		}
		t.Fatalf("folder %s missing; snapshot=%#v", relative, snapshot.Objects)
		return uuid.Nil
	}
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	folderID := folderIDAt(a, "F")
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootA, a.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if state := server.states[folderID]; state.Name != "Server" {
		t.Fatalf("server move state=%#v", state)
	}
	localBefore, err := os.Stat(filepath.Join(rootB, "F"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	preStore, _ := clientsync.NewStore(b.index)
	ready, err := preStore.ListReady(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var moveOperation uuid.UUID
	for _, item := range ready {
		if item.Mutation.ObjectID == folderID && item.Mutation.Kind == clientsync.Move {
			moveOperation = item.Mutation.OperationID
		}
	}
	if moveOperation == uuid.Nil {
		t.Fatal("missing local move operation")
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatalf("initial recovery sync: %v", err)
	}
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
			t.Fatalf("restart recovery sync %d: %v", i, err)
		}
	}
	if got := folderIDAt(b, "Server"); got != folderID {
		t.Fatalf("canonical id=%s want=%s", got, folderID)
	}
	recoveredName := clientsync.ConflictFolderName("Local", moveOperation)
	recoveredPath := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + recoveredName
	recoveredInfo, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recoveredPath)))
	if err != nil || !os.SameFile(localBefore, recoveredInfo) {
		t.Fatalf("recovered inode: %v", err)
	}
	recoveredID := folderIDAt(b, recoveredPath)
	if recoveredID == folderID || recoveredID == uuid.Nil {
		t.Fatalf("recovered id=%s", recoveredID)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("unresolved=%t err=%v", unresolved, err)
	}
	recovery, err := store.ConflictFolderDivergentMoveRecovery(ctx, moveOperation)
	if err != nil || recovery == nil || recovery.State != "completed" || recovery.RecoveredFolderID != recoveredID {
		t.Fatalf("recovery=%#v err=%v", recovery, err)
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conflict_folder_divergent_move_recoveries SET attempted_relative='Other' WHERE operation_id=?`, moveOperation.String())
		return err
	}); err == nil {
		t.Fatal("divergent recovery identity mutated")
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM conflict_folder_divergent_move_recoveries WHERE operation_id=?`, moveOperation.String())
		return err
	}); err == nil {
		t.Fatal("divergent recovery deleted")
	}
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO conflict_folder_divergent_move_recoveries SELECT * FROM conflict_folder_divergent_move_recoveries WHERE operation_id=?`, moveOperation.String())
		return err
	}); err == nil {
		t.Fatal("divergent recovery replaced")
	}
	var replacementCount int
	if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM sync_outbox WHERE operation_id=? AND mutation='create' AND object_id=? AND object_type='folder' AND parent_id=? AND status IN ('pending','attempted','accepted')`, recovery.NewOperationID.String(), recoveredID.String(), clientsync.ConflictRecoveredID.String()).Scan(&replacementCount)
	}); err != nil || replacementCount != 1 {
		t.Fatalf("replacement count=%d err=%v", replacementCount, err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if got := folderIDAt(a, recoveredPath); got != recoveredID {
		t.Fatalf("A recovered id=%s want=%s", got, recoveredID)
	}
}

func TestDivergentEmptyFolderMoveRecoveryFaultBoundaries(t *testing.T) {
	injected := errors.New("injected divergent recovery fault")
	cases := []struct {
		name        string
		install     func() func()
		compensated bool
	}{
		{"after evacuation move", func() func() {
			testHookAfterDivergentFolderEvacuationMove = func() error { return injected }
			return func() { testHookAfterDivergentFolderEvacuationMove = nil }
		}, true},
		{"after evacuation reconcile", func() func() {
			testHookAfterDivergentFolderEvacuationReconcile = func() error { return injected }
			return func() { testHookAfterDivergentFolderEvacuationReconcile = nil }
		}, true},
		{"evacuated transition failure", func() func() {
			testHookBeforeDivergentFolderEvacuatedTransition = func() error { return injected }
			return func() { testHookBeforeDivergentFolderEvacuatedTransition = nil }
		}, true},
		{"after canonical stage create", func() func() {
			testHookAfterDivergentCanonicalStageCreate = func() error { return injected }
			return func() { testHookAfterDivergentCanonicalStageCreate = nil }
		}, false},
		{"after canonical publish rename", func() func() {
			testHookAfterDivergentCanonicalPublish = func() error { return injected }
			return func() { testHookAfterDivergentCanonicalPublish = nil }
		}, false},
		{"after canonical cleanup", func() func() {
			testHookAfterDivergentCanonicalCleanup = func() error { return injected }
			return func() { testHookAfterDivergentCanonicalCleanup = nil }
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := b.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := a.Snapshot(ctx)
			var folderID uuid.UUID
			for _, object := range snapshot.Objects {
				if object.RelativePath == "F" {
					folderID = object.ID
				}
			}
			if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcile.Run(ctx, rootA, a.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
				t.Fatal(err)
			}
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
				t.Fatal(err)
			}
			store, _ := clientsync.NewStore(b.index)
			ready, _ := store.ListReady(ctx, 100)
			var operation uuid.UUID
			for _, item := range ready {
				if item.Mutation.ObjectID == folderID {
					operation = item.Mutation.OperationID
				}
			}
			clear := tc.install()
			err = b.SyncOnce(ctx, remote)
			clear()
			if !errors.Is(err, injected) {
				t.Fatalf("fault sync=%v", err)
			}
			if tc.name == "after canonical cleanup" {
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE conflict_folder_divergent_move_recoveries SET state='completed' WHERE operation_id=?`, operation.String())
					return err
				}); err == nil {
					t.Fatal("adjacent completed state spoof accepted")
				}
			}
			if tc.compensated {
				if info, statErr := os.Stat(filepath.Join(rootB, "Local")); statErr != nil || !info.IsDir() {
					t.Fatalf("source not compensated: %v", statErr)
				}
			}
			for i := 0; i < 4; i++ {
				if err := b.SyncOnce(ctx, remote); err != nil {
					t.Fatalf("resume %d: %v", i, err)
				}
			}
			recovery, err := store.ConflictFolderDivergentMoveRecovery(ctx, operation)
			if err != nil || recovery == nil || recovery.State != "completed" {
				t.Fatalf("recovery=%#v err=%v", recovery, err)
			}
			canonical, err := os.Stat(filepath.Join(rootB, "Server"))
			if err != nil || !canonical.IsDir() {
				t.Fatalf("canonical missing: %v", err)
			}
			recovered, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recovery.RecoveryRelative)))
			if err != nil || !recovered.IsDir() {
				t.Fatalf("recovered missing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rootB, "Local")); !os.IsNotExist(err) {
				t.Fatalf("source remains after recovery: %v", err)
			}
		})
	}
}

func TestSyncOnceRecoversEditedDirectNoteInDivergentRootFolderMove(t *testing.T) {
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
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	folderID := func() uuid.UUID {
		snapshot, _ := b.Snapshot(ctx)
		for _, o := range snapshot.Objects {
			if o.RelativePath == "F" {
				return o.ID
			}
		}
		return uuid.Nil
	}()
	if folderID == uuid.Nil {
		t.Fatal("folder missing")
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	note, _, err := b.CreateNote(ctx, "Local/N.md", "first\n", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := b.ReadNote(ctx, "Local/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Local/N.md", current.Revision, "final bytes\n", []string{"final"}); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(rootB, "Local", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(filepath.Join(rootB, "Local"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(b.index)
	ready, err := store.ListReady(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var moveOperation uuid.UUID
	for _, item := range ready {
		if item.Mutation.ObjectID == folderID && item.Mutation.Kind == clientsync.Move {
			moveOperation = item.Mutation.OperationID
		}
	}
	if moveOperation == uuid.Nil {
		t.Fatal("move operation missing")
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootA, a.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil && !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	store, _ = clientsync.NewStore(b.index)
	for i := 0; i < 6; i++ {
		if err := b.SyncOnce(ctx, remote); err != nil && !errors.Is(err, ErrUnresolvedOutbound) {
			recovery, _ := store.ConflictFolderDivergentMoveRecovery(ctx, moveOperation)
			snapshot, _ := b.Snapshot(ctx)
			plan, _ := store.ActiveApplyPlan(ctx)
			t.Fatalf("recovery sync %d: %v recovery=%#v plan=%#v objects=%#v", i, err, recovery, plan, snapshot.Objects)
		}
	}
	recovered := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFolderName("Local", moveOperation)
	actual, err := os.ReadFile(filepath.Join(rootB, filepath.FromSlash(recovered), "N.md"))
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("recovered bytes err=%v", err)
	}
	inspection, err := frontmatter.Inspect(actual)
	if err != nil || inspection.NoteID != note.ID {
		t.Fatalf("recovered identity=%s err=%v", inspection.NoteID, err)
	}
	recoveredInfo, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recovered)))
	if err != nil || !os.SameFile(sourceInfo, recoveredInfo) {
		t.Fatalf("recovered inode err=%v", err)
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || unresolved {
		t.Fatalf("unresolved=%t err=%v", unresolved, err)
	}
}

func TestSyncOnceRejectsUnsafeDivergentDirectNoteRecoveries(t *testing.T) {
	cases := []string{"branch", "attempted", "nested", "unindexed"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
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
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := b.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := b.Snapshot(ctx)
			var folderID uuid.UUID
			for _, o := range snapshot.Objects {
				if o.RelativePath == "F" {
					folderID = o.ID
				}
			}
			if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
				t.Fatal(err)
			}
			store, _ := clientsync.NewStore(b.index)
			switch kind {
			case "branch", "attempted":
				note, _, err := b.CreateNote(ctx, "Local/N.md", "one\n", nil)
				if err != nil {
					t.Fatal(err)
				}
				var createOp uuid.UUID
				if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
					var raw string
					if err := tx.QueryRow(`SELECT operation_id FROM sync_outbox WHERE object_id=? AND mutation='create'`, note.ID.String()).Scan(&raw); err != nil {
						return err
					}
					var parseErr error
					createOp, parseErr = uuid.Parse(raw)
					return parseErr
				}); err != nil {
					t.Fatal(err)
				}
				if kind == "attempted" {
					if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
						_, err := tx.Exec(`UPDATE sync_outbox SET status='attempted',attempted_at_ms=1 WHERE operation_id=? AND status='pending'`, createOp.String())
						return err
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					current, _ := b.ReadNote(ctx, "Local/N.md")
					_, _, err = b.SaveNote(ctx, "Local/N.md", current.Revision, "two\n", nil)
					if err != nil {
						t.Fatal(err)
					}
					content, readErr := os.ReadFile(filepath.Join(rootB, "Local", "N.md"))
					if readErr != nil {
						t.Fatal(readErr)
					}
					hash := sha256.Sum256(content)
					op := uuid.Must(uuid.NewV7())
					if err := b.index.WithTransaction(ctx, func(tx *sql.Tx) error {
						if _, err := tx.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,name,blob_hash,dependency_operation_id,status,created_at_ms) VALUES(?,'update',?,'note',1,'',?,?,'pending',1)`, op.String(), note.ID.String(), hash[:], createOp.String()); err != nil {
							return err
						}
						_, err := tx.Exec(`INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, op.String(), createOp.String())
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			case "nested":
				if err := os.Mkdir(filepath.Join(rootB, "Local", "Sub"), 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := b.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			case "unindexed":
				if err := os.WriteFile(filepath.Join(rootB, "Local", "raw.bin"), []byte("raw"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(filepath.Join(rootB, "Local"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcile.Run(ctx, rootA, a.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
				t.Fatal(err)
			}
			if err := a.SyncOnce(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if err := b.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
				t.Fatalf("unsafe recovery err=%v", err)
			}
			after, err := os.Stat(filepath.Join(rootB, "Local"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("source changed: %v", err)
			}
			ready, _ := store.ListReady(ctx, 100)
			var moveOp uuid.UUID
			for _, item := range ready {
				if item.Mutation.ObjectID == folderID && item.Mutation.Kind == clientsync.Move {
					moveOp = item.Mutation.OperationID
				}
			}
			if moveOp != uuid.Nil {
				recovery, err := store.ConflictFolderDivergentMoveRecovery(ctx, moveOp)
				if err != nil || recovery != nil {
					t.Fatalf("unsafe recovery journal=%#v err=%v", recovery, err)
				}
			}
		})
	}
}

func TestSyncOnceRejectsNonemptyDivergentRootFolderMove(t *testing.T) {
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
	if _, _, err := a.CreateNote(ctx, "F/N.md", "kept\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootA, a.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(rootB, "F"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(ctx, rootB, b.index, reconcile.Options{MoveCandidates: []string{"F"}}); err != nil {
		t.Fatal(err)
	}
	local, err := os.Stat(filepath.Join(rootB, "Local"))
	if err != nil || !os.SameFile(before, local) {
		t.Fatalf("local inode before conflict: %v", err)
	}
	if err := b.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("nonempty divergent sync=%v", err)
	}
	after, err := os.Stat(filepath.Join(rootB, "Local"))
	if err != nil || !os.SameFile(local, after) {
		t.Fatalf("nonempty source changed: %v", err)
	}
	if _, err := b.ReadNote(ctx, "Local/N.md"); err != nil {
		t.Fatalf("nonempty child lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "Server")); !os.IsNotExist(err) {
		t.Fatalf("canonical target materialized for unsupported subtree: %v", err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || !unresolved {
		t.Fatalf("unresolved=%t err=%v", unresolved, err)
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
	if err := b.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("divergent folder move err=%v", err)
	}
	store, _ := clientsync.NewStore(b.index)
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil || !unresolved {
		t.Fatalf("divergent folder move unresolved=%t err=%v", unresolved, err)
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
func (r *cycleRemote) PreserveAndDeleteEmptyFolder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64) (remotehttp.PreserveDeleteFolderResult, error) {
	return remotehttp.PreserveDeleteFolderResult{}, errors.New("unsupported test resolution")
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
		return nil, clientsync.ErrBlobMissing
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

func TestSyncOnceAppliesIndependentRootNotesBehindUnresolvedIntent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"X.md", "Y.md", "Z.md", "W.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("initial "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	x, _ := core.ReadNote(ctx, "X.md")
	y, _ := core.ReadNote(ctx, "Y.md")
	z, _ := core.ReadNote(ctx, "Z.md")
	w, _ := core.ReadNote(ctx, "W.md")
	store, _ := clientsync.NewStore(core.index)
	base, _ := store.ConfirmedCursor(ctx)
	server.maxPull = 2
	if _, _, err := core.SaveNote(ctx, "X.md", x.Revision, "local unresolved\n", nil); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ListReady(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var xOperation uuid.UUID
	for _, item := range ready {
		if item.Mutation.ObjectID == x.ID {
			xOperation = item.Mutation.OperationID
		}
	}
	if xOperation == uuid.Nil {
		t.Fatal("missing X operation")
	}
	server.results[xOperation] = clientsync.Result{Conflict: "type_mismatch", Canonical: &clientsync.CanonicalState{ObjectType: clientsync.Folder, Revision: 2, Name: "X-folder"}}
	makeBytes := func(id uuid.UUID, body string) ([]byte, []byte) {
		doc, err := frontmatter.EnsureIdentity([]byte(body), id)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(doc.Markdown)
		server.blobs[hash] = doc.Markdown
		return doc.Markdown, hash[:]
	}
	xBytes, xHash := makeBytes(x.ID, "remote X\n")
	_ = xBytes
	_, y2Hash := makeBytes(y.ID, "remote Y2\n")
	y3, y3Hash := makeBytes(y.ID, "remote Y3\n")
	createID := uuid.New()
	_, createHash := makeBytes(createID, "remote create\n")
	appendChange := func(change clientsync.Change) {
		change.Cursor = uint64(len(server.changes) + 1)
		change.OperationID = uuid.Must(uuid.NewV7())
		server.changes = append(server.changes, change)
	}
	appendChange(clientsync.Change{ObjectID: x.ID, ObjectType: clientsync.Note, Mutation: clientsync.Update, Revision: 2, Name: "X.md", BlobHash: xHash})
	appendChange(clientsync.Change{ObjectID: y.ID, ObjectType: clientsync.Note, Mutation: clientsync.Update, Revision: 2, Name: "Y.md", BlobHash: y2Hash})
	appendChange(clientsync.Change{ObjectID: y.ID, ObjectType: clientsync.Note, Mutation: clientsync.Update, Revision: 3, Name: "Y.md", BlobHash: y3Hash})
	appendChange(clientsync.Change{ObjectID: z.ID, ObjectType: clientsync.Note, Mutation: clientsync.Delete, Revision: 2, Name: "Z.md", BlobHash: append([]byte(nil), server.states[z.ID].BlobHash...), Deleted: true})
	appendChange(clientsync.Change{ObjectID: createID, ObjectType: clientsync.Note, Mutation: clientsync.Create, Revision: 1, Name: "Created.md", BlobHash: createHash})
	appendChange(clientsync.Change{ObjectID: w.ID, ObjectType: clientsync.Note, Mutation: clientsync.Move, Revision: 2, Name: "Moved.md", BlobHash: append([]byte(nil), server.states[w.ID].BlobHash...)})
	appendChange(clientsync.Change{ObjectID: uuid.New(), ObjectType: clientsync.Folder, Mutation: clientsync.Create, Revision: 1, Name: "Folder"})
	q, _, err := core.CreateNote(ctx, "Q.md", "outbound while blocked\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	blockedPullStart := len(server.pullAfters)
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("sync=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Y.md")); err != nil || !bytes.Equal(got, y3) {
		t.Fatalf("Y chain=%q err=%v", got, err)
	}
	yBeforeRestart, err := os.Stat(filepath.Join(root, "Y.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Z.md")); !os.IsNotExist(err) {
		t.Fatalf("Z delete=%v", err)
	}
	if _, ok := server.states[q.ID]; !ok {
		t.Fatal("independent outbound Q was not submitted")
	}
	if len(server.pullAfters)-blockedPullStart < 4 {
		t.Fatalf("blocked pull was not paginated: %v", server.pullAfters[blockedPullStart:])
	}
	downloaded, _ := store.DownloadedCursor(ctx)
	confirmed, _ := store.ConfirmedCursor(ctx)
	if confirmed != base || downloaded != uint64(len(server.changes)) {
		t.Fatalf("confirmed=%d base=%d downloaded=%d server=%d", confirmed, base, downloaded, len(server.changes))
	}
	xItem, _, _ := store.InboxChange(ctx, base+1)
	yItem, _, _ := store.InboxChange(ctx, base+2)
	y3Item, _, _ := store.InboxChange(ctx, base+3)
	if xItem.State != "pending" || yItem.State != "applied" || y3Item.State != "applied" {
		t.Fatalf("states X/Y2/Y3=%s/%s/%s", xItem.State, yItem.State, y3Item.State)
	}
	for cursor := base + 5; cursor <= base+7; cursor++ {
		item, _, err := store.InboxChange(ctx, cursor)
		if err != nil || item.State != "pending" {
			t.Fatalf("ineligible cursor %d state=%s err=%v", cursor, item.State, err)
		}
	}
	beforePulls := len(server.pullAfters)
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core, _, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	store, _ = clientsync.NewStore(core.index)
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, ErrUnresolvedOutbound) {
		t.Fatalf("restart sync=%v", err)
	}
	if len(server.pullAfters) <= beforePulls || server.pullAfters[beforePulls] != downloaded {
		t.Fatalf("restart pulls=%v downloaded=%d", server.pullAfters[beforePulls:], downloaded)
	}
	if got, err := os.ReadFile(filepath.Join(root, "Y.md")); err != nil || !bytes.Equal(got, y3) {
		t.Fatalf("Y3=%q err=%v", got, err)
	}
	yAfterRestart, err := os.Stat(filepath.Join(root, "Y.md"))
	if err != nil || !os.SameFile(yBeforeRestart, yAfterRestart) || !yBeforeRestart.ModTime().Equal(yAfterRestart.ModTime()) {
		t.Fatalf("Y chain republished after restart: %v", err)
	}
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != base {
		t.Fatalf("restart confirmed=%d base=%d", confirmed, base)
	}
}

func TestSyncOnceLegacyReplaySkipsAlreadyAppliedIndependentStep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"X.md", "Y.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("initial "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}}
	remote := &memoryRemote{server: server}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	x, _ := core.ReadNote(ctx, "X.md")
	y, _ := core.ReadNote(ctx, "Y.md")
	store, _ := clientsync.NewStore(core.index)
	base, _ := store.ConfirmedCursor(ctx)
	remoteBytes := func(id uuid.UUID, body string) ([]byte, []byte) {
		doc, err := frontmatter.EnsureIdentity([]byte(body), id)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(doc.Markdown)
		server.blobs[hash] = doc.Markdown
		return doc.Markdown, hash[:]
	}
	xBytes, xHash := remoteBytes(x.ID, "remote X\n")
	yBytes, yHash := remoteBytes(y.ID, "remote Y\n")
	changes := []clientsync.Change{{Cursor: base + 1, OperationID: uuid.Must(uuid.NewV7()), ObjectID: x.ID, ObjectType: clientsync.Note, Mutation: clientsync.Update, Revision: 2, Name: "X.md", BlobHash: xHash}, {Cursor: base + 2, OperationID: uuid.Must(uuid.NewV7()), ObjectID: y.ID, ObjectType: clientsync.Note, Mutation: clientsync.Update, Revision: 2, Name: "Y.md", BlobHash: yHash}}
	server.changes = append(server.changes, changes...)
	if err := store.IngestPullPage(ctx, base, base+2, changes); err != nil {
		t.Fatal(err)
	}
	planID := uuid.Must(uuid.NewV7())
	if err := store.CreateInboxApplyPlan(ctx, base+2, planID); err != nil {
		t.Fatal(err)
	}
	if err := core.ExecuteActiveApplyPlan(ctx, remote); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(root, "Y.md"))
	if err != nil {
		t.Fatal(err)
	}
	folderChange := clientsync.Change{Cursor: base + 3, OperationID: uuid.Must(uuid.NewV7()), ObjectID: uuid.New(), ObjectType: clientsync.Folder, Mutation: clientsync.Create, Revision: 1, Name: "AfterReplay"}
	server.changes = append(server.changes, folderChange)
	server.states[folderChange.ObjectID] = folderChange
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != base {
		t.Fatalf("confirmed before replay=%d", confirmed)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(root, "Y.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("already-applied Y was published again")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "X.md")); !bytes.Equal(got, xBytes) {
		t.Fatalf("X=%q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "Y.md")); !bytes.Equal(got, yBytes) {
		t.Fatalf("Y=%q", got)
	}
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != base+3 {
		t.Fatalf("confirmed after replay=%d", confirmed)
	}
	if info, err := os.Stat(filepath.Join(root, "AfterReplay")); err != nil || !info.IsDir() {
		t.Fatalf("new suffix change missing: %v", err)
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
	if downloaded, err := store.DownloadedCursor(ctx); err != nil || downloaded != 1 {
		t.Fatalf("downloaded=%d err=%v", downloaded, err)
	}
	if inbox, found, err := store.InboxChange(ctx, 1); err != nil || !found || inbox.State != "pending" {
		t.Fatalf("inbox=%#v found=%t err=%v", inbox, found, err)
	}
	incidents, err := core.IntegrityIncidents(ctx, 10)
	if err != nil || len(incidents) != 1 || incidents[0].Code != "hash_mismatch" || incidents[0].ObjectID != object {
		t.Fatalf("hash incidents=%#v err=%v", incidents, err)
	}
	delete(remote.blobs, hash)
	if err := core.SyncOnce(ctx, remote); !errors.Is(err, clientsync.ErrBlobMissing) {
		t.Fatalf("missing blob retry=%v", err)
	}
	incidents, err = store.ListOpenIntegrityIncidents(ctx, 10)
	if err != nil || len(incidents) != 2 {
		t.Fatalf("missing incidents=%#v err=%v", incidents, err)
	}
	codes := map[string]bool{}
	for _, incident := range incidents {
		codes[incident.Code] = true
	}
	if !codes["missing_blob"] || !codes["hash_mismatch"] {
		t.Fatalf("incident codes=%v", codes)
	}
	if _, err := os.Stat(filepath.Join(root, "Remote.md")); !os.IsNotExist(err) {
		t.Fatalf("remote file=%v", err)
	}
}

func remoteFolderChanges(t *testing.T, names ...string) []clientsync.Change {
	t.Helper()
	changes := make([]clientsync.Change, len(names))
	for i, name := range names {
		changes[i] = clientsync.Change{Cursor: uint64(i + 1), Mutation: clientsync.Create, OperationID: uuid.Must(uuid.NewV7()), ObjectID: uuid.New(), ObjectType: clientsync.Folder, Revision: 1, Name: name}
	}
	return changes
}

func TestSyncOnceMirrorsPaginatedPullIntoAppliedInbox(t *testing.T) {
	ctx := context.Background()
	core, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	changes := remoteFolderChanges(t, "One", "Two", "Three")
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}, changes: changes, maxPull: 1}
	for _, change := range changes {
		server.states[change.ObjectID] = change
	}
	if err := core.SyncOnce(ctx, &memoryRemote{server: server}); err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(core.index)
	confirmed, err := store.ConfirmedCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := store.DownloadedCursor(ctx)
	if err != nil || confirmed != 3 || downloaded != confirmed {
		t.Fatalf("confirmed=%d downloaded=%d err=%v", confirmed, downloaded, err)
	}
	for cursor := uint64(1); cursor <= 3; cursor++ {
		item, found, err := store.InboxChange(ctx, cursor)
		if err != nil || !found || item.State != "applied" || item.ApplyingAt == nil || item.AppliedAt == nil {
			t.Fatalf("cursor=%d item=%#v found=%t err=%v", cursor, item, found, err)
		}
	}
}

func TestSyncOnceReplaysInboxExactlyAfterFailureBeforeApply(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	core, _, err := Initialize(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	changes := remoteFolderChanges(t, "Remote")
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}, changes: changes}
	server.states[changes[0].ObjectID] = changes[0]
	injected := errors.New("stop after inbox ingest")
	testHookAfterInboxIngest = func() error { testHookAfterInboxIngest = nil; return injected }
	defer func() { testHookAfterInboxIngest = nil }()
	if err := core.SyncOnce(ctx, &memoryRemote{server: server}); !errors.Is(err, injected) {
		t.Fatalf("first sync=%v", err)
	}
	store, _ := clientsync.NewStore(core.index)
	if confirmed, _ := store.ConfirmedCursor(ctx); confirmed != 0 {
		t.Fatalf("confirmed=%d", confirmed)
	}
	if downloaded, _ := store.DownloadedCursor(ctx); downloaded != 1 {
		t.Fatalf("downloaded=%d", downloaded)
	}
	if err := core.SyncOnce(ctx, &memoryRemote{server: server}); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.InboxChange(ctx, 1)
	if err != nil || !found || item.State != "applied" {
		t.Fatalf("item=%#v found=%t err=%v", item, found, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "Remote"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestSyncOnceRejectsMismatchedInboxReplay(t *testing.T) {
	ctx := context.Background()
	core, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	changes := remoteFolderChanges(t, "Original")
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}, changes: changes}
	server.states[changes[0].ObjectID] = changes[0]
	injected := errors.New("stop after inbox ingest")
	testHookAfterInboxIngest = func() error { testHookAfterInboxIngest = nil; return injected }
	defer func() { testHookAfterInboxIngest = nil }()
	if err := core.SyncOnce(ctx, &memoryRemote{server: server}); !errors.Is(err, injected) {
		t.Fatalf("first sync=%v", err)
	}
	server.changes[0].Name = "Changed"
	if err := core.SyncOnce(ctx, &memoryRemote{server: server}); err == nil || !strings.Contains(err.Error(), "payload mismatch") {
		t.Fatalf("mismatch err=%v", err)
	}
	store, _ := clientsync.NewStore(core.index)
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}

func TestSyncOnceReconcilesInboxAfterExistingApplyPlanCompletes(t *testing.T) {
	ctx := context.Background()
	core, _, err := Initialize(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	changes := remoteFolderChanges(t, "ExistingPlan")
	server := &memorySyncServer{blobs: map[[32]byte][]byte{}, results: map[uuid.UUID]clientsync.Result{}, states: map[uuid.UUID]clientsync.Change{}, changes: changes}
	server.states[changes[0].ObjectID] = changes[0]
	remote := &memoryRemote{server: server}
	store, _ := clientsync.NewStore(core.index)
	if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: uuid.Must(uuid.NewV7()), FromCursor: 0, ThroughCursor: 1, Steps: changes}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.InboxChange(ctx, 1); err != nil || found {
		t.Fatalf("inbox unexpectedly present found=%t err=%v", found, err)
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.InboxChange(ctx, 1)
	if err != nil || !found || item.State != "applied" {
		t.Fatalf("item=%#v found=%t err=%v", item, found, err)
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil || active != nil {
		t.Fatalf("active=%#v err=%v", active, err)
	}
}
