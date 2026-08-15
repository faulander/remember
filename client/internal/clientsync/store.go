// Package clientsync owns durable client-side synchronization coordinator state.
package clientsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type MutationKind string
type ObjectType string

// BlobResolver returns immutable bytes for an authenticated remote blob hash.
type BlobResolver interface {
	ResolveBlob(context.Context, [sha256.Size]byte) ([]byte, error)
}

type BlobResolverFunc func(context.Context, [sha256.Size]byte) ([]byte, error)

func (f BlobResolverFunc) ResolveBlob(ctx context.Context, hash [sha256.Size]byte) ([]byte, error) {
	return f(ctx, hash)
}

const (
	Create MutationKind = "create"
	Update MutationKind = "update"
	Move   MutationKind = "move"
	Delete MutationKind = "delete"
	Note   ObjectType   = "note"
	Folder ObjectType   = "folder"
)

type Mutation struct {
	OperationID               uuid.UUID
	Kind                      MutationKind
	ObjectID                  uuid.UUID
	ObjectType                ObjectType
	BaseRevision              uint64
	ParentID                  *uuid.UUID
	Name                      string
	BlobHash                  []byte
	DependencyOperationID     *uuid.UUID
	AdditionalDependencies    []uuid.UUID
	FolderSourceRelative      string
	FolderDevice, FolderInode uint64
}

type OutboxItem struct {
	Sequence int64
	Mutation Mutation
	Status   string
}
type CanonicalState struct {
	ObjectType ObjectType
	Revision   uint64
	ParentID   *uuid.UUID
	Name       string
	BlobHash   []byte
	Deleted    bool
}

type Result struct {
	Accepted         bool
	Conflict         string
	Revision, Cursor uint64
	Canonical        *CanonicalState
}

type Change struct {
	Cursor                uint64
	Mutation              MutationKind
	OperationID, ObjectID uuid.UUID
	ObjectType            ObjectType
	Revision              uint64
	ParentID              *uuid.UUID
	Name                  string
	BlobHash              []byte
	Deleted               bool
	State                 string
}

type ApplyPlan struct {
	ID                        uuid.UUID
	FromCursor, ThroughCursor uint64
	Status                    string
	Steps                     []Change
}

type ConflictFolderPublication struct {
	FolderID                      uuid.UUID
	TargetRelative, StageRelative string
	Nonce                         [32]byte
	Device, Inode                 uint64
	State                         string
}

type ConflictFolderRestoration struct {
	OperationID, FolderID         uuid.UUID
	TargetRelative, StageRelative string
	Nonce                         [32]byte
	Device, Inode                 uint64
	State                         string
}

type ConflictMaterialization struct {
	OperationID, SourceObjectID, ConflictNoteID uuid.UUID
	OriginalRelative, TargetRelative            string
	SourceHash, MaterializedHash                [32]byte
	StagedRelative, State                       string
	RebasedOperationID                          *uuid.UUID
}

type ConflictItem struct {
	Outbox    OutboxItem
	Code      string
	Canonical *CanonicalState
}

type FolderMutation struct {
	PlanID         uuid.UUID
	StepIndex      int
	FolderID       uuid.UUID
	Mutation       MutationKind
	SourceRelative string
	TargetRelative string
	Device, Inode  uint64
}

type FolderPublication struct {
	PlanID            uuid.UUID
	StepIndex         int
	FolderID          uuid.UUID
	TargetRelative    string
	StageRelative     string
	Nonce             [32]byte
	Device, Inode     uint64
	CleanupAuthorized bool
	Cleaned           bool
}

type Store struct {
	index *localindex.Index
	clock func() time.Time
}

func NewStore(index *localindex.Index) (*Store, error) {
	if index == nil {
		return nil, errors.New("nil local index")
	}
	return &Store{index: index, clock: time.Now}, nil
}

func validObjectID(id uuid.UUID) bool    { return id != uuid.Nil && id.Variant() == uuid.RFC4122 }
func validOperationID(id uuid.UUID) bool { return validObjectID(id) && id.Version() == 7 }

// ValidateMutation enforces the same immutable operation shape accepted by the
// server before a transport may send bearer-authenticated data.
func ValidateMutation(m Mutation) error { return validateMutation(m) }

func validateMutation(m Mutation) error {
	if !validOperationID(m.OperationID) || !validObjectID(m.ObjectID) || (m.ObjectType != Note && m.ObjectType != Folder) {
		return errors.New("invalid sync mutation identity")
	}
	if m.Kind != Create && m.Kind != Update && m.Kind != Move && m.Kind != Delete {
		return errors.New("invalid sync mutation kind")
	}
	if m.ParentID != nil && !validObjectID(*m.ParentID) {
		return errors.New("invalid sync parent")
	}
	if m.DependencyOperationID != nil && (!validOperationID(*m.DependencyOperationID) || *m.DependencyOperationID == m.OperationID) {
		return errors.New("invalid sync dependency")
	}
	seenDependencies := make(map[uuid.UUID]struct{})
	if m.DependencyOperationID != nil {
		seenDependencies[*m.DependencyOperationID] = struct{}{}
	}
	for _, dependency := range m.AdditionalDependencies {
		if !validOperationID(dependency) || dependency == m.OperationID {
			return errors.New("invalid additional sync dependency")
		}
		seenDependencies[dependency] = struct{}{}
	}
	if m.BaseRevision > math.MaxInt64 {
		return errors.New("sync base revision exceeds SQLite range")
	}
	shapeValid := false
	switch m.Kind {
	case Create:
		shapeValid = m.BaseRevision == 0 && m.Name != "" &&
			((m.ObjectType == Note && len(m.BlobHash) == sha256.Size) || (m.ObjectType == Folder && len(m.BlobHash) == 0))
	case Update:
		shapeValid = m.BaseRevision > 0 && m.ObjectType == Note && m.ParentID == nil && m.Name == "" && len(m.BlobHash) == sha256.Size
	case Move:
		shapeValid = m.BaseRevision > 0 && m.Name != "" && len(m.BlobHash) == 0
	case Delete:
		shapeValid = m.BaseRevision > 0 && m.ParentID == nil && m.Name == "" && len(m.BlobHash) == 0
	}
	if !shapeValid {
		return errors.New("invalid sync mutation shape")
	}
	if (m.Kind == Create || m.Kind == Move) && naming.ValidateComponent(m.Name) != nil {
		return errors.New("invalid sync mutation name")
	}
	hasFolderBinding := m.FolderSourceRelative != "" || m.FolderDevice != 0 || m.FolderInode != 0
	if hasFolderBinding && (m.ObjectType != Folder || (m.Kind != Move && m.Kind != Delete) || naming.ValidateUserRelativePath(m.FolderSourceRelative) != nil || m.FolderDevice == 0 || m.FolderInode == 0 || m.FolderDevice > math.MaxInt64 || m.FolderInode > math.MaxInt64) {
		return errors.New("invalid local folder mutation binding")
	}
	return nil
}

