package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
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
}

type memoryRemote struct{ server *memorySyncServer }

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
func (r *memoryRemote) Submit(_ context.Context, m clientsync.Mutation) (clientsync.Result, error) {
	if result, ok := r.server.results[m.OperationID]; ok {
		return result, nil
	}
	state := r.server.states[m.ObjectID]
	if m.Kind == clientsync.Create {
		state = clientsync.Change{ObjectID: m.ObjectID, ObjectType: m.ObjectType, Revision: 1, ParentID: m.ParentID, Name: m.Name, BlobHash: append([]byte(nil), m.BlobHash...)}
	} else {
		state.Revision = m.BaseRevision + 1
		if m.Kind == clientsync.Update {
			state.BlobHash = append([]byte(nil), m.BlobHash...)
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
	page := remotehttp.PullPage{NextCursor: after}
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
