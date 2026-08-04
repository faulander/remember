// Package blob stores immutable, content-addressed Markdown payloads.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	MaxBlobBytes          int64 = 8 * 1024 * 1024
	DefaultUserQuotaBytes int64 = 1024 * 1024 * 1024
	MaxUserQuotaBytes     int64 = 1024 * 1024 * 1024 * 1024
)

var (
	ErrUnavailable   = errors.New("blob unavailable")
	ErrTooLarge      = errors.New("blob exceeds 8 MiB")
	ErrHashMismatch  = errors.New("blob hash mismatch")
	ErrQuotaExceeded = errors.New("user blob quota exceeded")
	ErrIntegrity     = errors.New("blob integrity failure")
	ErrUnsafeStorage = errors.New("unsafe blob storage")
	ErrClosed        = errors.New("blob repository closed")
)

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Repository struct {
	db                    *sql.DB
	blobRoot, stagingRoot string
	blobStorage           *securedRoot
	stagingStorage        *securedRoot
	clock                 Clock
	userQuotaBytes        int64
	mu                    sync.RWMutex
	closed                bool
}

type UserRepository struct {
	repository *Repository
	userID     uuid.UUID
}
type PutResult struct {
	Hash [sha256.Size]byte
	Size int64
}
type RecoveryReport struct{ Removed int }
type AuditReport struct {
	Registered int
	Missing    int
	Corrupt    int
	Orphans    int
	Malformed  int
	Symlinks   int
}

func (r AuditReport) Healthy() bool {
	return r.Missing == 0 && r.Corrupt == 0 && r.Malformed == 0 && r.Symlinks == 0
}

func Open(db *sql.DB, blobRoot, stagingRoot string) (*Repository, error) {
	return OpenWithQuota(db, blobRoot, stagingRoot, DefaultUserQuotaBytes)
}

func OpenWithQuota(db *sql.DB, blobRoot, stagingRoot string, userQuotaBytes int64) (*Repository, error) {
	return openWithQuota(db, blobRoot, stagingRoot, systemClock{}, userQuotaBytes)
}

func open(db *sql.DB, blobRoot, stagingRoot string, clock Clock) (*Repository, error) {
	return openWithQuota(db, blobRoot, stagingRoot, clock, DefaultUserQuotaBytes)
}

func openWithQuota(db *sql.DB, blobRoot, stagingRoot string, clock Clock, userQuotaBytes int64) (*Repository, error) {
	if db == nil || clock == nil {
		return nil, errors.New("blob repository dependency is nil")
	}
	if userQuotaBytes <= 0 || userQuotaBytes > MaxUserQuotaBytes {
		return nil, errors.New("invalid user blob quota")
	}
	blobs, err := prepareRoot(blobRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare blob root: %w", err)
	}
	staging, err := prepareRoot(stagingRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare staging root: %w", err)
	}
	if blobs == staging {
		return nil, fmt.Errorf("%w: roots must be distinct", ErrUnsafeStorage)
	}
	blobInfo, blobErr := os.Stat(blobs)
	stagingInfo, stagingErr := os.Stat(staging)
	if blobErr != nil || stagingErr != nil || os.SameFile(blobInfo, stagingInfo) {
		return nil, fmt.Errorf("%w: roots resolve to the same directory", ErrUnsafeStorage)
	}
	if err := ensureSameFilesystem(blobs, staging); err != nil {
		return nil, err
	}
	blobStorage, err := openSecuredRoot(blobs)
	if err != nil {
		return nil, err
	}
	stagingStorage, err := openSecuredRoot(staging)
	if err != nil {
		_ = blobStorage.close()
		return nil, err
	}
	sameIdentity, sameFilesystem, identityErr := securedRootIdentity(blobStorage, stagingStorage)
	if identityErr != nil || sameIdentity || !sameFilesystem {
		_ = stagingStorage.close()
		_ = blobStorage.close()
		return nil, fmt.Errorf("%w: secured roots are not distinct on one filesystem", ErrUnsafeStorage)
	}
	if err := ensureBlobBase(blobStorage); err != nil {
		_ = stagingStorage.close()
		_ = blobStorage.close()
		return nil, err
	}
	return &Repository{
		db: db, blobRoot: blobs, stagingRoot: staging,
		blobStorage: blobStorage, stagingStorage: stagingStorage, clock: clock,
		userQuotaBytes: userQuotaBytes,
	}, nil
}

func prepareRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
		return "", ErrUnsafeStorage
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafeStorage
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		return "", err
	}
	if err := syncRootCreation(resolved); err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func (r *Repository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	stagingErr := r.stagingStorage.close()
	blobErr := r.blobStorage.close()
	if stagingErr != nil {
		return stagingErr
	}
	return blobErr
}
func (r *Repository) beginOperation() (func(), error) {
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, ErrClosed
	}
	return r.mu.RUnlock, nil
}
func (r *Repository) ForUser(userID uuid.UUID) (*UserRepository, error) {
	done, err := r.beginOperation()
	if err != nil {
		return nil, err
	}
	defer done()
	if userID == uuid.Nil || userID.Variant() != uuid.RFC4122 || userID.Version() != 7 {
		return nil, fmt.Errorf("invalid blob user id")
	}
	return &UserRepository{repository: r, userID: userID}, nil
}

func (u *UserRepository) Put(ctx context.Context, expected [sha256.Size]byte, source io.Reader) (PutResult, error) {
	if source == nil {
		return PutResult{}, errors.New("blob source is nil")
	}
	r := u.repository
	done, err := r.beginOperation()
	if err != nil {
		return PutResult{}, err
	}
	defer done()
	stage, file, err := createStage(r.stagingStorage)
	if err != nil {
		return PutResult{}, err
	}
	keep := true
	defer func() {
		if keep {
			_ = removeStage(r.stagingStorage, stage)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, MaxBlobBytes+1))
	if err != nil {
		file.Close()
		return PutResult{}, fmt.Errorf("stream blob: %w", err)
	}
	if written > MaxBlobBytes {
		file.Close()
		return PutResult{}, ErrTooLarge
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return PutResult{}, fmt.Errorf("sync staged blob: %w", err)
	}
	if err := file.Close(); err != nil {
		return PutResult{}, fmt.Errorf("close staged blob: %w", err)
	}
	var actual [sha256.Size]byte
	copy(actual[:], hash.Sum(nil))
	if !bytes.Equal(actual[:], expected[:]) {
		return PutResult{}, ErrHashMismatch
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PutResult{}, fmt.Errorf("begin blob entitlement: %w", err)
	}
	defer tx.Rollback()
	// A write before quota reads serializes independent Repository instances on
	// SQLite's durable state. This prevents two concurrent new entitlements from
	// both observing the same remaining quota.
	activeResult, err := tx.ExecContext(ctx, `UPDATE users SET id=id WHERE id=? AND status='active'`, u.userID[:])
	if err != nil {
		return PutResult{}, fmt.Errorf("lock blob user: %w", err)
	}
	activeRows, err := activeResult.RowsAffected()
	if err != nil {
		return PutResult{}, fmt.Errorf("count active blob user: %w", err)
	}
	if activeRows != 1 {
		return PutResult{}, ErrUnavailable
	}
	var entitled int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM user_content_blobs WHERE user_id=? AND hash=?)`, u.userID[:], expected[:]).Scan(&entitled); err != nil {
		return PutResult{}, fmt.Errorf("read blob entitlement: %w", err)
	}
	if entitled == 0 {
		var usage int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0)
			FROM user_content_blobs ub JOIN content_blobs b ON b.hash=ub.hash
			WHERE ub.user_id=?`, u.userID[:]).Scan(&usage); err != nil {
			return PutResult{}, fmt.Errorf("read blob quota usage: %w", err)
		}
		if usage < 0 || written > r.userQuotaBytes || usage > r.userQuotaBytes-written {
			return PutResult{}, ErrQuotaExceeded
		}
	}
	if err := publish(r.stagingStorage, stage, r.blobStorage, expected, written); err != nil {
		return PutResult{}, err
	}
	keep = false // publish removes staging on success or verified dedupe.
	now := r.clock.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms)
		VALUES(?,?,1,?) ON CONFLICT(hash) DO NOTHING`, expected[:], written, now); err != nil {
		return PutResult{}, fmt.Errorf("register blob: %w", err)
	}
	var size int64
	var available int
	if err := tx.QueryRowContext(ctx, "SELECT size_bytes,available FROM content_blobs WHERE hash=?", expected[:]).Scan(&size, &available); err != nil {
		return PutResult{}, fmt.Errorf("validate blob registration: %w", err)
	}
	if size != written || (available != 0 && available != 1) {
		return PutResult{}, ErrIntegrity
	}
	if available == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE content_blobs SET available=1
			WHERE hash=? AND size_bytes=? AND available=0`, expected[:], written)
		if err != nil {
			return PutResult{}, fmt.Errorf("promote verified blob: %w", err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return PutResult{}, ErrIntegrity
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_content_blobs(user_id,hash,entitled_at_ms)
		VALUES(?,?,?) ON CONFLICT(user_id,hash) DO NOTHING`, u.userID[:], expected[:], now); err != nil {
		return PutResult{}, fmt.Errorf("grant blob entitlement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PutResult{}, fmt.Errorf("commit blob entitlement: %w", err)
	}
	return PutResult{Hash: expected, Size: written}, nil
}

func (u *UserRepository) Get(ctx context.Context, hash [sha256.Size]byte) ([]byte, error) {
	r := u.repository
	done, err := r.beginOperation()
	if err != nil {
		return nil, err
	}
	defer done()
	var size int64
	err = r.db.QueryRowContext(ctx, `SELECT b.size_bytes FROM users u
		JOIN user_content_blobs ub ON ub.user_id=u.id
		JOIN content_blobs b ON b.hash=ub.hash
		WHERE u.id=? AND u.status='active' AND ub.hash=? AND b.available=1`, u.userID[:], hash[:]).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("read blob entitlement: %w", err)
	}
	if size < 0 || size > MaxBlobBytes {
		return nil, ErrIntegrity
	}
	content, err := readBlob(r.blobStorage, hash, MaxBlobBytes)
	if err != nil {
		return nil, ErrIntegrity
	}
	actual := sha256.Sum256(content)
	if int64(len(content)) != size || !bytes.Equal(actual[:], hash[:]) {
		return nil, ErrIntegrity
	}
	return content, nil
}

func (r *Repository) RecoverStaging(ctx context.Context) (RecoveryReport, error) {
	done, err := r.beginOperation()
	if err != nil {
		return RecoveryReport{}, err
	}
	defer done()
	entries, err := stagingEntries(r.stagingStorage)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := removeStage(r.stagingStorage, entry); err != nil {
			return report, err
		}
		report.Removed++
	}
	return report, nil
}

func (r *Repository) Audit(ctx context.Context) (AuditReport, error) {
	done, err := r.beginOperation()
	if err != nil {
		return AuditReport{}, err
	}
	defer done()
	report := AuditReport{}
	registered := make(map[string]int64)
	availableBlobs := make(map[string]int64)
	rows, err := r.db.QueryContext(ctx, "SELECT hash,size_bytes,available FROM content_blobs")
	if err != nil {
		return report, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return report, err
		}
		var hash []byte
		var size int64
		var available int
		if err := rows.Scan(&hash, &size, &available); err != nil {
			rows.Close()
			return report, err
		}
		if len(hash) != sha256.Size {
			rows.Close()
			return report, ErrIntegrity
		}
		encoded := hex.EncodeToString(hash)
		registered[encoded] = size
		if available == 1 {
			availableBlobs[encoded] = size
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	if err := rows.Close(); err != nil {
		return report, err
	}
	report.Registered = len(registered)
	for encoded, size := range availableBlobs {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		hashBytes, _ := hex.DecodeString(encoded)
		var hash [sha256.Size]byte
		copy(hash[:], hashBytes)
		content, err := readBlob(r.blobStorage, hash, MaxBlobBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				report.Missing++
			} else {
				report.Corrupt++
			}
			continue
		}
		actual := sha256.Sum256(content)
		if int64(len(content)) != size || !bytes.Equal(actual[:], hash[:]) {
			report.Corrupt++
		}
	}
	seen, malformed, symlinks, err := scanBlobFiles(ctx, r.blobStorage)
	if err != nil {
		return report, err
	}
	report.Malformed, report.Symlinks = malformed, symlinks
	for encoded := range seen {
		if _, ok := registered[encoded]; !ok {
			report.Orphans++
		}
	}
	return report, nil
}