func (s *Store) Enqueue(ctx context.Context, mutations []Mutation) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error { return s.enqueueTx(ctx, tx, mutations) })
}

func (s *Store) ReplaceSnapshotAndEnqueue(ctx context.Context, snapshot localindex.Snapshot, mutations []Mutation, cancelPending []uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return s.replaceSnapshotAndEnqueueTx(ctx, tx, snapshot, mutations, cancelPending)
	})
}

// CaptureSnapshot keeps projection reads, derived operations, snapshot
// replacement and Outbox insertion under one SQLite writer transaction.
func (s *Store) CaptureSnapshot(ctx context.Context, snapshot *localindex.Snapshot, recoveryMode bool, derive func(*sql.Tx) ([]Mutation, []uuid.UUID, error)) error {
	if snapshot == nil {
		return errors.New("nil captured snapshot")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var bootstrap string
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='bootstrap_required'`).Scan(&bootstrap)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if recoveryMode || bootstrap == "1" {
			return localindex.ReplaceSnapshotTx(ctx, tx, *snapshot)
		}
		mutations, cancelPending, err := derive(tx)
		if err != nil {
			return err
		}
		return s.replaceSnapshotAndEnqueueTx(ctx, tx, *snapshot, mutations, cancelPending)
	})
}

func (s *Store) replaceSnapshotAndEnqueueTx(ctx context.Context, tx *sql.Tx, snapshot localindex.Snapshot, mutations []Mutation, cancelPending []uuid.UUID) error {
	if err := localindex.ReplaceSnapshotTx(ctx, tx, snapshot); err != nil {
		return err
	}
	for _, id := range cancelPending {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE object_id=? AND status='pending'`, id.String()); err != nil {
			return err
		}
	}
	return s.enqueueTx(ctx, tx, mutations)
}
func (s *Store) enqueueTx(ctx context.Context, tx *sql.Tx, mutations []Mutation) error {
	superseded := make(map[uuid.UUID]bool)
	for _, m := range mutations {
		if err := validateMutation(m); err != nil {
			return err
		}
		if m.Kind == Move && m.ObjectType == Folder && m.ParentID != nil && *m.ParentID == ConflictRecoveredID && m.BaseRevision == 1 {
			var raw string
			err := tx.QueryRowContext(ctx, `SELECT r.new_operation_id FROM conflict_folder_divergent_move_recoveries r JOIN sync_outbox create_op ON create_op.operation_id=r.new_operation_id WHERE r.recovered_folder_id=? AND r.recovery_relative=? AND r.state='completed' AND create_op.object_id=r.recovered_folder_id AND create_op.mutation='create' AND create_op.status IN ('pending','attempted','accepted')`, m.ObjectID.String(), ConflictRootName+"/"+ConflictRecoveredName+"/"+m.Name).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				createID, parseErr := uuid.Parse(raw)
				if parseErr != nil {
					return errors.New("corrupt divergent recovery create operation")
				}
				if mutationDependsOn(m, createID) {
					continue
				}
			}
		}
		projectedRevision, projectedDependency, durable, err := s.ProjectionTx(ctx, tx, m.ObjectID)
		if err != nil {
			return err
		}
		if m.Kind == Create {
			var baselineOrAttempted int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_baselines WHERE object_id=?) OR
				EXISTS(SELECT 1 FROM sync_outbox WHERE object_id=? AND status='attempted')`, m.ObjectID.String(), m.ObjectID.String()).Scan(&baselineOrAttempted); err != nil {
				return err
			}
			if baselineOrAttempted != 0 {
				return errors.New("create cannot follow durable object state")
			}
			var referencedPending int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox o WHERE o.object_id=? AND o.status='pending' AND
				EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.dependency_operation_id=o.operation_id)`, m.ObjectID.String()).Scan(&referencedPending); err != nil {
				return err
			}
			if referencedPending != 0 {
				return errors.New("referenced pending create cannot be coalesced")
			}
		} else if !durable || projectedRevision != m.BaseRevision || projectedDependency != nil && !mutationDependsOn(m, *projectedDependency) {
			return errors.New("mutation base or dependency does not match durable projection")
		}
		// Coalescing never rewrites immutable payload. An unattempted predecessor is
		// retained as superseded history and a fresh operation is appended.
		if !superseded[m.ObjectID] {
			// Never supersede an operation referenced by durable dependency state or
			// by the operation currently being appended.
			protected := make(map[uuid.UUID]struct{})
			if m.DependencyOperationID != nil {
				protected[*m.DependencyOperationID] = struct{}{}
			}
			for _, dependency := range m.AdditionalDependencies {
				protected[dependency] = struct{}{}
			}
			rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM sync_outbox o
				WHERE object_id=? AND status='pending' AND
				NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.dependency_operation_id=o.operation_id)`, m.ObjectID.String())
			if err != nil {
				return err
			}
			var candidates []string
			for rows.Next() {
				var candidate string
				if err := rows.Scan(&candidate); err != nil {
					rows.Close()
					return err
				}
				id, parseErr := uuid.Parse(candidate)
				if parseErr == nil {
					if _, keep := protected[id]; keep {
						continue
					}
				}
				candidates = append(candidates, candidate)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, candidate := range candidates {
				if _, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND status='pending'`, candidate); err != nil {
					return err
				}
			}
			superseded[m.ObjectID] = true
		}
		var parent, blob, dep any
		if m.ParentID != nil {
			parent = m.ParentID.String()
		}
		if len(m.BlobHash) != 0 {
			blob = m.BlobHash
		}
		if m.DependencyOperationID != nil {
			dep = m.DependencyOperationID.String()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_outbox(operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,status,created_at_ms)
			VALUES(?,?,?,?,?,?,?,?,?,'pending',?)`, m.OperationID.String(), m.Kind, m.ObjectID.String(), m.ObjectType, m.BaseRevision, parent, m.Name, blob, dep, s.clock().UTC().UnixMilli())
		if err != nil {
			return fmt.Errorf("enqueue sync mutation: %w", err)
		}
		if m.FolderSourceRelative != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_outbox_folder_intents(operation_id,folder_id,mutation_kind,source_relative,device,inode) VALUES(?,?,?,?,?,?)`, m.OperationID.String(), m.ObjectID.String(), m.Kind, m.FolderSourceRelative, m.FolderDevice, m.FolderInode); err != nil {
				return fmt.Errorf("bind local folder mutation: %w", err)
			}
		}
		dependencies := append([]uuid.UUID(nil), m.AdditionalDependencies...)
		if m.DependencyOperationID != nil {
			dependencies = append(dependencies, *m.DependencyOperationID)
		}
		seen := make(map[uuid.UUID]struct{})
		for _, dependency := range dependencies {
			if _, duplicate := seen[dependency]; duplicate {
				continue
			}
			seen[dependency] = struct{}{}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_outbox_dependencies(operation_id,dependency_operation_id) VALUES(?,?)`, m.OperationID.String(), dependency.String()); err != nil {
				return fmt.Errorf("enqueue sync dependency: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]OutboxItem, error) {
	return s.listReady(ctx, limit, false)
}

// ListReady returns dependency-ready immutable operations, including attempted
// operations whose prior HTTP outcome was ambiguous and must be replayed with
// the same operation ID.
func (s *Store) ListReady(ctx context.Context, limit int) ([]OutboxItem, error) {
	return s.listReady(ctx, limit, true)
}

func (s *Store) listReady(ctx context.Context, limit int, includeAttempted bool) ([]OutboxItem, error) {
	if limit <= 0 || limit > 500 {
		return nil, errors.New("invalid ready limit")
	}
	statuses := "o.status='pending'"
	if includeAttempted {
		statuses = "o.status IN ('pending','attempted')"
	}
	var result []OutboxItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.sequence,o.operation_id,o.mutation,o.object_id,o.object_type,o.base_revision,o.parent_id,o.name,o.blob_hash,o.dependency_operation_id,o.status
			FROM sync_outbox o WHERE `+statuses+` AND NOT EXISTS(
				SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
				WHERE d.operation_id=o.operation_id AND prerequisite.status<>'accepted' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=prerequisite.operation_id AND r.resolution IN ('folder_move_reverted','note_move_equivalent')))
			ORDER BY o.sequence LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item OutboxItem
			var op, obj string
			var parent, dep sql.NullString
			if err := rows.Scan(&item.Sequence, &op, &item.Mutation.Kind, &obj, &item.Mutation.ObjectType, &item.Mutation.BaseRevision, &parent, &item.Mutation.Name, &item.Mutation.BlobHash, &dep, &item.Status); err != nil {
				return err
			}
			item.Mutation.OperationID, _ = uuid.Parse(op)
			item.Mutation.ObjectID, _ = uuid.Parse(obj)
			if parent.Valid {
				id, _ := uuid.Parse(parent.String)
				item.Mutation.ParentID = &id
			}
			if dep.Valid {
				id, _ := uuid.Parse(dep.String)
				item.Mutation.DependencyOperationID = &id
			}
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) MarkAttempted(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid operation id")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='attempted',attempted_at_ms=COALESCE(attempted_at_ms,?) WHERE operation_id=? AND status IN ('pending','attempted') AND NOT EXISTS(
			SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
			WHERE d.operation_id=sync_outbox.operation_id AND prerequisite.status<>'accepted' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=prerequisite.operation_id AND r.resolution IN ('folder_move_reverted','note_move_equivalent')))`, s.clock().UTC().UnixMilli(), operationID.String())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("operation is not pending")
		}
		return nil
	})
}
func (s *Store) RecordResult(ctx context.Context, operationID uuid.UUID, result Result) error {
	if !validOperationID(operationID) {
		return errors.New("invalid operation id")
	}
	if (result.Accepted && (result.Revision == 0 || result.Cursor == 0 || result.Conflict != "" || result.Canonical != nil)) ||
		(!result.Accepted && (result.Conflict == "" || result.Revision != 0 || result.Cursor != 0)) ||
		result.Revision > math.MaxInt64 || result.Cursor > math.MaxInt64 {
		return errors.New("invalid operation result")
	}
	if result.Canonical != nil {
		state := result.Canonical
		if (state.ObjectType != Note && state.ObjectType != Folder) || state.Revision == 0 || state.Revision > math.MaxInt64 || naming.ValidateComponent(state.Name) != nil || state.ParentID != nil && !validObjectID(*state.ParentID) || state.ObjectType == Note && len(state.BlobHash) != sha256.Size || state.ObjectType == Folder && len(state.BlobHash) != 0 {
			return errors.New("invalid canonical conflict state")
		}
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if result.Accepted {
			var unmet int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d
				JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
				WHERE d.operation_id=? AND prerequisite.status<>'accepted' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=prerequisite.operation_id AND r.resolution IN ('folder_move_reverted','note_move_equivalent'))`, operationID.String()).Scan(&unmet); err != nil {
				return err
			}
			if unmet != 0 {
				return errors.New("operation dependencies are not accepted")
			}
		}
		status := "conflict"
		if result.Accepted {
			status = "accepted"
		}
		res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status=?,result_revision=?,result_cursor=?,conflict_code=? WHERE operation_id=? AND status IN ('pending','attempted')`, status, nullablePositive(result.Revision), nullablePositive(result.Cursor), nullableString(result.Conflict), operationID.String())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("operation result already final or missing")
		}
		if result.Canonical != nil {
			var parent, blob any
			if result.Canonical.ParentID != nil {
				parent = result.Canonical.ParentID.String()
			}
			if len(result.Canonical.BlobHash) != 0 {
				blob = result.Canonical.BlobHash
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_conflict_states(operation_id,object_type,revision,parent_id,name,blob_hash,deleted) VALUES(?,?,?,?,?,?,?)`, operationID.String(), result.Canonical.ObjectType, result.Canonical.Revision, parent, result.Canonical.Name, blob, result.Canonical.Deleted); err != nil {
				return err
			}
		}
		if result.Accepted {
			var object string
			var base uint64
			if err := tx.QueryRowContext(ctx, `SELECT object_id,base_revision FROM sync_outbox WHERE operation_id=?`, operationID.String()).Scan(&object, &base); err != nil {
				return err
			}
			if base == math.MaxInt64 || result.Revision != base+1 {
				return errors.New("accepted revision does not match immutable operation base")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO sync_baselines(object_id,revision,operation_id) VALUES(?,?,?)
				ON CONFLICT(object_id) DO UPDATE SET revision=excluded.revision,operation_id=excluded.operation_id
				WHERE excluded.revision > sync_baselines.revision`, object, result.Revision, operationID.String())
			if err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) CanonicalConflictState(ctx context.Context, operationID uuid.UUID) (*CanonicalState, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid operation id")
	}
	var result *CanonicalState
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var state CanonicalState
		var parent sql.NullString
		var deleted int
		err := tx.QueryRowContext(ctx, `SELECT object_type,revision,parent_id,name,blob_hash,deleted FROM sync_conflict_states WHERE operation_id=?`, operationID.String()).Scan(&state.ObjectType, &state.Revision, &parent, &state.Name, &state.BlobHash, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		state.Deleted = deleted == 1
		if parent.Valid {
			id, err := uuid.Parse(parent.String)
			if err != nil {
				return errors.New("corrupt canonical conflict parent")
			}
			state.ParentID = &id
		}
		result = &state
		return nil
	})
	return result, err
}

