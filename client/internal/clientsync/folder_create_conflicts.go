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

type ConflictFolderCreateNoteMember struct {
	NoteID, OldOperationID, NewOperationID uuid.UUID
	Name                                   string
	BlobHash                               [32]byte
}

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

func (s *Store) PendingDirectNoteCreates(ctx context.Context, rootOperationID, rootID uuid.UUID) ([]ConflictFolderCreateNoteMember, error) {
	var out []ConflictFolderCreateNoteMember
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT n.object_id,n.operation_id,n.name,n.blob_hash FROM sync_outbox_dependencies d JOIN sync_outbox n ON n.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND n.status='pending' AND n.mutation='create' AND n.object_type='note' AND n.parent_id=? ORDER BY n.sequence`, rootOperationID.String(), rootID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var object, operation string
			var blob []byte
			var m ConflictFolderCreateNoteMember
			if err := rows.Scan(&object, &operation, &m.Name, &blob); err != nil {
				return err
			}
			m.NoteID, err = uuid.Parse(object)
			if err != nil {
				return err
			}
			m.OldOperationID, err = uuid.Parse(operation)
			if err != nil || len(blob) != 32 {
				return errors.New("corrupt direct note create")
			}
			copy(m.BlobHash[:], blob)
			m.NewOperationID, err = uuid.NewV7()
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) PutConflictFolderCreateRecoveryWithNotes(ctx context.Context, r ConflictFolderCreateRecovery, members []ConflictFolderCreateNoteMember) error {
	if !validFolderCreateRecovery(r) || r.State != "prepared" || len(members) == 0 {
		return errors.New("invalid direct-note folder recovery")
	}
	rootOperationID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_create_recoveries(operation_id,source_folder_id,recovered_folder_id,source_relative,target_relative,device,inode,state) SELECT o.operation_id,o.object_id,?,?,?,?,?,? FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict' AND o.conflict_code IN ('path_collision','parent_unavailable') AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, r.RecoveredFolderID.String(), r.SourceRelative, r.TargetRelative, r.Device, r.Inode, r.State, r.OperationID.String(), r.SourceFolderID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("direct-note folder recovery unavailable")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_create_note_roots(operation_id,new_root_operation_id) VALUES(?,?)`, r.OperationID.String(), rootOperationID.String()); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		for _, m := range members {
			if !validObjectID(m.NoteID) || !validOperationID(m.OldOperationID) || !validOperationID(m.NewOperationID) || seen[m.NoteID] || naming.ValidateComponent(m.Name) != nil {
				return errors.New("invalid direct-note recovery member")
			}
			seen[m.NoteID] = true
			res, err = tx.ExecContext(ctx, `INSERT INTO conflict_folder_create_note_members(operation_id,note_id,old_operation_id,new_operation_id,name,blob_hash) SELECT ?,n.object_id,n.operation_id,?,?,n.blob_hash FROM sync_outbox n JOIN sync_outbox_dependencies d ON d.operation_id=n.operation_id WHERE n.operation_id=? AND n.object_id=? AND n.status='pending' AND n.mutation='create' AND n.object_type='note' AND n.parent_id=? AND n.name=? AND n.blob_hash=? AND d.dependency_operation_id=? AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=n.operation_id AND extra.dependency_operation_id<>?) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies child JOIN sync_outbox later ON later.operation_id=child.operation_id WHERE child.dependency_operation_id=n.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, r.OperationID.String(), m.NewOperationID.String(), m.Name, m.OldOperationID.String(), m.NoteID.String(), r.SourceFolderID.String(), m.Name, m.BlobHash[:], r.OperationID.String(), r.OperationID.String())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("direct-note recovery member unavailable")
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox n ON n.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND n.status IN ('pending','attempted','replay_mismatch','conflict')`, r.OperationID.String()).Scan(&count); err != nil {
			return err
		}
		if count != len(members) {
			return errors.New("direct-note recovery manifest incomplete")
		}
		return nil
	})
}

func (s *Store) ConflictFolderCreateNoteMembers(ctx context.Context, operationID uuid.UUID) ([]ConflictFolderCreateNoteMember, error) {
	var out []ConflictFolderCreateNoteMember
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash FROM conflict_folder_create_note_members WHERE operation_id=? ORDER BY note_id`, operationID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var note, old, newID string
			var blob []byte
			var m ConflictFolderCreateNoteMember
			if err := rows.Scan(&note, &old, &newID, &m.Name, &blob); err != nil {
				return err
			}
			m.NoteID, err = uuid.Parse(note)
			if err != nil {
				return err
			}
			m.OldOperationID, err = uuid.Parse(old)
			if err != nil {
				return err
			}
			m.NewOperationID, err = uuid.Parse(newID)
			if err != nil || len(blob) != 32 {
				return errors.New("corrupt folder recovery member")
			}
			copy(m.BlobHash[:], blob)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
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
		var rawRootOperation string
		rootErr := tx.QueryRowContext(ctx, `SELECT new_root_operation_id FROM conflict_folder_create_note_roots WHERE operation_id=?`, r.OperationID.String()).Scan(&rawRootOperation)
		if rootErr != nil && !errors.Is(rootErr, sql.ErrNoRows) {
			return rootErr
		}
		var rootOperationID uuid.UUID
		if errors.Is(rootErr, sql.ErrNoRows) {
			rootOperationID, err = uuid.NewV7()
		} else {
			rootOperationID, err = uuid.Parse(rawRootOperation)
		}
		if err != nil {
			return err
		}
		parent := ConflictRecoveredID
		mutations := []Mutation{{OperationID: rootOperationID, Kind: Create, ObjectID: r.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(r.TargetRelative)}}
		rows, err := tx.QueryContext(ctx, `SELECT note_id,old_operation_id,new_operation_id,name,blob_hash FROM conflict_folder_create_note_members WHERE operation_id=? ORDER BY note_id`, r.OperationID.String())
		if err != nil {
			return err
		}
		for rows.Next() {
			var note, old, newID, name string
			var blob []byte
			if err := rows.Scan(&note, &old, &newID, &name, &blob); err != nil {
				rows.Close()
				return err
			}
			noteID, parseErr := uuid.Parse(note)
			if parseErr != nil || len(blob) != 32 {
				rows.Close()
				return errors.New("corrupt direct-note recovery completion")
			}
			newOperation, parseErr := uuid.Parse(newID)
			if parseErr != nil {
				rows.Close()
				return parseErr
			}
			result, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND mutation='create' AND object_type='note' AND parent_id=? AND name=? AND blob_hash=? AND status='pending' AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox later ON later.operation_id=d.operation_id WHERE d.dependency_operation_id=sync_outbox.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))`, old, note, r.SourceFolderID.String(), name, blob)
			if err != nil {
				rows.Close()
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				rows.Close()
				return errors.New("direct-note recovery predecessor changed")
			}
			folder := r.RecoveredFolderID
			mutations = append(mutations, Mutation{OperationID: newOperation, Kind: Create, ObjectID: noteID, ObjectType: Note, ParentID: &folder, Name: name, BlobHash: blob, DependencyOperationID: &rootOperationID})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := s.enqueueTx(ctx, tx, mutations); err != nil {
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
