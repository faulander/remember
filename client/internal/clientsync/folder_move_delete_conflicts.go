package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
	"math"
	"path"
	"strings"
)

type ConflictFolderMoveDeleteRecovery struct {
	OperationID, FolderID, RecoveredFolderID, NewOperationID uuid.UUID
	AttemptedRelative, TargetRelative                        string
	Device, Inode, CanonicalRevision                         uint64
	State                                                    string
}

func validFolderMoveDeleteRecovery(r ConflictFolderMoveDeleteRecovery) bool {
	prefix := ConflictRootName + "/" + ConflictRecoveredName + "/"
	return validOperationID(r.OperationID) && validObjectID(r.FolderID) && validObjectID(r.RecoveredFolderID) && validOperationID(r.NewOperationID) && r.FolderID != r.RecoveredFolderID && naming.ValidateUserRelativePath(r.AttemptedRelative) == nil && strings.HasPrefix(r.TargetRelative, prefix) && path.Base(r.TargetRelative) == ConflictFolderName(path.Base(r.AttemptedRelative), r.OperationID) && naming.ValidateComponent(path.Base(r.TargetRelative)) == nil && r.Device > 0 && r.Inode > 0 && r.Device <= math.MaxInt64 && r.Inode <= math.MaxInt64 && r.CanonicalRevision > 0 && (r.State == "prepared" || r.State == "moved" || r.State == "completed")
}
func (s *Store) ConflictFolderMoveDeleteRecovery(ctx context.Context, operationID uuid.UUID) (*ConflictFolderMoveDeleteRecovery, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid folder move/delete recovery lookup")
	}
	var result *ConflictFolderMoveDeleteRecovery
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderMoveDeleteRecovery
		var op, folder, recovered, newOperation string
		err := tx.QueryRowContext(ctx, `SELECT operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision,state FROM conflict_folder_move_delete_recoveries WHERE operation_id=?`, operationID.String()).Scan(&op, &folder, &recovered, &newOperation, &r.AttemptedRelative, &r.TargetRelative, &r.Device, &r.Inode, &r.CanonicalRevision, &r.State)
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
		if err != nil {
			return err
		}
		r.RecoveredFolderID, err = uuid.Parse(recovered)
		if err == nil {
			r.NewOperationID, err = uuid.Parse(newOperation)
		}
		if err != nil || !validFolderMoveDeleteRecovery(r) {
			return errors.New("corrupt folder move/delete recovery")
		}
		result = &r
		return nil
	})
	return result, err
}
func (s *Store) PutConflictFolderMoveDeleteRecovery(ctx context.Context, r ConflictFolderMoveDeleteRecovery) error {
	if !validFolderMoveDeleteRecovery(r) || r.State != "prepared" {
		return errors.New("invalid folder move/delete recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_delete_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision,state) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.TargetRelative, r.Device, r.Inode, r.CanonicalRevision, r.State)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move/delete recovery unavailable")
		}
		return nil
	})
}
func (s *Store) MarkConflictFolderMoveDeleteMoved(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_move_delete_recoveries SET state='moved' WHERE operation_id=? AND state IN ('prepared','moved')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move/delete recovery transition unavailable")
		}
		return nil
	})
}
func (s *Store) CompleteConflictFolderMoveDeleteRecovery(ctx context.Context, r ConflictFolderMoveDeleteRecovery) error {
	if !validFolderMoveDeleteRecovery(r) || (r.State != "moved" && r.State != "completed") {
		return errors.New("invalid folder move/delete completion")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var folder, recovered, newOperation, attempted, target, state string
		var device, inode, revision uint64
		if err := tx.QueryRowContext(ctx, `SELECT folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision,state FROM conflict_folder_move_delete_recoveries WHERE operation_id=?`, r.OperationID.String()).Scan(&folder, &recovered, &newOperation, &attempted, &target, &device, &inode, &revision, &state); err != nil {
			return err
		}
		if folder != r.FolderID.String() || recovered != r.RecoveredFolderID.String() || newOperation != r.NewOperationID.String() || attempted != r.AttemptedRelative || target != r.TargetRelative || device != r.Device || inode != r.Inode || revision != r.CanonicalRevision || state != "moved" {
			return errors.New("folder move/delete completion identity mismatch")
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_move_delete_recoveries SET state='completed' WHERE operation_id=? AND state='moved'`, r.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move/delete completion unavailable")
		}
		parent := ConflictRecoveredID
		if err := s.enqueueTx(ctx, tx, []Mutation{{OperationID: r.NewOperationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.TargetRelative)}}); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) VALUES(?,'folder_move_deleted_recovered',?)`, r.OperationID.String(), s.clock().UTC().UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move/delete resolution unavailable")
		}
		return nil
	})
}
func (s *Store) FolderMoveDeleteRecoveryMatches(ctx context.Context, change Change) (bool, error) {
	if change.Mutation != Delete || change.ObjectType != Folder || !change.Deleted || change.Revision == 0 {
		return false, errors.New("invalid folder delete match")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries r JOIN sync_outbox o ON o.operation_id=r.operation_id JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE r.folder_id=? AND r.canonical_revision=? AND r.state IN ('moved','completed') AND o.object_id=r.folder_id AND o.mutation='move' AND o.object_type='folder' AND o.status='conflict' AND o.conflict_code='object_deleted' AND c.object_type='folder' AND c.deleted=1 AND c.revision=r.canonical_revision AND c.blob_hash IS NULL)`, change.ObjectID.String(), change.Revision).Scan(&exists)
	})
	return exists != 0, err
}
