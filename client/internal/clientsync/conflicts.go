package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

func (s *Store) ListConflicts(ctx context.Context, limit int) ([]ConflictItem, error) {
	if limit <= 0 || limit > 100 {
		return nil, errors.New("invalid conflict list limit")
	}
	var result []ConflictItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.sequence,o.operation_id,o.mutation,o.object_id,o.object_type,o.base_revision,o.parent_id,o.name,o.blob_hash,o.conflict_code,
			c.object_type,c.revision,c.parent_id,c.name,c.blob_hash,c.deleted
			FROM sync_outbox o LEFT JOIN sync_conflict_states c ON c.operation_id=o.operation_id
			WHERE o.status='conflict' AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN ('copy_staged','copy_published','completed'))
			ORDER BY o.sequence LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item ConflictItem
			var op, object string
			var parent, canonicalType, canonicalParent, canonicalName sql.NullString
			var canonicalRevision sql.NullInt64
			var canonicalBlob []byte
			var canonicalDeleted sql.NullInt64
			if err := rows.Scan(&item.Outbox.Sequence, &op, &item.Outbox.Mutation.Kind, &object, &item.Outbox.Mutation.ObjectType, &item.Outbox.Mutation.BaseRevision, &parent, &item.Outbox.Mutation.Name, &item.Outbox.Mutation.BlobHash, &item.Code, &canonicalType, &canonicalRevision, &canonicalParent, &canonicalName, &canonicalBlob, &canonicalDeleted); err != nil {
				return err
			}
			item.Outbox.Status = "conflict"
			item.Outbox.Mutation.OperationID, err = uuid.Parse(op)
			if err != nil {
				return errors.New("corrupt conflict operation id")
			}
			item.Outbox.Mutation.ObjectID, err = uuid.Parse(object)
			if err != nil {
				return errors.New("corrupt conflict object id")
			}
			if parent.Valid {
				id, err := uuid.Parse(parent.String)
				if err != nil {
					return err
				}
				item.Outbox.Mutation.ParentID = &id
			}
			if canonicalRevision.Valid {
				state := &CanonicalState{ObjectType: ObjectType(canonicalType.String), Revision: uint64(canonicalRevision.Int64), Name: canonicalName.String, BlobHash: append([]byte(nil), canonicalBlob...), Deleted: canonicalDeleted.Int64 == 1}
				if canonicalParent.Valid {
					id, err := uuid.Parse(canonicalParent.String)
					if err != nil {
						return err
					}
					state.ParentID = &id
				}
				item.Canonical = state
			}
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) ConflictFolderPublication(ctx context.Context, folderID uuid.UUID) (*ConflictFolderPublication, error) {
	if !validObjectID(folderID) {
		return nil, errors.New("invalid conflict folder id")
	}
	var result *ConflictFolderPublication
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var p ConflictFolderPublication
		var id string
		var nonce []byte
		err := tx.QueryRowContext(ctx, `SELECT folder_id,target_relative,stage_relative,nonce,device,inode,state FROM conflict_folder_publications WHERE folder_id=?`, folderID.String()).Scan(&id, &p.TargetRelative, &p.StageRelative, &nonce, &p.Device, &p.Inode, &p.State)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p.FolderID, err = uuid.Parse(id)
		if err != nil || len(nonce) != 32 {
			return errors.New("corrupt conflict folder publication")
		}
		copy(p.Nonce[:], nonce)
		result = &p
		return nil
	})
	return result, err
}

