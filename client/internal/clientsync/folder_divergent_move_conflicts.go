package clientsync

import (
	"bytes"
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
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.status='conflict' AND o.object_type='folder' AND o.mutation='move' AND o.conflict_code='base_revision_mismatch' AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict')))`, operationID.String()).Scan(&eligible)
	})
	return eligible != 0, err
}

func (s *Store) PutConflictFolderDivergentMoveRecovery(ctx context.Context, r ConflictFolderDivergentMoveRecovery) error {
	if !validConflictFolderDivergentMoveRecovery(r) || r.State != "prepared" {
		return errors.New("invalid divergent folder move recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var dependents int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox o ON o.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND o.status IN ('pending','attempted','replay_mismatch','conflict')`, r.OperationID.String()).Scan(&dependents); err != nil {
			return err
		}
		if dependents != 0 {
			return ErrDivergentFolderMoveIneligible
		}
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
func (s *Store) PutConflictFolderDivergentMoveRecoveryWithNotes(ctx context.Context, r ConflictFolderDivergentMoveRecovery, members []ConflictFolderCreateNoteMember) error {
	if !validConflictFolderDivergentMoveRecovery(r) || r.State != "prepared" || len(members) == 0 {
		return errors.New("invalid divergent folder note recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		seen := map[uuid.UUID]bool{}
		for _, m := range members {
			if !validObjectID(m.NoteID) || !validOperationID(m.OldOperationID) || !validOperationID(m.NewOperationID) || seen[m.NoteID] || naming.ValidateComponent(m.Name) != nil {
				return errors.New("invalid divergent note member")
			}
			seen[m.NoteID] = true
			previous := m.OldOperationID
			for i, chain := range m.Chain {
				if !validOperationID(chain.OperationID) || chain.PreviousOperationID != previous {
					return errors.New("invalid divergent note chain")
				}
				var children int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE dependency_operation_id=?`, previous.String()).Scan(&children); err != nil || children != 1 {
					return errors.New("divergent note chain branches")
				}
				var exact int
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.mutation='update' AND o.object_type='note' AND o.status='pending' AND o.blob_hash=? AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=o.operation_id AND d.dependency_operation_id=?) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies x WHERE x.operation_id=o.operation_id AND x.dependency_operation_id<>?))`, chain.OperationID.String(), m.NoteID.String(), chain.BlobHash[:], previous.String(), previous.String()).Scan(&exact); err != nil || exact != 1 {
					return errors.New("divergent note chain changed")
				}
				if i == len(m.Chain)-1 {
					var descendants int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE dependency_operation_id=?`, chain.OperationID.String()).Scan(&descendants); err != nil || descendants != 0 {
						return errors.New("divergent final update has dependents")
					}
				}
				previous = chain.OperationID
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'prepared')`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.CanonicalRelative, r.RecoveryRelative, r.SourceDevice, r.SourceInode, r.CanonicalRevision, r.CanonicalNonce[:])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent recovery unavailable")
		}
		for _, m := range members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_note_members(operation_id,note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash) VALUES(?,?,?,?,?,?,?)`, r.OperationID.String(), m.NoteID.String(), m.OldOperationID.String(), m.NewOperationID.String(), m.Name, m.BlobHash[:], m.CreateBlobHash[:]); err != nil {
				return err
			}
			for ordinal, chain := range m.Chain {
				if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_note_chains(operation_id,note_id,ordinal,old_operation_id,previous_operation_id,blob_hash) VALUES(?,?,?,?,?,?)`, r.OperationID.String(), m.NoteID.String(), ordinal+1, chain.OperationID.String(), chain.PreviousOperationID.String(), chain.BlobHash[:]); err != nil {
					return err
				}
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox n ON n.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND n.status IN ('pending','attempted','replay_mismatch','conflict')`, r.OperationID.String()).Scan(&count); err != nil {
			return err
		}
		if count != len(members) {
			return errors.New("divergent note manifest incomplete")
		}
		return nil
	})
}
func (s *Store) PutConflictFolderDivergentMoveRecoveryWithRecursiveManifest(ctx context.Context, r ConflictFolderDivergentMoveRecovery, manifest ConflictRecursiveLocalFolderManifest) error {
	if !validConflictFolderDivergentMoveRecovery(r) || r.State != "prepared" || manifest.OperationID != r.OperationID || manifest.NewRootOperationID != r.NewOperationID {
		return errors.New("invalid recursive divergent folder move recovery")
	}
	manifest.Kind = RecursiveFolderDivergentMoveRecovery
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'prepared')`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.CanonicalRelative, r.RecoveryRelative, r.SourceDevice, r.SourceInode, r.CanonicalRevision, r.CanonicalNonce[:])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("recursive divergent folder move recovery unavailable")
		}
		return putRecursiveLocalFolderManifestTx(ctx, tx, manifest)
	})
}
func (s *Store) ConflictFolderDivergentMoveNoteMembers(ctx context.Context, operationID uuid.UUID) ([]ConflictFolderCreateNoteMember, error) {
	var out []ConflictFolderCreateNoteMember
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash FROM conflict_folder_divergent_move_note_members WHERE operation_id=? ORDER BY note_id`, operationID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m ConflictFolderCreateNoteMember
			var note, old, newID string
			var blob, createBlob []byte
			if err := rows.Scan(&note, &old, &newID, &m.Name, &blob, &createBlob); err != nil {
				return err
			}
			m.NoteID, err = uuid.Parse(note)
			if err == nil {
				m.OldOperationID, err = uuid.Parse(old)
			}
			if err == nil {
				m.NewOperationID, err = uuid.Parse(newID)
			}
			if err != nil || len(blob) != 32 || len(createBlob) != 32 {
				return errors.New("corrupt divergent note member")
			}
			copy(m.BlobHash[:], blob)
			copy(m.CreateBlobHash[:], createBlob)
			chainRows, err := tx.QueryContext(ctx, `SELECT old_operation_id,previous_operation_id,blob_hash FROM conflict_folder_divergent_move_note_chains WHERE operation_id=? AND note_id=? ORDER BY ordinal`, operationID.String(), note)
			if err != nil {
				return err
			}
			for chainRows.Next() {
				var entry ConflictFolderCreateNoteChainOperation
				var op, previous string
				var h []byte
				if err := chainRows.Scan(&op, &previous, &h); err != nil {
					chainRows.Close()
					return err
				}
				entry.OperationID, err = uuid.Parse(op)
				if err == nil {
					entry.PreviousOperationID, err = uuid.Parse(previous)
				}
				if err != nil || len(h) != 32 {
					chainRows.Close()
					return errors.New("corrupt divergent note chain")
				}
				copy(entry.BlobHash[:], h)
				m.Chain = append(m.Chain, entry)
			}
			chainRows.Close()
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func validateDivergentNoteTopologyTx(ctx context.Context, tx *sql.Tx, r ConflictFolderDivergentMoveRecovery) error {
	rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,name,blob_hash,create_blob_hash FROM conflict_folder_divergent_move_note_members WHERE operation_id=?`, r.OperationID.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var note, old, name string
		var finalHash, createHash []byte
		if err := rows.Scan(&note, &old, &name, &finalHash, &createHash); err != nil {
			return err
		}
		var valid, count int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.mutation='create' AND o.object_type='note' AND o.parent_id=? AND o.name=? AND o.blob_hash=? AND o.status='pending' AND NOT EXISTS(SELECT 1 FROM sync_baselines b WHERE b.object_id=o.object_id))`, old, note, r.FolderID.String(), name, createHash).Scan(&valid); err != nil || valid != 1 {
			return errors.New("divergent note create changed")
		}
		chainRows, err := tx.QueryContext(ctx, `SELECT ordinal,old_operation_id,previous_operation_id,blob_hash FROM conflict_folder_divergent_move_note_chains WHERE operation_id=? AND note_id=? ORDER BY ordinal`, r.OperationID.String(), note)
		if err != nil {
			return err
		}
		previous := old
		ordinal := 0
		lastHash := createHash
		var chainOps []string
		for chainRows.Next() {
			ordinal++
			var got int
			var op, dependency string
			var hash []byte
			if err := chainRows.Scan(&got, &op, &dependency, &hash); err != nil {
				chainRows.Close()
				return err
			}
			if got != ordinal || dependency != previous {
				return errors.New("divergent note chain order changed")
			}
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.mutation='update' AND o.object_type='note' AND o.status='pending' AND o.blob_hash=? AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=o.operation_id)=1 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=o.operation_id AND d.dependency_operation_id=?))`, op, note, hash, previous).Scan(&valid); err != nil || valid != 1 {
				chainRows.Close()
				return errors.New("divergent note update changed")
			}
			chainOps = append(chainOps, op)
			previous = op
			lastHash = hash
		}
		chainRows.Close()
		if !bytes.Equal(lastHash, finalHash) {
			return errors.New("divergent note final hash changed")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=?`, note).Scan(&count); err != nil || count != 1+len(chainOps) {
			return errors.New("divergent note history disconnected")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE dependency_operation_id=? OR dependency_operation_id IN (SELECT old_operation_id FROM conflict_folder_divergent_move_note_chains WHERE operation_id=? AND note_id=?)`, old, r.OperationID.String(), note).Scan(&count); err != nil || count != len(chainOps) {
			return errors.New("divergent note outgoing topology changed")
		}
	}
	return rows.Err()
}

