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
	if count != 4 {
		t.Errorf("migration count = %d, want 4", count)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %o, want 600", info.Mode().Perm())
	}
}

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