func (s *Store) RecordReplayMismatch(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid operation id")
	}
	return s.finalStatus(ctx, operationID, "replay_mismatch")
}
func (s *Store) finalStatus(ctx context.Context, id uuid.UUID, status string) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status=? WHERE operation_id=? AND status IN ('pending','attempted')`, status, id.String())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("operation missing or final")
		}
		return nil
	})
}

// Projection returns the last confirmed revision plus immutable queued
// operations that a later local intent must depend on.
func (s *Store) Projection(ctx context.Context, objectID uuid.UUID) (uint64, *uuid.UUID, bool, error) {
	if !validObjectID(objectID) {
		return 0, nil, false, errors.New("invalid object id")
	}
	var revision uint64
	var dependency *uuid.UUID
	var durable bool
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		revision, dependency, durable, err = s.ProjectionTx(ctx, tx, objectID)
		return err
	})
	return revision, dependency, durable, err
}

func (s *Store) ProjectionTx(ctx context.Context, tx *sql.Tx, objectID uuid.UUID) (uint64, *uuid.UUID, bool, error) {
	if !validObjectID(objectID) {
		return 0, nil, false, errors.New("invalid object id")
	}
	var revision uint64
	durable := false
	var dependency *uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM sync_baselines WHERE object_id=?`, objectID.String()).Scan(&revision); err == nil {
		durable = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM sync_outbox WHERE object_id=? AND status IN ('pending','attempted') ORDER BY sequence`, objectID.String())
	if err != nil {
		return 0, nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, nil, false, err
		}
		id, err := uuid.Parse(raw)
		if err != nil || !validOperationID(id) {
			return 0, nil, false, errors.New("corrupt queued operation id")
		}
		if revision == math.MaxInt64 {
			return 0, nil, false, errors.New("projected revision overflow")
		}
		revision++
		copyID := id
		dependency = &copyID
		durable = true
	}
	return revision, dependency, durable, rows.Err()
}

func (s *Store) HasUnresolvedOutbox(ctx context.Context) (bool, error) {
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE (status IN ('pending','attempted','replay_mismatch') AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_members n JOIN conflict_folder_create_recoveries f ON f.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND f.state IN ('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n JOIN conflict_folder_move_delete_recoveries d ON d.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND d.state IN ('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n JOIN conflict_folder_divergent_move_recoveries d ON d.operation_id=n.operation_id WHERE (n.old_operation_id=o.operation_id OR EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id AND c.old_operation_id=o.operation_id)) AND d.state IN ('evacuated','canonical_published','completed'))) OR (status='conflict' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN ('copy_staged','copy_published','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries f WHERE f.operation_id=o.operation_id AND f.state IN ('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN ('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN ('evacuated','canonical_published','completed')) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=o.operation_id AND p.state='resolved')) )`).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) HasUnresolvedLocalIntent(ctx context.Context, objectID uuid.UUID) (bool, error) {
	if !validObjectID(objectID) {
		return false, errors.New("invalid object id")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_unresolved_local_intents WHERE object_id=?)`, objectID.String()).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) BaselineMatchesOperation(ctx context.Context, objectID uuid.UUID, revision uint64, operationID uuid.UUID) (bool, error) {
	if !validObjectID(objectID) || !validOperationID(operationID) || revision == 0 {
		return false, errors.New("invalid baseline operation")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_baselines WHERE object_id=? AND revision=? AND operation_id=?)`, objectID.String(), revision, operationID.String()).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) BaselineMatchesAppliedNote(ctx context.Context, objectID uuid.UUID, parentID uuid.UUID, name string, hash []byte) (bool, error) {
	if !validObjectID(objectID) || naming.ValidateComponent(name) != nil || len(hash) != sha256.Size {
		return false, errors.New("invalid applied note baseline")
	}
	var parent any
	if parentID != uuid.Nil {
		parent = parentID.String()
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_baselines b JOIN apply_steps s ON s.object_id=b.object_id AND s.revision=b.revision AND s.operation_id=b.operation_id JOIN apply_plans p ON p.plan_id=s.plan_id WHERE b.object_id=? AND s.object_type='note' AND s.state='applied' AND p.status='completed' AND s.name=? AND s.blob_hash=? AND ((s.parent_id IS NULL AND ? IS NULL) OR s.parent_id=?))`, objectID.String(), name, hash, parent, parent).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) Baseline(ctx context.Context, objectID uuid.UUID) (uint64, bool, error) {
	if !validObjectID(objectID) {
		return 0, false, errors.New("invalid object id")
	}
	var rev uint64
	found := false
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT revision FROM sync_baselines WHERE object_id=?`, objectID.String()).Scan(&rev)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			found = true
		}
		return err
	})
	return rev, found, err
}
func (s *Store) PrepareBootstrap(ctx context.Context, mutations []Mutation) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var value string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='bootstrap_required'`).Scan(&value); err != nil {
			return err
		}
		if value != "1" {
			return errors.New("sync bootstrap is not required")
		}
		if err := s.enqueueTx(ctx, tx, mutations); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET value='0' WHERE key='bootstrap_required'`)
		return err
	})
}
func (s *Store) BootstrapRequired(ctx context.Context) (bool, error) {
	required := false
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var value string
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='bootstrap_required'`).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		required = value == "1"
		return err
	})
	return required, err
}
func (s *Store) ConfirmedCursor(ctx context.Context) (uint64, error) {
	var value uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='confirmed_cursor'`).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		value, err = strconv.ParseUint(raw, 10, 63)
		if err != nil || strconv.FormatUint(value, 10) != raw {
			return errors.New("invalid confirmed cursor")
		}
		return nil
	})
	return value, err
}
func (s *Store) SetConfirmedCursor(ctx context.Context, cursor uint64) error {
	if cursor > math.MaxInt64 {
		return errors.New("confirmed cursor exceeds SQLite range")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var current uint64
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='confirmed_cursor'`).Scan(&raw)
		if err == nil {
			current, err = strconv.ParseUint(raw, 10, 63)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if cursor < current {
			return errors.New("confirmed cursor cannot move backwards")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('confirmed_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(cursor))
		return err
	})
}

func (s *Store) CreateApplyPlan(ctx context.Context, plan ApplyPlan) error {
	if !validOperationID(plan.ID) || plan.ThroughCursor < plan.FromCursor || plan.ThroughCursor > math.MaxInt64 {
		return errors.New("invalid apply plan")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var raw string
		confirmed := uint64(0)
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='confirmed_cursor'`).Scan(&raw)
		if err == nil {
			confirmed, err = strconv.ParseUint(raw, 10, 63)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if confirmed != plan.FromCursor {
			return errors.New("apply plan does not start at confirmed cursor")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO apply_plans(plan_id,from_cursor,through_cursor,status,created_at_ms) VALUES(?,?,?,'prepared',?)`, plan.ID.String(), plan.FromCursor, plan.ThroughCursor, s.clock().UTC().UnixMilli())
		if err != nil {
			return err
		}
		previousCursor := plan.FromCursor
		projectedRevisions := make(map[uuid.UUID]uint64)
		loadedProjection := make(map[uuid.UUID]bool)
		for n, step := range plan.Steps {
			if previousCursor == math.MaxInt64 || step.Cursor != previousCursor+1 || step.Cursor > plan.ThroughCursor || !validOperationID(step.OperationID) || !validObjectID(step.ObjectID) ||
				(step.ObjectType != Note && step.ObjectType != Folder) ||
				(step.Mutation != Create && step.Mutation != Update && step.Mutation != Move && step.Mutation != Delete) ||
				step.Revision == 0 || step.Revision > math.MaxInt64 ||
				(step.Mutation == Create && step.Revision != 1) || (step.Mutation != Create && step.Revision < 2) ||
				(step.ParentID != nil && !validObjectID(*step.ParentID)) || step.Name == "" || naming.ValidateComponent(step.Name) != nil ||
				(step.ObjectType == Note && len(step.BlobHash) != sha256.Size) || (step.ObjectType == Folder && len(step.BlobHash) != 0) ||
				(step.Deleted != (step.Mutation == Delete)) {
				return errors.New("invalid apply step")
			}
			if !loadedProjection[step.ObjectID] {
				var baseline uint64
				err := tx.QueryRowContext(ctx, `SELECT revision FROM sync_baselines WHERE object_id=?`, step.ObjectID.String()).Scan(&baseline)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				projectedRevisions[step.ObjectID] = baseline
				loadedProjection[step.ObjectID] = true
			}
			state := "pending"
			projected := projectedRevisions[step.ObjectID]
			if step.Revision <= projected {
				state = "applied"
			} else if projected == math.MaxInt64 || step.Revision != projected+1 {
				return errors.New("apply step revision is not contiguous")
			} else {
				projectedRevisions[step.ObjectID] = step.Revision
			}
			var parent, blob any
			if step.ParentID != nil {
				parent = step.ParentID.String()
			}
			if len(step.BlobHash) != 0 {
				blob = step.BlobHash
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, plan.ID.String(), n, step.Cursor, step.OperationID.String(), step.ObjectID.String(), step.Mutation, step.ObjectType, step.Revision, parent, step.Name, blob, state)
			if err != nil {
				return err
			}
			previousCursor = step.Cursor
		}
		if len(plan.Steps) == 0 || previousCursor != plan.ThroughCursor {
			return errors.New("apply steps do not cover plan cursor")
		}
		return nil
	})
}
func (s *Store) ActiveApplyPlan(ctx context.Context) (*ApplyPlan, error) {
	var plan *ApplyPlan
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var p ApplyPlan
		var id string
		err := tx.QueryRowContext(ctx, `SELECT plan_id,from_cursor,through_cursor,status FROM apply_plans WHERE status IN ('prepared','applying')`).Scan(&id, &p.FromCursor, &p.ThroughCursor, &p.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p.ID, _ = uuid.Parse(id)
		rows, err := tx.QueryContext(ctx, `SELECT cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state FROM apply_steps WHERE plan_id=? ORDER BY step_index`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Change
			var op, obj string
			var parent sql.NullString
			if err := rows.Scan(&c.Cursor, &op, &obj, &c.Mutation, &c.ObjectType, &c.Revision, &parent, &c.Name, &c.BlobHash, &c.State); err != nil {
				return err
			}
			c.OperationID, _ = uuid.Parse(op)
			c.ObjectID, _ = uuid.Parse(obj)
			c.Deleted = c.Mutation == Delete
			if parent.Valid {
				x, _ := uuid.Parse(parent.String)
				c.ParentID = &x
			}
			p.Steps = append(p.Steps, c)
		}
		plan = &p
		return rows.Err()
	})
	return plan, err
}

func (s *Store) BeginApplyPlan(ctx context.Context, planID uuid.UUID) error {
	if !validOperationID(planID) {
		return errors.New("invalid apply plan id")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var linkedCursor uint64
		linkErr := tx.QueryRowContext(ctx, `SELECT cursor FROM sync_inbox_apply_plans WHERE plan_id=?`, planID.String()).Scan(&linkedCursor)
		if linkErr != nil && !errors.Is(linkErr, sql.ErrNoRows) {
			return linkErr
		}
		if linkErr == nil {
			var planStatus, inboxState string
			if err := tx.QueryRowContext(ctx, `SELECT p.status,i.state FROM apply_plans p JOIN sync_inbox_apply_plans l ON l.plan_id=p.plan_id JOIN sync_inbox_changes i ON i.cursor=l.cursor WHERE p.plan_id=?`, planID.String()).Scan(&planStatus, &inboxState); err != nil {
				return err
			}
			if planStatus == "applying" && inboxState == "applying" {
				return linkedInboxParentBindingValidTx(ctx, tx, planID)
			}
			if planStatus == "prepared" && inboxState == "applying" {
				if err := linkedInboxParentBindingValidTx(ctx, tx, planID); err != nil {
					return err
				}
				result, err := tx.ExecContext(ctx, `UPDATE apply_plans SET status='applying' WHERE plan_id=? AND status='prepared'`, planID.String())
				if err != nil {
					return err
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					return errors.New("linked apply plan begin recovery race")
				}
				return nil
			}
			if planStatus != "prepared" || inboxState != "pending" {
				return errors.New("linked inbox apply plan is not prepared")
			}
			now := s.clock().UTC().UnixMilli()
			result, err := tx.ExecContext(ctx, `UPDATE sync_inbox_changes SET state='applying',applying_at_ms=max(?,ingested_at_ms) WHERE cursor=? AND state='pending'`, now, linkedCursor)
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				return errors.New("linked inbox begin race")
			}
			result, err = tx.ExecContext(ctx, `UPDATE apply_plans SET status='applying' WHERE plan_id=? AND status='prepared'`, planID.String())
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				return errors.New("linked apply plan begin race")
			}
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE apply_plans SET status='applying' WHERE plan_id=? AND status='prepared'`, planID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM apply_plans WHERE plan_id=?`, planID.String()).Scan(&status); err != nil {
			return err
		}
		if status != "applying" {
			return errors.New("apply plan is not active")
		}
		return nil
	})
}

func (s *Store) PutFolderMutation(ctx context.Context, mutation FolderMutation) error {
	validTarget := mutation.Mutation == Move && naming.ValidateUserRelativePath(mutation.TargetRelative) == nil || mutation.Mutation == Delete && len(mutation.TargetRelative) > 0
	if !validOperationID(mutation.PlanID) || mutation.StepIndex < 0 || !validObjectID(mutation.FolderID) || (mutation.Mutation != Move && mutation.Mutation != Delete) || naming.ValidateUserRelativePath(mutation.SourceRelative) != nil || !validTarget || mutation.Device == 0 || mutation.Inode == 0 || mutation.Device > math.MaxInt64 || mutation.Inode > math.MaxInt64 {
		return errors.New("invalid folder mutation binding")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO apply_folder_mutations(plan_id,step_index,folder_id,mutation_kind,source_relative,target_relative,device,inode)
			SELECT ?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM apply_steps WHERE plan_id=? AND step_index=? AND object_id=? AND mutation=? AND object_type='folder')`, mutation.PlanID.String(), mutation.StepIndex, mutation.FolderID.String(), mutation.Mutation, mutation.SourceRelative, mutation.TargetRelative, mutation.Device, mutation.Inode, mutation.PlanID.String(), mutation.StepIndex, mutation.FolderID.String(), mutation.Mutation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder mutation does not match apply step")
		}
		return nil
	})
}

