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

func TestOutboxBlobCleanupRequiresFinalUnreferencedHash(t *testing.T) {
	ctx := context.Background()
	index, err := localindex.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, _ := NewStore(index)
	hash := sha256.Sum256([]byte("blob"))
	op := uuid.Must(uuid.NewV7())
	if err := store.Enqueue(ctx, []Mutation{{OperationID: op, Kind: Create, ObjectID: uuid.New(), ObjectType: Note, Name: "N.md", BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	if items, err := store.PendingOutboxBlobCleanups(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("pending cleanup=%v err=%v", items, err)
	}
	if err := store.RecordResult(ctx, op, Result{Accepted: true, Revision: 1, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	if items, err := store.PendingOutboxBlobCleanups(ctx, 10); err != nil || len(items) != 1 || items[0].Hash != hash {
		t.Fatalf("accepted cleanup=%v err=%v", items, err)
	}
	first, _ := store.PendingOutboxBlobCleanups(ctx, 10)
	if err := store.MarkOutboxBlobCleaned(ctx, first[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOutboxBlobCleaned(ctx, first[0]); err != nil {
		t.Fatal(err)
	}
	if items, err := store.PendingOutboxBlobCleanups(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("cleaned candidates=%v err=%v", items, err)
	}
	second := uuid.Must(uuid.NewV7())
	if err := store.Enqueue(ctx, []Mutation{{OperationID: second, Kind: Create, ObjectID: uuid.New(), ObjectType: Note, Name: "Again.md", BlobHash: hash[:]}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResult(ctx, second, Result{Accepted: true, Revision: 1, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	reused, err := store.PendingOutboxBlobCleanups(ctx, 10)
	if err != nil || len(reused) != 1 || reused[0].Hash != hash || reused[0].ThroughSequence <= first[0].ThroughSequence {
		t.Fatalf("reused cleanup=%v err=%v", reused, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sync_blob_cleanups SET cleaned_at_ms=cleaned_at_ms`)
		return err
	}); err == nil {
		t.Fatal("blob cleanup history was mutable")
	}
}