func (s *Store) PutConflictFolderPublication(ctx context.Context, p ConflictFolderPublication) error {
	expectedTarget, expectedStage := "", ".remember/conflicts/folders/"+p.FolderID.String()
	if p.FolderID == ConflictRootID {
		expectedTarget = ConflictRootName
	} else if p.FolderID == ConflictRecoveredID {
		expectedTarget = ConflictRootName + "/" + ConflictRecoveredName
	}
	if expectedTarget == "" || p.TargetRelative != expectedTarget || p.StageRelative != expectedStage || p.Device == 0 || p.Inode == 0 || p.Device > math.MaxInt64 || p.Inode > math.MaxInt64 || p.State != "prepared" {
		return errors.New("invalid conflict folder publication")
	}
	zero := true
	for _, b := range p.Nonce {
		zero = zero && b == 0
	}
	if zero {
		return errors.New("invalid conflict folder nonce")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_publications(folder_id,target_relative,stage_relative,nonce,device,inode,state) VALUES(?,?,?,?,?,?,?)`, p.FolderID.String(), p.TargetRelative, p.StageRelative, p.Nonce[:], p.Device, p.Inode, p.State)
		return err
	})
}

func (s *Store) MarkConflictFolderPublication(ctx context.Context, id uuid.UUID, state string) error {
	if !validObjectID(id) || (state != "published" && state != "cleaned") {
		return errors.New("invalid conflict folder transition")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_publications SET state=? WHERE folder_id=? AND (state=? OR (state='prepared' AND ?='published') OR (state='published' AND ?='cleaned'))`, state, id.String(), state, state, state)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict folder transition unavailable")
		}
		return nil
	})
}

func (s *Store) ConflictMutationKind(ctx context.Context, operationID uuid.UUID) (MutationKind, error) {
	var kind MutationKind
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT mutation FROM sync_outbox WHERE operation_id=?`, operationID.String()).Scan(&kind)
	})
	return kind, err
}

func (s *Store) HasStagedConflictForObject(ctx context.Context, objectID uuid.UUID) (bool, error) {
	if !validObjectID(objectID) {
		return false, errors.New("invalid conflict object")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_materializations WHERE source_object_id=? AND state IN ('copy_staged','copy_published'))`, objectID.String()).Scan(&exists)
	})
	return exists != 0, err
}

func (s *Store) ConflictMaterialization(ctx context.Context, operationID uuid.UUID) (*ConflictMaterialization, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid conflict operation")
	}
	var result *ConflictMaterialization
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var m ConflictMaterialization
		var op, obj, note string
		var source, materialized []byte
		var rebase sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT operation_id,source_object_id,conflict_note_id,original_relative,target_relative,source_hash,materialized_hash,staged_relative,state,rebased_operation_id FROM conflict_materializations WHERE operation_id=?`, operationID.String()).Scan(&op, &obj, &note, &m.OriginalRelative, &m.TargetRelative, &source, &materialized, &m.StagedRelative, &m.State, &rebase)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m.OperationID, err = uuid.Parse(op)
		if err != nil {
			return err
		}
		m.SourceObjectID, err = uuid.Parse(obj)
		if err != nil {
			return err
		}
		m.ConflictNoteID, err = uuid.Parse(note)
		if err != nil || len(source) != 32 || len(materialized) != 32 {
			return errors.New("corrupt conflict materialization")
		}
		copy(m.SourceHash[:], source)
		copy(m.MaterializedHash[:], materialized)
		if err := setRebasedOperation(&m, rebase); err != nil {
			return err
		}
		if !validConflictMaterializationShape(m) {
			return errors.New("corrupt conflict materialization shape")
		}
		result = &m
		return nil
	})
	return result, err
}

func uuidString(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func setRebasedOperation(m *ConflictMaterialization, raw sql.NullString) error {
	if !raw.Valid {
		return nil
	}
	id, err := uuid.Parse(raw.String)
	if err != nil || !validOperationID(id) {
		return errors.New("corrupt rebased conflict operation")
	}
	m.RebasedOperationID = &id
	return nil
}

func validConflictMaterializationShape(m ConflictMaterialization) bool {
	expectedStage := ".remember/conflicts/materializations/" + m.OperationID.String() + ".md"
	targetParent := ConflictRootName + "/" + ConflictRecoveredName
	expectedTarget := targetParent + "/" + ConflictFileName(path.Base(m.OriginalRelative), m.OperationID)
	return validOperationID(m.OperationID) && validObjectID(m.SourceObjectID) && validObjectID(m.ConflictNoteID) && (m.RebasedOperationID == nil || validOperationID(*m.RebasedOperationID)) && naming.ValidateRelativePath(m.OriginalRelative) == nil && m.TargetRelative == expectedTarget && naming.ValidateComponent(path.Base(m.TargetRelative)) == nil && m.StagedRelative == expectedStage
}

func (s *Store) PutConflictMaterialization(ctx context.Context, m ConflictMaterialization) error {
	if !validConflictMaterializationShape(m) || m.State != "prepared" {
		return errors.New("invalid conflict materialization")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_materializations(operation_id,source_object_id,conflict_note_id,original_relative,target_relative,source_hash,materialized_hash,staged_relative,state,rebased_operation_id) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sync_outbox WHERE operation_id=? AND object_id=? AND status='conflict' AND ((mutation='delete' AND ? IS NOT NULL) OR (mutation IN ('update','create','move') AND ? IS NULL)))`, m.OperationID.String(), m.SourceObjectID.String(), m.ConflictNoteID.String(), m.OriginalRelative, m.TargetRelative, m.SourceHash[:], m.MaterializedHash[:], m.StagedRelative, m.State, uuidString(m.RebasedOperationID), m.OperationID.String(), m.SourceObjectID.String(), uuidString(m.RebasedOperationID), uuidString(m.RebasedOperationID))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict operation unavailable")
		}
		return nil
	})
}