func (s *Store) FolderMutation(ctx context.Context, planID uuid.UUID, stepIndex int) (*FolderMutation, error) {
	if !validOperationID(planID) || stepIndex < 0 {
		return nil, errors.New("invalid folder mutation key")
	}
	var result *FolderMutation
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var m FolderMutation
		var folderID string
		err := tx.QueryRowContext(ctx, `SELECT folder_id,mutation_kind,source_relative,target_relative,device,inode FROM apply_folder_mutations WHERE plan_id=? AND step_index=?`, planID.String(), stepIndex).Scan(&folderID, &m.Mutation, &m.SourceRelative, &m.TargetRelative, &m.Device, &m.Inode)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m.PlanID, m.StepIndex = planID, stepIndex
		m.FolderID, err = uuid.Parse(folderID)
		if err != nil {
			return errors.New("corrupt folder mutation binding")
		}
		result = &m
		return nil
	})
	return result, err
}

func (s *Store) LatestFolderMutationTarget(ctx context.Context, planID, folderID uuid.UUID) (string, bool, error) {
	if !validOperationID(planID) || !validObjectID(folderID) {
		return "", false, errors.New("invalid folder mutation lookup")
	}
	var target string
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT target_relative FROM apply_folder_mutations WHERE plan_id=? AND folder_id=? ORDER BY step_index DESC LIMIT 1`, planID.String(), folderID.String()).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return target, target != "", err
}

func (s *Store) PutFolderPublication(ctx context.Context, publication FolderPublication) error {
	zeroNonce := true
	for _, value := range publication.Nonce {
		zeroNonce = zeroNonce && value == 0
	}
	expectedStage := fmt.Sprintf(".remember/apply/folders/%s/%d", publication.PlanID.String(), publication.StepIndex)
	recoveredBase := path.Base(publication.TargetRelative)
	recoveredTarget := !IsReservedConflictFolder(publication.FolderID) && publication.TargetRelative == ConflictRootName+"/"+ConflictRecoveredName+"/"+recoveredBase && naming.ValidateComponent(recoveredBase) == nil
	preservedTarget := strings.HasPrefix(publication.TargetRelative, ConflictRootName+"/"+ConflictRecoveredName+"/")
	if preservedTarget {
		for _, part := range strings.Split(publication.TargetRelative, "/")[2:] {
			if naming.ValidateComponent(part) != nil {
				preservedTarget = false
				break
			}
		}
	}
	validTarget := naming.ValidateUserRelativePath(publication.TargetRelative) == nil || (publication.FolderID == ConflictRootID && publication.TargetRelative == ConflictRootName) || (publication.FolderID == ConflictRecoveredID && publication.TargetRelative == ConflictRootName+"/"+ConflictRecoveredName) || recoveredTarget || preservedTarget
	if !validOperationID(publication.PlanID) || publication.StepIndex < 0 || !validObjectID(publication.FolderID) || !validTarget || publication.StageRelative != expectedStage || zeroNonce || publication.Device > math.MaxInt64 || publication.Inode > math.MaxInt64 {
		return errors.New("invalid folder publication")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var objectID, mutation, objectType, state string
		var parent sql.NullString
		var cursor uint64
		if err := tx.QueryRowContext(ctx, `SELECT object_id,mutation,object_type,state,parent_id,cursor FROM apply_steps WHERE plan_id=? AND step_index=?`, publication.PlanID.String(), publication.StepIndex).Scan(&objectID, &mutation, &objectType, &state, &parent, &cursor); err != nil {
			return err
		}
		if objectID != publication.FolderID.String() || mutation != string(Create) || objectType != string(Folder) || state != "pending" || recoveredTarget && (!parent.Valid || parent.String != ConflictRecoveredID.String()) {
			return errors.New("folder publication does not match pending create")
		}
		if preservedTarget && !recoveredTarget {
			var exact int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.state='resolved' AND p.recovered_folder_id=? AND p.first_cursor=? UNION ALL SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE p.state='resolved' AND c.recovered_folder_id=? AND c.create_cursor=? UNION ALL SELECT 1 FROM apply_steps parent_step JOIN apply_folder_publications parent_pub ON parent_pub.plan_id=parent_step.plan_id AND parent_pub.step_index=parent_step.step_index WHERE parent_step.plan_id=? AND parent_step.object_id=? AND parent_step.mutation='create' AND parent_step.object_type='folder' AND parent_step.cursor<? AND parent_pub.target_relative LIKE ?)`, objectID, cursor, objectID, cursor, publication.PlanID.String(), parent.String, cursor, ConflictRootName+"/"+ConflictRecoveredName+"/%").Scan(&exact); err != nil || exact != 1 {
				return errors.New("folder publication is not preserve-delete bound")
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO apply_folder_publications(plan_id,step_index,folder_id,target_relative,stage_relative,nonce,device,inode,cleanup_authorized) VALUES(?,?,?,?,?,?,?,?,0)`, publication.PlanID.String(), publication.StepIndex, publication.FolderID.String(), publication.TargetRelative, publication.StageRelative, publication.Nonce[:], publication.Device, publication.Inode)
		return err
	})
}

func (s *Store) FolderPublicationForFolder(ctx context.Context, planID, folderID uuid.UUID) (*FolderPublication, error) {
	if !validOperationID(planID) || !validObjectID(folderID) {
		return nil, errors.New("invalid folder publication lookup")
	}
	var result *FolderPublication
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var p FolderPublication
		var rawFolder string
		var nonce []byte
		var cleanup int
		err := tx.QueryRowContext(ctx, `SELECT step_index,folder_id,target_relative,stage_relative,nonce,device,inode,cleanup_authorized FROM apply_folder_publications WHERE plan_id=? AND folder_id=? ORDER BY step_index LIMIT 1`, planID.String(), folderID.String()).Scan(&p.StepIndex, &rawFolder, &p.TargetRelative, &p.StageRelative, &nonce, &p.Device, &p.Inode, &cleanup)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p.PlanID = planID
		p.FolderID, err = uuid.Parse(rawFolder)
		if err != nil || p.FolderID != folderID || len(nonce) != 32 {
			return errors.New("corrupt folder publication lookup")
		}
		copy(p.Nonce[:], nonce)
		p.CleanupAuthorized = cleanup == 1
		result = &p
		return nil
	})
	return result, err
}

func (s *Store) FolderPublicationConsumedByDelete(ctx context.Context, planID, folderID uuid.UUID) (bool, error) {
	if !validOperationID(planID) || !validObjectID(folderID) {
		return false, errors.New("invalid consumed folder publication lookup")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apply_folder_publications f JOIN apply_plans p ON p.plan_id=f.plan_id JOIN apply_steps d ON d.plan_id=p.plan_id WHERE f.plan_id=? AND f.folder_id=? AND f.cleanup_authorized=1 AND f.cleaned_at_ms IS NOT NULL AND p.status='applying' AND d.object_id=f.folder_id AND d.object_type='folder' AND d.mutation='delete')`, planID.String(), folderID.String()).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) MarkFolderPublicationConsumedByDelete(ctx context.Context, planID, folderID uuid.UUID) error {
	if !validOperationID(planID) || !validObjectID(folderID) {
		return errors.New("invalid consumed folder publication")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE apply_folder_publications SET cleaned_at_ms=COALESCE(cleaned_at_ms,?) WHERE plan_id=? AND folder_id=? AND cleanup_authorized=1 AND EXISTS(SELECT 1 FROM apply_plans p JOIN apply_steps d ON d.plan_id=p.plan_id WHERE p.plan_id=? AND p.status='applying' AND d.object_id=? AND d.object_type='folder' AND d.mutation='delete' AND d.state IN ('pending','applied'))`, s.clock().UTC().UnixMilli(), planID.String(), folderID.String(), planID.String(), folderID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder publication delete consumption unavailable")
		}
		return nil
	})
}

func (s *Store) FolderPublication(ctx context.Context, planID uuid.UUID, stepIndex int) (*FolderPublication, error) {
	if !validOperationID(planID) || stepIndex < 0 {
		return nil, errors.New("invalid folder publication key")
	}
	var result *FolderPublication
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var folderID string
		var nonce []byte
		var device, inode uint64
		var cleanup int
		p := FolderPublication{PlanID: planID, StepIndex: stepIndex}
		err := tx.QueryRowContext(ctx, `SELECT folder_id,target_relative,stage_relative,nonce,device,inode,cleanup_authorized FROM apply_folder_publications WHERE plan_id=? AND step_index=?`, planID.String(), stepIndex).Scan(&folderID, &p.TargetRelative, &p.StageRelative, &nonce, &device, &inode, &cleanup)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		id, err := uuid.Parse(folderID)
		if err != nil || !validObjectID(id) || len(nonce) != 32 {
			return errors.New("corrupt folder publication")
		}
		p.FolderID, p.Device, p.Inode, p.CleanupAuthorized = id, device, inode, cleanup == 1
		copy(p.Nonce[:], nonce)
		result = &p
		return nil
	})
	return result, err
}

func (s *Store) CompletedFolderPublications(ctx context.Context) ([]FolderPublication, error) {
	var result []FolderPublication
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT f.plan_id,f.step_index,f.folder_id,f.target_relative,f.stage_relative,f.nonce,f.device,f.inode,f.cleanup_authorized
			FROM apply_folder_publications f JOIN apply_plans p ON p.plan_id=f.plan_id
			WHERE p.status='completed' AND f.cleanup_authorized=1 AND f.cleaned_at_ms IS NULL ORDER BY p.completed_at_ms,f.step_index`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p FolderPublication
			var planID, folderID string
			var nonce []byte
			var cleanup int
			if err := rows.Scan(&planID, &p.StepIndex, &folderID, &p.TargetRelative, &p.StageRelative, &nonce, &p.Device, &p.Inode, &cleanup); err != nil {
				return err
			}
			p.PlanID, err = uuid.Parse(planID)
			if err != nil {
				return errors.New("corrupt folder publication plan")
			}
			p.FolderID, err = uuid.Parse(folderID)
			if err != nil || len(nonce) != 32 || cleanup != 1 {
				return errors.New("corrupt completed folder publication")
			}
			copy(p.Nonce[:], nonce)
			p.CleanupAuthorized = true
			result = append(result, p)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) MarkFolderPublicationCleaned(ctx context.Context, planID uuid.UUID, stepIndex int) error {
	if !validOperationID(planID) || stepIndex < 0 {
		return errors.New("invalid folder publication key")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE apply_folder_publications SET cleaned_at_ms=COALESCE(cleaned_at_ms,?)
			WHERE plan_id=? AND step_index=? AND cleanup_authorized=1
			AND EXISTS(SELECT 1 FROM apply_plans WHERE plan_id=? AND status='completed')`, s.clock().UTC().UnixMilli(), planID.String(), stepIndex, planID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder publication is not cleanable")
		}
		return nil
	})
}

func (s *Store) MarkFolderStepAppliedAndAuthorizeCleanup(ctx context.Context, planID uuid.UUID, stepIndex int) error {
	if !validOperationID(planID) || stepIndex < 0 {
		return errors.New("invalid apply step")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM apply_steps WHERE plan_id=? AND step_index=?`, planID.String(), stepIndex).Scan(&state); err != nil {
			return err
		}
		if state == "pending" {
			res, err := tx.ExecContext(ctx, `UPDATE apply_steps SET state='applied' WHERE plan_id=? AND step_index=? AND EXISTS(SELECT 1 FROM apply_plans WHERE plan_id=? AND status='applying')`, planID.String(), stepIndex, planID.String())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("folder apply plan is not active")
			}
		} else if state != "applied" {
			return errors.New("apply step is not active")
		}
		res, err := tx.ExecContext(ctx, `UPDATE apply_folder_publications SET cleanup_authorized=1 WHERE plan_id=? AND step_index=?`, planID.String(), stepIndex)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder publication missing")
		}
		return nil
	})
}

