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
	version := r.Version
	if version == 0 {
		version = 1
	}
	if version == 1 {
		var b [64]byte
		copy(b[0:16], r.OperationID[:])
		copy(b[16:32], r.ConflictOperationID[:])
		copy(b[32:48], r.FolderID[:])
		binary.BigEndian.PutUint64(b[48:56], r.ExpectedRevision)
		copy(b[56:], []byte("presdel1"))
		return sha256.Sum256(b[:])
	}
	var b [80]byte
	copy(b[0:16], r.OperationID[:])
	copy(b[16:32], r.ConflictOperationID[:])
	copy(b[32:48], r.FolderID[:])
	binary.BigEndian.PutUint64(b[48:56], r.ExpectedRevision)
	binary.BigEndian.PutUint64(b[56:64], version)
	binary.BigEndian.PutUint64(b[64:72], r.KnownCursor)
	if version == 3 {
		copy(b[72:], []byte("presdel3"))
	} else {
		copy(b[72:], []byte("presdel2"))
	}
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
	version := r.Version
	if version == 0 {
		version = 1
	}
	r.Version = version
	if version == 2 || version == 3 {
		return a.preserveAndDeleteDirectChildren(ctx, r)
	}
	if version != 1 || r.KnownCursor != 0 {
		return PreserveDeleteFolderResult{}, ErrInvalidInput
	}
	return a.preserveAndDeleteEmptyFolderV1(ctx, r)
}