func (s *Store) MarkConflictCopyStaged(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_materializations SET state='copy_staged' WHERE operation_id=? AND state IN ('prepared','copy_staged')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict materialization transition unavailable")
		}
		_, err = tx.ExecContext(ctx, `WITH RECURSIVE doomed(operation_id) AS (SELECT d.operation_id FROM sync_outbox_dependencies d WHERE d.dependency_operation_id=? UNION SELECT d.operation_id FROM sync_outbox_dependencies d JOIN doomed ON d.dependency_operation_id=doomed.operation_id) UPDATE sync_outbox SET status='superseded' WHERE operation_id IN (SELECT operation_id FROM doomed) AND status IN ('pending','attempted')`, operationID.String())
		return err
	})
}
func (s *Store) MarkConflictCopyPublished(ctx context.Context, operationID uuid.UUID) error {
	return s.markConflictState(ctx, operationID, "copy_published", "copy_staged")
}
func (s *Store) markConflictState(ctx context.Context, operationID uuid.UUID, state, prior string) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_materializations SET state=? WHERE operation_id=? AND state IN (?,?)`, state, operationID.String(), state, prior)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict materialization transition unavailable")
		}
		return nil
	})
}

func (s *Store) StagedConflictMaterializations(ctx context.Context) ([]ConflictMaterialization, error) {
	var result []ConflictMaterialization
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT operation_id,source_object_id,conflict_note_id,original_relative,target_relative,source_hash,materialized_hash,staged_relative,state,rebased_operation_id FROM conflict_materializations WHERE state IN ('copy_staged','copy_published') ORDER BY operation_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m ConflictMaterialization
			var op, obj, note string
			var source, materialized []byte
			var rebase sql.NullString
			if err := rows.Scan(&op, &obj, &note, &m.OriginalRelative, &m.TargetRelative, &source, &materialized, &m.StagedRelative, &m.State, &rebase); err != nil {
				return err
			}
			m.OperationID, err = uuid.Parse(op)
			if err != nil {
				return err
			}
			m.SourceObjectID, err = uuid.Parse(obj)
			if err != nil {
				return err
			}
			m.ConflictNoteID, err = uuid.Parse(note)
			if err != nil || len(source) != 32 || len(materialized) != 32 {
				return errors.New("corrupt staged conflict materialization")
			}
			copy(m.SourceHash[:], source)
			copy(m.MaterializedHash[:], materialized)
			if err := setRebasedOperation(&m, rebase); err != nil {
				return err
			}
			if !validConflictMaterializationShape(m) {
				return errors.New("corrupt staged conflict materialization shape")
			}
			result = append(result, m)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) CompletedConflictCleanups(ctx context.Context) ([]ConflictMaterialization, error) {
	var result []ConflictMaterialization
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT operation_id,source_object_id,conflict_note_id,original_relative,target_relative,source_hash,materialized_hash,staged_relative,state,rebased_operation_id FROM conflict_materializations WHERE state='completed' AND cleaned_at_ms IS NULL ORDER BY operation_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m ConflictMaterialization
			var op, obj, note string
			var source, materialized []byte
			var rebase sql.NullString
			if err := rows.Scan(&op, &obj, &note, &m.OriginalRelative, &m.TargetRelative, &source, &materialized, &m.StagedRelative, &m.State, &rebase); err != nil {
				return err
			}
			m.OperationID, err = uuid.Parse(op)
			if err != nil {
				return err
			}
			m.SourceObjectID, err = uuid.Parse(obj)
			if err != nil {
				return err
			}
			m.ConflictNoteID, err = uuid.Parse(note)
			if err != nil || len(source) != 32 || len(materialized) != 32 {
				return errors.New("corrupt conflict cleanup")
			}
			copy(m.SourceHash[:], source)
			copy(m.MaterializedHash[:], materialized)
			if err := setRebasedOperation(&m, rebase); err != nil {
				return err
			}
			if !validConflictMaterializationShape(m) {
				return errors.New("corrupt conflict cleanup shape")
			}
			result = append(result, m)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) MarkConflictMaterializationCleaned(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid conflict cleanup")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_materializations SET cleaned_at_ms=COALESCE(cleaned_at_ms,?) WHERE operation_id=? AND state='completed'`, s.clock().UTC().UnixMilli(), operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict cleanup unavailable")
		}
		return nil
	})
}

