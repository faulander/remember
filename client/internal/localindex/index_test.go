package localindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func TestOpenReplaceReadAndReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), ".remember", "index.db")
	index, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	hash := sha256.Sum256([]byte("note bytes"))
	folderID := uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc")
	noteID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	want := Snapshot{
		Objects: []Object{
			{
				ID: folderID, Type: ObjectFolder, RelativePath: "Folder",
				CollisionPath: "folder", IdentityState: IdentityNew,
			},
			{
				ID: noteID, Type: ObjectNote, RelativePath: "Folder/Note.md",
				CollisionPath: "folder/note.md", ParentID: folderID,
				ContentHash: append([]byte(nil), hash[:]...), IdentityState: IdentityKnown,
			},
		},
		Issues: []Issue{{Code: "invalid_name", RelativePath: "Bad.", Detail: "trailing_character"}},
	}
	if err := index.ReplaceSnapshot(ctx, want); err != nil {
		t.Fatalf("ReplaceSnapshot() error = %v", err)
	}
	got, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadSnapshot() = %#v, want %#v", got, want)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer reopened.Close()
	got, err = reopened.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot() after reopen error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot after reopen = %#v, want %#v", got, want)
	}
}

func TestReplaceSnapshotRollsBackOnConstraintFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	original := Snapshot{Objects: []Object{{
		ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		Type: ObjectNote, RelativePath: "Original.md", CollisionPath: "original.md",
		ContentHash: bytes.Repeat([]byte{1}, sha256.Size), IdentityState: IdentityKnown,
	}}}
	if err := index.ReplaceSnapshot(ctx, original); err != nil {
		t.Fatal(err)
	}

	invalid := Snapshot{Objects: []Object{
		{
			ID:   uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc"),
			Type: ObjectFolder, RelativePath: "One", CollisionPath: "same", IdentityState: IdentityNew,
		},
		{
			ID:   uuid.MustParse("018f4c3a-5678-7abc-8123-123456789abc"),
			Type: ObjectFolder, RelativePath: "One", CollisionPath: "one-duplicate", IdentityState: IdentityNew,
		},
	}}
	if err := index.ReplaceSnapshot(ctx, invalid); err == nil {
		t.Fatal("ReplaceSnapshot() accepted duplicate relative path")
	}
	got, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("failed replacement changed snapshot: got %#v, want %#v", got, original)
	}
}

func TestReplaceSnapshotRetainsCaseCollidingPathsWithIssues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	snapshot := Snapshot{
		Objects: []Object{
			{ID: uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc"), Type: ObjectNote, RelativePath: "Note.md", CollisionPath: "note.md", IdentityState: IdentityKnown},
			{ID: uuid.MustParse("018f4c3a-5678-7abc-8123-123456789abc"), Type: ObjectNote, RelativePath: "note.MD", CollisionPath: "note.md", IdentityState: IdentityKnown},
		},
		Issues: []Issue{
			{Code: "name_collision", RelativePath: "Note.md", Detail: "note.md"},
			{Code: "name_collision", RelativePath: "note.MD", Detail: "note.md"},
		},
	}
	if err := index.ReplaceSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("ReplaceSnapshot() rejected visible collision: %v", err)
	}
	got, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, snapshot) {
		t.Errorf("snapshot = %#v, want %#v", got, snapshot)
	}
}

func TestIndexSchemaContainsNoNoteContentColumn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	rows, err := index.db.QueryContext(ctx, "PRAGMA table_info(objects)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "content" || name == "markdown" || name == "frontmatter" {
			t.Errorf("objects table contains forbidden content column %q", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceSnapshotRejectsInvalidHashBeforeMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	err = index.ReplaceSnapshot(ctx, Snapshot{Objects: []Object{{
		ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		Type: ObjectNote, RelativePath: "Note.md", CollisionPath: "note.md",
		ContentHash: []byte{1, 2, 3}, IdentityState: IdentityKnown,
	}}})
	if err == nil {
		t.Fatal("ReplaceSnapshot() accepted invalid content hash")
	}
	got, readErr := index.ReadSnapshot(ctx)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(got.Objects) != 0 || len(got.Issues) != 0 {
		t.Errorf("invalid replacement mutated index: %#v", got)
	}
}
