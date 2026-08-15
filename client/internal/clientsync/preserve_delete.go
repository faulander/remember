package clientsync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
	"math"
)

type FolderPreserveDeleteClone struct {
	OriginalFolderID, RecoveredFolderID, SourceParentID, TargetParentID uuid.UUID
	CreateCursor, DeleteCursor, SourceRevision, Depth                   uint64
	Name                                                                string
}
type FolderPreserveDeleteNoteMove struct {
	NoteID, SourceParentID, TargetParentID     uuid.UUID
	MoveCursor, SourceRevision, TargetRevision uint64
	Name                                       string
	BlobHash                                   []byte
}
type FolderPreserveDeleteResolution struct {
	ConflictOperationID, ResolutionOperationID, FolderID, RecoveredFolderID uuid.UUID
	ExpectedRevision, KnownCursor, FirstCursor, LastCursor, RequestVersion  uint64
	RecoveredFolderName                                                     string
	State                                                                   string
	Clones                                                                  []FolderPreserveDeleteClone
	NoteMoves                                                               []FolderPreserveDeleteNoteMove
}

func (s *Store) PrepareFolderPreserveDelete(ctx context.Context, conflict, resolution uuid.UUID, known uint64) (*FolderPreserveDeleteResolution, error) {
	if !validOperationID(conflict) || !validOperationID(resolution) || known > math.MaxInt64 {
		return nil, errors.New("invalid preserve delete resolution")
	}
	var out FolderPreserveDeleteResolution
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var folder string
		var revision uint64
		if err := tx.QueryRowContext(ctx, `SELECT o.object_id,c.revision FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE o.operation_id=? AND o.object_type='folder' AND o.mutation='delete' AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND c.object_type='folder' AND c.deleted=0`, conflict.String()).Scan(&folder, &revision); err != nil {
			return err
		}
		id, err := uuid.Parse(folder)
		if err != nil {
			return err
		}
		version := uint64(1)
		if known > 0 {
			version = 4
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_resolutions(conflict_operation_id,resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor) VALUES(?,?,?,?,'prepared',?,?)`, conflict.String(), resolution.String(), folder, revision, version, nullablePositiveInt(known))
		if err != nil {
			var resolutionText, existingFolder, state string
			var existingRevision, existingVersion, first, last uint64
			var existingKnown sql.NullInt64
			var recovered, recoveredName sql.NullString
			if scanErr := tx.QueryRowContext(ctx, `SELECT resolution_operation_id,folder_id,expected_revision,state,request_version,known_cursor,COALESCE(first_cursor,0),COALESCE(last_cursor,0),recovered_folder_id,recovered_folder_name FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=?`, conflict.String()).Scan(&resolutionText, &existingFolder, &existingRevision, &state, &existingVersion, &existingKnown, &first, &last, &recovered, &recoveredName); scanErr != nil || existingFolder != folder || existingRevision != revision {
				return err
			}
			persisted, parseErr := uuid.Parse(resolutionText)
			if parseErr != nil {
				return parseErr
			}
			out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: persisted, FolderID: id, ExpectedRevision: revision, KnownCursor: uint64(existingKnown.Int64), RequestVersion: existingVersion, FirstCursor: first, LastCursor: last, State: state}
			if recovered.Valid {
				out.RecoveredFolderID, _ = uuid.Parse(recovered.String)
			}
			if recoveredName.Valid {
				out.RecoveredFolderName = recoveredName.String
			}
			return nil
		}
		out = FolderPreserveDeleteResolution{ConflictOperationID: conflict, ResolutionOperationID: resolution, FolderID: id, ExpectedRevision: revision, KnownCursor: known, RequestVersion: version, State: "prepared"}
		return nil
	})
	return &out, err
}

func (s *Store) PromotePreparedFolderPreserveDeleteV4(ctx context.Context, conflict, resolution uuid.UUID, known uint64) (*FolderPreserveDeleteResolution, error) {
	if !validOperationID(conflict) || !validOperationID(resolution) || known == 0 || known > math.MaxInt64 {
		return nil, errors.New("invalid preserve delete promotion")
	}
	var out FolderPreserveDeleteResolution
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET resolution_operation_id=?,request_version=4 WHERE conflict_operation_id=? AND state='prepared' AND request_version IN(2,3) AND known_cursor=?`, resolution.String(), conflict.String(), known)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("preserve delete promotion unavailable")
		}
		var folder string
		if err = tx.QueryRowContext(ctx, `SELECT folder_id,expected_revision FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=?`, conflict.String()).Scan(&folder, &out.ExpectedRevision); err != nil {
			return err
		}
		out.FolderID, err = uuid.Parse(folder)
		if err != nil {
			return err
		}
		out.ConflictOperationID = conflict
		out.ResolutionOperationID = resolution
		out.KnownCursor = known
		out.RequestVersion = 4
		out.State = "prepared"
		return nil
	})
	return &out, err
}

