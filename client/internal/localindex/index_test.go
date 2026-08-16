package localindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`PRAGMA user_version`).Scan(&version) }); err != nil || version != schemaVersion {
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
		if _, err := tx.Exec(`DROP VIEW sync_inbox_valid_nested_bindings; DROP VIEW sync_inbox_valid_parent_bindings; DROP VIEW sync_independent_inbox_candidates; DROP VIEW sync_inbox_note_ancestry; DROP TRIGGER sync_inbox_parent_binding_insert_guard; DROP TRIGGER sync_inbox_parent_binding_no_update; DROP TRIGGER sync_inbox_parent_binding_no_delete; DROP TABLE sync_inbox_parent_bindings`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sync_state(key,value) VALUES('confirmed_cursor','42') ON CONFLICT(key) DO UPDATE SET value='42'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP VIEW conflict_folder_divergent_tree_replacements_complete; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_update_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_no_delete; DROP TRIGGER conflict_folder_divergent_tree_member_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_member_no_update; DROP TRIGGER conflict_folder_divergent_tree_member_no_delete; DROP TRIGGER conflict_folder_divergent_tree_chain_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_chain_no_update; DROP TRIGGER conflict_folder_divergent_tree_chain_no_delete; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_no_update; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_no_delete; DROP TABLE conflict_folder_divergent_tree_canonical_folders; DROP TABLE conflict_folder_divergent_tree_note_chains; DROP TABLE conflict_folder_divergent_tree_members; DROP TABLE conflict_folder_divergent_tree_manifests; CREATE VIEW sync_unresolved_local_intents AS SELECT object_id FROM sync_outbox WHERE 0; CREATE TRIGGER conflict_folder_divergent_move_state_guard BEFORE UPDATE ON conflict_folder_divergent_move_recoveries BEGIN SELECT 1; END`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TRIGGER sync_folder_preserve_delete_note_insert_guard; DROP TRIGGER sync_folder_preserve_delete_note_no_update; DROP TRIGGER sync_folder_preserve_delete_note_no_delete; DROP TABLE sync_folder_preserve_delete_note_moves; DROP TRIGGER sync_folder_preserve_delete_clone_insert_guard; DROP TRIGGER sync_folder_preserve_delete_clone_no_update; DROP TRIGGER sync_folder_preserve_delete_clone_no_delete; DROP TABLE sync_folder_preserve_delete_clones; DROP TRIGGER sync_folder_preserve_delete_resolution_insert_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_immutable; DROP TRIGGER sync_folder_preserve_delete_resolution_seal_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete; DROP TABLE sync_folder_preserve_delete_resolutions; DROP TRIGGER conflict_folder_divergent_move_note_members_no_update; DROP TRIGGER conflict_folder_divergent_move_note_members_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_update; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_members_guard; DROP TRIGGER conflict_folder_divergent_move_note_chains_guard; DROP TABLE conflict_folder_divergent_move_note_chains; DROP TABLE conflict_folder_divergent_move_note_members; DROP TRIGGER conflict_folder_divergent_move_insert_guard; DROP TRIGGER conflict_folder_divergent_move_identity_immutable; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_move_no_delete; DROP TABLE conflict_folder_divergent_move_recoveries; DROP TRIGGER sync_integrity_incident_insert_guard; DROP TRIGGER sync_integrity_incident_binding_immutable; DROP TRIGGER sync_integrity_incident_progress_guard; DROP TRIGGER sync_integrity_incident_no_delete; DROP TRIGGER sync_integrity_incident_step_binding_immutable; DROP TRIGGER sync_integrity_incident_step_no_delete; DROP TABLE sync_integrity_incidents; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER sync_inbox_apply_plan_conflict_guard; DROP TRIGGER sync_inbox_apply_plan_insert_guard; DROP TRIGGER sync_inbox_apply_plan_immutable; DROP TRIGGER sync_inbox_apply_plan_no_delete; DROP TRIGGER sync_inbox_linked_inbox_state_guard; DROP TRIGGER sync_inbox_linked_plan_state_guard; DROP TRIGGER sync_inbox_linked_step_state_guard; DROP TRIGGER sync_inbox_linked_plan_payload_immutable; DROP TRIGGER sync_inbox_linked_plan_no_delete; DROP TRIGGER sync_inbox_linked_step_no_insert; DROP TRIGGER sync_inbox_linked_step_payload_immutable; DROP TRIGGER sync_inbox_linked_step_no_delete; DROP TABLE sync_inbox_apply_plans; DROP TABLE sync_inbox_changes; DELETE FROM sync_state WHERE key='downloaded_cursor'; PRAGMA user_version=24`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS conflict_folder_move_delete_recoveries_manifest_guard; DROP TRIGGER IF EXISTS conflict_folder_divergent_move_state_guard; DROP TRIGGER IF EXISTS sync_conflict_resolutions_recursive_local_folder_guard; DROP TABLE conflict_recursive_local_folder_note_chains; DROP TABLE conflict_recursive_local_folder_members; DROP TABLE conflict_recursive_local_folder_recoveries`); err != nil {
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
		if _, err := tx.Exec(`DROP VIEW sync_inbox_valid_nested_bindings; DROP VIEW sync_inbox_valid_parent_bindings; DROP VIEW sync_independent_inbox_candidates; DROP VIEW sync_inbox_note_ancestry; DROP TRIGGER sync_inbox_parent_binding_insert_guard; DROP TRIGGER sync_inbox_parent_binding_no_update; DROP TRIGGER sync_inbox_parent_binding_no_delete; DROP TABLE sync_inbox_parent_bindings`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP VIEW conflict_folder_divergent_tree_replacements_complete; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_update_guard; DROP TRIGGER conflict_folder_divergent_tree_manifest_no_delete; DROP TRIGGER conflict_folder_divergent_tree_member_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_member_no_update; DROP TRIGGER conflict_folder_divergent_tree_member_no_delete; DROP TRIGGER conflict_folder_divergent_tree_chain_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_chain_no_update; DROP TRIGGER conflict_folder_divergent_tree_chain_no_delete; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_insert_guard; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_no_update; DROP TRIGGER conflict_folder_divergent_tree_canonical_folder_no_delete; DROP TABLE conflict_folder_divergent_tree_canonical_folders; DROP TABLE conflict_folder_divergent_tree_note_chains; DROP TABLE conflict_folder_divergent_tree_members; DROP TABLE conflict_folder_divergent_tree_manifests; CREATE VIEW sync_unresolved_local_intents AS SELECT object_id FROM sync_outbox WHERE 0; CREATE TRIGGER conflict_folder_divergent_move_state_guard BEFORE UPDATE ON conflict_folder_divergent_move_recoveries BEGIN SELECT 1; END`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TRIGGER sync_folder_preserve_delete_note_insert_guard; DROP TRIGGER sync_folder_preserve_delete_note_no_update; DROP TRIGGER sync_folder_preserve_delete_note_no_delete; DROP TABLE sync_folder_preserve_delete_note_moves; DROP TRIGGER sync_folder_preserve_delete_clone_insert_guard; DROP TRIGGER sync_folder_preserve_delete_clone_no_update; DROP TRIGGER sync_folder_preserve_delete_clone_no_delete; DROP TABLE sync_folder_preserve_delete_clones; DROP TRIGGER sync_folder_preserve_delete_resolution_insert_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_immutable; DROP TRIGGER sync_folder_preserve_delete_resolution_seal_guard; DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete; DROP TABLE sync_folder_preserve_delete_resolutions; DROP TRIGGER conflict_folder_divergent_move_note_members_no_update; DROP TRIGGER conflict_folder_divergent_move_note_members_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_update; DROP TRIGGER conflict_folder_divergent_move_note_chains_no_delete; DROP TRIGGER conflict_folder_divergent_move_note_members_guard; DROP TRIGGER conflict_folder_divergent_move_note_chains_guard; DROP TABLE conflict_folder_divergent_move_note_chains; DROP TABLE conflict_folder_divergent_move_note_members; DROP TRIGGER conflict_folder_divergent_move_insert_guard; DROP TRIGGER conflict_folder_divergent_move_identity_immutable; DROP TRIGGER conflict_folder_divergent_move_state_guard; DROP TRIGGER conflict_folder_divergent_move_no_delete; DROP TABLE conflict_folder_divergent_move_recoveries; DROP TRIGGER sync_integrity_incident_insert_guard; DROP TRIGGER sync_integrity_incident_binding_immutable; DROP TRIGGER sync_integrity_incident_progress_guard; DROP TRIGGER sync_integrity_incident_no_delete; DROP TRIGGER sync_integrity_incident_step_binding_immutable; DROP TRIGGER sync_integrity_incident_step_no_delete; DROP TABLE sync_integrity_incidents; DROP VIEW sync_unresolved_local_intents; DROP TRIGGER sync_inbox_apply_plan_conflict_guard; DROP TRIGGER sync_inbox_apply_plan_insert_guard; DROP TRIGGER sync_inbox_apply_plan_immutable; DROP TRIGGER sync_inbox_apply_plan_no_delete; DROP TRIGGER sync_inbox_linked_inbox_state_guard; DROP TRIGGER sync_inbox_linked_plan_state_guard; DROP TRIGGER sync_inbox_linked_step_state_guard; DROP TRIGGER sync_inbox_linked_plan_payload_immutable; DROP TRIGGER sync_inbox_linked_plan_no_delete; DROP TRIGGER sync_inbox_linked_step_no_insert; DROP TRIGGER sync_inbox_linked_step_payload_immutable; DROP TRIGGER sync_inbox_linked_step_no_delete; DROP TABLE sync_inbox_apply_plans; PRAGMA user_version=25`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS conflict_folder_move_delete_recoveries_manifest_guard; DROP TRIGGER IF EXISTS conflict_folder_divergent_move_state_guard; DROP TRIGGER IF EXISTS sync_conflict_resolutions_recursive_local_folder_guard; DROP TABLE conflict_recursive_local_folder_note_chains; DROP TABLE conflict_recursive_local_folder_members; DROP TABLE conflict_recursive_local_folder_recoveries`); err != nil {
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
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`PRAGMA user_version`).Scan(&version) }); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var name string
		return tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='sync_inbox_apply_plans'`).Scan(&name)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestV35MigrationPreservesPreparedAndResolvedV2Rows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v34.db")
	db := openLocalSchemaVersion(t, path, 34)
	preparedConflict, resolvedConflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	preparedResolution, resolvedResolution := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	preparedFolder, resolvedFolder := uuid.New(), uuid.New()
	for _, row := range []struct {
		conflict, folder uuid.UUID
	}{
		{preparedConflict, preparedFolder},
		{resolvedConflict, resolvedFolder},
	} {
		if _, err := db.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,status,conflict_code,created_at_ms) VALUES(?,'delete',?,'folder',1,NULL,'',NULL,'conflict','base_revision_mismatch',1)`, row.conflict.String(), row.folder.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO sync_conflict_states(operation_id,object_type,revision,parent_id,name,blob_hash,deleted) VALUES(?,'folder',2,NULL,'Moved',NULL,0)`, row.conflict.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor) VALUES(?,?,?,2,'prepared',2,9)`, preparedConflict.String(), preparedResolution.String(), preparedFolder.String()); err != nil {
		t.Fatal(err)
	}
	recoveredRoot, originalChild, recoveredChild := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Exec(`INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor,clone_count) VALUES(?,?,?,2,'prepared',2,9,1)`, resolvedConflict.String(), resolvedResolution.String(), resolvedFolder.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sync_folder_preserve_delete_resolutions SET state='resolved',recovered_folder_id=?,recovered_cursor=10,deleted_cursor=13,first_cursor=10,last_cursor=13 WHERE conflict_operation_id=?`, recoveredRoot.String(), resolvedConflict.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_folder_preserve_delete_clones(conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor) VALUES(?,0,?,?,11,12)`, resolvedConflict.String(), originalChild.String(), recoveredChild.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	index, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, queryErr := tx.Query(`SELECT conflict_operation_id,resolution_operation_id,state,request_version,known_cursor,clone_count,note_count,recovered_folder_name FROM sync_folder_preserve_delete_resolutions ORDER BY conflict_operation_id`)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var conflict, resolution, state string
			var version, known, cloneCount, noteCount int
			var recoveredName sql.NullString
			if queryErr = rows.Scan(&conflict, &resolution, &state, &version, &known, &cloneCount, &noteCount, &recoveredName); queryErr != nil {
				return queryErr
			}
			if version != 2 || known != 9 || noteCount != 0 || recoveredName.Valid {
				t.Fatalf("migrated row=%s/%s/%d/%d/%d/%v", conflict, state, version, known, noteCount, recoveredName)
			}
			if conflict == preparedConflict.String() && (resolution != preparedResolution.String() || state != "prepared" || cloneCount != 0) {
				t.Fatalf("prepared row=%s/%s/%d", resolution, state, cloneCount)
			}
			if conflict == resolvedConflict.String() && (resolution != resolvedResolution.String() || state != "resolved" || cloneCount != 1) {
				t.Fatalf("resolved row=%s/%s/%d", resolution, state, cloneCount)
			}
			seen[conflict] = true
		}
		if queryErr = rows.Err(); queryErr != nil {
			return queryErr
		}
		if !seen[preparedConflict.String()] || !seen[resolvedConflict.String()] {
			t.Fatalf("migrated rows=%v", seen)
		}
		var sourceRevision sql.NullInt64
		var name sql.NullString
		if queryErr = tx.QueryRow(`SELECT source_revision,name FROM sync_folder_preserve_delete_clones WHERE conflict_operation_id=?`, resolvedConflict.String()).Scan(&sourceRevision, &name); queryErr != nil {
			return queryErr
		}
		if sourceRevision.Valid || name.Valid {
			t.Fatalf("legacy clone gained V3 descriptors: %v/%v", sourceRevision, name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestV36MigrationBuildsNestedInboxEligibilityView(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v35.db")
	db := openLocalSchemaVersion(t, path, 35)
	parent, nested, root := uuid.New(), uuid.New(), uuid.New()
	nestedOperation, rootOperation := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := bytes.Repeat([]byte{7}, sha256.Size)
	if _, err := db.Exec(`INSERT INTO objects(object_id,object_type,relative_path,collision_path,parent_id,folder_device,folder_inode,identity_state) VALUES(?,'folder','Folder','folder',NULL,11,22,'known')`, parent.String()); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id       uuid.UUID
		revision int
	}{
		{parent, 3},
		{nested, 1},
		{root, 1},
	} {
		if _, err := db.Exec(`INSERT INTO sync_baselines(object_id,revision) VALUES(?,?)`, row.id.String(), row.revision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE sync_baselines SET operation_id=? WHERE object_id=?`, uuid.Must(uuid.NewV7()).String(), parent.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_inbox_changes(cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,state,ingested_at_ms) VALUES(1,?,?,'update','note',2,NULL,'Root.md',?,0,'pending',1),(2,?,?,'update','note',2,?,'Nested.md',?,0,'pending',1)`, rootOperation.String(), root.String(), hash, nestedOperation.String(), nested.String(), parent.String(), hash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	index, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err := index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		if version != schemaVersion {
			t.Fatalf("version=%d", version)
		}
		rows, err := tx.Query(`SELECT cursor,parent_id FROM sync_independent_inbox_candidates ORDER BY cursor`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var cursor int
			var parentID sql.NullString
			if err := rows.Scan(&cursor, &parentID); err != nil {
				return err
			}
			got = append(got, fmt.Sprintf("%d:%s", cursor, parentID.String))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		want := []string{"1:", "2:" + parent.String()}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("eligible rows=%v want=%v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestV39MigrationPreservesDirectBindingAndRootEligibility(t *testing.T) {
	db := openLocalSchemaVersion(t, filepath.Join(t.TempDir(), "v36.db"), 36)
	defer db.Close()

	parent, nested, root := uuid.New(), uuid.New(), uuid.New()
	parentOperation, nestedOperation, rootOperation := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	plan := uuid.Must(uuid.NewV7())
	hash := bytes.Repeat([]byte{9}, sha256.Size)
	if _, err := db.Exec(`INSERT INTO objects(object_id,object_type,relative_path,collision_path,parent_id,folder_device,folder_inode,identity_state) VALUES(?,'folder','Folder','folder',NULL,11,22,'known')`, parent.String()); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id        uuid.UUID
		revision  int
		operation any
	}{
		{parent, 3, parentOperation.String()},
		{nested, 1, nil},
		{root, 1, nil},
	} {
		if _, err := db.Exec(`INSERT INTO sync_baselines(object_id,revision,operation_id) VALUES(?,?,?)`, row.id.String(), row.revision, row.operation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sync_inbox_changes(cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,state,ingested_at_ms) VALUES(1,?,?,'update','note',2,?,'Nested.md',?,0,'pending',1),(2,?,?,'update','note',2,NULL,'Root.md',?,0,'pending',1)`, nestedOperation.String(), nested.String(), parent.String(), hash, rootOperation.String(), root.String(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO apply_plans(plan_id,from_cursor,through_cursor,status,created_at_ms) VALUES(?,0,1,'prepared',1)`, plan.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_inbox_parent_bindings(plan_id,inbox_cursor,parent_id,parent_relative,device,inode,baseline_revision,baseline_operation_id) VALUES(?,1,?,'Folder',11,22,3,?)`, plan.String(), parent.String(), parentOperation.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) VALUES(?,0,1,?,?,'update','note',2,?,'Nested.md',?,'pending')`, plan.String(), nestedOperation.String(), nested.String(), parent.String(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_inbox_apply_plans(plan_id,cursor) VALUES(?,1)`, plan.String()); err != nil {
		t.Fatal(err)
	}

	script, err := migrations.ReadFile("migrations/039_recursive_note_inbox_ancestry.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=39`); err != nil {
		t.Fatal(err)
	}

	var depth, cursor, revision int
	var ancestorID, relative, operation string
	var ancestorParent sql.NullString
	var device, inode int64
	if err = db.QueryRow(`SELECT inbox_cursor,depth,ancestor_id,ancestor_parent_id,ancestor_relative,device,inode,baseline_revision,baseline_operation_id FROM sync_inbox_parent_bindings WHERE plan_id=?`, plan.String()).Scan(&cursor, &depth, &ancestorID, &ancestorParent, &relative, &device, &inode, &revision, &operation); err != nil {
		t.Fatal(err)
	}
	if cursor != 1 || depth != 1 || ancestorID != parent.String() || ancestorParent.Valid || relative != "Folder" || device != 11 || inode != 22 || revision != 3 || operation != parentOperation.String() {
		t.Fatalf("migrated binding=%d/%d/%s/%v/%s/%d/%d/%d/%s", cursor, depth, ancestorID, ancestorParent, relative, device, inode, revision, operation)
	}
	var eligible []int
	rows, err := db.Query(`SELECT cursor FROM sync_independent_inbox_candidates ORDER BY cursor`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cursor int
		if err := rows.Scan(&cursor); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		eligible = append(eligible, cursor)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eligible, []int{1, 2}) {
		t.Fatalf("eligible=%v", eligible)
	}
	if _, err := db.Exec(`UPDATE sync_inbox_parent_bindings SET ancestor_relative='Changed' WHERE plan_id=?`, plan.String()); err == nil {
		t.Fatal("migrated direct binding became mutable")
	}
}

func TestV40MigrationPreservesPreparedDivergentRecoveryAndAcceptsSealedTree(t *testing.T) {
	db := openLocalSchemaVersion(t, filepath.Join(t.TempDir(), "v39.db"), 39)
	defer db.Close()
	operation, replacementRootOperation, baselineOperation, replacementNoteOperation := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	rootID, recoveredRootID, noteID, recoveredNoteID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	nonce := bytes.Repeat([]byte{1}, sha256.Size)
	sourceHash, recoveredHash := bytes.Repeat([]byte{2}, sha256.Size), bytes.Repeat([]byte{3}, sha256.Size)
	recoveryRelative := "_Konflikte/Wiederhergestellt/Local (Konflikt - " + operation.String() + ")"
	if _, err := db.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,status,conflict_code,created_at_ms) VALUES(?,'move',?,'folder',1,NULL,'Local',NULL,'conflict','base_revision_mismatch',1)`, operation.String(), rootID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_conflict_states(operation_id,object_type,revision,parent_id,name,blob_hash,deleted) VALUES(?,'folder',2,NULL,'Server',NULL,0)`, operation.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_outbox_folder_intents(operation_id,folder_id,mutation_kind,source_relative,device,inode) VALUES(?,?,'move','F',11,22)`, operation.String(), rootID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conflict_folder_divergent_move_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'prepared')`, operation.String(), rootID.String(), recoveredRootID.String(), replacementRootOperation.String(), "Local", "Server", recoveryRelative, 11, 22, 2, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_baselines(object_id,revision,operation_id) VALUES(?,1,?)`, noteID.String(), baselineOperation.String()); err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/040_divergent_server_known_tree.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=40`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conflict_folder_divergent_tree_manifests(operation_id,new_root_operation_id,member_count,known_count,sealed) VALUES(?,?,?,?,0)`, operation.String(), replacementRootOperation.String(), 1, 1); err != nil {
		t.Fatal(err)
	}
	blockedNoteID, blockedRecoveredNoteID := uuid.New(), uuid.New()
	blockedBaselineOperation, blockedUpdateOperation, blockedReplacementOperation := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`INSERT INTO sync_baselines(object_id,revision,operation_id) VALUES(?,1,?)`, blockedNoteID.String(), blockedBaselineOperation.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,name,blob_hash,status,created_at_ms) VALUES(?,'update',?,'note',1,'',?,'pending',2)`, blockedUpdateOperation.String(), blockedNoteID.String(), sourceHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conflict_folder_divergent_tree_members(operation_id,ordinal,source_object_id,recovered_object_id,source_parent_id,recovered_parent_id,source_operation_id,new_operation_id,object_type,name,relative_path,depth,source_revision,source_blob_hash,recovered_blob_hash) VALUES(?,1,?,?,?,?,?,?,'note','Blocked.md','Blocked.md',1,1,?,?)`, operation.String(), blockedNoteID.String(), blockedRecoveredNoteID.String(), rootID.String(), recoveredRootID.String(), blockedBaselineOperation.String(), blockedReplacementOperation.String(), sourceHash, recoveredHash); err == nil {
		t.Fatal("known descendant with open intent entered divergent tree manifest")
	}
	if _, err := db.Exec(`INSERT INTO conflict_folder_divergent_tree_members(operation_id,ordinal,source_object_id,recovered_object_id,source_parent_id,recovered_parent_id,source_operation_id,new_operation_id,object_type,name,relative_path,depth,source_revision,source_blob_hash,recovered_blob_hash) VALUES(?,1,?,?,?,?,?,?,'note','N.md','N.md',1,1,?,?)`, operation.String(), noteID.String(), recoveredNoteID.String(), rootID.String(), recoveredRootID.String(), baselineOperation.String(), replacementNoteOperation.String(), sourceHash, recoveredHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE conflict_folder_divergent_tree_manifests SET sealed=1 WHERE operation_id=?`, operation.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE conflict_folder_divergent_move_recoveries SET state='evacuated' WHERE operation_id=?`, operation.String()); err != nil {
		t.Fatal(err)
	}
	var state string
	var memberCount, sealed int
	if err := db.QueryRow(`SELECT r.state,m.member_count,m.sealed FROM conflict_folder_divergent_move_recoveries r JOIN conflict_folder_divergent_tree_manifests m ON m.operation_id=r.operation_id WHERE r.operation_id=?`, operation.String()).Scan(&state, &memberCount, &sealed); err != nil {
		t.Fatal(err)
	}
	if state != "evacuated" || memberCount != 1 || sealed != 1 {
		t.Fatalf("migrated recovery state=%s members=%d sealed=%d", state, memberCount, sealed)
	}
	if _, err := db.Exec(`UPDATE conflict_folder_divergent_tree_members SET name='Changed.md' WHERE operation_id=?`, operation.String()); err == nil {
		t.Fatal("sealed migrated tree member became mutable")
	}
}

func openLocalSchemaVersion(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA recursive_triggers=ON`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for next := 1; next <= version; next++ {
		prefix := fmt.Sprintf("%03d_", next)
		var filename string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) {
				filename = "migrations/" + entry.Name()
				break
			}
		}
		script, readErr := migrations.ReadFile(filename)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = db.Exec(string(script)); err != nil {
			t.Fatalf("migration %d: %v", next, err)
		}
		if _, err = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", next)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestOpenRejectsNewerLocalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=41`); err != nil {
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

func TestV37MigrationPreservesLegacyRowsAndGuardsRecursiveFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v36.db")
	db := openLocalSchemaVersion(t, path, 36)
	preparedV1, preparedV3, resolvedV2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	resolutionV1, resolutionV3, resolutionV2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folderV1, folderV3, folderV2 := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		conflict uuid.UUID
		folder   uuid.UUID
	}{
		{preparedV1, folderV1},
		{preparedV3, folderV3},
		{resolvedV2, folderV2},
	} {
		if _, err := db.Exec(`INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,status,conflict_code,created_at_ms) VALUES(?,'delete',?,'folder',1,NULL,'',NULL,'conflict','base_revision_mismatch',1)`, row.conflict.String(), row.folder.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO sync_conflict_states(operation_id,object_type,revision,parent_id,name,blob_hash,deleted) VALUES(?,'folder',2,NULL,'Moved',NULL,0)`, row.conflict.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor) VALUES(?,?,?,2,'prepared',1,NULL),(?,?,?,2,'prepared',3,9),(?,?,?,2,'prepared',2,9)`, preparedV1.String(), resolutionV1.String(), folderV1.String(), preparedV3.String(), resolutionV3.String(), folderV3.String(), resolvedV2.String(), resolutionV2.String(), folderV2.String()); err != nil {
		t.Fatal(err)
	}
	recoveredRoot, originalChild, recoveredChild := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Exec(`UPDATE sync_folder_preserve_delete_resolutions SET state='sealing',recovered_folder_id=?,recovered_cursor=10,deleted_cursor=13,first_cursor=10,last_cursor=13,clone_count=1 WHERE conflict_operation_id=?`, recoveredRoot.String(), resolvedV2.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sync_folder_preserve_delete_clones(conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor) VALUES(?,0,?,?,11,12)`, resolvedV2.String(), originalChild.String(), recoveredChild.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sync_folder_preserve_delete_resolutions SET state='resolved' WHERE conflict_operation_id=?`, resolvedV2.String()); err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/037_recursive_preserve_delete.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
	var rowCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id IN(?,?,?)`, preparedV1.String(), preparedV3.String(), resolvedV2.String()).Scan(&rowCount); err != nil || rowCount != 3 {
		t.Fatalf("rows=%d err=%v", rowCount, err)
	}
	var sourceParent, targetParent sql.NullString
	var depth sql.NullInt64
	if err = db.QueryRow(`SELECT source_parent_id,target_parent_id,depth FROM sync_folder_preserve_delete_clones WHERE conflict_operation_id=?`, resolvedV2.String()).Scan(&sourceParent, &targetParent, &depth); err != nil {
		t.Fatal(err)
	}
	if sourceParent.Valid || targetParent.Valid || depth.Valid {
		t.Fatalf("legacy recursive fields=%v/%v/%v", sourceParent, targetParent, depth)
	}
	if _, err = db.Exec(`UPDATE sync_folder_preserve_delete_clones SET source_parent_id=?,target_parent_id=?,depth=1 WHERE conflict_operation_id=?`, folderV2.String(), recoveredRoot.String(), resolvedV2.String()); err == nil {
		t.Fatal("recursive clone fields were mutable")
	}
	if _, err = db.Exec(`UPDATE sync_folder_preserve_delete_resolutions SET request_version=4 WHERE conflict_operation_id=?`, preparedV3.String()); err == nil {
		t.Fatal("request version changed without resolution identity transition")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}
