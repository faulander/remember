package clientsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math"
	"path"
	"strings"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

var ErrDivergentFolderMoveIneligible = errors.New("divergent folder move is not eligible for automatic recovery")

type ConflictFolderDivergentMoveRecovery struct {
	OperationID, FolderID, RecoveredFolderID, NewOperationID                      uuid.UUID
	AttemptedRelative, CanonicalRelative, RecoveryRelative                        string
	SourceDevice, SourceInode, CanonicalRevision, CanonicalDevice, CanonicalInode uint64
	CanonicalNonce                                                                [sha256.Size]byte
	State                                                                         string
}

func validConflictFolderDivergentMoveRecovery(r ConflictFolderDivergentMoveRecovery) bool {
	nonzero := false
	for _, b := range r.CanonicalNonce {
		nonzero = nonzero || b != 0
	}
	prefix := ConflictRootName + "/" + ConflictRecoveredName + "/"
	return validOperationID(r.OperationID) && validObjectID(r.FolderID) && validObjectID(r.RecoveredFolderID) && validOperationID(r.NewOperationID) && r.FolderID != r.RecoveredFolderID && r.OperationID != r.NewOperationID && naming.ValidateComponent(r.AttemptedRelative) == nil && naming.ValidateComponent(r.CanonicalRelative) == nil && r.AttemptedRelative != r.CanonicalRelative && strings.HasPrefix(r.RecoveryRelative, prefix) && path.Dir(r.RecoveryRelative) == ConflictRootName+"/"+ConflictRecoveredName && naming.ValidateComponent(path.Base(r.RecoveryRelative)) == nil && r.SourceDevice > 0 && r.SourceInode > 0 && r.SourceDevice <= math.MaxInt64 && r.SourceInode <= math.MaxInt64 && r.CanonicalRevision > 0 && nonzero && ((r.State == "prepared" || r.State == "evacuated") && r.CanonicalDevice == 0 && r.CanonicalInode == 0 || (r.State == "canonical_prepared" || r.State == "canonical_published" || r.State == "completed") && r.CanonicalDevice > 0 && r.CanonicalInode > 0)
}
func (s *Store) ConflictFolderDivergentMoveRecovery(ctx context.Context, operationID uuid.UUID) (*ConflictFolderDivergentMoveRecovery, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid divergent folder move recovery lookup")
	}
	var result *ConflictFolderDivergentMoveRecovery
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderDivergentMoveRecovery
		var op, folder, recovered, newOp string
		var nonce []byte
		var device, inode sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,canonical_device,canonical_inode,state FROM conflict_folder_divergent_move_recoveries WHERE operation_id=?`, operationID.String()).Scan(&op, &folder, &recovered, &newOp, &r.AttemptedRelative, &r.CanonicalRelative, &r.RecoveryRelative, &r.SourceDevice, &r.SourceInode, &r.CanonicalRevision, &nonce, &device, &inode, &r.State)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r.OperationID, err = uuid.Parse(op)
		if err == nil {
			r.FolderID, err = uuid.Parse(folder)
		}
		if err == nil {
			r.RecoveredFolderID, err = uuid.Parse(recovered)
		}
		if err == nil {
			r.NewOperationID, err = uuid.Parse(newOp)
		}
		if device.Valid {
			r.CanonicalDevice = uint64(device.Int64)
		}
		if inode.Valid {
			r.CanonicalInode = uint64(inode.Int64)
		}
		if err != nil || len(nonce) != 32 {
			return errors.New("corrupt divergent folder move recovery")
		}
		copy(r.CanonicalNonce[:], nonce)
		if !validConflictFolderDivergentMoveRecovery(r) {
			return errors.New("corrupt divergent folder move recovery")
		}
		result = &r
		return nil
	})
	return result, err
}
func (s *Store) DivergentFolderMoveRecoveryEligible(ctx context.Context, operationID uuid.UUID) (bool, error) {
	if !validOperationID(operationID) {
		return false, errors.New("invalid divergent folder move eligibility lookup")
	}
	var eligible int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.status='conflict' AND o.object_type='folder' AND o.mutation='move' AND o.conflict_code='base_revision_mismatch' AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict')) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict')))`, operationID.String()).Scan(&eligible)
	})
	return eligible != 0, err
}