func nullablePositiveInt(value uint64) any {
	if value == 0 {
		return nil
	}
	return value
}

func (s *Store) KnownPreserveDeleteCursor(ctx context.Context, conflict uuid.UUID) (uint64, error) {
	var cursor uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT CAST(s.value AS INTEGER) FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_inbox_changes i ON i.object_id=o.object_id AND i.object_type='folder' AND i.mutation='move' AND i.revision=c.revision AND i.name=c.name AND i.parent_id IS c.parent_id JOIN sync_state s ON s.key='downloaded_cursor' WHERE o.operation_id=? AND i.cursor<=CAST(s.value AS INTEGER) ORDER BY i.cursor DESC LIMIT 1`, conflict.String()).Scan(&cursor)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return cursor, err
}

func nullableUUIDString(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}
func (s *Store) FolderPreserveDeleteRecoveryCreateParent(ctx context.Context, change Change) (bool, *uuid.UUID, error) {
	if change.ObjectType != Folder || change.Mutation != Create || change.Deleted || change.Revision != 1 {
		return false, nil, nil
	}
	var parent string
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT expected_parent FROM (SELECT p.recovered_folder_id object_id,p.first_cursor cursor,? expected_parent,p.request_version,p.recovered_folder_name expected_name FROM sync_folder_preserve_delete_resolutions p WHERE p.state='resolved' UNION ALL SELECT c.recovered_folder_id,c.create_cursor,CASE WHEN p.request_version=4 THEN c.target_parent_id ELSE p.recovered_folder_id END,p.request_version,c.name FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE p.state='resolved') WHERE object_id=? AND cursor=? AND (request_version<3 OR expected_name=?)`, ConflictRecoveredID.String(), change.ObjectID.String(), change.Cursor, change.Name).Scan(&parent)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	id, err := uuid.Parse(parent)
	if err != nil {
		return false, nil, err
	}
	return true, &id, nil
}
func (s *Store) FolderPreserveDeleteCanonicalMoveMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Move || change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p JOIN sync_conflict_states c ON c.operation_id=p.conflict_operation_id WHERE p.folder_id=? AND p.expected_revision=? AND p.state='resolved' AND c.object_type='folder' AND c.revision=? AND c.parent_id IS ? AND c.name=? AND c.deleted=0 AND (p.request_version=1 OR ?<=p.known_cursor))`, change.ObjectID.String(), change.Revision, change.Revision, nullableUUIDString(change.ParentID), change.Name, change.Cursor).Scan(&exists)
	})
	return exists == 1, err
}
func (s *Store) FolderPreserveDeleteMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Folder || change.Mutation != Delete || !change.Deleted {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p JOIN sync_conflict_states s ON s.operation_id=p.conflict_operation_id WHERE p.folder_id=? AND p.state='resolved' AND p.last_cursor=? AND ?=p.expected_revision+1 AND (p.request_version<3 OR (s.revision=p.expected_revision AND s.name=? AND s.parent_id IS ?)) UNION ALL SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE c.original_folder_id=? AND c.delete_cursor=? AND p.state='resolved' AND (p.request_version<3 OR (?=c.source_revision+1 AND c.name=? AND ? IS CASE WHEN p.request_version=4 THEN c.source_parent_id ELSE p.folder_id END)))`, change.ObjectID.String(), change.Cursor, change.Revision, change.Name, nullableUUIDString(change.ParentID), change.ObjectID.String(), change.Cursor, change.Revision, change.Name, nullableUUIDString(change.ParentID)).Scan(&exists)
	})
	return exists == 1, err
}
func (s *Store) FolderPreserveDeleteNoteMoveMatches(ctx context.Context, change Change) (bool, error) {
	if change.ObjectType != Note || change.Mutation != Move || change.Deleted || len(change.BlobHash) != 32 {
		return false, nil
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves n JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=n.conflict_operation_id WHERE p.state='resolved' AND n.note_id=? AND n.move_cursor=? AND n.target_revision=? AND n.target_parent_id IS ? AND n.name=? AND n.blob_hash=?)`, change.ObjectID.String(), change.Cursor, change.Revision, nullableUUIDString(change.ParentID), change.Name, change.BlobHash).Scan(&exists)
	})
	return exists == 1, err
}

func (s *Store) CompleteFolderPreserveDelete(ctx context.Context, conflict, recovered uuid.UUID, recoveredName string, first, last uint64, clones []FolderPreserveDeleteClone, notes []FolderPreserveDeleteNoteMove) error {
	if !validOperationID(conflict) || !validObjectID(recovered) || first == 0 || last < first || last > math.MaxInt64 {
		return errors.New("invalid preserve delete completion")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var version uint64
		var folderRaw string
		if err := tx.QueryRowContext(ctx, `SELECT request_version,folder_id FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=?`, conflict.String()).Scan(&version, &folderRaw); err != nil {
			return err
		}
		folder, err := uuid.Parse(folderRaw)
		if err != nil {
			return err
		}
		if err = validateFolderPreserveDeleteCompletion(version, folder, recovered, recoveredName, first, last, clones, notes); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET state='sealing',recovered_folder_id=?,recovered_folder_name=?,recovered_cursor=?,deleted_cursor=?,first_cursor=?,last_cursor=?,clone_count=?,note_count=? WHERE conflict_operation_id=? AND state='prepared'`, recovered.String(), nullableNonEmptyString(recoveredName), first, last, first, last, len(clones), len(notes), conflict.String())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return verifyFolderPreserveDeleteReplay(ctx, tx, conflict, recovered, recoveredName, first, last, clones, notes)
		}
		localDelete := func(id uuid.UUID, typ string, base uint64) (any, error) {
			rows, e := tx.QueryContext(ctx, `WITH RECURSIVE required(operation_id) AS (SELECT dependency_operation_id FROM sync_outbox_dependencies WHERE operation_id=? UNION SELECT d.dependency_operation_id FROM sync_outbox_dependencies d JOIN required r ON d.operation_id=r.operation_id) SELECT o.operation_id FROM sync_outbox o JOIN required r ON r.operation_id=o.operation_id WHERE o.object_id=? AND o.object_type=? AND o.mutation='delete' AND o.status IN('pending','attempted','replay_mismatch') AND (?=0 OR o.base_revision=?) ORDER BY o.operation_id`, conflict.String(), id.String(), typ, base, base)
			if e != nil {
				return nil, e
			}
			defer rows.Close()
			var values []string
			for rows.Next() {
				var value string
				if e = rows.Scan(&value); e != nil {
					return nil, e
				}
				values = append(values, value)
			}
			if e = rows.Err(); e != nil {
				return nil, e
			}
			if len(values) > 1 {
				return nil, errors.New("ambiguous local preserve delete intent")
			}
			if len(values) == 1 {
				return values[0], nil
			}
			return nil, nil
		}
		for i, clone := range clones {
			op, e := localDelete(clone.OriginalFolderID, "folder", clone.SourceRevision)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_clones(conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name,local_delete_operation_id,source_parent_id,target_parent_id,depth) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, conflict.String(), i, clone.OriginalFolderID.String(), clone.RecoveredFolderID.String(), clone.CreateCursor, clone.DeleteCursor, nullablePositiveInt(clone.SourceRevision), nullableNonEmptyString(clone.Name), op, nullableRecursiveUUID(version, clone.SourceParentID), nullableRecursiveUUID(version, clone.TargetParentID), nullableRecursiveDepth(version, clone.Depth)); e != nil {
				return e
			}
		}
		for i, item := range notes {
			op, e := localDelete(item.NoteID, "note", item.SourceRevision)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO sync_folder_preserve_delete_note_moves(conflict_operation_id,ordinal,note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash,local_delete_operation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, conflict.String(), i, item.NoteID.String(), item.MoveCursor, item.SourceRevision, item.TargetRevision, item.SourceParentID.String(), item.TargetParentID.String(), item.Name, item.BlobHash, op); e != nil {
				return e
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE status IN('pending','attempted','replay_mismatch') AND operation_id IN(SELECT local_delete_operation_id FROM sync_folder_preserve_delete_clones WHERE conflict_operation_id=? UNION ALL SELECT local_delete_operation_id FROM sync_folder_preserve_delete_note_moves WHERE conflict_operation_id=?)`, conflict.String(), conflict.String()); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE sync_folder_preserve_delete_resolutions SET state='resolved' WHERE conflict_operation_id=? AND state='sealing'`, conflict.String())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("preserve delete seal unavailable")
		}
		return nil
	})
}