func (s *Store) CompleteConflictMaterializationAndRebaseDelete(ctx context.Context, m ConflictMaterialization, canonical CanonicalState) error {
	if m.RebasedOperationID == nil || canonical.ObjectType != Note || canonical.Deleted || canonical.Revision == 0 {
		return errors.New("invalid delete conflict rebase")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var baseline uint64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM sync_baselines WHERE object_id=?`, m.SourceObjectID.String()).Scan(&baseline); err != nil || baseline != canonical.Revision {
			return errors.New("delete conflict canonical baseline unavailable")
		}
		var dependencyRaw string
		if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM sync_outbox WHERE object_id=? AND mutation='create' AND status IN ('pending','attempted') ORDER BY sequence DESC LIMIT 1`, m.ConflictNoteID.String()).Scan(&dependencyRaw); err != nil {
			return errors.New("delete conflict copy operation unavailable")
		}
		dependency, err := uuid.Parse(dependencyRaw)
		if err != nil || !validOperationID(dependency) {
			return errors.New("invalid delete conflict dependency")
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_materializations SET state='completed' WHERE operation_id=? AND state IN ('copy_published','completed')`, m.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict completion unavailable")
		}
		return s.enqueueTx(ctx, tx, []Mutation{{OperationID: *m.RebasedOperationID, Kind: Delete, ObjectID: m.SourceObjectID, ObjectType: Note, BaseRevision: canonical.Revision, AdditionalDependencies: []uuid.UUID{dependency}}})
	})
}

func (s *Store) CompleteConflictMaterialization(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid conflict operation")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var object string
		if err := tx.QueryRowContext(ctx, `SELECT source_object_id FROM conflict_materializations WHERE operation_id=? AND state IN ('copy_published','completed')`, operationID.String()).Scan(&object); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `WITH RECURSIVE doomed(operation_id) AS (
			SELECT ? UNION SELECT d.operation_id FROM sync_outbox_dependencies d JOIN doomed ON d.dependency_operation_id=doomed.operation_id
		) UPDATE sync_outbox SET status='superseded' WHERE operation_id IN (SELECT operation_id FROM doomed) AND status IN ('pending','attempted')`, operationID.String()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE object_id=? AND status IN ('pending','attempted')`, object); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_materializations SET state='completed' WHERE operation_id=? AND state IN ('copy_published','completed')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("conflict materialization completion unavailable")
		}
		return nil
	})
}
