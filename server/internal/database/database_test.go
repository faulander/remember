package database

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	synccore "github.com/faulander/remember/server/internal/sync"
	"github.com/google/uuid"
)

func TestOpenConfiguresSQLiteAndMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sqlite", "remember.db")
	db, err := Open(ctx, path, 750*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	expectedPragmas := map[string]string{
		"foreign_keys":   "1",
		"journal_mode":   "wal",
		"synchronous":    "2",
		"trusted_schema": "0",
		"busy_timeout":   "750",
	}
	assertPragmas(t, ctx, db, expectedPragmas)
	db.SetMaxIdleConns(0)
	if _, err := db.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	db.SetMaxIdleConns(1)
	assertPragmas(t, ctx, db, expectedPragmas)
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 9 {
		t.Errorf("migration count = %d, want 9", count)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %o, want 600", info.Mode().Perm())
	}
}

func TestV8MigrationPreservesV1V2RowsAndCloneMappings(t *testing.T) {
	ctx := context.Background()
	db := openTestDatabase(t)
	if err := migrateFS(ctx, db, migrationFiles, "migrations/00[1-7]_*.sql"); err != nil {
		t.Fatal(err)
	}
	user, device := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms) VALUES(?,?,?,'hash',1,'active',1)`, user[:], user.String()+"@example.com", user.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms) VALUES(?,?,'Device','active',1,1)`, user[:], device[:]); err != nil {
		t.Fatal(err)
	}
	service, err := synccore.NewService(db, migrationClock{})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.ForActor(user, device)
	if err != nil {
		t.Fatal(err)
	}
	submit := func(mutation synccore.Mutation) synccore.SubmitResult {
		t.Helper()
		result, submitErr := actor.Submit(ctx, mutation)
		if submitErr != nil || !result.Accepted {
			t.Fatalf("submit=%#v err=%v", result, submitErr)
		}
		return result
	}
	newMutation := func(kind synccore.MutationKind, object uuid.UUID, typ synccore.ObjectType, base uint64) synccore.Mutation {
		return synccore.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: kind, ObjectID: object, ObjectType: typ, BaseRevision: base}
	}

	root, child, recoveredRoot, recoveredChild := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	createRoot := newMutation(synccore.MutationCreate, root, synccore.ObjectFolder, 0)
	createRoot.Name = "Root"
	submit(createRoot)
	createChild := newMutation(synccore.MutationCreate, child, synccore.ObjectFolder, 0)
	createChild.ParentID, createChild.Name = &root, "Child"
	submit(createChild)
	moveRoot := newMutation(synccore.MutationMove, root, synccore.ObjectFolder, 1)
	moveRoot.Name = "Moved"
	moved := submit(moveRoot)
	conflict := newMutation(synccore.MutationDelete, root, synccore.ObjectFolder, 1)
	conflictResult, err := actor.Submit(ctx, conflict)
	if err != nil || conflictResult.Conflict != synccore.ConflictBaseRevisionMismatch {
		t.Fatalf("conflict=%#v err=%v", conflictResult, err)
	}
	createRecoveredRoot := newMutation(synccore.MutationCreate, recoveredRoot, synccore.ObjectFolder, 0)
	createRecoveredRoot.Name = "Recovered"
	first := submit(createRecoveredRoot)
	createRecoveredChild := newMutation(synccore.MutationCreate, recoveredChild, synccore.ObjectFolder, 0)
	createRecoveredChild.ParentID, createRecoveredChild.Name = &recoveredRoot, "Child"
	cloneCreate := submit(createRecoveredChild)
	childDelete := submit(newMutation(synccore.MutationDelete, child, synccore.ObjectFolder, 1))
	rootDelete := submit(newMutation(synccore.MutationDelete, root, synccore.ObjectFolder, moved.Revision))
	v2Operation := uuid.Must(uuid.NewV7())
	if _, err = db.Exec(`INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_cursor,deleted_cursor,status,created_at_ms,request_version,known_cursor,first_cursor,last_cursor,clone_count) VALUES(?,?,?,?,?,?,?,?,?,?,'completed',1,2,?,?,?,1)`, user[:], device[:], v2Operation[:], bytes.Repeat([]byte{2}, 32), conflict.OperationID[:], root[:], moved.Revision, recoveredRoot[:], first.Cursor, rootDelete.Cursor, moved.Cursor, first.Cursor, rootDelete.Cursor); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sync_folder_preserve_delete_clones(user_id,resolution_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor) VALUES(?,?,0,?,?,?,?)`, user[:], v2Operation[:], child[:], recoveredChild[:], cloneCreate.Cursor, childDelete.Cursor); err != nil {
		t.Fatal(err)
	}

	v1Root, v1Recovered := uuid.New(), uuid.New()
	v1Create := newMutation(synccore.MutationCreate, v1Root, synccore.ObjectFolder, 0)
	v1Create.Name = "V1"
	submit(v1Create)
	v1Move := newMutation(synccore.MutationMove, v1Root, synccore.ObjectFolder, 1)
	v1Move.Name = "V1 moved"
	v1Moved := submit(v1Move)
	v1Conflict := newMutation(synccore.MutationDelete, v1Root, synccore.ObjectFolder, 1)
	v1ConflictResult, err := actor.Submit(ctx, v1Conflict)
	if err != nil || v1ConflictResult.Conflict != synccore.ConflictBaseRevisionMismatch {
		t.Fatalf("v1 conflict=%#v err=%v", v1ConflictResult, err)
	}
	v1Recovery := newMutation(synccore.MutationCreate, v1Recovered, synccore.ObjectFolder, 0)
	v1Recovery.Name = "V1 recovered"
	v1First := submit(v1Recovery)
	v1Last := submit(newMutation(synccore.MutationDelete, v1Root, synccore.ObjectFolder, v1Moved.Revision))
	v1Operation := uuid.Must(uuid.NewV7())
	if _, err = db.Exec(`INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_cursor,deleted_cursor,status,created_at_ms,request_version,known_cursor,first_cursor,last_cursor,clone_count) VALUES(?,?,?,?,?,?,?,?,?,?,'completed',1,1,NULL,?,?,0)`, user[:], device[:], v1Operation[:], bytes.Repeat([]byte{1}, 32), v1Conflict.OperationID[:], v1Root[:], v1Moved.Revision, v1Recovered[:], v1First.Cursor, v1Last.Cursor, v1First.Cursor, v1Last.Cursor); err != nil {
		t.Fatal(err)
	}

	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		operation uuid.UUID
		version   int
		clones    int
	}{
		{v1Operation, 1, 0},
		{v2Operation, 2, 1},
	} {
		var version, cloneCount, noteCount int
		var recoveredName sql.NullString
		if err = db.QueryRow(`SELECT request_version,clone_count,note_count,recovered_folder_name FROM sync_folder_preserve_delete_resolutions WHERE user_id=? AND resolution_operation_id=?`, user[:], expected.operation[:]).Scan(&version, &cloneCount, &noteCount, &recoveredName); err != nil {
			t.Fatal(err)
		}
		if version != expected.version || cloneCount != expected.clones || noteCount != 0 || recoveredName.Valid {
			t.Fatalf("migrated resolution=%d/%d/%d/%v", version, cloneCount, noteCount, recoveredName)
		}
	}
	var sourceRevision sql.NullInt64
	var cloneName sql.NullString
	if err = db.QueryRow(`SELECT source_revision,name FROM sync_folder_preserve_delete_clones WHERE user_id=? AND resolution_operation_id=?`, user[:], v2Operation[:]).Scan(&sourceRevision, &cloneName); err != nil {
		t.Fatal(err)
	}
	if sourceRevision.Valid || cloneName.Valid {
		t.Fatalf("legacy clone gained V3 descriptors: %v/%v", sourceRevision, cloneName)
	}
}

type migrationClock struct{}

func (migrationClock) Now() time.Time { return time.UnixMilli(1).UTC() }

func TestIdentitySchemaRejectsTextInBlobIdentifiers(t *testing.T) {
	t.Parallel()

	db := openTestDatabase(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`
		INSERT INTO users(id, email_delivery, email_canonical, password_hash, password_policy, status, created_at_ms)
		VALUES ('1234567890123456', 'a@example.com', 'a@example.com', 'hash', 1, 'pending_verification', 1)`)
	if err == nil {
		t.Fatal("users accepted a TEXT identifier")
	}
}

func TestSessionMigrationUpgradesExistingDevicesStrictly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDatabase(t)
	if err := migrateFS(ctx, db, migrationFiles, "migrations/00[1-3]_*.sql"); err != nil {
		t.Fatal(err)
	}
	user, device := []byte("aaaaaaaaaaaaaaaa"), []byte("dddddddddddddddd")
	if _, err := db.Exec(`INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms)
		VALUES(?,'a@example.com','a@example.com','hash',1,'active',1)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,revoked_at_ms)
		VALUES(?,?,'existing','active',42,10)`, user, device); err != nil {
		t.Fatal(err)
	}
	revokedDevice := []byte("rrrrrrrrrrrrrrrr")
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms)
		VALUES(?,?,'legacy revoked','revoked',43)`, user, revokedDevice); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var updated int64
	var activeRevoked sql.NullInt64
	if err := db.QueryRow("SELECT updated_at_ms,revoked_at_ms FROM devices WHERE user_id=? AND id=?", user, device).Scan(&updated, &activeRevoked); err != nil || updated != 42 || activeRevoked.Valid {
		t.Fatalf("normalized active device updated=%d revoked=%v err=%v", updated, activeRevoked, err)
	}
	var revokedAt int64
	if err := db.QueryRow("SELECT revoked_at_ms FROM devices WHERE user_id=? AND id=?", user, revokedDevice).Scan(&revokedAt); err != nil || revokedAt != 43 {
		t.Fatalf("normalized revoked device timestamp=%d err=%v", revokedAt, err)
	}
	if _, err := db.Exec("UPDATE devices SET status='revoked' WHERE user_id=? AND id=?", user, device); err == nil {
		t.Fatal("upgraded device accepted inconsistent revoked lifecycle")
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported an upgraded-schema violation")
	}
}

