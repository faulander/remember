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

func TestIndexDurabilityPragmas(t *testing.T) {
	index, err := Open(context.Background(), filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for name, want := range map[string]string{"foreign_keys": "1", "journal_mode": "wal", "synchronous": "2"} {
		var got string
		if err := index.db.QueryRow(`PRAGMA ` + name).Scan(&got); err != nil || got != want {
			t.Errorf("pragma %s=%q err=%v want=%q", name, got, err, want)
		}
	}
}

func TestV1UpgradePreservesSnapshotAndMarksBootstrap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script) + `; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err = db.Exec(`INSERT INTO objects(object_id,object_type,relative_path,collision_path,identity_state) VALUES(?, 'folder','Legacy','legacy','known')`, id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO watcher_state(key,value) VALUES('kept','yes')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	index, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil || len(snapshot.Objects) != 1 || snapshot.Objects[0].ID != id {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	value, ok, err := index.State(ctx, "kept")
	if err != nil || !ok || value != "yes" {
		t.Fatalf("state=%q/%t %v", value, ok, err)
	}
	var bootstrap string
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT value FROM sync_state WHERE key='bootstrap_required'`).Scan(&bootstrap)
	}); err != nil || bootstrap != "1" {
		t.Fatalf("bootstrap=%q err=%v", bootstrap, err)
	}
	var version int
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`PRAGMA user_version`).Scan(&version) }); err != nil || version != 32 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestV25MigrationSeedsDownloadedCursorFromConfirmed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v24.db")
	index, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sync_state(key,value) VALUES('confirmed_cursor','42') ON CONFLICT(key) DO UPDATE SET value='42'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TRIGGER sync_folder_preserve_delete_resolution_insert_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_immutable; DROP TRIGGER sync_folder_preserve_delete_resolution_state_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete; DROP TABLE sync_folder_preserve_delete_resolutions; DROP TRIGGER conflict_folder_divergent_move_note_members_no_update; DROP TRIGGER conflict_folder_divergent_move_note_members_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_update; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_members_guard; DROP TRIGGER conflict_folder_divergent_move_note_chains_guard; DROP TABLE conflict_folder_divergent_move_note_chains; DROP TABLE conflict_folder_divergent_move_note_members; DROP TRIGGER conflict_folder_divergent_move_insert_guard; DROP TRIGGER conflict_folder_divergent_move_identity_immutable; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_move_no_delete; DROP TABLE conflict_folder_divergent_move_recoveries; DROP TRIGGER sync_integrity_incident_insert_guard; DROP TRIGGER sync_integrity_incident_binding_immutable; DROP TRIGGER sync_integrity_incident_progress_guard; DROP TRIGGER sync_integrity_incident_no_delete; DROP TRIGGER sync_integrity_incident_step_binding_immutable; DROP TRIGGER sync_integrity_incident_step_no_delete; DROP TABLE sync_integrity_incidents; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER sync_inbox_apply_plan_conflict_guard; DROP TRIGGER sync_inbox_apply_plan_insert_guard; DROP TRIGGER sync_inbox_apply_plan_immutable; DROP TRIGGER sync_inbox_apply_plan_no_delete; DROP TRIGGER sync_inbox_linked_inbox_state_guard; DROP TRIGGER sync_inbox_linked_plan_state_guard; DROP TRIGGER sync_inbox_linked_step_state_guard; DROP TRIGGER sync_inbox_linked_plan_payload_immutable; DROP TRIGGER sync_inbox_linked_plan_no_delete; DROP TRIGGER sync_inbox_linked_step_no_insert; DROP TRIGGER sync_inbox_linked_step_payload_immutable; DROP TRIGGER sync_inbox_linked_step_no_delete; DROP TABLE sync_inbox_apply_plans; DROP TABLE sync_inbox_changes; DELETE FROM sync_state WHERE key='downloaded_cursor'; PRAGMA user_version=24`); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	var downloaded string
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT value FROM sync_state WHERE key='downloaded_cursor'`).Scan(&downloaded)
	}); err != nil || downloaded != "42" {
		t.Fatalf("downloaded=%q err=%v", downloaded, err)
	}
}

func TestV26MigrationAddsInboxApplyPlanLinks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v25.db")
	index, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TRIGGER sync_folder_preserve_delete_resolution_insert_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_immutable; DROP TRIGGER sync_folder_preserve_delete_resolution_state_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete; DROP TABLE sync_folder_preserve_delete_resolutions; DROP TRIGGER conflict_folder_divergent_move_note_members_no_update; DROP TRIGGER conflict_folder_divergent_move_note_members_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_update; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_members_guard; DROP TRIGGER conflict_folder_divergent_move_note_chains_guard; DROP TABLE conflict_folder_divergent_move_note_chains; DROP TABLE conflict_folder_divergent_move_note_members; DROP TRIGGER conflict_folder_divergent_move_insert_guard; DROP TRIGGER conflict_folder_divergent_move_identity_immutable; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_move_no_delete; DROP TABLE conflict_folder_divergent_move_recoveries; DROP TRIGGER sync_integrity_incident_insert_guard; DROP TRIGGER sync_integrity_incident_binding_immutable; DROP TRIGGER sync_integrity_incident_progress_guard; DROP TRIGGER sync_integrity_incident_no_delete; DROP TRIGGER sync_integrity_incident_step_binding_immutable; DROP TRIGGER sync_integrity_incident_step_no_delete; DROP TABLE sync_integrity_incidents; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER sync_inbox_apply_plan_conflict_guard; DROP TRIGGER sync_inbox_apply_plan_insert_guard; DROP TRIGGER sync_inbox_apply_plan_immutable; DROP TRIGGER sync_inbox_apply_plan_no_delete; DROP TRIGGER sync_inbox_linked_inbox_state_guard; DROP TRIGGER sync_inbox_linked_plan_state_guard; DROP TRIGGER sync_inbox_linked_step_state_guard; DROP TRIGGER sync_inbox_linked_plan_payload_immutable; DROP TRIGGER sync_inbox_linked_plan_no_delete; DROP TRIGGER sync_inbox_linked_step_no_insert; DROP TRIGGER sync_inbox_linked_step_payload_immutable; DROP TRIGGER sync_inbox_linked_step_no_delete; DROP TABLE sync_inbox_apply_plans; PRAGMA user_version=25`); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	var version int
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`PRAGMA user_version`).Scan(&version) }); err != nil || version != 32 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		return tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='sync_inbox_apply_plans'`).Scan(&name)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsNewerLocalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=33`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("newer schema accepted")
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