func (s *Store) PutConflictFolderDivergentMoveRecovery(ctx context.Context, r ConflictFolderDivergentMoveRecovery) error {
	if !validConflictFolderDivergentMoveRecovery(r) || r.State != "prepared" {
		return errors.New("invalid divergent folder move recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'prepared')`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.CanonicalRelative, r.RecoveryRelative, r.SourceDevice, r.SourceInode, r.CanonicalRevision, r.CanonicalNonce[:])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent folder move recovery unavailable")
		}
		return nil
	})
}
func (s *Store) MarkConflictFolderDivergentMoveEvacuated(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='evacuated' WHERE operation_id=? AND state='prepared'`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent folder evacuation transition unavailable")
		}
		return nil
	})
}
func (s *Store) MarkConflictFolderDivergentMoveCanonicalPrepared(ctx context.Context, operationID uuid.UUID, device, inode uint64) error {
	if device == 0 || inode == 0 || device > math.MaxInt64 || inode > math.MaxInt64 {
		return errors.New("invalid divergent canonical identity")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='canonical_prepared',canonical_device=?,canonical_inode=? WHERE operation_id=? AND state='evacuated'`, device, inode, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent canonical preparation transition unavailable")
		}
		return nil
	})
}
func (s *Store) MarkConflictFolderDivergentMoveCanonicalPublished(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='canonical_published' WHERE operation_id=? AND state='canonical_prepared'`, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent canonical publication transition unavailable")
		}
		return nil
	})
}
func (s *Store) CompleteConflictFolderDivergentMoveRecovery(ctx context.Context, r ConflictFolderDivergentMoveRecovery) error {
	if !validConflictFolderDivergentMoveRecovery(r) || r.State != "canonical_published" {
		return errors.New("invalid divergent folder move completion")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var ok int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries recovery JOIN sync_outbox o ON o.operation_id=recovery.operation_id JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_baselines b ON b.object_id=recovery.folder_id JOIN sync_inbox_changes applied ON applied.operation_id=b.operation_id AND applied.object_id=b.object_id WHERE recovery.operation_id=? AND recovery.state='canonical_published' AND recovery.folder_id=? AND recovery.recovered_folder_id=? AND recovery.new_operation_id=? AND recovery.attempted_relative=? AND recovery.canonical_relative=? AND recovery.recovery_relative=? AND recovery.source_device=? AND recovery.source_inode=? AND recovery.canonical_revision=? AND recovery.canonical_nonce=? AND recovery.canonical_device=? AND recovery.canonical_inode=? AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND c.object_type='folder' AND c.deleted=0 AND c.parent_id IS NULL AND c.name=recovery.canonical_relative AND c.revision=recovery.canonical_revision AND b.revision=recovery.canonical_revision AND applied.state='applied' AND applied.object_type='folder' AND applied.mutation='move' AND applied.revision=recovery.canonical_revision AND applied.parent_id IS NULL AND applied.name=recovery.canonical_relative AND applied.deleted=0 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict')) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict')))`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.CanonicalRelative, r.RecoveryRelative, r.SourceDevice, r.SourceInode, r.CanonicalRevision, r.CanonicalNonce[:], r.CanonicalDevice, r.CanonicalInode).Scan(&ok); err != nil {
			return err
		}
		if ok == 0 {
			return errors.New("divergent folder move completion identity mismatch")
		}
		parent := ConflictRecoveredID
		if err := s.enqueueTx(ctx, tx, []Mutation{{OperationID: r.NewOperationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.RecoveryRelative)}}); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='completed' WHERE operation_id=? AND state='canonical_published'`, r.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent folder move completion unavailable")
		}
		return nil
	})
}