func TestSessionSchemaEnforcesTenantAndTokenIntegrity(t *testing.T) {
	t.Parallel()
	db := openTestDatabase(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	userA := []byte("aaaaaaaaaaaaaaaa")
	userB := []byte("bbbbbbbbbbbbbbbb")
	for _, user := range []struct {
		id, email []byte
	}{{userA, []byte("a@example.com")}, {userB, []byte("b@example.com")}} {
		if _, err := db.Exec(`INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms)
			VALUES(?,?,?,'hash',1,'active',1)`, user.id, user.email, user.email); err != nil {
			t.Fatal(err)
		}
	}
	device := []byte("dddddddddddddddd")
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms)
		VALUES(?,?,'invalid','active',1)`, userA, []byte("xxxxxxxxxxxxxxxx")); err == nil {
		t.Fatal("device accepted a missing updated timestamp")
	}
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms,revoked_at_ms)
		VALUES(?,?,'invalid','active',1,1,1)`, userA, []byte("yyyyyyyyyyyyyyyy")); err == nil {
		t.Fatal("active device accepted a revocation timestamp")
	}
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms)
		VALUES(?,?,'device','active',1,1)`, userA, device); err != nil {
		t.Fatal(err)
	}
	sessionID := []byte("ssssssssssssssss")
	absoluteExpiry := int64(1 + (30 * 24 * time.Hour / time.Millisecond))
	_, err := db.Exec(`INSERT INTO sessions(user_id,id,device_id,status,created_at_ms,expires_at_ms)
		VALUES(?,?,?,'active',1,?)`, userB, sessionID, device, absoluteExpiry)
	if err == nil {
		t.Fatal("session accepted a cross-tenant device")
	}
	if _, err := db.Exec(`INSERT INTO sessions(user_id,id,device_id,status,created_at_ms,expires_at_ms)
		VALUES(?,?,?,'active',1,?)`, userA, sessionID, device, absoluteExpiry); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO access_tokens(token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES('not-a-blob-hash',?,?,?,1,2)`, userA, sessionID, device)
	if err == nil {
		t.Fatal("access token accepted TEXT hash")
	}

	deviceB := []byte("eeeeeeeeeeeeeeee")
	sessionB := []byte("tttttttttttttttt")
	if _, err := db.Exec(`INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms)
		VALUES(?,?,'device B','active',1,1)`, userB, deviceB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(user_id,id,device_id,status,created_at_ms,expires_at_ms)
		VALUES(?,?,?,'active',1,?)`, userB, sessionB, deviceB, absoluteExpiry); err != nil {
		t.Fatal(err)
	}
	hashA, hashB := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if _, err := db.Exec(`INSERT INTO refresh_tokens(token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES(?,?,?,?,1,?)`, hashA, userA, sessionID, device, absoluteExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO refresh_tokens(token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES(?,?,?,?,1,?)`, hashB, userB, sessionB, deviceB, absoluteExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE refresh_tokens SET consumed_at_ms=2,replaced_by_hash=? WHERE token_hash=?`, hashB, hashA); err == nil {
		t.Fatal("refresh lineage accepted a cross-session successor")
	}
}

func TestMigrationFailureRollsBackFailedFile(t *testing.T) {
	t.Parallel()

	db := openTestDatabase(t)
	source := fstest.MapFS{
		"001_ok.sql":   {Data: []byte("CREATE TABLE stable(id INTEGER PRIMARY KEY);")},
		"002_fail.sql": {Data: []byte("CREATE TABLE transient(id INTEGER); THIS IS NOT SQL;")},
	}
	err := migrateFS(context.Background(), db, source, "*.sql")
	if err == nil {
		t.Fatal("migrateFS() accepted invalid migration")
	}
	if tableExists(t, db, "transient") {
		t.Error("failed migration left a partial table")
	}
	if !tableExists(t, db, "stable") {
		t.Error("previous successful migration was lost")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("migration ledger count = %d, want 1", count)
	}
}

func TestMigrationRejectsChangedAppliedFile(t *testing.T) {
	t.Parallel()

	db := openTestDatabase(t)
	first := fstest.MapFS{"001_test.sql": {Data: []byte("CREATE TABLE original(id INTEGER);")}}
	if err := migrateFS(context.Background(), db, first, "*.sql"); err != nil {
		t.Fatal(err)
	}
	changed := fstest.MapFS{"001_test.sql": {Data: []byte("CREATE TABLE changed(id INTEGER);")}}
	err := migrateFS(context.Background(), db, changed, "*.sql")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed migration error = %v", err)
	}
}

func TestMigrationRejectsNonPrefixLedger(t *testing.T) {
	t.Parallel()

	db := openTestDatabase(t)
	source := fstest.MapFS{
		"001_one.sql": {Data: []byte("CREATE TABLE one(id INTEGER);")},
		"002_two.sql": {Data: []byte("CREATE TABLE two(id INTEGER);")},
	}
	if err := migrateFS(context.Background(), db, source, "*.sql"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	err := migrateFS(context.Background(), db, source, "*.sql")
	if err == nil || !strings.Contains(err.Error(), "exact history prefix") {
		t.Fatalf("non-prefix ledger error = %v", err)
	}
}

func TestMigrationRejectsUnknownAppliedVersion(t *testing.T) {
	t.Parallel()

	db := openTestDatabase(t)
	source := fstest.MapFS{"001_test.sql": {Data: []byte("CREATE TABLE one(id INTEGER);")}}
	if err := migrateFS(context.Background(), db, source, "*.sql"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, name, checksum) VALUES (99, '099_future.sql', 'x')"); err != nil {
		t.Fatal(err)
	}
	if err := migrateFS(context.Background(), db, source, "*.sql"); err == nil {
		t.Fatal("migrateFS() accepted unknown applied version")
	}
}

func TestDiscoverMigrationsRejectsDuplicateVersions(t *testing.T) {
	t.Parallel()

	source := fstest.MapFS{
		"001_one.sql": {Data: []byte("SELECT 1;")},
		"001_two.sql": {Data: []byte("SELECT 2;")},
	}
	if _, err := discoverMigrations(source, "*.sql"); err == nil {
		t.Fatal("discoverMigrations() accepted duplicate versions")
	}
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func assertPragmas(t *testing.T, ctx context.Context, db *sql.DB, expected map[string]string) {
	t.Helper()
	for pragma, want := range expected {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("read PRAGMA %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}
