package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

func validFolderRestoration(r ConflictFolderRestoration) bool {
	return validOperationID(r.OperationID) && validObjectID(r.FolderID) && naming.ValidateUserRelativePath(r.TargetRelative) == nil && r.StageRelative == ".remember/conflicts/restores/"+r.OperationID.String() && r.Device > 0 && r.Inode > 0 && r.Device <= math.MaxInt64 && r.Inode <= math.MaxInt64 && (r.State == "prepared" || r.State == "published" || r.State == "completed")
}

func (s *Store) ConflictFolderRestoration(ctx context.Context, operationID uuid.UUID) (*ConflictFolderRestoration, error) {
	var result *ConflictFolderRestoration
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderRestoration
		var op, folder string
		var nonce []byte
		err := tx.QueryRowContext(ctx, `SELECT operation_id,folder_id,target_relative,stage_relative,nonce,device,inode,state FROM conflict_folder_restorations WHERE operation_id=?`, operationID.String()).Scan(&op, &folder, &r.TargetRelative, &r.StageRelative, &nonce, &r.Device, &r.Inode, &r.State)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r.OperationID, err = uuid.Parse(op)
		if err != nil {
			return err
		}
		r.FolderID, err = uuid.Parse(folder)
		if err != nil || len(nonce) != 32 {
			return errors.New("corrupt conflict folder restoration")
		}
		copy(r.Nonce[:], nonce)
		if !validFolderRestoration(r) {
			return errors.New("corrupt conflict folder restoration shape")
		}
		result = &r
		return nil
	})
	return result, err
}

func (s *Store) PutConflictFolderRestoration(ctx context.Context, r ConflictFolderRestoration) error {
	if !validFolderRestoration(r) || r.State != "prepared" {
		return errors.New("invalid conflict folder restoration")
	}
	zero := true
	for _, b := range r.Nonce {
		zero = zero && b == 0
	}
	if zero {
		return errors.New("invalid conflict folder restoration nonce")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_restorations(operation_id,folder_id,target_relative,stage_relative,nonce,device,inode,state) SELECT ?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE o.operation_id=? AND o.object_id=? AND o.object_type='folder' AND o.mutation='delete' AND o.status='conflict' AND o.conflict_code='folder_not_empty' AND c.object_type='folder' AND c.deleted=0)`, r.OperationID.String(), r.FolderID.String(), r.TargetRelative, r.StageRelative, r.Nonce[:], r.Device, r.Inode, r.State, r.OperationID.String(), r.FolderID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder-not-empty conflict unavailable")
		}
		return nil
	})
}

func (s *Store) MarkConflictFolderRestorationPublished(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_restorations SET state='published' WHERE operation_id=? AND state IN ('prepared','published')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder restoration publication unavailable")
		}
		return nil
	})
}

func (s *Store) CompleteFolderNotEmptyResolution(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_restorations SET state='completed' WHERE operation_id=? AND state IN ('published','completed')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder restoration completion unavailable")
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) SELECT o.operation_id,'folder_not_empty_preserved',? FROM sync_outbox o JOIN conflict_folder_restorations r ON r.operation_id=o.operation_id WHERE o.operation_id=? AND o.mutation='delete' AND o.object_type='folder' AND o.status='conflict' AND o.conflict_code='folder_not_empty' AND r.state='completed' ON CONFLICT(operation_id) DO NOTHING`, s.clock().UTC().UnixMilli(), operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_conflict_resolutions WHERE operation_id=? AND resolution='folder_not_empty_preserved')`, operationID.String()).Scan(&exists); err != nil || exists == 0 {
				return errors.New("folder-not-empty resolution unavailable")
			}
		}
		_, err = tx.ExecContext(ctx, `WITH RECURSIVE doomed(operation_id) AS (SELECT d.operation_id FROM sync_outbox_dependencies d WHERE d.dependency_operation_id=? UNION SELECT d.operation_id FROM sync_outbox_dependencies d JOIN doomed ON d.dependency_operation_id=doomed.operation_id) UPDATE sync_outbox SET status='superseded' WHERE operation_id IN (SELECT operation_id FROM doomed) AND status IN ('pending','attempted')`, operationID.String())
		return err
	})
}
