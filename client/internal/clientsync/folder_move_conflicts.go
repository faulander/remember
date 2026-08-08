package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type ConflictFolderMoveRevert struct {
	OperationID, FolderID                uuid.UUID
	AttemptedRelative, CanonicalRelative string
	Device, Inode                        uint64
	State                                string
}

func validFolderMoveRevert(r ConflictFolderMoveRevert) bool {
	return validOperationID(r.OperationID) && validObjectID(r.FolderID) && naming.ValidateUserRelativePath(r.AttemptedRelative) == nil && naming.ValidateUserRelativePath(r.CanonicalRelative) == nil && r.Device > 0 && r.Inode > 0 && r.Device <= math.MaxInt64 && r.Inode <= math.MaxInt64 && (r.State == "prepared" || r.State == "moved" || r.State == "completed")
}

func (s *Store) ConflictFolderMoveRevert(ctx context.Context, operationID uuid.UUID) (*ConflictFolderMoveRevert, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid folder move revert lookup")
	}
	var result *ConflictFolderMoveRevert
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderMoveRevert
		var operation, folder string
		err := tx.QueryRowContext(ctx, `SELECT operation_id,folder_id,attempted_relative,canonical_relative,device,inode,state FROM conflict_folder_move_reverts WHERE operation_id=?`, operationID.String()).Scan(&operation, &folder, &r.AttemptedRelative, &r.CanonicalRelative, &r.Device, &r.Inode, &r.State)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r.OperationID, err = uuid.Parse(operation)
		if err != nil {
			return err
		}
		r.FolderID, err = uuid.Parse(folder)
		if err != nil || !validFolderMoveRevert(r) {
			return errors.New("corrupt folder move revert")
		}
		result = &r
		return nil
	})
	return result, err
}

func (s *Store) PutConflictFolderMoveRevert(ctx context.Context, r ConflictFolderMoveRevert) error {
	if !validFolderMoveRevert(r) || r.State != "prepared" {
		return errors.New("invalid folder move revert")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_reverts(operation_id,folder_id,attempted_relative,canonical_relative,device,inode,state) SELECT o.operation_id,o.object_id,?,?,?,?,? FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id WHERE o.operation_id=? AND o.object_id=? AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code IN ('path_collision','parent_unavailable','folder_cycle','base_revision_mismatch') AND c.object_type='folder' AND c.deleted=0 AND ((o.conflict_code='base_revision_mismatch' AND c.revision>o.base_revision AND c.name=o.name AND ((c.parent_id IS NULL AND o.parent_id IS NULL) OR c.parent_id=o.parent_id)) OR (o.conflict_code<>'base_revision_mismatch' AND c.revision=o.base_revision AND i.source_relative=?)) AND i.mutation_kind='move' AND i.device=? AND i.inode=? AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, r.AttemptedRelative, r.CanonicalRelative, r.Device, r.Inode, r.State, r.OperationID.String(), r.FolderID.String(), r.CanonicalRelative, r.Device, r.Inode)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move conflict is not safely revertible")
		}
		return nil
	})
}

func (s *Store) MarkConflictFolderMoveReverted(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_move_reverts SET state='moved' WHERE operation_id=? AND state IN ('prepared','moved')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move revert transition unavailable")
		}
		return nil
	})
}

func (s *Store) CompleteConflictFolderMoveRevert(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_move_reverts SET state='completed' WHERE operation_id=? AND state IN ('moved','completed')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move revert completion unavailable")
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) SELECT r.operation_id,'folder_move_reverted',? FROM conflict_folder_move_reverts r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE r.operation_id=? AND r.state='completed' AND o.status='conflict' AND o.mutation='move' AND o.object_type='folder' AND o.conflict_code IN ('path_collision','parent_unavailable','folder_cycle','base_revision_mismatch') ON CONFLICT(operation_id) DO NOTHING`, s.clock().UTC().UnixMilli(), operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_conflict_resolutions WHERE operation_id=? AND resolution='folder_move_reverted')`, operationID.String()).Scan(&exists); err != nil || exists == 0 {
				return errors.New("folder move revert resolution unavailable")
			}
		}
		return nil
	})
}
