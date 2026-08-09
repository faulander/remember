// Package localindex stores reconstructable device-local metadata.
package localindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schemaVersion = 19

//go:embed migrations/*.sql
var migrations embed.FS

// ObjectType is the synchronized filesystem object kind.
type ObjectType string

const (
	ObjectNote   ObjectType = "note"
	ObjectFolder ObjectType = "folder"
)

// IdentityState captures whether an identity is usable without guessing.
type IdentityState string

const (
	IdentityKnown   IdentityState = "known"
	IdentityNew     IdentityState = "new"
	IdentityPending IdentityState = "pending"
)

// Object is reconstructable technical metadata, never note content.
type Object struct {
	ID            uuid.UUID
	Type          ObjectType
	RelativePath  string
	CollisionPath string
	ParentID      uuid.UUID
	ContentHash   []byte
	FolderDevice  uint64
	FolderInode   uint64
	IdentityState IdentityState
}

// Issue is a local problem shown to the user and excluded from telemetry.
type Issue struct {
	Code         string
	RelativePath string
	Detail       string
}

// Snapshot is a transactionally consistent index view.
type Snapshot struct {
	Objects []Object
	Issues  []Issue
}

// Index owns one SQLite connection pool limited to a single writer.
type Index struct {
	db *sql.DB
}

// Open creates or migrates a local index at path.
func Open(ctx context.Context, path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open local index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	index := &Index{db: db}
	if err := index.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return index, nil
}

// Close releases the index database.
func (i *Index) Close() error { return i.db.Close() }

// WithTransaction serializes one durable local coordinator transition.
func (i *Index) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (i *Index) initialize(ctx context.Context) error {
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := i.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure local index: %w", err)
		}
	}

	var version int
	if err := i.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read index schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("local index schema %d is newer than supported %d", version, schemaVersion)
	}
	openedVersion := version
	for next := version + 1; next <= schemaVersion; next++ {
		name := fmt.Sprintf("migrations/%03d_", next)
		entries, err := migrations.ReadDir("migrations")
		if err != nil {
			return fmt.Errorf("list index migrations: %w", err)
		}
		var filename string
		for _, entry := range entries {
			candidate := "migrations/" + entry.Name()
			if strings.HasPrefix(candidate, name) && strings.HasSuffix(candidate, ".sql") {
				if filename != "" {
					return fmt.Errorf("duplicate local index migration %d", next)
				}
				filename = candidate
			}
		}
		if filename == "" {
			return fmt.Errorf("missing local index migration %d", next)
		}
		script, err := migrations.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read index migration %d: %w", next, err)
		}
		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin index migration %d: %w", next, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply index migration %d: %w", next, err)
		}
		if openedVersion == 1 && next == 2 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('bootstrap_required','1')`); err != nil {
				tx.Rollback()
				return fmt.Errorf("mark sync bootstrap required: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record index schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit index migration %d: %w", next, err)
		}
	}
	return nil
}

// ReplaceSnapshot atomically replaces reconstructable objects and current
// local issues. Any failed insert rolls back the complete replacement.
func (i *Index) ReplaceSnapshot(ctx context.Context, snapshot Snapshot) error {
	return i.WithTransaction(ctx, func(tx *sql.Tx) error { return ReplaceSnapshotTx(ctx, tx, snapshot) })
}

// ReplaceSnapshotTx replaces reconstructable state inside a caller-owned
// transaction so observation and Outbox capture can commit atomically.
func ReplaceSnapshotTx(ctx context.Context, tx *sql.Tx, snapshot Snapshot) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM local_issues; DELETE FROM objects"); err != nil {
		return fmt.Errorf("clear index snapshot: %w", err)
	}
	for _, object := range snapshot.Objects {
		var parent any
		if object.ParentID != uuid.Nil {
			parent = object.ParentID.String()
		}
		var hash any
		if len(object.ContentHash) != 0 {
			hash = object.ContentHash
		}
		var device, inode any
		if object.FolderDevice != 0 || object.FolderInode != 0 {
			device, inode = object.FolderDevice, object.FolderInode
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO objects (
				object_id, object_type, relative_path, collision_path,
				parent_id, content_hash, folder_device, folder_inode, identity_state
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			object.ID.String(), object.Type, object.RelativePath, object.CollisionPath,
			parent, hash, device, inode, object.IdentityState,
		)
		if err != nil {
			return fmt.Errorf("insert indexed object %q: %w", object.RelativePath, err)
		}
	}
	for _, issue := range snapshot.Issues {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO local_issues(code, relative_path, detail) VALUES (?, ?, ?)",
			issue.Code, issue.RelativePath, issue.Detail,
		); err != nil {
			return fmt.Errorf("insert local issue: %w", err)
		}
	}
	return nil
}

// SetState stores a small device-local coordinator value.
func (i *Index) SetState(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("index state key is empty")
	}
	_, err := i.db.ExecContext(ctx, `
		INSERT INTO watcher_state(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set index state %q: %w", key, err)
	}
	return nil
}

