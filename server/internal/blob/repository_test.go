package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/database"
	"github.com/google/uuid"
)

func TestPutGetDurableDedupeAndPermissions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	content := []byte("# durable markdown\n")
	hash := sha256.Sum256(content)
	first, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content))
	if err != nil || first != second {
		t.Fatalf("dedupe result=%#v err=%v", second, err)
	}
	got, err := f.users[0].Get(context.Background(), hash)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("get=%q err=%v", got, err)
	}
	path := hashPath(f.blobs, hash)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("blob mode=%o", info.Mode().Perm())
	}
	for _, root := range []string{f.blobs, f.staging} {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("root mode=%o", info.Mode().Perm())
		}
	}
	f.repo.Close()
	if _, err := f.users[0].Get(context.Background(), hash); !errors.Is(err, ErrClosed) {
		t.Errorf("closed get=%v", err)
	}
}

func TestPutLimitsAndHashMismatchLeaveNoPublishedBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	content := bytes.Repeat([]byte{'x'}, int(MaxBlobBytes)+1)
	hash := sha256.Sum256(content)
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large=%v", err)
	}
	small := []byte("small")
	wrong := sha256.Sum256([]byte("other"))
	if _, err := f.users[0].Put(context.Background(), wrong, bytes.NewReader(small)); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("mismatch=%v", err)
	}
	entries, _ := os.ReadDir(f.staging)
	if len(entries) != 0 {
		t.Errorf("staging entries=%d", len(entries))
	}
}

