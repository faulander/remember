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
		var dependents int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox o ON o.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND o.status IN ('pending','attempted','replay_mismatch','conflict')`, r.OperationID.String()).Scan(&dependents); err != nil {
			return err
		}
		if dependents != 0 {
			return errors.New("folder move/delete recovery has active dependents")
		}
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
func (s *Store) PutConflictFolderMoveDeleteRecoveryWithNotes(ctx context.Context, r ConflictFolderMoveDeleteRecovery, members []ConflictFolderCreateNoteMember) error {
	if !validFolderMoveDeleteRecovery(r) || r.State != "prepared" || len(members) == 0 {
		return errors.New("invalid folder move/delete note recovery")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		seen := map[uuid.UUID]bool{}
		for _, m := range members {
			if !validObjectID(m.NoteID) || !validOperationID(m.OldOperationID) || !validOperationID(m.NewOperationID) || seen[m.NoteID] || naming.ValidateComponent(m.Name) != nil {
				return errors.New("invalid folder move/delete note member")
			}
			seen[m.NoteID] = true
			previous := m.OldOperationID
			for i, chain := range m.Chain {
				if !validOperationID(chain.OperationID) || chain.PreviousOperationID != previous {
					return errors.New("invalid folder move/delete note chain")
				}
				var children int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE dependency_operation_id=?`, previous.String()).Scan(&children); err != nil || children != 1 {
					return errors.New("folder move/delete note chain branches")
				}
				res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND mutation='update' AND object_type='note' AND status='pending' AND blob_hash=? AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=sync_outbox.operation_id AND d.dependency_operation_id=?) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies x WHERE x.operation_id=sync_outbox.operation_id AND x.dependency_operation_id<>?)`, chain.OperationID.String(), m.NoteID.String(), chain.BlobHash[:], previous.String(), previous.String())
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return errors.New("folder move/delete note chain changed")
				}
				if i == len(m.Chain)-1 {
					var descendants int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE dependency_operation_id=?`, chain.OperationID.String()).Scan(&descendants); err != nil || descendants != 0 {
						return errors.New("folder move/delete final update has dependents")
					}
				}
				previous = chain.OperationID
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_delete_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision,state) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.TargetRelative, r.Device, r.Inode, r.CanonicalRevision, r.State)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("folder move/delete note recovery unavailable")
		}
		for _, m := range members {
			res, err = tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_delete_note_members(operation_id,note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash) VALUES(?,?,?,?,?,?,?)`, r.OperationID.String(), m.NoteID.String(), m.OldOperationID.String(), m.NewOperationID.String(), m.Name, m.BlobHash[:], m.CreateBlobHash[:])
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("folder move/delete note member unavailable")
			}
			for ordinal, chain := range m.Chain {
				if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_delete_note_chains(operation_id,note_id,ordinal,old_operation_id,previous_operation_id,blob_hash) VALUES(?,?,?,?,?,?)`, r.OperationID.String(), m.NoteID.String(), ordinal+1, chain.OperationID.String(), chain.PreviousOperationID.String(), chain.BlobHash[:]); err != nil {
					return err
				}
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox n ON n.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND n.status IN ('pending','attempted','replay_mismatch','conflict')`, r.OperationID.String()).Scan(&count); err != nil {
			return err
		}
		if count != len(members) {
			return errors.New("folder move/delete note manifest incomplete")
		}
		return nil
	})
}
func (s *Store) PutConflictFolderMoveDeleteRecoveryWithRecursiveManifest(ctx context.Context, r ConflictFolderMoveDeleteRecovery, manifest ConflictRecursiveLocalFolderManifest) error {
	if !validFolderMoveDeleteRecovery(r) || r.State != "prepared" || manifest.OperationID != r.OperationID || manifest.NewRootOperationID != r.NewOperationID {
		return errors.New("invalid recursive folder move/delete recovery")
	}
	manifest.Kind = RecursiveFolderMoveDeleteRecovery
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_move_delete_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision,state) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.OperationID.String(), r.FolderID.String(), r.RecoveredFolderID.String(), r.NewOperationID.String(), r.AttemptedRelative, r.TargetRelative, r.Device, r.Inode, r.CanonicalRevision, r.State)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("recursive folder move/delete recovery unavailable")
		}
		return putRecursiveLocalFolderManifestTx(ctx, tx, manifest)
	})
}
func (s *Store) ConflictFolderMoveDeleteNoteMembers(ctx context.Context, operationID uuid.UUID) ([]ConflictFolderCreateNoteMember, error) {
	var out []ConflictFolderCreateNoteMember
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash FROM conflict_folder_move_delete_note_members WHERE operation_id=? ORDER BY note_id`, operationID.String())
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
				return errors.New("corrupt folder move/delete note member")
			}
			copy(m.BlobHash[:], blob)
			copy(m.CreateBlobHash[:], createBlob)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
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
		foundRecursive, recursiveMutations, err := recursiveLocalFolderReplacementMutationsTx(ctx, tx, RecursiveFolderMoveDeleteRecovery, r.OperationID, r.RecoveredFolderID, path.Base(r.TargetRelative))
		if err != nil {
			return err
		}
		if foundRecursive {
			if err := s.enqueueTx(ctx, tx, recursiveMutations); err != nil {
				return err
			}
			res, err = tx.ExecContext(ctx, `INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms) VALUES(?,'folder_move_deleted_recovered',?)`, r.OperationID.String(), s.clock().UTC().UnixMilli())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("recursive folder move/delete resolution unavailable")
			}
			return nil
		}
		parent := ConflictRecoveredID
		mutations := []Mutation{{OperationID: r.NewOperationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.TargetRelative)}}
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash,create_blob_hash FROM conflict_folder_move_delete_note_members WHERE operation_id=? ORDER BY note_id`, r.OperationID.String())
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
				return errors.New("corrupt folder move/delete note completion")
			}
			newOperation, e := uuid.Parse(newID)
			if e != nil {
				rows.Close()
				return e
			}
			res, e := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND mutation='create' AND object_type='note' AND parent_id=? AND name=? AND blob_hash=? AND status='pending' AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox later ON later.operation_id=d.operation_id WHERE d.dependency_operation_id=sync_outbox.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, old, note, r.FolderID.String(), name, createBlob)
			if e != nil {
				rows.Close()
				return e
			}
			if n, _ := res.RowsAffected(); n != 1 {
				rows.Close()
				return errors.New("folder move/delete note predecessor changed")
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
