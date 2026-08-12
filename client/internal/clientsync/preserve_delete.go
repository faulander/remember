package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"math"
)

type FolderPreserveDeleteClone struct {
	OriginalFolderID, RecoveredFolderID uuid.UUID
	CreateCursor, DeleteCursor          uint64
}
type FolderPreserveDeleteResolution struct {
	ConflictOperationID, ResolutionOperationID, FolderID, RecoveredFolderID uuid.UUID
	ExpectedRevision, KnownCursor, FirstCursor, LastCursor, RequestVersion  uint64
	State                                                                   string
	Clones                                                                  []FolderPreserveDeleteClone
}

func (s *Store) PrepareFolderPreserveDelete(ctx context.Context, conflict, resolution uuid.UUID, known uint64) (*FolderPreserveDeleteResolution, error) {
	if !validOperationID(conflict) || !validOperationID(resolution) || known > math.MaxInt64 {
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
		version := uint64(1)
		if known > 0 {
			version = 2
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor) VALUES(?,?,?,?,'prepared',?,?)`, conflict.String(), resolution.String(), folder, revision, version, nullablePositiveInt(known))
		if err != nil {
			var resolutionText, existingFolder, state string
			var existingRevision, existingVersion, first, last uint64
			var existingKnown sql.NullInt64
			var recovered sql.NullString
			if scanErr := tx.QueryRowContext(ctx, `SELECT resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor,COALESCE(first_cursor,0),COALESCE(last_cursor,0),recovered_folder_id FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=?`, conflict.String()).Scan(&resolutionText, &existingFolder, &existingRevision, &state, &existingVersion, &existingKnown, &first, &last, &recovered); scanErr != nil || existingFolder != folder || existingRevision != revision {
				return err
			}
			persisted, parseErr := uuid.Parse(resolutionText)
			if parseErr != nil {
				return parseErr
			}
			out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: persisted, FolderID: id, ExpectedRevision: revision, KnownCursor: uint64(existingKnown.Int64), RequestVersion: existingVersion, FirstCursor: first, LastCursor: last, State: state}
			if recovered.Valid {
				out.RecoveredFolderID, _ = uuid.Parse(recovered.String)
			}
			return nil
		}
		out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: resolution, FolderID: id, ExpectedRevision: revision, KnownCursor: known, RequestVersion: version, State: "prepared"}
		return nil
	})
	return &out, err
}

func nullablePositiveInt(value uint64) any {
	if value == 0 {
		return nil
	}
	return value
}

func (s *Store) KnownPreserveDeleteCursor(ctx context.Context, conflict uuid.UUID) (uint64, error) {
	var cursor uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT CAST(s.value AS INTEGER) FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_inbox_changes i ON i.object_id=o.object_id AND i.object_type='folder' AND i.mutation='move' AND i.revision=c.revision AND i.name=c.name AND i.parent_id IS c.parent_id JOIN sync_state s ON s.key='confirmed_cursor' WHERE o.operation_id=? AND i.cursor<=CAST(s.value AS INTEGER) ORDER BY i.cursor DESC LIMIT 1`, conflict.String()).Scan(&cursor)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return cursor, err
}