func validateFolderPreserveDeleteCompletion(version uint64, folder, recovered uuid.UUID, recoveredName string, first, last uint64, clones []FolderPreserveDeleteClone, notes []FolderPreserveDeleteNoteMove) error {
	f, n := uint64(len(clones)), uint64(len(notes))
	if version == 4 {
		if f+n+1 > 10000 || f > (math.MaxInt64-1-n)/2 || first > math.MaxInt64-(2*f+n+1) || last != first+2*f+n+1 {
			return errors.New("invalid preserve delete span")
		}
	} else if f+n > 10000 || last != first+2*f+n+1 {
		return errors.New("invalid preserve delete span")
	}
	if version >= 3 {
		if naming.ValidateComponent(recoveredName) != nil {
			return errors.New("invalid preserve delete recovery name")
		}
	} else if recoveredName != "" {
		return errors.New("preserve delete legacy descriptor mismatch")
	}
	if version != 3 && version != 4 && len(notes) > 0 {
		return errors.New("preserve delete version mismatch")
	}
	seen := map[uuid.UUID]bool{folder: true, recovered: true}
	sourceTargets := map[uuid.UUID]uuid.UUID{folder: recovered}
	sourceDepths := map[uuid.UUID]uint64{folder: 0}
	sourceRanks := map[uuid.UUID]int{folder: 0}
	deleteCursors := make(map[uint64]bool, len(clones))
	deleteStart := first + 1 + f + n
	var previousDepth uint64
	var previousOriginal uuid.UUID
	for i, clone := range clones {
		if !validObjectID(clone.OriginalFolderID) || !validObjectID(clone.RecoveredFolderID) || seen[clone.OriginalFolderID] || seen[clone.RecoveredFolderID] || clone.CreateCursor != first+1+uint64(i) {
			return errors.New("invalid preserve delete clone")
		}
		if version == 4 {
			targetParent, parentExists := sourceTargets[clone.SourceParentID]
			parentDepth, depthExists := sourceDepths[clone.SourceParentID]
			parentDelete := last
			for _, prior := range clones[:i] {
				if prior.OriginalFolderID == clone.SourceParentID {
					parentDelete = prior.DeleteCursor
					break
				}
			}
			if !parentExists || !depthExists || clone.TargetParentID != targetParent || clone.Depth == 0 || clone.Depth > 256 || clone.Depth != parentDepth+1 || clone.DeleteCursor < deleteStart || clone.DeleteCursor >= last || deleteCursors[clone.DeleteCursor] || clone.DeleteCursor >= parentDelete || clone.SourceRevision == 0 || clone.SourceRevision > math.MaxInt64 || naming.ValidateComponent(clone.Name) != nil || i > 0 && (clone.Depth < previousDepth || clone.Depth == previousDepth && bytes.Compare(clone.OriginalFolderID[:], previousOriginal[:]) <= 0) {
				return errors.New("invalid preserve delete clone")
			}
			sourceTargets[clone.OriginalFolderID] = clone.RecoveredFolderID
			sourceDepths[clone.OriginalFolderID] = clone.Depth
			sourceRanks[clone.OriginalFolderID] = i + 1
			deleteCursors[clone.DeleteCursor] = true
			previousDepth, previousOriginal = clone.Depth, clone.OriginalFolderID
		} else {
			if clone.SourceParentID != uuid.Nil || clone.TargetParentID != uuid.Nil || clone.Depth != 0 || clone.DeleteCursor != first+1+f+n+uint64(i) {
				return errors.New("invalid preserve delete legacy clone")
			}
			if version == 3 {
				if clone.SourceRevision == 0 || naming.ValidateComponent(clone.Name) != nil {
					return errors.New("invalid preserve delete clone descriptor")
				}
			} else if clone.SourceRevision != 0 || clone.Name != "" {
				return errors.New("preserve delete legacy clone mismatch")
			}
		}
		seen[clone.OriginalFolderID], seen[clone.RecoveredFolderID] = true, true
	}
	previousParentRank := -1
	var previousNote uuid.UUID
	for i, item := range notes {
		if !validObjectID(item.NoteID) || seen[item.NoteID] || item.MoveCursor != first+1+f+uint64(i) || item.SourceRevision == 0 || item.TargetRevision != item.SourceRevision+1 || item.Name == "" || len(item.BlobHash) != 32 {
			return errors.New("invalid preserve delete note")
		}
		if version == 4 {
			targetParent, mapped := sourceTargets[item.SourceParentID]
			rank, ranked := sourceRanks[item.SourceParentID]
			if !mapped || !ranked || item.TargetParentID != targetParent || item.SourceRevision > math.MaxInt64 || item.TargetRevision > math.MaxInt64 || naming.ValidateComponent(item.Name) != nil || rank < previousParentRank || rank == previousParentRank && bytes.Compare(item.NoteID[:], previousNote[:]) <= 0 {
				return errors.New("invalid preserve delete note")
			}
			previousParentRank, previousNote = rank, item.NoteID
		} else if item.SourceParentID != folder || item.TargetParentID != recovered {
			return errors.New("invalid preserve delete note")
		}
		seen[item.NoteID] = true
	}
	return nil
}