func (s *Store) MarkConflictFolderDivergentMoveEvacuated(ctx context.Context, operationID uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var r ConflictFolderDivergentMoveRecovery
		var op, folder, recovered, newOp string
		var nonce []byte
		if err := tx.QueryRowContext(ctx, `SELECT operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,state FROM conflict_folder_divergent_move_recoveries WHERE operation_id=? AND state='prepared'`, operationID.String()).Scan(&op, &folder, &recovered, &newOp, &r.AttemptedRelative, &r.CanonicalRelative, &r.RecoveryRelative, &r.SourceDevice, &r.SourceInode, &r.CanonicalRevision, &nonce, &r.State); err != nil {
			return err
		}
		var err error
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
		if err != nil || len(nonce) != 32 {
			return errors.New("corrupt divergent recovery")
		}
		copy(r.CanonicalNonce[:], nonce)
		if err := validateDivergentNoteTopologyTx(ctx, tx, r); err != nil {
			return err
		}
		var treeExists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests WHERE operation_id=? AND sealed=1)`, operationID.String()).Scan(&treeExists); err != nil {
			return err
		}
		if treeExists == 1 {
			manifest, err := conflictFolderDivergentTreeManifestTx(ctx, tx, operationID)
			if err != nil {
				return err
			}
			if err := validateDivergentFolderTreeTopologyTx(ctx, tx, r.FolderID, *manifest); err != nil {
				return err
			}
		}
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
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries recovery JOIN sync_outbox o ON o.operation_id=recovery.operation_id JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_baselines b ON b.object_id=recovery.folder_id JOIN sync_inbox_changes applied ON applied.operation_id=b.operation_id AND applied.object_id=b.object_id WHERE recovery.operation_id=? AND recovery.state='canonical_published' AND recovery.folder_id=? AND recovery.recovered_folder_id=? AND recovery.new_operation_id=? AND recovery.attempted_relative=? AND recovery.canonical_relative=? AND recovery.recovery_relative=? AND recovery.source_device=? AND recovery.source_inode=? AND recovery.canonical_revision=? AND recovery.canonical_nonce=? AND recovery.canonical_device=? AND recovery.canonical_inode=? AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND c.object_type='folder' AND c.deleted=0 AND c.parent_id IS NULL AND c.name=recovery.canonical_relative AND c.revision=recovery.canonical_revision AND b.revision=recovery.canonical_revision AND applied.state='applied' AND applied.object_type='folder' AND applied.mutation='move' AND applied.revision=recovery.canonical_revision AND applied.parent_id IS NULL AND applied.name=recovery.canonical_relative AND applied.deleted=0 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict')))`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.CanonicalRelative, r.RecoveryRelative, r.SourceDevice, r.SourceInode, r.CanonicalRevision, r.CanonicalNonce[:], r.CanonicalDevice, r.CanonicalInode).Scan(&ok); err != nil {
			return err
		}
		if ok == 0 {
			return errors.New("divergent folder move completion identity mismatch")
		}
		foundTree, treeMutations, err := divergentFolderTreeReplacementMutationsTx(ctx, tx, r)
		if err != nil {
			return err
		}
		if foundTree {
			if err := s.enqueueTx(ctx, tx, treeMutations); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='completed' WHERE operation_id=? AND state='canonical_published'`, r.OperationID.String())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("divergent folder tree completion unavailable")
			}
			return nil
		}
		foundRecursive, recursiveMutations, err := recursiveLocalFolderReplacementMutationsTx(ctx, tx, RecursiveFolderDivergentMoveRecovery, r.OperationID, r.RecoveredFolderID, path.Base(r.RecoveryRelative))
		if err != nil {
			return err
		}
		if foundRecursive {
			if err := s.enqueueTx(ctx, tx, recursiveMutations); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='completed' WHERE operation_id=? AND state='canonical_published'`, r.OperationID.String())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("recursive divergent folder move completion unavailable")
			}
			return nil
		}
		if err := validateDivergentNoteTopologyTx(ctx, tx, r); err != nil {
			return err
		}
		var chainCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflict_folder_divergent_move_note_chains WHERE operation_id=?`, r.OperationID.String()).Scan(&chainCount); err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE status='pending' AND operation_id IN (SELECT old_operation_id FROM conflict_folder_divergent_move_note_chains WHERE operation_id=?)`, r.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := changed.RowsAffected(); int(n) != chainCount {
			return errors.New("divergent note chain completion changed")
		}
		parent := ConflictRecoveredID
		mutations := []Mutation{{OperationID: r.NewOperationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.RecoveryRelative)}}
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash FROM conflict_folder_divergent_move_note_members WHERE operation_id=? ORDER BY note_id`, r.OperationID.String())
		if err != nil {
			return err
		}
		for rows.Next() {
			var note, old, newID, name string
			var blob, createBlob []byte
			if err := rows.Scan(&note, &old, &newID, &name, &blob, &createBlob); err != nil {
				rows.Close()
				return err
			}
			noteID, e := uuid.Parse(note)
			if e != nil || len(blob) != 32 || len(createBlob) != 32 {
				rows.Close()
				return errors.New("corrupt divergent note completion")
			}
			newOperation, e := uuid.Parse(newID)
			if e != nil {
				rows.Close()
				return e
			}
			result, e := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND mutation='create' AND object_type='note' AND parent_id=? AND name=? AND blob_hash=? AND status='pending' AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox later ON later.operation_id=d.operation_id WHERE d.dependency_operation_id=sync_outbox.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, old, note, r.FolderID.String(), name, createBlob)
			if e != nil {
				rows.Close()
				return e
			}
			if n, _ := result.RowsAffected(); n != 1 {
				rows.Close()
				return errors.New("divergent note predecessor changed")
			}
			folder := r.RecoveredFolderID
			mutations = append(mutations, Mutation{OperationID: newOperation, Kind: Create, ObjectID: noteID, ObjectType: Note, ParentID: &folder, Name: name, BlobHash: blob, DependencyOperationID: &r.NewOperationID})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if err := s.enqueueTx(ctx, tx, mutations); err != nil {
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