func (s *Store) MarkApplyStepApplied(ctx context.Context, planID uuid.UUID, stepIndex int) error {
	if !validOperationID(planID) || stepIndex < 0 {
		return errors.New("invalid apply step")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var folderPublication int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apply_folder_publications WHERE plan_id=? AND step_index=?)`, planID.String(), stepIndex).Scan(&folderPublication); err != nil {
			return err
		}
		if folderPublication != 0 {
			return errors.New("folder step requires atomic cleanup authorization")
		}
		res, err := tx.ExecContext(ctx, `UPDATE apply_steps SET state='applied' WHERE plan_id=? AND step_index=? AND state='pending'
			AND EXISTS(SELECT 1 FROM apply_plans WHERE plan_id=? AND status='applying')`, planID.String(), stepIndex, planID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT s.state FROM apply_steps s JOIN apply_plans p ON p.plan_id=s.plan_id WHERE s.plan_id=? AND s.step_index=? AND p.status='applying'`, planID.String(), stepIndex).Scan(&state); err != nil {
			return err
		}
		if state != "applied" {
			return errors.New("apply step is not active")
		}
		return nil
	})
}

func (s *Store) CompleteApplyPlan(ctx context.Context, planID uuid.UUID) error {
	if !validOperationID(planID) {
		return errors.New("invalid apply plan id")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var linkedCursor uint64
		linkErr := tx.QueryRowContext(ctx, `SELECT cursor FROM sync_inbox_apply_plans WHERE plan_id=?`, planID.String()).Scan(&linkedCursor)
		if linkErr == nil {
			return s.completeInboxApplyPlanTx(ctx, tx, planID, linkedCursor)
		}
		if !errors.Is(linkErr, sql.ErrNoRows) {
			return linkErr
		}
		var from, through uint64
		if err := tx.QueryRowContext(ctx, `SELECT from_cursor,through_cursor FROM apply_plans WHERE plan_id=? AND status='applying'`, planID.String()).Scan(&from, &through); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM apply_steps WHERE plan_id=? AND state<>'applied'`, planID.String()).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return errors.New("apply plan has pending steps")
		}
		var raw string
		current := uint64(0)
		err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key='confirmed_cursor'`).Scan(&raw)
		if err == nil {
			current, err = strconv.ParseUint(raw, 10, 63)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current != from {
			return errors.New("confirmed cursor changed during apply")
		}
		rows, err := tx.QueryContext(ctx, `SELECT object_id,revision,operation_id FROM apply_steps WHERE plan_id=? ORDER BY step_index`, planID.String())
		if err != nil {
			return err
		}
		type baseline struct {
			object    string
			revision  uint64
			operation string
		}
		var values []baseline
		for rows.Next() {
			var value baseline
			if err := rows.Scan(&value.object, &value.revision, &value.operation); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_baselines(object_id,revision,operation_id) VALUES(?,?,?)
				ON CONFLICT(object_id) DO UPDATE SET revision=excluded.revision,operation_id=excluded.operation_id WHERE excluded.revision>sync_baselines.revision`, value.object, value.revision, value.operation); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('confirmed_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(through)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE apply_plans SET status='completed',completed_at_ms=? WHERE plan_id=? AND status='applying'`, s.clock().UTC().UnixMilli(), planID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("apply plan completion race")
		}
		return nil
	})
}

func mutationDependsOn(m Mutation, expected uuid.UUID) bool {
	if m.DependencyOperationID != nil && *m.DependencyOperationID == expected {
		return true
	}
	for _, dependency := range m.AdditionalDependencies {
		if dependency == expected {
			return true
		}
	}
	return false
}

func nullablePositive(v uint64) any {
	if v == 0 {
		return nil
	}
	return v
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