func nullableRecursiveUUID(version uint64, id uuid.UUID) any {
	if version != 4 {
		return nil
	}
	return id.String()
}

func nullableRecursiveDepth(version, depth uint64) any {
	if version != 4 {
		return nil
	}
	return depth
}

func verifyFolderPreserveDeleteReplay(ctx context.Context, tx *sql.Tx, conflict, recovered uuid.UUID, recoveredName string, first, last uint64, clones []FolderPreserveDeleteClone, notes []FolderPreserveDeleteNoteMove) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_folder_preserve_delete_resolutions WHERE conflict_operation_id=? AND state='resolved' AND recovered_folder_id=? AND recovered_folder_name IS ? AND first_cursor=? AND last_cursor=? AND clone_count=? AND note_count=?`, conflict.String(), recovered.String(), nullableNonEmptyString(recoveredName), first, last, len(clones), len(notes)).Scan(&count); err != nil || count != 1 {
		return errors.New("preserve delete completion unavailable")
	}
	rows, err := tx.QueryContext(ctx, `SELECT original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name,source_parent_id,target_parent_id,depth FROM sync_folder_preserve_delete_clones WHERE conflict_operation_id=? ORDER BY ordinal`, conflict.String())
	if err != nil {
		return err
	}
	i := 0
	for rows.Next() {
		if i >= len(clones) {
			rows.Close()
			return errors.New("preserve delete clone replay mismatch")
		}
		var a, b string
		var c, d uint64
		var sourceRevision, depth sql.NullInt64
		var name, sourceParent, targetParent sql.NullString
		if err = rows.Scan(&a, &b, &c, &d, &sourceRevision, &name, &sourceParent, &targetParent, &depth); err != nil {
			rows.Close()
			return err
		}
		x := clones[i]
		recursive := x.SourceParentID != uuid.Nil
		if a != x.OriginalFolderID.String() || b != x.RecoveredFolderID.String() || c != x.CreateCursor || d != x.DeleteCursor || uint64(sourceRevision.Int64) != x.SourceRevision || name.String != x.Name || sourceParent.Valid != recursive || targetParent.Valid != recursive || depth.Valid != recursive || recursive && (sourceParent.String != x.SourceParentID.String() || targetParent.String != x.TargetParentID.String() || uint64(depth.Int64) != x.Depth) {
			rows.Close()
			return errors.New("preserve delete clone replay mismatch")
		}
		i++
	}
	if err = rows.Close(); err != nil || i != len(clones) {
		return errors.New("preserve delete clone replay mismatch")
	}
	rows, err = tx.QueryContext(ctx, `SELECT note_id,move_cursor,source_revision,target_revision,source_parent_id,target_parent_id,name,blob_hash FROM sync_folder_preserve_delete_note_moves WHERE conflict_operation_id=? ORDER BY ordinal`, conflict.String())
	if err != nil {
		return err
	}
	i = 0
	for rows.Next() {
		if i >= len(notes) {
			rows.Close()
			return errors.New("preserve delete note replay mismatch")
		}
		var id, sp, tp, name string
		var mc, sr, tr uint64
		var hash []byte
		if err = rows.Scan(&id, &mc, &sr, &tr, &sp, &tp, &name, &hash); err != nil {
			rows.Close()
			return err
		}
		x := notes[i]
		if id != x.NoteID.String() || mc != x.MoveCursor || sr != x.SourceRevision || tr != x.TargetRevision || sp != x.SourceParentID.String() || tp != x.TargetParentID.String() || name != x.Name || !bytes.Equal(hash, x.BlobHash) {
			rows.Close()
			return errors.New("preserve delete note replay mismatch")
		}
		i++
	}
	if err = rows.Close(); err != nil || i != len(notes) {
		return errors.New("preserve delete note replay mismatch")
	}
	return nil
}

func nullableNonEmptyString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) MarkPreserveDeleteProbeAttempted(ctx context.Context, operation uuid.UUID) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='attempted',attempted_at_ms=COALESCE(attempted_at_ms,?) WHERE operation_id=? AND status IN('pending','attempted') AND mutation='delete' AND object_type='folder' AND EXISTS(SELECT 1 FROM sync_inbox_changes i WHERE i.object_id=sync_outbox.object_id AND i.object_type='folder' AND i.mutation='move' AND i.revision>sync_outbox.base_revision AND i.cursor<=CAST((SELECT value FROM sync_state WHERE key='downloaded_cursor') AS INTEGER))`, s.clock().UTC().UnixMilli(), operation.String())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return errors.New("preserve delete probe unavailable")
		}
		return nil
	})
}