func TestQuotaBoundaryIdempotenceAndTenantLogicalDedupe(t *testing.T) {
	t.Parallel()
	f := newFixtureWithQuota(t, 2, 8)
	first := []byte("12345")
	firstHash := sha256.Sum256(first)
	if _, err := f.users[0].Put(context.Background(), firstHash, bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Put(context.Background(), firstHash, bytes.NewReader(first)); err != nil {
		t.Fatalf("idempotent entitlement consumed quota: %v", err)
	}
	last := []byte("678")
	lastHash := sha256.Sum256(last)
	if _, err := f.users[0].Put(context.Background(), lastHash, bytes.NewReader(last)); err != nil {
		t.Fatalf("exact quota boundary rejected: %v", err)
	}
	over := []byte("x")
	overHash := sha256.Sum256(over)
	if _, err := f.users[0].Put(context.Background(), overHash, bytes.NewReader(over)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over quota error=%v", err)
	}
	if _, err := os.Stat(hashPath(f.blobs, overHash)); !os.IsNotExist(err) {
		t.Fatalf("over-quota blob was published: %v", err)
	}
	if report, err := f.repo.Audit(context.Background()); err != nil || report.Orphans != 0 {
		t.Fatalf("over-quota audit=%#v err=%v", report, err)
	}
	// Global physical dedupe does not waive tenant-logical quota accounting.
	if _, err := f.users[1].Put(context.Background(), firstHash, bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[1].Put(context.Background(), lastHash, bytes.NewReader(last)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[1].Put(context.Background(), overHash, bytes.NewReader(over)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second tenant logical quota error=%v", err)
	}
}

func TestConcurrentQuotaRaceAllowsOnlyOnePublication(t *testing.T) {
	f := newFixtureWithQuota(t, 1, 6)
	var sequence int
	var databaseName, databasePath string
	if err := f.db.QueryRow("PRAGMA database_list").Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	otherDB, err := database.Open(context.Background(), databasePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	otherRepo, err := openWithQuota(otherDB, f.blobs, f.staging, fixedClock{time.Unix(100, 0)}, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer otherRepo.Close()
	otherUser, err := otherRepo.ForUser(f.ids[0])
	if err != nil {
		t.Fatal(err)
	}
	contents := [][]byte{[]byte("aaaa"), []byte("bbbb")}
	boundUsers := []*UserRepository{f.users[0], otherUser}
	start := make(chan struct{})
	results := make(chan struct {
		hash [sha256.Size]byte
		err  error
	}, 2)
	for index, content := range contents {
		content := content
		bound := boundUsers[index]
		go func() {
			<-start
			hash := sha256.Sum256(content)
			_, err := bound.Put(context.Background(), hash, bytes.NewReader(content))
			results <- struct {
				hash [sha256.Size]byte
				err  error
			}{hash, err}
		}()
	}
	close(start)
	success, quota := 0, 0
	var rejected [sha256.Size]byte
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			success++
		case errors.Is(result.err, ErrQuotaExceeded):
			quota++
			rejected = result.hash
		default:
			t.Fatalf("unexpected quota race error=%v", result.err)
		}
	}
	if success != 1 || quota != 1 {
		t.Fatalf("quota race success=%d quota=%d", success, quota)
	}
	if _, err := os.Stat(hashPath(f.blobs, rejected)); !os.IsNotExist(err) {
		t.Fatalf("quota loser published final blob: %v", err)
	}
	if report, err := f.repo.Audit(context.Background()); err != nil || report.Orphans != 0 || report.Registered != 1 {
		t.Fatalf("quota race audit=%#v err=%v", report, err)
	}
}

func TestCrossTenantUnknownAndInactiveAreSameUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 2)
	content := []byte("private")
	hash := sha256.Sum256(content)
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	unknown := sha256.Sum256([]byte("unknown"))
	_, foreignErr := f.users[1].Get(context.Background(), hash)
	_, unknownErr := f.users[1].Get(context.Background(), unknown)
	if !errors.Is(foreignErr, ErrUnavailable) || !errors.Is(unknownErr, ErrUnavailable) || foreignErr.Error() != unknownErr.Error() {
		t.Fatalf("foreign=%v unknown=%v", foreignErr, unknownErr)
	}
	if _, err := f.db.Exec("UPDATE users SET status='deletion_pending' WHERE id=?", f.ids[0][:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Get(context.Background(), hash); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("inactive get=%v", err)
	}
	inactiveContent := []byte("inactive write")
	inactiveHash := sha256.Sum256(inactiveContent)
	if _, err := f.users[0].Put(context.Background(), inactiveHash, bytes.NewReader(inactiveContent)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("inactive put=%v", err)
	}
	if _, err := os.Stat(hashPath(f.blobs, inactiveHash)); !os.IsNotExist(err) {
		t.Fatalf("inactive user published blob: %v", err)
	}
}

func TestEntitledMissingCorruptAndExistingCorruptTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	one := []byte("one")
	oneHash := sha256.Sum256(one)
	if _, err := f.users[0].Put(context.Background(), oneHash, bytes.NewReader(one)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hashPath(f.blobs, oneHash)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Get(context.Background(), oneHash); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("missing=%v", err)
	}

	two := []byte("two")
	twoHash := sha256.Sum256(two)
	if _, err := f.users[0].Put(context.Background(), twoHash, bytes.NewReader(two)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hashPath(f.blobs, twoHash), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Get(context.Background(), twoHash); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt get=%v", err)
	}
	if _, err := f.users[0].Put(context.Background(), twoHash, bytes.NewReader(two)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt dedupe target=%v", err)
	}
}

func TestPutPromotesVerifiedUnavailableRegistration(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	content := []byte("verified promotion")
	hash := sha256.Sum256(content)
	if _, err := f.db.Exec("INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms) VALUES(?,?,0,1)", hash[:], len(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	var available int
	if err := f.db.QueryRow("SELECT available FROM content_blobs WHERE hash=?", hash[:]).Scan(&available); err != nil || available != 1 {
		t.Fatalf("available=%d err=%v", available, err)
	}
}

func TestAuditDoesNotClassifyUnavailableRegistrationAsOrphan(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	content := []byte("registered but unavailable")
	hash := sha256.Sum256(content)
	path := hashPath(f.blobs, hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms) VALUES(?,?,0,1)", hash[:], len(content)); err != nil {
		t.Fatal(err)
	}
	report, err := f.repo.Audit(context.Background())
	if err != nil || report.Orphans != 0 || report.Registered != 1 {
		t.Fatalf("audit=%#v err=%v", report, err)
	}
}

func TestInsecureBlobPermissionsFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission policy")
	}
	f := newFixture(t, 1)
	content := []byte("private permissions")
	hash := sha256.Sum256(content)
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hashPath(f.blobs, hash), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Get(context.Background(), hash); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("insecure get=%v", err)
	}
	report, err := f.repo.Audit(context.Background())
	if err != nil || report.Corrupt != 1 || report.Malformed == 0 || report.Healthy() {
		t.Fatalf("audit=%#v err=%v", report, err)
	}
}

