package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path"
	"strings"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type ConflictFolderCreateRecovery struct {
	OperationID, SourceFolderID, RecoveredFolderID uuid.UUID
	SourceRelative, TargetRelative                 string
	Device, Inode                                  uint64
	State                                          string
}

func validFolderCreateRecovery(r ConflictFolderCreateRecovery) bool {
	prefix := ConflictRootName + "/" + ConflictRecoveredName + "/"
	return validOperationID(r.OperationID) && validObjectID(r.SourceFolderID) && validObjectID(r.RecoveredFolderID) && r.SourceFolderID != r.RecoveredFolderID && naming.ValidateUserRelativePath(r.SourceRelative) == nil && strings.HasPrefix(r.TargetRelative, prefix) && path.Base(r.TargetRelative) == ConflictFolderName(path.Base(r.SourceRelative), r.OperationID) && naming.ValidateComponent(path.Base(r.TargetRelative)) == nil && r.Device > 0 && r.Inode > 0 && r.Device <= math.MaxInt64 && r.Inode <= math.MaxInt64 && (r.State == "prepared" || r.State == "moved" || r.State == "completed")
}

func (s *Store) ConflictFolderCreateRecovery(ctx context.Context, operationID uuid.UUID) (*ConflictFolderCreateRecovery, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid folder create recovery lookup")
	}
	var result *ConflictFolderCreateRecovery
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderCreateRecovery
		var operation, source, recovered string
		err := tx.QueryRowContext(ctx, `SELECT operation_id,source_folder_id,recovered_folder_id,source_relative,target_relative,device,inode,state FROM conflict_folder_create_recoveries WHERE operation_id=?`, operationID.String()).Scan(&operation, &source, &recovered, &r.SourceRelative, &r.TargetRelative, &r.Device, &r.Inode, &r.State)
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
		r.SourceFolderID, err = uuid.Parse(source)
		if err != nil {
			return err
		}
		r.RecoveredFolderID, err = uuid.Parse(recovered)
		if err != nil || !validFolderCreateRecovery(r) {
			return errors.New("corrupt folder create recovery")
		}
		result = &r
		return nil
	})
	return result, err
}

func (s *Store) PutConflictFolderCreateRecovery(ctx context.Context, r ConflictFolderCreateRecovery) error {
	if !validFolderCreateRecovery(r) || r.State != "prepared" {
		return errors.New("invalid folder create recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_create_recoveries(operation_id,source_folder_id,recovered_folder_id,source_relative,target_relative,device,inode,state) SELECT o.operation_id,o.object_id,?,?,?,?,?,? FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict' AND o.conflict_code IN ('path_collision','parent_unavailable') AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict')) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))`, r.RecoveredFolderID.String(), r.SourceRelative, r.TargetRelative, r.Device, r.Inode, r.State, r.OperationID.String(), r.SourceFolderID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder create collision is not safely recoverable")
		}
		return nil
	})
}

func (s *Store) MarkConflictFolderCreateMoved(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_create_recoveries SET state='moved' WHERE operation_id=? AND state IN ('prepared','moved')`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder create recovery transition unavailable")
		}
		return nil
	})
}

func (s *Store) CompleteConflictFolderCreateRecovery(ctx context.Context, r ConflictFolderCreateRecovery) error {
	if !validFolderCreateRecovery(r) || (r.State != "moved" && r.State != "completed") {
		return errors.New("invalid folder create recovery completion")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var operation, source, recovered, sourceRelative, targetRelative, state string
		var device, inode uint64
		if err := tx.QueryRowContext(ctx, `SELECT operation_id,source_folder_id,recovered_folder_id,source_relative,target_relative,device,inode,state FROM conflict_folder_create_recoveries WHERE operation_id=?`, r.OperationID.String()).Scan(&operation, &source, &recovered, &sourceRelative, &targetRelative, &device, &inode, &state); err != nil {
			return err
		}
		if operation != r.OperationID.String() || source != r.SourceFolderID.String() || recovered != r.RecoveredFolderID.String() || sourceRelative != r.SourceRelative || targetRelative != r.TargetRelative || device != r.Device || inode != r.Inode || (state != "moved" && state != "completed") {
			return errors.New("folder create recovery completion identity mismatch")
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_create_recoveries SET state='completed' WHERE operation_id=? AND state IN ('moved','completed')`, r.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder create recovery completion unavailable")
		}
		operationID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		parent := ConflictRecoveredID
		if err := s.enqueueTx(ctx, tx, []Mutation{{OperationID: operationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.TargetRelative)}}); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) SELECT operation_id,'folder_create_collision_recovered',? FROM conflict_folder_create_recoveries WHERE operation_id=? AND state='completed' ON CONFLICT(operation_id) DO NOTHING`, s.clock().UTC().UnixMilli(), r.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder create recovery resolution unavailable")
		}
		return nil
	})
}