func (a *ActorService) preserveAndDeleteEmptyFolderV1(ctx context.Context, r PreserveDeleteFolderRequest) (PreserveDeleteFolderResult, error) {
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
		return PreserveDeleteFolderResult{RecoveredFolderID: id, RecoveredCursor: recoveredCursor, DeletedCursor: deletedCursor, FirstCursor: recoveredCursor, LastCursor: deletedCursor}, nil
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_cursor,deleted_cursor,status,created_at_ms,request_version,first_cursor,last_cursor) VALUES(?,?,?,?,?,?,?,?,?,?,'completed',?,1,?,?)`, a.userID[:], a.deviceID[:], r.OperationID[:], hash[:], r.ConflictOperationID[:], r.FolderID[:], r.ExpectedRevision, recoveredID[:], recoveredCursor, deletedCursor, now, recoveredCursor, deletedCursor); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	return PreserveDeleteFolderResult{RecoveredFolderID: recoveredID, RecoveredCursor: recoveredCursor, DeletedCursor: deletedCursor, FirstCursor: recoveredCursor, LastCursor: deletedCursor}, nil
}

func (a *ActorService) preserveAndDeleteDirectChildren(ctx context.Context, r PreserveDeleteFolderRequest) (PreserveDeleteFolderResult, error) {
	if !validV7(r.OperationID) || !validV7(r.ConflictOperationID) || !validObjectID(r.FolderID) || r.ExpectedRevision == 0 || r.KnownCursor == 0 {
		return PreserveDeleteFolderResult{}, ErrInvalidInput
	}
	hash := preserveRequestHash(r)
	tx, err := a.service.db.BeginTx(ctx, nil)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	defer tx.Rollback()
	if err = a.requireActiveActor(ctx, tx); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	var stored, recovered, device []byte
	var version, known, first, last uint64
	var status string
	var recoveredName sql.NullString
	var cloneCount, noteCount int
	err = tx.QueryRowContext(ctx, `SELECT request_hash,recovered_folder_id,recovered_folder_name,device_id,request_version,known_cursor,first_cursor,last_cursor,status,clone_count,note_count FROM sync_folder_preserve_delete_resolutions WHERE user_id=? AND resolution_operation_id=?`, a.userID[:], r.OperationID[:]).Scan(&stored, &recovered, &recoveredName, &device, &version, &known, &first, &last, &status, &cloneCount, &noteCount)
	if err == nil {
		if len(stored) != 32 || len(device) != 16 || subtle.ConstantTimeCompare(device, a.deviceID[:]) != 1 || subtle.ConstantTimeCompare(stored, hash[:]) != 1 || version != r.Version || known != r.KnownCursor || status != "completed" || (version == 3 && (!recoveredName.Valid || recoveredName.String == "")) {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		root, e := uuid.FromBytes(recovered)
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		result := PreserveDeleteFolderResult{RecoveredFolderID: root, RecoveredFolderName: recoveredName.String, RecoveredCursor: first, DeletedCursor: last, FirstCursor: first, LastCursor: last}
		rows, e := tx.QueryContext(ctx, `SELECT original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name FROM sync_folder_preserve_delete_clones WHERE user_id=? AND resolution_operation_id=? ORDER BY ordinal`, a.userID[:], r.OperationID[:])
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		defer rows.Close()
		for rows.Next() {
			var oldb, newb []byte
			var c, d uint64
			var sourceRevision sql.NullInt64
			var cloneName sql.NullString
			if e = rows.Scan(&oldb, &newb, &c, &d, &sourceRevision, &cloneName); e != nil {
				return PreserveDeleteFolderResult{}, e
			}
			oldID, e := uuid.FromBytes(oldb)
			if e != nil {
				return PreserveDeleteFolderResult{}, e
			}
			newID, e := uuid.FromBytes(newb)
			if e != nil {
				return PreserveDeleteFolderResult{}, e
			}
			if version == 3 && (!sourceRevision.Valid || sourceRevision.Int64 <= 0 || !cloneName.Valid || cloneName.String == "") {
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			result.Clones = append(result.Clones, PreserveDeleteFolderClone{OriginalFolderID: oldID, RecoveredFolderID: newID, CreateCursor: c, DeleteCursor: d, SourceRevision: uint64(sourceRevision.Int64), Name: cloneName.String})
		}
		if e = rows.Err(); e != nil || len(result.Clones) != cloneCount {
			if e != nil {
				return PreserveDeleteFolderResult{}, e
			}
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		noteRows, e := tx.QueryContext(ctx, `SELECT note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash FROM sync_folder_preserve_delete_note_moves WHERE user_id=? AND resolution_operation_id=? ORDER BY ordinal`, a.userID[:], r.OperationID[:])
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		for noteRows.Next() {
			var idb, sourceb, targetb, hash []byte
			var item PreserveDeleteNoteMove
			if e = noteRows.Scan(&idb, &item.MoveCursor, &item.SourceRevision, &item.TargetRevision, &sourceb, &targetb, &item.Name, &hash); e != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			if item.NoteID, e = uuid.FromBytes(idb); e != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			if item.SourceParentID, e = uuid.FromBytes(sourceb); e != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			if item.TargetParentID, e = uuid.FromBytes(targetb); e != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			item.BlobHash = append([]byte(nil), hash...)
			result.NoteMoves = append(result.NoteMoves, item)
		}
		if e = noteRows.Close(); e != nil || len(result.NoteMoves) != noteCount {
			if e != nil {
				return PreserveDeleteFolderResult{}, e
			}
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		if e = tx.Commit(); e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PreserveDeleteFolderResult{}, err
	}
	var typ, mutation, resultCode, conflict string
	var object []byte
	var conflictRevision sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT proposed_type,mutation,object_id,result,conflict_code,conflict_revision FROM sync_operations WHERE user_id=? AND device_id=? AND operation_id=?`, a.userID[:], a.deviceID[:], r.ConflictOperationID[:]).Scan(&typ, &mutation, &object, &resultCode, &conflict, &conflictRevision); err != nil {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if typ != "folder" || mutation != "delete" || resultCode != "conflict" || conflict != "base_revision_mismatch" || len(object) != 16 || subtle.ConstantTimeCompare(object, r.FolderID[:]) != 1 || !conflictRevision.Valid || uint64(conflictRevision.Int64) != r.ExpectedRevision {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	root, exists, err := loadObject(ctx, tx, a.userID, r.FolderID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if !exists || root.Deleted || root.Type != ObjectFolder || root.Revision != r.ExpectedRevision {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	var serverFrontier, canonicalCursor uint64
	if err = tx.QueryRowContext(ctx, `SELECT last_cursor FROM user_cursor_counters WHERE user_id=?`, a.userID[:]).Scan(&serverFrontier); err != nil || r.KnownCursor > serverFrontier {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if err = tx.QueryRowContext(ctx, `SELECT l.cursor FROM sync_object_versions v JOIN sync_change_log l ON l.user_id=v.user_id AND l.operation_id=v.operation_id WHERE v.user_id=? AND v.object_id=? AND v.revision=? AND l.mutation='move'`, a.userID[:], r.FolderID[:], r.ExpectedRevision).Scan(&canonicalCursor); err != nil || canonicalCursor > r.KnownCursor {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	var late int
	err = tx.QueryRowContext(ctx, `WITH RECURSIVE subtree(object_id) AS (SELECT ? UNION SELECT DISTINCT v.object_id FROM sync_object_versions v JOIN subtree p ON v.parent_id=p.object_id WHERE v.user_id=?) SELECT 1 FROM sync_object_versions v JOIN subtree s ON s.object_id=v.object_id JOIN sync_change_log l ON l.user_id=v.user_id AND l.operation_id=v.operation_id WHERE v.user_id=? AND l.cursor>? LIMIT 1`, r.FolderID[:], a.userID[:], a.userID[:], r.KnownCursor).Scan(&late)
	if err == nil {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PreserveDeleteFolderResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT object_id FROM sync_objects WHERE user_id=? AND parent_id=? AND deleted=0 ORDER BY object_id`, a.userID[:], r.FolderID[:])
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	var children, notes []objectState
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return PreserveDeleteFolderResult{}, err
		}
		id, e := uuid.FromBytes(b)
		if e != nil {
			rows.Close()
			return PreserveDeleteFolderResult{}, e
		}
		child, ok, e := loadObject(ctx, tx, a.userID, id)
		if e != nil || !ok {
			rows.Close()
			return PreserveDeleteFolderResult{}, e
		}
		switch child.Type {
		case ObjectFolder:
			var nested int
			e = tx.QueryRowContext(ctx, `SELECT 1 FROM sync_objects WHERE user_id=? AND parent_id=? AND deleted=0 LIMIT 1`, a.userID[:], id[:]).Scan(&nested)
			if e == nil {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			if !errors.Is(e, sql.ErrNoRows) {
				rows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			children = append(children, child)
		case ObjectNote:
			if r.Version != 3 || len(child.BlobHash) != sha256.Size {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			var available int
			e = tx.QueryRowContext(ctx, `SELECT b.available FROM content_blobs b JOIN user_content_blobs ub ON ub.hash=b.hash WHERE ub.user_id=? AND b.hash=?`, a.userID[:], child.BlobHash).Scan(&available)
			if errors.Is(e, sql.ErrNoRows) || available != 1 {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			if e != nil {
				rows.Close()
				return PreserveDeleteFolderResult{}, e
			}
			notes = append(notes, child)
		default:
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		if len(children)+len(notes) > 10000 {
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
	}
	if err = rows.Close(); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if err = a.ensureConflictNamespace(ctx, tx); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	now := a.service.clock.Now().UTC().UnixMilli()
	rootClone, err := uuid.NewV7()
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	name, key, err := recoveryName(ctx, tx, a.userID, root.Name, r.OperationID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	rootCreate, err := a.createPreservedFolder(ctx, tx, rootClone, ConflictRecoveredID, name, key, now)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	first = rootCreate
	clones := make([]PreserveDeleteFolderClone, len(children))
	for i, child := range children {
		newID, e := uuid.NewV7()
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		cursor, e := a.createPreservedFolder(ctx, tx, newID, rootClone, child.Name, child.NameKey, now)
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		clones[i] = PreserveDeleteFolderClone{OriginalFolderID: child.ID, RecoveredFolderID: newID, CreateCursor: cursor, SourceRevision: child.Revision, Name: child.Name}
	}
	noteMoves := make([]PreserveDeleteNoteMove, len(notes))
	for i, note := range notes {
		cursor, e := a.movePreservedNote(ctx, tx, note, rootClone, now)
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		noteMoves[i] = PreserveDeleteNoteMove{NoteID: note.ID, SourceParentID: r.FolderID, TargetParentID: rootClone, MoveCursor: cursor, SourceRevision: note.Revision, TargetRevision: note.Revision + 1, Name: note.Name, BlobHash: append([]byte(nil), note.BlobHash...)}
	}
	for i, child := range children {
		cursor, e := a.deletePreservedFolder(ctx, tx, child, now)
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		clones[i].DeleteCursor = cursor
	}
	last, err = a.deletePreservedFolder(ctx, tx, root, now)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	status = "completed"
	if r.Version == 3 {
		status = "preparing"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_folder_name,recovered_cursor,deleted_cursor,status,created_at_ms,request_version,known_cursor,first_cursor,last_cursor,clone_count,note_count) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.userID[:], a.deviceID[:], r.OperationID[:], hash[:], r.ConflictOperationID[:], r.FolderID[:], r.ExpectedRevision, rootClone[:], name, first, last, status, now, r.Version, r.KnownCursor, first, last, len(clones), len(noteMoves)); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	for i, clone := range clones {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_clones(user_id,resolution_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name) VALUES(?,?,?,?,?,?,?,?,?)`, a.userID[:], r.OperationID[:], i, clone.OriginalFolderID[:], clone.RecoveredFolderID[:], clone.CreateCursor, clone.DeleteCursor, clone.SourceRevision, clone.Name); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
	}
	for i, note := range noteMoves {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_note_moves(user_id,resolution_operation_id,ordinal,note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.userID[:], r.OperationID[:], i, note.NoteID[:], note.MoveCursor, note.SourceRevision, note.TargetRevision, note.SourceParentID[:], note.TargetParentID[:], note.Name, note.BlobHash); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
	}
	if r.Version == 3 {
		result, e := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET status='completed' WHERE user_id=? AND resolution_operation_id=? AND status='preparing'`, a.userID[:], r.OperationID[:])
		if e != nil {
			return PreserveDeleteFolderResult{}, e
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
	}
	if err = tx.Commit(); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	return PreserveDeleteFolderResult{RecoveredFolderID: rootClone, RecoveredFolderName: name, RecoveredCursor: first, DeletedCursor: last, FirstCursor: first, LastCursor: last, Clones: clones, NoteMoves: noteMoves}, nil
}

func (a *ActorService) createPreservedFolder(ctx context.Context, tx *sql.Tx, id, parent uuid.UUID, name, key string, now int64) (uint64, error) {
	op, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_objects(user_id,object_id,object_type,revision,parent_id,parent_key,name,name_key,blob_hash,deleted,created_at_ms,updated_at_ms) VALUES(?,?,'folder',1,?,?,?,?,NULL,0,?,?)`, a.userID[:], id[:], parent[:], parent[:], name, key, now, now); err != nil {
		return 0, err
	}
	cursor, err := allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return 0, err
	}
	intent, hash, err := canonicalize(Mutation{OperationID: op, Kind: MutationCreate, ObjectID: id, ObjectType: ObjectFolder, ParentID: &parent, Name: name})
	if err != nil {
		return 0, err
	}
	if err = a.insertOperation(ctx, tx, intent, hash, now, SubmitResult{Accepted: true, Revision: 1, Cursor: cursor}); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_object_versions(user_id,object_id,revision,operation_id,object_type,parent_id,name,name_key,blob_hash,deleted,created_at_ms) VALUES(?,?,1,?,'folder',?,?,?,NULL,0,?)`, a.userID[:], id[:], op[:], parent[:], name, key, now); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_change_log(user_id,cursor,object_id,revision,operation_id,mutation,created_at_ms) VALUES(?,?,?,?,?,'create',?)`, a.userID[:], cursor, id[:], 1, op[:], now)
	return cursor, err
}
func (a *ActorService) deletePreservedFolder(ctx context.Context, tx *sql.Tx, current objectState, now int64) (uint64, error) {
	op, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	revision := current.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE sync_objects SET revision=?,deleted=1,updated_at_ms=? WHERE user_id=? AND object_id=? AND revision=? AND deleted=0`, revision, now, a.userID[:], current.ID[:], current.Revision)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return 0, ErrPreserveDeleteUnavailable
	}
	cursor, err := allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return 0, err
	}
	intent, hash, err := canonicalize(Mutation{OperationID: op, Kind: MutationDelete, ObjectID: current.ID, ObjectType: ObjectFolder, BaseRevision: current.Revision})
	if err != nil {
		return 0, err
	}
	if err = a.insertOperation(ctx, tx, intent, hash, now, SubmitResult{Accepted: true, Revision: revision, Cursor: cursor}); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_object_versions(user_id,object_id,revision,operation_id,object_type,parent_id,name,name_key,blob_hash,deleted,created_at_ms) VALUES(?,?,?,?,?,?,?,?,NULL,1,?)`, a.userID[:], current.ID[:], revision, op[:], ObjectFolder, nullableUUID(current.ParentID), current.Name, current.NameKey, now); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_change_log(user_id,cursor,object_id,revision,operation_id,mutation,created_at_ms) VALUES(?,?,?,?,?,'delete',?)`, a.userID[:], cursor, current.ID[:], revision, op[:], now)
	return cursor, err
}

func (a *ActorService) movePreservedNote(ctx context.Context, tx *sql.Tx, current objectState, parent uuid.UUID, now int64) (uint64, error) {
	op, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	revision := current.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE sync_objects SET revision=?,parent_id=?,parent_key=?,updated_at_ms=? WHERE user_id=? AND object_id=? AND revision=? AND deleted=0 AND object_type='note'`, revision, parent[:], parent[:], now, a.userID[:], current.ID[:], current.Revision)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return 0, ErrPreserveDeleteUnavailable
	}
	cursor, err := allocateCursor(ctx, tx, a.userID)
	if err != nil {
		return 0, err
	}
	intent, hash, err := canonicalize(Mutation{OperationID: op, Kind: MutationMove, ObjectID: current.ID, ObjectType: ObjectNote, BaseRevision: current.Revision, ParentID: &parent, Name: current.Name})
	if err != nil {
		return 0, err
	}
	if err = a.insertOperation(ctx, tx, intent, hash, now, SubmitResult{Accepted: true, Revision: revision, Cursor: cursor}); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_object_versions(user_id,object_id,revision,operation_id,object_type,parent_id,name,name_key,blob_hash,deleted,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,0,?)`, a.userID[:], current.ID[:], revision, op[:], ObjectNote, parent[:], current.Name, current.NameKey, current.BlobHash, now); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_change_log(user_id,cursor,object_id,revision,operation_id,mutation,created_at_ms) VALUES(?,?,?,?,?,'move',?)`, a.userID[:], cursor, current.ID[:], revision, op[:], now)
	return cursor, err
}