func (s *Store) PendingPreserveDeleteProbe(ctx context.Context) (*OutboxItem, error) {
	var item OutboxItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var op, obj string
		err := tx.QueryRowContext(ctx, `SELECT o.sequence,o.operation_id,o.object_id,o.base_revision,o.status FROM sync_outbox o WHERE o.status IN('pending','attempted') AND o.mutation='delete' AND o.object_type='folder' AND EXISTS(SELECT 1 FROM sync_inbox_changes i WHERE i.object_id=o.object_id AND i.object_type='folder' AND i.mutation='move' AND i.revision>o.base_revision AND i.cursor<=CAST((SELECT value FROM sync_state WHERE key='downloaded_cursor') AS INTEGER)) ORDER BY o.sequence LIMIT 1`).Scan(&item.Sequence, &op, &obj, &item.Mutation.BaseRevision, &item.Status)
		if err != nil {
			return err
		}
		item.Mutation.OperationID, _ = uuid.Parse(op)
		item.Mutation.ObjectID, _ = uuid.Parse(obj)
		item.Mutation.Kind = Delete
		item.Mutation.ObjectType = Folder
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Store) PendingBlockedFolderDeletes(ctx context.Context) (map[uuid.UUID]uint64, error) {
	out := map[uuid.UUID]uint64{}
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.object_id,o.base_revision FROM sync_outbox o WHERE o.status IN('pending','attempted') AND o.mutation='delete' AND o.object_type='folder' AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox prerequisite ON prerequisite.operation_id=d.dependency_operation_id WHERE d.operation_id=o.operation_id AND prerequisite.status NOT IN('accepted','superseded') AND prerequisite.mutation='delete')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			var revision uint64
			if err = rows.Scan(&raw, &revision); err != nil {
				return err
			}
			id, err := uuid.Parse(raw)
			if err != nil {
				return err
			}
			out[id] = revision
		}
		return rows.Err()
	})
	return out, err
}
