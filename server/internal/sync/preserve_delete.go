package sync

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

func preserveRequestHash(r PreserveDeleteFolderRequest) [32]byte {
	var b [64]byte
	copy(b[0:16], r.OperationID[:])
	copy(b[16:32], r.ConflictOperationID[:])
	copy(b[32:48], r.FolderID[:])
	binary.BigEndian.PutUint64(b[48:56], r.ExpectedRevision)
	copy(b[56:], []byte("presdel1"))
	return sha256.Sum256(b[:])
}

func recoveryName(ctx context.Context, tx *sql.Tx, user uuid.UUID, stem string, operation uuid.UUID) (string, string, error) {
	baseSuffix := " (Wiederhergestellt - " + operation.String() + ")"
	for attempt := 0; attempt < 1000; attempt++ {
		attemptSuffix := ""
		if attempt > 0 {
			attemptSuffix = fmt.Sprintf("-%d", attempt)
		}
		candidateStem := stem
		for len([]byte(candidateStem+baseSuffix+attemptSuffix)) > 180 {
			_, size := utf8.DecodeLastRuneInString(candidateStem)
			if size == 0 {
				return "", "", ErrInvalidInput
			}
			candidateStem = candidateStem[:len(candidateStem)-size]
		}
		candidate := candidateStem + baseSuffix + attemptSuffix
		name, key, err := normalizeName(candidate)
		if err != nil {
			continue
		}
		var occupied int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM sync_objects WHERE user_id=? AND parent_key=? AND name_key=? AND deleted=0 LIMIT 1`, user[:], ConflictRecoveredID[:], key).Scan(&occupied)
		if errors.Is(err, sql.ErrNoRows) {
			return name, key, nil
		}
		if err != nil {
			return "", "", err
		}
	}
	return "", "", ErrPreserveDeleteUnavailable
}

func (a *ActorService) PreserveAndDeleteEmptyFolder(ctx context.Context, r PreserveDeleteFolderRequest) (PreserveDeleteFolderResult, error) {
	if !validV7(r.OperationID) || !validV7(r.ConflictOperationID) || !validObjectID(r.FolderID) || r.ExpectedRevision == 0 {
		return PreserveDeleteFolderResult{}, ErrInvalidInput
	}
	hash := preserveRequestHash(r)
	tx, err := a.service.db.BeginTx(ctx, nil)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	defer tx.Rollback()
	if err := a.requireActiveActor(ctx, tx); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	var stored, recovered, storedDevice []byte
	var recoveredCursor, deletedCursor uint64
	err = tx.QueryRowContext(ctx, `SELECT request_hash,recovered_folder_id,recovered_cursor,deleted_cursor,device_id FROM sync_folder_preserve_delete_resolutions WHERE user_id=? AND resolution_operation_id=?`, a.userID[:], r.OperationID[:]).Scan(&stored, &recovered, &recoveredCursor, &deletedCursor, &storedDevice)
	if err == nil {
		if len(stored) != 32 || len(storedDevice) != 16 || subtle.ConstantTimeCompare(storedDevice, a.deviceID[:]) != 1 || subtle.ConstantTimeCompare(stored, hash[:]) != 1 {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		id, parseErr := uuid.FromBytes(recovered)
		if parseErr != nil {
			return PreserveDeleteFolderResult{}, parseErr
		}
		if err := tx.Commit(); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
		return PreserveDeleteFolderResult{id, recoveredCursor, deletedCursor}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PreserveDeleteFolderResult{}, err
	}
	var proposedType, mutation, result, conflict string
	var proposedObject []byte
	var conflictRevision sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT proposed_type,mutation,object_id,result,conflict_code,conflict_revision FROM sync_operations WHERE user_id=? AND device_id=? AND operation_id=?`, a.userID[:], a.deviceID[:], r.ConflictOperationID[:]).Scan(&proposedType, &mutation, &proposedObject, &result, &conflict, &conflictRevision); err != nil {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if proposedType != "folder" || mutation != "delete" || result != "conflict" || conflict != "base_revision_mismatch" || len(proposedObject) != 16 || subtle.ConstantTimeCompare(proposedObject, r.FolderID[:]) != 1 || !conflictRevision.Valid || uint64(conflictRevision.Int64) != r.ExpectedRevision {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	current, exists, err := loadObject(ctx, tx, a.userID, r.FolderID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if !exists || current.Deleted || current.Type != ObjectFolder || current.Revision != r.ExpectedRevision {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	var child int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sync_object_versions WHERE user_id=? AND parent_id=? LIMIT 1`, a.userID[:], r.FolderID[:]).Scan(&child)
	if err == nil {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PreserveDeleteFolderResult{}, err
	}
	if err := a.ensureConflictNamespace(ctx, tx); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	recoveredID, err := uuid.NewV7()
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	createOp, err := uuid.NewV7()
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	deleteOp, err := uuid.NewV7()
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	name, key, err := recoveryName(ctx, tx, a.userID, current.Name, r.OperationID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	now := a.service.clock.Now().UTC().UnixMilli()
	parent := ConflictRecoveredID
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_objects(user_id,object_id,object_type,revision,parent_id,parent_key,name,name_key,blob_hash,deleted,created_at_ms,updated_at_ms) VALUES(?,?, 'folder',1,?,?,?,?,NULL,0,?,?)`, a.userID[:], recoveredID[:], parent[:], parent[:], name, key, now, now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	recoveredCursor, err = allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	createIntent := canonicalIntent{Mutation: Mutation{OperationID: createOp, Kind: MutationCreate, ObjectID: recoveredID, ObjectType: ObjectFolder, ParentID: &parent, Name: name}, nameKey: key}
	createIntent, createHash, err := canonicalize(createIntent.Mutation)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if err := a.insertOperation(ctx, tx, createIntent, createHash, now, SubmitResult{Accepted: true, Revision: 1, Cursor: recoveredCursor}); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_object_versions(user_id,object_id,revision,operation_id,object_type,parent_id,name,name_key,blob_hash,deleted,created_at_ms) VALUES(?,?,1,?,'folder',?,?,?,NULL,0,?)`, a.userID[:], recoveredID[:], createOp[:], parent[:], name, key, now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_change_log(user_id,cursor,object_id,revision,operation_id,mutation,created_at_ms) VALUES(?,?,?,?,?,'create',?)`, a.userID[:], recoveredCursor, recoveredID[:], 1, createOp[:], now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	nextRevision := current.Revision + 1
	updated, err := tx.ExecContext(ctx, `UPDATE sync_objects SET revision=?,deleted=1,updated_at_ms=? WHERE user_id=? AND object_id=? AND revision=? AND deleted=0`, nextRevision, now, a.userID[:], r.FolderID[:], current.Revision)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	deletedCursor, err = allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	deleteIntent := canonicalIntent{Mutation: Mutation{OperationID: deleteOp, Kind: MutationDelete, ObjectID: r.FolderID, ObjectType: ObjectFolder, BaseRevision: current.Revision}}
	deleteIntent, deleteHash, err := canonicalize(deleteIntent.Mutation)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if err := a.insertOperation(ctx, tx, deleteIntent, deleteHash, now, SubmitResult{Accepted: true, Revision: nextRevision, Cursor: deletedCursor}); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_object_versions(user_id,object_id,revision,operation_id,object_type,parent_id,name,name_key,blob_hash,deleted,created_at_ms) VALUES(?,?,?,?,?,?,?,?,NULL,1,?)`, a.userID[:], r.FolderID[:], nextRevision, deleteOp[:], ObjectFolder, nullableUUID(current.ParentID), current.Name, current.NameKey, now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_change_log(user_id,cursor,object_id,revision,operation_id,mutation,created_at_ms) VALUES(?,?,?,?,?,'delete',?)`, a.userID[:], deletedCursor, r.FolderID[:], nextRevision, deleteOp[:], now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_cursor,deleted_cursor,status,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,'completed',?)`, a.userID[:], a.deviceID[:], r.OperationID[:], hash[:], r.ConflictOperationID[:], r.FolderID[:], r.ExpectedRevision, recoveredID[:], recoveredCursor, deletedCursor, now); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	return PreserveDeleteFolderResult{recoveredID, recoveredCursor, deletedCursor}, nil
}
