package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
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
	switch version {
	case 3:
		copy(b[72:], []byte("presdel3"))
	case 4:
		copy(b[72:], []byte("presdel4"))
	default:
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
	if version == 4 {
		return a.preserveAndDeleteRecursive(ctx, r)
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

type recursivePreserveNode struct {
	state objectState
	depth uint64
}

func (a *ActorService) preserveAndDeleteRecursive(ctx context.Context, r PreserveDeleteFolderRequest) (PreserveDeleteFolderResult, error) {
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
		if len(stored) != sha256.Size || len(device) != 16 || subtle.ConstantTimeCompare(device, a.deviceID[:]) != 1 || subtle.ConstantTimeCompare(stored, hash[:]) != 1 || version != 4 || known != r.KnownCursor || status != "completed" || !recoveredName.Valid || recoveredName.String == "" || cloneCount < 0 || noteCount < 0 || last != first+2*uint64(cloneCount)+uint64(noteCount)+1 {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		rootClone, parseErr := uuid.FromBytes(recovered)
		if parseErr != nil {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		out := PreserveDeleteFolderResult{RecoveredFolderID: rootClone, RecoveredFolderName: recoveredName.String, RecoveredCursor: first, DeletedCursor: last, FirstCursor: first, LastCursor: last}
		cloneRows, queryErr := tx.QueryContext(ctx, `SELECT original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name,source_parent_id,target_parent_id,depth FROM sync_folder_preserve_delete_clones WHERE user_id=? AND resolution_operation_id=? ORDER BY ordinal`, a.userID[:], r.OperationID[:])
		if queryErr != nil {
			return PreserveDeleteFolderResult{}, queryErr
		}
		for cloneRows.Next() {
			var original, cloneID, sourceParent, targetParent []byte
			var item PreserveDeleteFolderClone
			if queryErr = cloneRows.Scan(&original, &cloneID, &item.CreateCursor, &item.DeleteCursor, &item.SourceRevision, &item.Name, &sourceParent, &targetParent, &item.Depth); queryErr != nil {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, queryErr
			}
			if item.OriginalFolderID, queryErr = uuid.FromBytes(original); queryErr != nil {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.RecoveredFolderID, queryErr = uuid.FromBytes(cloneID); queryErr != nil {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.SourceParentID, queryErr = uuid.FromBytes(sourceParent); queryErr != nil {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.TargetParentID, queryErr = uuid.FromBytes(targetParent); queryErr != nil {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.SourceRevision == 0 || item.Name == "" || item.Depth == 0 || item.Depth > 256 || item.CreateCursor != first+1+uint64(len(out.Clones)) || item.DeleteCursor <= first+uint64(cloneCount)+uint64(noteCount) || item.DeleteCursor >= last {
				cloneRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			out.Clones = append(out.Clones, item)
		}
		if queryErr = cloneRows.Err(); queryErr != nil {
			cloneRows.Close()
			return PreserveDeleteFolderResult{}, queryErr
		}
		if queryErr = cloneRows.Close(); queryErr != nil {
			return PreserveDeleteFolderResult{}, queryErr
		}
		if len(out.Clones) != cloneCount {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		mapped := map[uuid.UUID]uuid.UUID{r.FolderID: rootClone}
		depths := map[uuid.UUID]uint64{r.FolderID: 0}
		deleteCursors := map[uuid.UUID]uint64{r.FolderID: last}
		for _, item := range out.Clones {
			parentClone, ok := mapped[item.SourceParentID]
			if !ok || parentClone != item.TargetParentID || depths[item.SourceParentID]+1 != item.Depth {
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			mapped[item.OriginalFolderID] = item.RecoveredFolderID
			depths[item.OriginalFolderID] = item.Depth
			deleteCursors[item.OriginalFolderID] = item.DeleteCursor
		}
		for _, item := range out.Clones {
			if item.DeleteCursor >= deleteCursors[item.SourceParentID] {
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
		}
		noteRows, queryErr := tx.QueryContext(ctx, `SELECT note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash FROM sync_folder_preserve_delete_note_moves WHERE user_id=? AND resolution_operation_id=? ORDER BY ordinal`, a.userID[:], r.OperationID[:])
		if queryErr != nil {
			return PreserveDeleteFolderResult{}, queryErr
		}
		for noteRows.Next() {
			var noteID, sourceParent, targetParent, blob []byte
			var item PreserveDeleteNoteMove
			if queryErr = noteRows.Scan(&noteID, &item.MoveCursor, &item.SourceRevision, &item.TargetRevision, &sourceParent, &targetParent, &item.Name, &blob); queryErr != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, queryErr
			}
			if item.NoteID, queryErr = uuid.FromBytes(noteID); queryErr != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.SourceParentID, queryErr = uuid.FromBytes(sourceParent); queryErr != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			if item.TargetParentID, queryErr = uuid.FromBytes(targetParent); queryErr != nil {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			item.BlobHash = append([]byte(nil), blob...)
			if item.MoveCursor != first+1+uint64(cloneCount)+uint64(len(out.NoteMoves)) || item.TargetRevision != item.SourceRevision+1 || item.SourceRevision == 0 || item.Name == "" || len(item.BlobHash) != sha256.Size || mapped[item.SourceParentID] != item.TargetParentID {
				noteRows.Close()
				return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
			}
			out.NoteMoves = append(out.NoteMoves, item)
		}
		if queryErr = noteRows.Err(); queryErr != nil {
			noteRows.Close()
			return PreserveDeleteFolderResult{}, queryErr
		}
		if queryErr = noteRows.Close(); queryErr != nil {
			return PreserveDeleteFolderResult{}, queryErr
		}
		if len(out.NoteMoves) != noteCount {
			return PreserveDeleteFolderResult{}, ErrOperationReplayMismatch
		}
		if err = tx.Commit(); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
		return out, nil
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
	var serverFrontier uint64
	if err = tx.QueryRowContext(ctx, `SELECT last_cursor FROM user_cursor_counters WHERE user_id=?`, a.userID[:]).Scan(&serverFrontier); err != nil || r.KnownCursor > serverFrontier {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}

	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(object_id,depth,path,cycle) AS (
		SELECT object_id,0,','||hex(object_id)||',',0 FROM sync_objects WHERE user_id=? AND object_id=? AND deleted=0
		UNION ALL
		SELECT o.object_id,t.depth+1,t.path||hex(o.object_id)||',',instr(t.path,','||hex(o.object_id)||',')>0
		FROM sync_objects o JOIN tree t ON o.parent_id=t.object_id
		WHERE o.user_id=? AND o.deleted=0 AND t.cycle=0 AND t.depth<257
	)
	SELECT o.object_id,o.object_type,o.revision,o.parent_id,o.name,o.name_key,o.blob_hash,t.depth,t.cycle,
	       CASE WHEN ub.hash IS NULL THEN 0 ELSE 1 END,COALESCE(b.available,0)
	FROM tree t JOIN sync_objects o ON o.user_id=? AND o.object_id=t.object_id
	LEFT JOIN user_content_blobs ub ON ub.user_id=o.user_id AND ub.hash=o.blob_hash
	LEFT JOIN content_blobs b ON b.hash=o.blob_hash
	ORDER BY t.depth,o.object_id LIMIT 10001`, a.userID[:], r.FolderID[:], a.userID[:], a.userID[:])
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	var folders, notes []recursivePreserveNode
	for rows.Next() {
		var idBytes, parentBytes, blob []byte
		var node recursivePreserveNode
		var cycle, entitled, available int
		if err = rows.Scan(&idBytes, &node.state.Type, &node.state.Revision, &parentBytes, &node.state.Name, &node.state.NameKey, &blob, &node.depth, &cycle, &entitled, &available); err != nil {
			rows.Close()
			return PreserveDeleteFolderResult{}, err
		}
		if node.state.ID, err = uuid.FromBytes(idBytes); err != nil {
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		if len(parentBytes) == 16 {
			parent, parseErr := uuid.FromBytes(parentBytes)
			if parseErr != nil {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			node.state.ParentID = &parent
		} else if len(parentBytes) != 0 {
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		node.state.BlobHash = append([]byte(nil), blob...)
		if cycle != 0 || node.depth > 256 || node.state.Revision == 0 || node.state.Name == "" || node.state.NameKey == "" {
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		switch node.state.Type {
		case ObjectFolder:
			if len(node.state.BlobHash) != 0 {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			folders = append(folders, node)
		case ObjectNote:
			if len(node.state.BlobHash) != sha256.Size || entitled != 1 || available != 1 {
				rows.Close()
				return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
			}
			notes = append(notes, node)
		default:
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		if len(folders)+len(notes) > 10000 {
			rows.Close()
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return PreserveDeleteFolderResult{}, err
	}
	if err = rows.Close(); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if len(folders) == 0 || folders[0].state.ID != r.FolderID || folders[0].depth != 0 || folders[0].state.Revision != r.ExpectedRevision {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	folderByID := make(map[uuid.UUID]recursivePreserveNode, len(folders))
	for _, folder := range folders {
		folderByID[folder.state.ID] = folder
	}
	for _, folder := range folders[1:] {
		if folder.state.ParentID == nil {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		parent, ok := folderByID[*folder.state.ParentID]
		if !ok || parent.depth+1 != folder.depth {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
	}
	for _, note := range notes {
		if note.state.ParentID == nil {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		parent, ok := folderByID[*note.state.ParentID]
		if !ok || parent.depth+1 != note.depth {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
	}
	var late int
	err = tx.QueryRowContext(ctx, `WITH RECURSIVE
		ranked(object_id,parent_id,deleted,rank) AS (
			SELECT v.object_id,v.parent_id,v.deleted,ROW_NUMBER() OVER(PARTITION BY v.object_id ORDER BY l.cursor DESC)
			FROM sync_object_versions v JOIN sync_change_log l ON l.user_id=v.user_id AND l.operation_id=v.operation_id
			WHERE v.user_id=? AND l.cursor<=?
		),
		frontier(object_id,parent_id,deleted) AS (
			SELECT object_id,parent_id,deleted FROM ranked WHERE rank=1
		),
		frontier_subtree(object_id) AS (
			SELECT object_id FROM frontier WHERE object_id=? AND deleted=0
			UNION
			SELECT f.object_id FROM frontier f JOIN frontier_subtree p ON f.parent_id=p.object_id WHERE f.deleted=0
		),
		current_subtree(object_id) AS (
			SELECT object_id FROM sync_objects WHERE user_id=? AND object_id=? AND deleted=0
			UNION
			SELECT o.object_id FROM sync_objects o JOIN current_subtree p ON o.parent_id=p.object_id WHERE o.user_id=? AND o.deleted=0
		),
		post_attached(object_id) AS (
			SELECT v.object_id
			FROM sync_object_versions v JOIN sync_change_log l ON l.user_id=v.user_id AND l.operation_id=v.operation_id
			JOIN frontier_subtree p ON p.object_id=v.parent_id
			WHERE v.user_id=? AND l.cursor>?
			UNION
			SELECT v.object_id
			FROM sync_object_versions v JOIN sync_change_log l ON l.user_id=v.user_id AND l.operation_id=v.operation_id
			JOIN post_attached p ON p.object_id=v.parent_id
			WHERE v.user_id=? AND l.cursor>?
		),
		candidates(object_id) AS (
			SELECT object_id FROM frontier_subtree
			UNION SELECT object_id FROM current_subtree
			UNION SELECT object_id FROM post_attached
		)
	SELECT 1 FROM candidates s JOIN sync_change_log l ON l.user_id=? AND l.object_id=s.object_id WHERE l.cursor>? LIMIT 1`, a.userID[:], r.KnownCursor, r.FolderID[:], a.userID[:], r.FolderID[:], a.userID[:], a.userID[:], r.KnownCursor, a.userID[:], r.KnownCursor, a.userID[:], r.KnownCursor).Scan(&late)
	if err == nil {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PreserveDeleteFolderResult{}, err
	}

	sort.Slice(folders[1:], func(i, j int) bool {
		left, right := folders[i+1], folders[j+1]
		if left.depth != right.depth {
			return left.depth < right.depth
		}
		return bytes.Compare(left.state.ID[:], right.state.ID[:]) < 0
	})
	folderOrder := make(map[uuid.UUID]int, len(folders))
	for i, folder := range folders {
		folderOrder[folder.state.ID] = i
	}
	sort.Slice(notes, func(i, j int) bool {
		leftParent, rightParent := folderOrder[*notes[i].state.ParentID], folderOrder[*notes[j].state.ParentID]
		if leftParent != rightParent {
			return leftParent < rightParent
		}
		return bytes.Compare(notes[i].state.ID[:], notes[j].state.ID[:]) < 0
	})

	if err = a.ensureConflictNamespace(ctx, tx); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	now := a.service.clock.Now().UTC().UnixMilli()
	rootClone, err := uuid.NewV7()
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	name, recoveredKey, err := recoveryName(ctx, tx, a.userID, folders[0].state.Name, r.OperationID)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	first, err = a.createPreservedFolder(ctx, tx, rootClone, ConflictRecoveredID, name, recoveredKey, now)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	mapped := map[uuid.UUID]uuid.UUID{r.FolderID: rootClone}
	clones := make([]PreserveDeleteFolderClone, 0, len(folders)-1)
	for _, folder := range folders[1:] {
		cloneID, createErr := uuid.NewV7()
		if createErr != nil {
			return PreserveDeleteFolderResult{}, createErr
		}
		sourceParent := *folder.state.ParentID
		targetParent, ok := mapped[sourceParent]
		if !ok {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		createCursor, createErr := a.createPreservedFolder(ctx, tx, cloneID, targetParent, folder.state.Name, folder.state.NameKey, now)
		if createErr != nil {
			return PreserveDeleteFolderResult{}, createErr
		}
		mapped[folder.state.ID] = cloneID
		clones = append(clones, PreserveDeleteFolderClone{OriginalFolderID: folder.state.ID, RecoveredFolderID: cloneID, SourceParentID: sourceParent, TargetParentID: targetParent, CreateCursor: createCursor, SourceRevision: folder.state.Revision, Depth: folder.depth, Name: folder.state.Name})
	}
	noteMoves := make([]PreserveDeleteNoteMove, 0, len(notes))
	for _, note := range notes {
		sourceParent := *note.state.ParentID
		targetParent, ok := mapped[sourceParent]
		if !ok {
			return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
		}
		moveCursor, moveErr := a.movePreservedNote(ctx, tx, note.state, targetParent, now)
		if moveErr != nil {
			return PreserveDeleteFolderResult{}, moveErr
		}
		noteMoves = append(noteMoves, PreserveDeleteNoteMove{NoteID: note.state.ID, SourceParentID: sourceParent, TargetParentID: targetParent, MoveCursor: moveCursor, SourceRevision: note.state.Revision, TargetRevision: note.state.Revision + 1, Name: note.state.Name, BlobHash: append([]byte(nil), note.state.BlobHash...)})
	}
	deleteOrder := append([]recursivePreserveNode(nil), folders[1:]...)
	sort.Slice(deleteOrder, func(i, j int) bool {
		if deleteOrder[i].depth != deleteOrder[j].depth {
			return deleteOrder[i].depth > deleteOrder[j].depth
		}
		return bytes.Compare(deleteOrder[i].state.ID[:], deleteOrder[j].state.ID[:]) < 0
	})
	cloneByOriginal := make(map[uuid.UUID]int, len(clones))
	for i := range clones {
		cloneByOriginal[clones[i].OriginalFolderID] = i
	}
	for _, folder := range deleteOrder {
		deleteCursor, deleteErr := a.deletePreservedFolder(ctx, tx, folder.state, now)
		if deleteErr != nil {
			return PreserveDeleteFolderResult{}, deleteErr
		}
		clones[cloneByOriginal[folder.state.ID]].DeleteCursor = deleteCursor
	}
	last, err = a.deletePreservedFolder(ctx, tx, folders[0].state, now)
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if last != first+2*uint64(len(clones))+uint64(len(noteMoves))+1 {
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(user_id,device_id,resolution_operation_id,request_hash,conflict_operation_id,folder_id,expected_revision,recovered_folder_id,recovered_folder_name,recovered_cursor,deleted_cursor,status,created_at_ms,request_version,known_cursor,first_cursor,last_cursor,clone_count,note_count) VALUES(?,?,?,?,?,?,?,?,?, ?,?,'preparing',?,4,?,?,?,?,?)`, a.userID[:], a.deviceID[:], r.OperationID[:], hash[:], r.ConflictOperationID[:], r.FolderID[:], r.ExpectedRevision, rootClone[:], name, first, last, now, r.KnownCursor, first, last, len(clones), len(noteMoves)); err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	for i, clone := range clones {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_clones(user_id,resolution_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name,source_parent_id,target_parent_id,depth) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, a.userID[:], r.OperationID[:], i, clone.OriginalFolderID[:], clone.RecoveredFolderID[:], clone.CreateCursor, clone.DeleteCursor, clone.SourceRevision, clone.Name, clone.SourceParentID[:], clone.TargetParentID[:], clone.Depth); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
	}
	for i, note := range noteMoves {
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_note_moves(user_id,resolution_operation_id,ordinal,note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.userID[:], r.OperationID[:], i, note.NoteID[:], note.MoveCursor, note.SourceRevision, note.TargetRevision, note.SourceParentID[:], note.TargetParentID[:], note.Name, note.BlobHash); err != nil {
			return PreserveDeleteFolderResult{}, err
		}
	}
	completed, err := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET status='completed' WHERE user_id=? AND resolution_operation_id=? AND status='preparing'`, a.userID[:], r.OperationID[:])
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	if affected, affectedErr := completed.RowsAffected(); affectedErr != nil || affected != 1 {
		if affectedErr != nil {
			return PreserveDeleteFolderResult{}, affectedErr
		}
		return PreserveDeleteFolderResult{}, ErrPreserveDeleteUnavailable
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