// State reads a device-local coordinator value.
func (i *Index) State(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := i.db.QueryRowContext(ctx, "SELECT value FROM watcher_state WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read index state %q: %w", key, err)
	}
	return value, true, nil
}

// ReadSnapshot reads a deterministic path-ordered index view.
func (i *Index) ReadSnapshot(ctx context.Context) (Snapshot, error) {
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin index snapshot read: %w", err)
	}
	defer tx.Rollback()

	var snapshot Snapshot
	rows, err := tx.QueryContext(ctx, `
		SELECT object_id, object_type, relative_path, collision_path,
		       parent_id, content_hash, folder_device, folder_inode, identity_state
		FROM objects ORDER BY relative_path`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("query indexed objects: %w", err)
	}
	for rows.Next() {
		var object Object
		var id string
		var parent sql.NullString
		var device, inode sql.NullInt64
		if err := rows.Scan(&id, &object.Type, &object.RelativePath, &object.CollisionPath,
			&parent, &object.ContentHash, &device, &inode, &object.IdentityState); err != nil {
			rows.Close()
			return Snapshot{}, fmt.Errorf("scan indexed object: %w", err)
		}
		object.ID, err = uuid.Parse(id)
		if err != nil {
			rows.Close()
			return Snapshot{}, fmt.Errorf("parse indexed object id: %w", err)
		}
		if device.Valid && inode.Valid {
			object.FolderDevice, object.FolderInode = uint64(device.Int64), uint64(inode.Int64)
		}
		if parent.Valid {
			object.ParentID, err = uuid.Parse(parent.String)
			if err != nil {
				rows.Close()
				return Snapshot{}, fmt.Errorf("parse indexed parent id: %w", err)
			}
		}
		snapshot.Objects = append(snapshot.Objects, object)
	}
	if err := rows.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close object rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate indexed objects: %w", err)
	}

	issueRows, err := tx.QueryContext(ctx,
		"SELECT code, relative_path, detail FROM local_issues ORDER BY relative_path, code, issue_id")
	if err != nil {
		return Snapshot{}, fmt.Errorf("query local issues: %w", err)
	}
	defer issueRows.Close()
	for issueRows.Next() {
		var issue Issue
		if err := issueRows.Scan(&issue.Code, &issue.RelativePath, &issue.Detail); err != nil {
			return Snapshot{}, fmt.Errorf("scan local issue: %w", err)
		}
		snapshot.Issues = append(snapshot.Issues, issue)
	}
	if err := issueRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate local issues: %w", err)
	}
	if err := issueRows.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close issue rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit index snapshot read: %w", err)
	}
	return snapshot, nil
}

func validateSnapshot(snapshot Snapshot) error {
	for _, object := range snapshot.Objects {
		if object.ID == uuid.Nil {
			return errors.New("indexed object has nil id")
		}
		if object.Type != ObjectNote && object.Type != ObjectFolder {
			return fmt.Errorf("indexed object has invalid type %q", object.Type)
		}
		if object.RelativePath == "" || object.CollisionPath == "" {
			return errors.New("indexed object has empty path")
		}
		if len(object.ContentHash) != 0 && len(object.ContentHash) != sha256.Size {
			return fmt.Errorf("indexed object has %d-byte content hash", len(object.ContentHash))
		}
		if object.Type == ObjectFolder && len(object.ContentHash) != 0 {
			return errors.New("indexed folder has content hash")
		}
		if object.IdentityState != IdentityKnown && object.IdentityState != IdentityNew && object.IdentityState != IdentityPending {
			return fmt.Errorf("indexed object has invalid identity state %q", object.IdentityState)
		}
	}
	return nil
}
