package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"math"
)

type FolderPreserveDeleteResolution struct {
	ConflictOperationID, ResolutionOperationID, FolderID, RecoveredFolderID uuid.UUID
	ExpectedRevision, RecoveredCursor, DeletedCursor                        uint64
	State                                                                   string
}

func (s *Store) PrepareFolderPreserveDelete(ctx context.Context, conflict, resolution uuid.UUID) (*FolderPreserveDeleteResolution, error) {
	if !validOperationID(conflict) || !validOperationID(resolution) {
		return nil, errors.New("invalid preserve delete resolution")
	}
	var out FolderPreserveDeleteResolution
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var folder string
		var revision uint64
		if err := tx.QueryRowContext(ctx, `SELECT o.object_id,c.revision FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE o.operation_id=? AND o.object_type='folder' AND o.mutation='delete' AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND c.object_type='folder' AND c.deleted=0`, conflict.String()).Scan(&folder, &revision); err != nil {
			return err
		}
		id, err := uuid.Parse(folder)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state) VALUES(?,?,?,?,'prepared')`, conflict.String(), resolution.String(), folder, revision)
		if err != nil {
			var existingResolution, existingFolder, state string
			var existingRevision uint64
			var recoveredCursor, deletedCursor sql.NullInt64
			var recovered sql.NullString
			if scanErr := tx.QueryRowContext(ctx, `SELECT resolution_operation_id,folder_id,expected_revision,state,recovered_folder_id,recovered_cursor,deleted_cursor FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=?`, conflict.String()).Scan(&existingResolution, &existingFolder, &existingRevision, &state, &recovered, &recoveredCursor, &deletedCursor); scanErr != nil || existingFolder != folder || existingRevision != revision {
				return err
			}
			persistedResolution, parseErr := uuid.Parse(existingResolution)
			if parseErr != nil {
				return parseErr
			}
			out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: persistedResolution, FolderID: id, ExpectedRevision: revision, State: state, RecoveredCursor: uint64(recoveredCursor.Int64), DeletedCursor: uint64(deletedCursor.Int64)}
			if recovered.Valid {
				out.RecoveredFolderID, _ = uuid.Parse(recovered.String)
			}
			return nil
		}
		out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: resolution, FolderID: id, ExpectedRevision: revision, State: "prepared"}
		return nil
	})
	return &out, err
}
func nullableUUIDString(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func (s *Store) FolderPreserveDeleteRecoveryCreateMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Create || change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.recovered_folder_id=? AND p.recovered_cursor=? AND p.state='resolved')`, change.ObjectID.String(), change.Cursor).Scan(&exists)
	})
	return exists == 1, err
}

func (s *Store) FolderPreserveDeleteCanonicalMoveMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Move || change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p JOIN sync_conflict_states c ON c.operation_id=p.conflict_operation_id WHERE p.folder_id=? AND p.expected_revision=? AND p.state='resolved' AND c.object_type='folder' AND c.revision=? AND c.parent_id IS ? AND c.name=? AND c.deleted=0)`, change.ObjectID.String(), change.Revision, change.Revision, nullableUUIDString(change.ParentID), change.Name).Scan(&exists)
	})
	return exists == 1, err
}

func (s *Store) FolderPreserveDeleteMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Delete || !change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p JOIN sync_conflict_states c ON c.operation_id=p.conflict_operation_id WHERE p.folder_id=? AND p.state='resolved' AND p.deleted_cursor=? AND ?=p.expected_revision+1 AND c.name=? AND c.parent_id IS ?)`, change.ObjectID.String(), change.Cursor, change.Revision, change.Name, nullableUUIDString(change.ParentID)).Scan(&exists)
	})
	return exists == 1, err
}

func (s *Store) CompleteFolderPreserveDelete(ctx context.Context, conflict, recovered uuid.UUID, recoveredCursor, deletedCursor uint64) error {
	if !validOperationID(conflict) || !validObjectID(recovered) || recoveredCursor == 0 || recoveredCursor >= math.MaxInt64 || deletedCursor != recoveredCursor+1 || deletedCursor > math.MaxInt64 {
		return errors.New("invalid preserve delete completion")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET state='resolved',recovered_folder_id=?,recovered_cursor=?,deleted_cursor=? WHERE conflict_operation_id=? AND state='prepared'`, recovered.String(), recoveredCursor, deletedCursor, conflict.String())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=? AND state='resolved' AND recovered_folder_id=? AND recovered_cursor=? AND deleted_cursor=?`, conflict.String(), recovered.String(), recoveredCursor, deletedCursor).Scan(&count); err != nil || count != 1 {
				return errors.New("preserve delete completion unavailable")
			}
		}
		return nil
	})
}
