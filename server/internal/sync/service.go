package sync

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultPullLimit = 100
	maxPullLimit     = 500
)

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Service struct {
	db    *sql.DB
	clock Clock
}
type ActorService struct {
	service          *Service
	userID, deviceID uuid.UUID
}

func NewService(db *sql.DB, clock Clock) (*Service, error) {
	if db == nil {
		return nil, errors.New("sync database is nil")
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Service{db: db, clock: clock}, nil
}

func (s *Service) ForActor(userID, deviceID uuid.UUID) (*ActorService, error) {
	if !validV7(userID) || !validV7(deviceID) {
		return nil, fmt.Errorf("%w: actor IDs must be UUIDv7", ErrInvalidInput)
	}
	return &ActorService{service: s, userID: userID, deviceID: deviceID}, nil
}

func (a *ActorService) Submit(ctx context.Context, request Mutation) (SubmitResult, error) {
	intent, hash, err := canonicalize(request)
	if err != nil {
		return SubmitResult{}, err
	}
	tx, err := a.service.db.BeginTx(ctx, nil)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("begin sync submit: %w", err)
	}
	defer tx.Rollback()
	if err := a.requireActiveActor(ctx, tx); err != nil {
		return SubmitResult{}, err
	}
	if replay, found, err := a.replay(ctx, tx, intent.OperationID, hash); err != nil {
		return SubmitResult{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return SubmitResult{}, err
		}
		return replay, nil
	}
	if (intent.Kind == MutationCreate || intent.Kind == MutationUpdate) && intent.ObjectType == ObjectNote {
		var available int
		err := tx.QueryRowContext(ctx, `SELECT b.available FROM content_blobs b
			JOIN user_content_blobs ub ON ub.hash=b.hash
			WHERE ub.user_id=? AND b.hash=?`, a.userID[:], intent.BlobHash).Scan(&available)
		if errors.Is(err, sql.ErrNoRows) {
			return SubmitResult{}, ErrBlobUnavailable
		}
		if err != nil {
			return SubmitResult{}, fmt.Errorf("check actor blob availability: %w", err)
		}
		if available != 1 {
			return SubmitResult{}, ErrBlobUnavailable
		}
	}
	now := a.service.clock.Now().UTC().UnixMilli()
	current, exists, err := loadObject(ctx, tx, a.userID, intent.ObjectID)
	if err != nil {
		return SubmitResult{}, err
	}
	if code, err := a.evaluate(ctx, tx, intent, current, exists); err != nil {
		return SubmitResult{}, err
	} else if code != "" {
		if err := a.insertOperation(ctx, tx, intent, hash, now, SubmitResult{Conflict: code}); err != nil {
			return SubmitResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return SubmitResult{}, fmt.Errorf("commit sync conflict: %w", err)
		}
		return SubmitResult{Conflict: code}, nil
	}

	next := current
	if intent.Kind == MutationCreate {
		next = objectState{ID: intent.ObjectID, Type: intent.ObjectType, Revision: 1,
			ParentID: intent.ParentID, Name: intent.Name, NameKey: intent.nameKey, BlobHash: clone(intent.BlobHash)}
		parentKey := parentKey(intent.ParentID)
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_objects(
			user_id, object_id, object_type, revision, parent_id, parent_key, name, name_key,
			blob_hash, deleted, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, 0, ?, ?)`,
			a.userID[:], next.ID[:], next.Type, nullableUUID(next.ParentID), parentKey,
			next.Name, next.NameKey, nullableBytes(next.BlobHash), now, now)
	} else {
		next.Revision++
		switch intent.Kind {
		case MutationUpdate:
			next.BlobHash = clone(intent.BlobHash)
		case MutationMove:
			next.ParentID, next.Name, next.NameKey = intent.ParentID, intent.Name, intent.nameKey
		case MutationDelete:
			next.Deleted = true
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE sync_objects SET revision=?, parent_id=?, parent_key=?,
			name=?, name_key=?, blob_hash=?, deleted=?, updated_at_ms=?
			WHERE user_id=? AND object_id=? AND revision=? AND deleted=0`,
			next.Revision, nullableUUID(next.ParentID), parentKey(next.ParentID), next.Name, next.NameKey,
			nullableBytes(next.BlobHash), boolInt(next.Deleted), now, a.userID[:], next.ID[:], current.Revision)
		if updateErr != nil {
			err = updateErr
		} else if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
			err = rowsErr
		} else if rows != 1 {
			return SubmitResult{}, fmt.Errorf("%w: conditional object update", ErrOperationReplayMismatch)
		}
	}
	if err != nil {
		return SubmitResult{}, fmt.Errorf("mutate sync object: %w", err)
	}
	cursor, err := allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return SubmitResult{}, err
	}
	accepted := SubmitResult{Accepted: true, Revision: next.Revision, Cursor: cursor}
	if err := a.insertOperation(ctx, tx, intent, hash, now, accepted); err != nil {
		return SubmitResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_object_versions(
		user_id, object_id, revision, operation_id, object_type, parent_id, name, name_key,
		blob_hash, deleted, created_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.userID[:], next.ID[:], next.Revision, intent.OperationID[:], next.Type, nullableUUID(next.ParentID),
		next.Name, next.NameKey, nullableBytes(next.BlobHash), boolInt(next.Deleted), now); err != nil {
		return SubmitResult{}, fmt.Errorf("insert object version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_change_log(
		user_id, cursor, object_id, revision, operation_id, mutation, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.userID[:], cursor, next.ID[:], next.Revision,
		intent.OperationID[:], intent.Kind, now); err != nil {
		return SubmitResult{}, fmt.Errorf("insert sync change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SubmitResult{}, fmt.Errorf("commit sync mutation: %w", err)
	}
	return accepted, nil
}

func (a *ActorService) Pull(ctx context.Context, after uint64, limit int) (PullResult, error) {
	if limit == 0 {
		limit = defaultPullLimit
	}
	if limit < 0 || limit > maxPullLimit {
		return PullResult{}, fmt.Errorf("%w: pull limit", ErrInvalidInput)
	}
	tx, err := a.service.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PullResult{}, err
	}
	defer tx.Rollback()
	if err := a.requireActiveActor(ctx, tx); err != nil {
		return PullResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.cursor, c.mutation, c.operation_id,
		v.object_id, v.object_type, v.revision, v.parent_id, v.name, v.blob_hash, v.deleted
		FROM sync_change_log c JOIN sync_object_versions v
		ON v.user_id=c.user_id AND v.object_id=c.object_id AND v.revision=c.revision
		WHERE c.user_id=? AND c.cursor>? ORDER BY c.cursor ASC LIMIT ?`, a.userID[:], after, limit+1)
	if err != nil {
		return PullResult{}, fmt.Errorf("query sync changes: %w", err)
	}
	defer rows.Close()
	changes := make([]VersionState, 0, limit+1)
	for rows.Next() {
		var item VersionState
		var opID, objectID, parent, blob []byte
		var deleted int
		if err := rows.Scan(&item.Cursor, &item.Mutation, &opID, &objectID, &item.ObjectType,
			&item.Revision, &parent, &item.Name, &blob, &deleted); err != nil {
			return PullResult{}, err
		}
		copy(item.OperationID[:], opID)
		copy(item.ObjectID[:], objectID)
		if len(parent) == 16 {
			id := uuid.UUID{}
			copy(id[:], parent)
			item.ParentID = &id
		}
		item.BlobHash = clone(blob)
		item.Deleted = deleted == 1
		changes = append(changes, item)
	}
	if err := rows.Err(); err != nil {
		return PullResult{}, err
	}
	hasMore := len(changes) > limit
	if hasMore {
		changes = changes[:limit]
	}
	next := after
	if len(changes) > 0 {
		next = changes[len(changes)-1].Cursor
	}
	if err := tx.Commit(); err != nil {
		return PullResult{}, err
	}
	return PullResult{Changes: changes, HasMore: hasMore, NextCursor: next}, nil
}

type canonicalIntent struct {
	Mutation
	nameKey string
}
type canonicalJSON struct {
	Policy, Operation, Kind, Object, Type string
	Base                                  uint64
	Parent, Name, Blob                    string
}

func canonicalize(in Mutation) (canonicalIntent, [32]byte, error) {
	if !validV7(in.OperationID) || !validObjectID(in.ObjectID) ||
		(in.ObjectType != ObjectNote && in.ObjectType != ObjectFolder) {
		return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: IDs/type", ErrInvalidInput)
	}
	if in.ParentID != nil && !validObjectID(*in.ParentID) {
		return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: parent ID", ErrInvalidInput)
	}
	out := canonicalIntent{Mutation: in}
	var err error
	switch in.Kind {
	case MutationCreate:
		if in.BaseRevision != 0 {
			return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: create base revision", ErrInvalidInput)
		}
		out.Name, out.nameKey, err = normalizeName(in.Name)
	case MutationUpdate:
		if in.BaseRevision == 0 || in.ObjectType != ObjectNote || in.ParentID != nil || in.Name != "" {
			err = ErrInvalidInput
		}
	case MutationMove:
		if in.BaseRevision == 0 || len(in.BlobHash) != 0 {
			return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: move shape", ErrInvalidInput)
		}
		out.Name, out.nameKey, err = normalizeName(in.Name)
	case MutationDelete:
		if in.BaseRevision == 0 || in.ParentID != nil || in.Name != "" || len(in.BlobHash) != 0 {
			err = ErrInvalidInput
		}
	default:
		err = ErrInvalidInput
	}
	if err != nil {
		return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: mutation shape", err)
	}
	if in.Kind == MutationCreate || in.Kind == MutationUpdate {
		if in.ObjectType == ObjectNote && len(in.BlobHash) != sha256.Size {
			return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: note blob", ErrInvalidInput)
		}
		if in.ObjectType == ObjectFolder && len(in.BlobHash) != 0 {
			return canonicalIntent{}, [32]byte{}, fmt.Errorf("%w: folder blob", ErrInvalidInput)
		}
	}
	parent := ""
	if out.ParentID != nil {
		parent = out.ParentID.String()
	}
	body, _ := json.Marshal(canonicalJSON{"remember-sync-intent-v1", out.OperationID.String(), string(out.Kind), out.ObjectID.String(), string(out.ObjectType), out.BaseRevision, parent, out.Name, hex.EncodeToString(out.BlobHash)})
	return out, sha256.Sum256(body), nil
}

func (a *ActorService) requireActiveActor(ctx context.Context, tx *sql.Tx) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM users u JOIN devices d ON d.user_id=u.id
		WHERE u.id=? AND u.status='active' AND d.id=? AND d.status='active'`, a.userID[:], a.deviceID[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInactiveActor
	}
	if err != nil {
		return fmt.Errorf("validate sync actor: %w", err)
	}
	return nil
}

func (a *ActorService) replay(ctx context.Context, tx *sql.Tx, operationID uuid.UUID, hash [32]byte) (SubmitResult, bool, error) {
	var stored []byte
	var result string
	var conflict sql.NullString
	var revision, cursor sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT request_hash,result,conflict_code,result_revision,result_cursor
		FROM sync_operations WHERE user_id=? AND operation_id=?`, a.userID[:], operationID[:]).Scan(&stored, &result, &conflict, &revision, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return SubmitResult{}, false, nil
	}
	if err != nil {
		return SubmitResult{}, false, err
	}
	if len(stored) != 32 || subtle.ConstantTimeCompare(stored, hash[:]) != 1 {
		return SubmitResult{}, false, ErrOperationReplayMismatch
	}
	if result == "accepted" {
		return SubmitResult{Accepted: true, Revision: uint64(revision.Int64), Cursor: uint64(cursor.Int64)}, true, nil
	}
	return SubmitResult{Conflict: ConflictCode(conflict.String)}, true, nil
}

func (a *ActorService) evaluate(ctx context.Context, tx *sql.Tx, in canonicalIntent, current objectState, exists bool) (ConflictCode, error) {
	if in.Kind == MutationCreate {
		if exists {
			return ConflictObjectExists, nil
		}
	} else {
		if !exists {
			return ConflictObjectMissing, nil
		}
		if current.Deleted {
			return ConflictObjectDeleted, nil
		}
		if current.Type != in.ObjectType {
			return ConflictTypeMismatch, nil
		}
		if current.Revision != in.BaseRevision {
			return ConflictBaseRevisionMismatch, nil
		}
	}
	if in.Kind == MutationCreate || in.Kind == MutationMove {
		if in.ParentID != nil {
			parent, found, err := loadObject(ctx, tx, a.userID, *in.ParentID)
			if err != nil {
				return "", err
			}
			if !found || parent.Deleted || parent.Type != ObjectFolder {
				return ConflictParentUnavailable, nil
			}
		}
		if in.Kind == MutationMove && current.Type == ObjectFolder && in.ParentID != nil {
			cycle, err := a.folderCycle(ctx, tx, current.ID, *in.ParentID)
			if err != nil {
				return "", err
			}
			if cycle {
				return ConflictFolderCycle, nil
			}
		}
		if err := a.validateTargetPath(ctx, tx, in, current); err != nil {
			return "", err
		}
		var collision int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM sync_objects WHERE user_id=? AND parent_key=? AND name_key=?
			AND deleted=0 AND object_id<>? LIMIT 1`, a.userID[:], parentKey(in.ParentID), in.nameKey, in.ObjectID[:]).Scan(&collision)
		if err == nil {
			return ConflictPathCollision, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	if in.Kind == MutationDelete && current.Type == ObjectFolder {
		var child int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM sync_objects WHERE user_id=? AND parent_id=? AND deleted=0 LIMIT 1`, a.userID[:], current.ID[:]).Scan(&child)
		if err == nil {
			return ConflictFolderNotEmpty, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", nil
}

func (a *ActorService) validateTargetPath(ctx context.Context, tx *sql.Tx, in canonicalIntent, current objectState) error {
	parts := []string{in.Name}
	parentID := in.ParentID
	for depth := 0; parentID != nil; depth++ {
		if depth >= 10000 {
			return fmt.Errorf("%w: folder ancestry too deep", ErrInvalidInput)
		}
		parent, found, err := loadObject(ctx, tx, a.userID, *parentID)
		if err != nil {
			return err
		}
		if !found || parent.Deleted || parent.Type != ObjectFolder {
			return fmt.Errorf("%w: invalid ancestor", ErrInvalidInput)
		}
		parts = append([]string{parent.Name}, parts...)
		parentID = parent.ParentID
	}
	target := strings.Join(parts, "/")
	if err := validateLogicalPath(target); err != nil {
		return err
	}
	if in.Kind != MutationMove || current.Type != ObjectFolder {
		return nil
	}

	type child struct {
		id   uuid.UUID
		name string
	}
	children := make(map[uuid.UUID][]child)
	rows, err := tx.QueryContext(ctx, `SELECT object_id,parent_id,name FROM sync_objects
		WHERE user_id=? AND deleted=0 AND parent_id IS NOT NULL`, a.userID[:])
	if err != nil {
		return fmt.Errorf("read folder descendants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var objectBytes, parentBytes []byte
		var name string
		if err := rows.Scan(&objectBytes, &parentBytes, &name); err != nil {
			return err
		}
		if len(objectBytes) != 16 || len(parentBytes) != 16 {
			return fmt.Errorf("%w: malformed stored hierarchy", ErrInvalidInput)
		}
		var objectID, parent uuid.UUID
		copy(objectID[:], objectBytes)
		copy(parent[:], parentBytes)
		children[parent] = append(children[parent], child{id: objectID, name: name})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	type pendingPath struct {
		path string
		uuid uuid.UUID
	}
	pending := make([]pendingPath, 0, len(children[current.ID]))
	for _, item := range children[current.ID] {
		pending = append(pending, pendingPath{path: item.name, uuid: item.id})
	}
	seen := 0
	for len(pending) > 0 {
		last := len(pending) - 1
		item := pending[last]
		pending = pending[:last]
		seen++
		if seen > 100000 {
			return fmt.Errorf("%w: folder subtree too large", ErrInvalidInput)
		}
		if err := validateLogicalPath(target + "/" + item.path); err != nil {
			return err
		}
		for _, nested := range children[item.uuid] {
			pending = append(pending, pendingPath{path: item.path + "/" + nested.name, uuid: nested.id})
		}
	}
	return nil
}

func (a *ActorService) folderCycle(ctx context.Context, tx *sql.Tx, objectID, parentID uuid.UUID) (bool, error) {
	current := parentID
	for range 10000 {
		if current == objectID {
			return true, nil
		}
		var parent []byte
		err := tx.QueryRowContext(ctx, `SELECT parent_id FROM sync_objects WHERE user_id=? AND object_id=?`, a.userID[:], current[:]).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) || len(parent) == 0 {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		copy(current[:], parent)
	}
	return false, fmt.Errorf("%w: folder ancestry too deep", ErrInvalidInput)
}

func (a *ActorService) insertOperation(ctx context.Context, tx *sql.Tx, in canonicalIntent, hash [32]byte, now int64, result SubmitResult) error {
	status := "conflict"
	var conflict any = string(result.Conflict)
	var revision, cursor any
	if result.Accepted {
		status = "accepted"
		conflict = nil
		revision = result.Revision
		cursor = result.Cursor
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sync_operations(user_id,device_id,operation_id,request_hash,mutation,
		object_id,proposed_type,proposed_base_revision,proposed_parent_id,proposed_name,proposed_name_key,
		proposed_blob_hash,result,conflict_code,result_revision,result_cursor,created_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.userID[:], a.deviceID[:], in.OperationID[:], hash[:], in.Kind,
		in.ObjectID[:], in.ObjectType, in.BaseRevision, nullableUUID(in.ParentID), nullableString(in.Name), nullableString(in.nameKey),
		nullableBytes(in.BlobHash), status, conflict, revision, cursor, now)
	if err != nil {
		return fmt.Errorf("persist sync operation: %w", err)
	}
	return nil
}

type objectState struct {
	ID            uuid.UUID
	Type          ObjectType
	Revision      uint64
	ParentID      *uuid.UUID
	Name, NameKey string
	BlobHash      []byte
	Deleted       bool
}

func loadObject(ctx context.Context, tx *sql.Tx, userID, objectID uuid.UUID) (objectState, bool, error) {
	var out objectState
	var id, parent, blob []byte
	var deleted int
	err := tx.QueryRowContext(ctx, `SELECT object_id,object_type,revision,parent_id,name,name_key,blob_hash,deleted FROM sync_objects WHERE user_id=? AND object_id=?`, userID[:], objectID[:]).Scan(&id, &out.Type, &out.Revision, &parent, &out.Name, &out.NameKey, &blob, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return objectState{}, false, nil
	}
	if err != nil {
		return objectState{}, false, err
	}
	copy(out.ID[:], id)
	if len(parent) == 16 {
		id := uuid.UUID{}
		copy(id[:], parent)
		out.ParentID = &id
	}
	out.BlobHash = clone(blob)
	out.Deleted = deleted == 1
	return out, true, nil
}
func allocateCursor(ctx context.Context, tx *sql.Tx, user uuid.UUID) (uint64, error) {
	var cursor uint64
	err := tx.QueryRowContext(ctx, `INSERT INTO user_cursor_counters(user_id,last_cursor) VALUES(?,1) ON CONFLICT(user_id) DO UPDATE SET last_cursor=last_cursor+1 RETURNING last_cursor`, user[:]).Scan(&cursor)
	return cursor, err
}
func validV7(id uuid.UUID) bool {
	return validObjectID(id) && id.Version() == 7
}
func validObjectID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122
}
func nullableUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id[:]
}
func parentKey(id *uuid.UUID) []byte {
	if id == nil {
		return make([]byte, 16)
	}
	return id[:]
}
func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func clone(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte(nil), value...)
}
