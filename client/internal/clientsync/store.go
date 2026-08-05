// Package clientsync owns durable client-side synchronization coordinator state.
package clientsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type MutationKind string
type ObjectType string

const (
	Create MutationKind = "create"
	Update MutationKind = "update"
	Move   MutationKind = "move"
	Delete MutationKind = "delete"
	Note   ObjectType   = "note"
	Folder ObjectType   = "folder"
)

type Mutation struct {
	OperationID            uuid.UUID
	Kind                   MutationKind
	ObjectID               uuid.UUID
	ObjectType             ObjectType
	BaseRevision           uint64
	ParentID               *uuid.UUID
	Name                   string
	BlobHash               []byte
	DependencyOperationID  *uuid.UUID
	AdditionalDependencies []uuid.UUID
}

type OutboxItem struct {
	Sequence int64
	Mutation Mutation
	Status   string
}
type Result struct {
	Accepted         bool
	Conflict         string
	Revision, Cursor uint64
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
}

type ApplyPlan struct {
	ID                        uuid.UUID
	FromCursor, ThroughCursor uint64
	Status                    string
	Steps                     []Change
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
	if limit <= 0 || limit > 500 {
		return nil, errors.New("invalid pending limit")
	}
	var result []OutboxItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.sequence,o.operation_id,o.mutation,o.object_id,o.object_type,o.base_revision,o.parent_id,o.name,o.blob_hash,o.dependency_operation_id,o.status
			FROM sync_outbox o WHERE o.status='pending' AND NOT EXISTS(
				SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
				WHERE d.operation_id=o.operation_id AND prerequisite.status<>'accepted')
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
		res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='attempted',attempted_at_ms=? WHERE operation_id=? AND status='pending' AND NOT EXISTS(
			SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
			WHERE d.operation_id=sync_outbox.operation_id AND prerequisite.status<>'accepted')`, s.clock().UTC().UnixMilli(), operationID.String())
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
	if (result.Accepted && (result.Revision == 0 || result.Cursor == 0 || result.Conflict != "")) ||
		(!result.Accepted && (result.Conflict == "" || result.Revision != 0 || result.Cursor != 0)) ||
		result.Revision > math.MaxInt64 || result.Cursor > math.MaxInt64 {
		return errors.New("invalid operation result")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if result.Accepted {
			var unmet int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d
				JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id
				WHERE d.operation_id=? AND prerequisite.status<>'accepted'`, operationID.String()).Scan(&unmet); err != nil {
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
		_, err := tx.ExecContext(ctx, `INSERT INTO apply_plans(plan_id,from_cursor,through_cursor,status,created_at_ms) VALUES(?,?,?,'prepared',?)`, plan.ID.String(), plan.FromCursor, plan.ThroughCursor, s.clock().UTC().UnixMilli())
		if err != nil {
			return err
		}
		previousCursor := plan.FromCursor
		for n, step := range plan.Steps {
			if previousCursor == math.MaxInt64 || step.Cursor != previousCursor+1 || step.Cursor > plan.ThroughCursor || !validOperationID(step.OperationID) || !validObjectID(step.ObjectID) ||
				(step.ObjectType != Note && step.ObjectType != Folder) ||
				(step.Mutation != Create && step.Mutation != Update && step.Mutation != Move && step.Mutation != Delete) ||
				step.Revision == 0 || step.Revision > math.MaxInt64 ||
				(step.ParentID != nil && !validObjectID(*step.ParentID)) ||
				(len(step.BlobHash) != 0 && len(step.BlobHash) != sha256.Size) {
				return errors.New("invalid apply step")
			}
			var parent, blob any
			if step.ParentID != nil {
				parent = step.ParentID.String()
			}
			if len(step.BlobHash) != 0 {
				blob = step.BlobHash
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending')`, plan.ID.String(), n, step.Cursor, step.OperationID.String(), step.ObjectID.String(), step.Mutation, step.ObjectType, step.Revision, parent, step.Name, blob)
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
		rows, err := tx.QueryContext(ctx, `SELECT cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash FROM apply_steps WHERE plan_id=? ORDER BY step_index`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Change
			var op, obj string
			var parent sql.NullString
			if err := rows.Scan(&c.Cursor, &op, &obj, &c.Mutation, &c.ObjectType, &c.Revision, &parent, &c.Name, &c.BlobHash); err != nil {
				return err
			}
			c.OperationID, _ = uuid.Parse(op)
			c.ObjectID, _ = uuid.Parse(obj)
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