func nullableUUIDString(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}
func (s *Store) FolderPreserveDeleteRecoveryCreateParent(ctx context.Context, change Change) (bool, *uuid.UUID, error) {
	if change.ObjectType != Folder || change.Mutation != Create || change.Deleted {
		return false, nil, nil
	}
	var parent string
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT expected_parent FROM (SELECT p.recovered_folder_id object_id,p.first_cursor cursor,? expected_parent FROM sync_folder_preserve_delete_resolutions p WHERE p.state='resolved' UNION ALL SELECT c.recovered_folder_id,c.create_cursor,p.recovered_folder_id FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE p.state='resolved') WHERE object_id=? AND cursor=?`, ConflictRecoveredID.String(), change.ObjectID.String(), change.Cursor).Scan(&parent)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	id, err := uuid.Parse(parent)
	if err != nil {
		return false, nil, err
	}
	return true, &id, nil
}
func (s *Store) FolderPreserveDeleteCanonicalMoveMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Move || change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p JOIN sync_conflict_states c ON c.operation_id=p.conflict_operation_id WHERE p.folder_id=? AND p.expected_revision=? AND p.state='resolved' AND c.object_type='folder' AND c.revision=? AND c.parent_id IS ? AND c.name=? AND c.deleted=0 AND (p.request_version=1 OR ?<=p.known_cursor))`, change.ObjectID.String(), change.Revision, change.Revision, nullableUUIDString(change.ParentID), change.Name, change.Cursor).Scan(&exists)
	})
	return exists == 1, err
}
func (s *Store) FolderPreserveDeleteMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Delete || !change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.folder_id=? AND p.state='resolved' AND p.last_cursor=? AND ?=p.expected_revision+1 UNION ALL SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE c.original_folder_id=? AND c.delete_cursor=? AND p.state='resolved')`, change.ObjectID.String(), change.Cursor, change.Revision, change.ObjectID.String(), change.Cursor).Scan(&exists)
	})
	return exists == 1, err
}
func (s *Store) CompleteFolderPreserveDelete(ctx context.Context, conflict, recovered uuid.UUID, first, last uint64, clones []FolderPreserveDeleteClone) error {
	if !validOperationID(conflict) || !validObjectID(recovered) || first == 0 || last < first || last > math.MaxInt64 {
		return errors.New("invalid preserve delete completion")
	}
	n := uint64(len(clones))
	if n > (math.MaxInt64-2)/2 || last != first+2*n+1 {
		return errors.New("invalid preserve delete span")
	}
	seen := map[uuid.UUID]bool{recovered: true}
	for index, c := range clones {
		if !validObjectID(c.OriginalFolderID) || !validObjectID(c.RecoveredFolderID) || seen[c.OriginalFolderID] || seen[c.RecoveredFolderID] || c.CreateCursor != first+1+uint64(index) || c.DeleteCursor != first+1+n+uint64(index) {
			return errors.New("invalid preserve delete clone")
		}
		seen[c.OriginalFolderID] = true
		seen[c.RecoveredFolderID] = true
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET state='resolved',recovered_folder_id=?,recovered_cursor=?,deleted_cursor=?,first_cursor=?,last_cursor=?,clone_count=? WHERE conflict_operation_id=? AND state='prepared' `, recovered.String(), first, last, first, last, len(clones), conflict.String())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=? AND state='resolved' AND recovered_folder_id=? AND first_cursor=? AND last_cursor=?`, conflict.String(), recovered.String(), first, last).Scan(&count); err != nil || count != 1 {
				return errors.New("preserve delete completion unavailable")
			}
			rows, e := tx.QueryContext(ctx, `SELECT original_folder_id,recovered_folder_id,create_cursor,delete_cursor FROM sync_folder_preserve_delete_clones WHERE conflict_operation_id=? ORDER BY ordinal`, conflict.String())
			if e != nil {
				return e
			}
			defer rows.Close()
			i := 0
			for rows.Next() {
				if i >= len(clones) {
					return errors.New("preserve delete clone replay mismatch")
				}
				var oldText, newText string
				var createCursor, deleteCursor uint64
				if e = rows.Scan(&oldText, &newText, &createCursor, &deleteCursor); e != nil {
					return e
				}
				c := clones[i]
				if oldText != c.OriginalFolderID.String() || newText != c.RecoveredFolderID.String() || createCursor != c.CreateCursor || deleteCursor != c.DeleteCursor {
					return errors.New("preserve delete clone replay mismatch")
				}
				i++
			}
			if e = rows.Err(); e != nil {
				return e
			}
			if i != len(clones) {
				return errors.New("preserve delete clone replay mismatch")
			}
			return nil
		}
		for i, c := range clones {
			if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_clones(conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor) VALUES(?,?,?,?,?,?)`, conflict.String(), i, c.OriginalFolderID.String(), c.RecoveredFolderID.String(), c.CreateCursor, c.DeleteCursor); err != nil {
				return err
			}
		}
		return nil
	})
}