func TestDatabaseFailureLeavesAuditableOrphan(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_blob_entitlement BEFORE INSERT ON user_content_blobs
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	content := []byte("orphan after db failure")
	hash := sha256.Sum256(content)
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err == nil {
		t.Fatal("DB failure accepted")
	}
	if _, err := os.Stat(hashPath(f.blobs, hash)); err != nil {
		t.Fatalf("published orphan missing: %v", err)
	}
	report, err := f.repo.Audit(context.Background())
	if err != nil || report.Orphans != 1 {
		t.Fatalf("audit=%#v err=%v", report, err)
	}
}

func TestRecoverStagingAndRejectUnsafeEntries(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 1)
	if err := os.WriteFile(filepath.Join(f.staging, ".upload-0123456789abcdef0123456789abcdef"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := f.repo.RecoverStaging(context.Background())
	if err != nil || report.Removed != 1 {
		t.Fatalf("recovery=%#v err=%v", report, err)
	}
	if err := os.WriteFile(filepath.Join(f.staging, "unexpected"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.RecoverStaging(context.Background()); !errors.Is(err, ErrUnsafeStorage) {
		t.Fatalf("unexpected entry=%v", err)
	}
	if err := os.Remove(filepath.Join(f.staging, "unexpected")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(t.TempDir(), filepath.Join(f.staging, ".upload-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.RecoverStaging(context.Background()); !errors.Is(err, ErrUnsafeStorage) {
			t.Fatalf("staging symlink=%v", err)
		}
	}
}

func TestAuditReportsMissingCorruptOrphanMalformedAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink audit requires privileges")
	}
	f := newFixture(t, 1)
	missingContent := []byte("missing")
	missing := sha256.Sum256(missingContent)
	corruptContent := []byte("corrupt registered")
	corrupt := sha256.Sum256(corruptContent)
	for hash, content := range map[[32]byte][]byte{missing: missingContent, corrupt: corruptContent} {
		if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(hashPath(f.blobs, missing)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hashPath(f.blobs, corrupt), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphanContent := []byte("orphan")
	orphan := sha256.Sum256(orphanContent)
	if err := os.MkdirAll(filepath.Dir(hashPath(f.blobs, orphan)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hashPath(f.blobs, orphan), orphanContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.blobs, "malformed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(f.blobs, "link")); err != nil {
		t.Fatal(err)
	}
	report, err := f.repo.Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Missing != 1 || report.Corrupt != 1 || report.Orphans != 1 || report.Malformed != 1 || report.Symlinks != 1 {
		t.Fatalf("audit=%#v", report)
	}
}

func TestCloseWaitsForActivePutBeforeClosingRootHandles(t *testing.T) {
	f := newFixture(t, 1)
	content := []byte("close synchronization")
	hash := sha256.Sum256(content)
	reader := &blockingReader{data: content, started: make(chan struct{}), release: make(chan struct{})}
	putDone := make(chan error, 1)
	go func() {
		_, err := f.users[0].Put(context.Background(), hash, reader)
		putDone <- err
	}()
	<-reader.started
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.repo.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during active Put: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(reader.release)
	if err := <-putDone; err != nil {
		t.Fatalf("Put failed while Close waited: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := f.users[0].Get(context.Background(), hash); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close Get error = %v", err)
	}
}

func TestSecuredRootHandlesSurvivePathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production secured handles are Unix-specific")
	}
	f := newFixture(t, 1)
	heldBlobs := f.blobs + "-held"
	heldStaging := f.staging + "-held"
	if err := os.Rename(f.blobs, heldBlobs); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.staging, heldStaging); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.staging, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("anchored roots")
	hash := sha256.Sum256(content)
	if _, err := f.users[0].Put(context.Background(), hash, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hashPath(heldBlobs, hash)); err != nil {
		t.Fatalf("blob not published through retained root: %v", err)
	}
	if entries, err := os.ReadDir(f.blobs); err != nil || len(entries) != 0 {
		t.Fatalf("replacement blob root was used: entries=%d err=%v", len(entries), err)
	}
	if report, err := f.repo.Audit(context.Background()); err != nil || !report.Healthy() {
		t.Fatalf("anchored audit=%#v err=%v", report, err)
	}
}

func TestOpenRejectsSameAndSymlinkRoots(t *testing.T) {
	t.Parallel()
	db, cleanup := testDB(t)
	defer cleanup()
	root := t.TempDir()
	if _, err := OpenWithQuota(db, filepath.Join(root, "invalid-blobs"), filepath.Join(root, "invalid-stage"), 0); err == nil {
		t.Fatal("zero quota accepted")
	}
	if _, err := OpenWithQuota(db, filepath.Join(root, "huge-blobs"), filepath.Join(root, "huge-stage"), MaxUserQuotaBytes+1); err == nil {
		t.Fatal("oversized quota accepted")
	}
	if _, err := Open(db, root, root); !errors.Is(err, ErrUnsafeStorage) {
		t.Fatalf("same root=%v", err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(db, link, t.TempDir()); !errors.Is(err, ErrUnsafeStorage) {
			t.Fatalf("symlink root=%v", err)
		}
	}
}

type blockingReader struct {
	mu               sync.Mutex
	data             []byte
	started, release chan struct{}
	announced        bool
}

func (r *blockingReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	if !r.announced {
		r.announced = true
		close(r.started)
		r.mu.Unlock()
		<-r.release
		r.mu.Lock()
	}
	defer r.mu.Unlock()
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer, r.data)
	r.data = r.data[count:]
	return count, nil
}

type fixture struct {
	db             *sql.DB
	repo           *Repository
	users          []*UserRepository
	ids            []uuid.UUID
	blobs, staging string
}

func newFixture(t *testing.T, users int) *fixture {
	t.Helper()
	return newFixtureWithQuota(t, users, DefaultUserQuotaBytes)
}

func newFixtureWithQuota(t *testing.T, users int, quota int64) *fixture {
	t.Helper()
	db, cleanup := testDB(t)
	t.Cleanup(cleanup)
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	staging := filepath.Join(root, "staging")
	repo, err := openWithQuota(db, blobs, staging, fixedClock{time.Unix(100, 0)}, quota)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	f := &fixture{db: db, repo: repo, blobs: blobs, staging: staging}
	for range users {
		id, _ := uuid.NewV7()
		if _, err := db.Exec(`INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms)
			VALUES(?,?,?,?,1,'active',1)`, id[:], id.String()+"@example.com", id.String()+"@example.com", "hash"); err != nil {
			t.Fatal(err)
		}
		bound, err := repo.ForUser(id)
		if err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, id)
		f.users = append(f.users, bound)
	}
	return f
}

type fixedClock struct{ time.Time }

func (c fixedClock) Now() time.Time { return c.Time }
func testDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background(), db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db, func() { db.Close() }
}
func hashPath(root string, hash [32]byte) string {
	encoded := fmtHash(hash)
	return filepath.Join(root, "sha256", encoded[:2], encoded[2:4], encoded)
}
func fmtHash(hash [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range hash {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&15]
	}
	return string(out)
}
