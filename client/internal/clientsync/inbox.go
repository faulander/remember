package clientsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

// InboxItem is an authenticated remote change plus its durable local apply state.
type InboxItem struct {
	Change     Change
	State      string
	IngestedAt int64
	ApplyingAt *int64
	AppliedAt  *int64
}

// InboxParentBinding is the immutable immediate-parent Folder identity used for
// rooted filesystem access by one recursively ancestry-bound Note plan.
type InboxParentBinding struct {
	ParentID     uuid.UUID
	RelativePath string
	Device       uint64
	Inode        uint64
}

func parseCursor(raw, label string) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 63)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		return 0, fmt.Errorf("invalid %s cursor", label)
	}
	return value, nil
}

func readSyncCursor(ctx context.Context, tx *sql.Tx, key, label string) (uint64, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key=?`, key).Scan(&raw); err != nil {
		return 0, err
	}
	return parseCursor(raw, label)
}

// DownloadedCursor is the durable frontier through which pull pages are stored.
func (s *Store) DownloadedCursor(ctx context.Context) (uint64, error) {
	var cursor uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		cursor, err = readSyncCursor(ctx, tx, "downloaded_cursor", "downloaded")
		return err
	})
	return cursor, err
}

func validateInboxChange(change Change) error {
	if change.State != "" || change.Cursor == 0 || change.Cursor > math.MaxInt64 || !validOperationID(change.OperationID) || !validObjectID(change.ObjectID) ||
		(change.ObjectType != Note && change.ObjectType != Folder) ||
		(change.Mutation != Create && change.Mutation != Update && change.Mutation != Move && change.Mutation != Delete) ||
		change.Revision == 0 || change.Revision > math.MaxInt64 ||
		(change.Mutation == Create && change.Revision != 1) || (change.Mutation != Create && change.Revision < 2) ||
		(change.ParentID != nil && !validObjectID(*change.ParentID)) || change.Name == "" || naming.ValidateComponent(change.Name) != nil ||
		(change.ObjectType == Note && len(change.BlobHash) != sha256.Size) || (change.ObjectType == Folder && change.BlobHash != nil) ||
		(change.Deleted != (change.Mutation == Delete)) {
		return errors.New("invalid inbox change")
	}
	return nil
}

// IngestPullPage durably stores one complete contiguous pull page. A page that
// is wholly below the downloaded frontier is accepted only as an exact replay.
func (s *Store) IngestPullPage(ctx context.Context, from, next uint64, changes []Change) error {
	if from > math.MaxInt64 || next > math.MaxInt64 || next < from || uint64(len(changes)) != next-from {
		return errors.New("invalid inbox page cursor range")
	}
	for i := range changes {
		if from == math.MaxInt64 || changes[i].Cursor != from+uint64(i)+1 {
			return errors.New("inbox page cursor is not contiguous")
		}
		if err := validateInboxChange(changes[i]); err != nil {
			return err
		}
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		downloaded, err := readSyncCursor(ctx, tx, "downloaded_cursor", "downloaded")
		if err != nil {
			return err
		}
		if from < downloaded {
			if next > downloaded {
				return errors.New("inbox replay overlaps downloaded frontier")
			}
			for i := range changes {
				stored, found, err := inboxItemTx(ctx, tx, changes[i].Cursor)
				if err != nil {
					return err
				}
				if !found || !sameChangePayload(stored.Change, changes[i]) {
					return errors.New("inbox replay payload mismatch")
				}
			}
			return nil
		}
		if from != downloaded {
			return errors.New("inbox page does not start at downloaded cursor")
		}
		now := s.clock().UTC().UnixMilli()
		for i := range changes {
			change := changes[i]
			var parent, blob any
			if change.ParentID != nil {
				parent = change.ParentID.String()
			}
			if len(change.BlobHash) != 0 {
				blob = change.BlobHash
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_inbox_changes(cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,state,ingested_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?)`, change.Cursor, change.OperationID.String(), change.ObjectID.String(), change.Mutation, change.ObjectType, change.Revision, parent, change.Name, blob, change.Deleted, now); err != nil {
				return err
			}
		}
		if next != downloaded {
			result, err := tx.ExecContext(ctx, `UPDATE sync_state SET value=? WHERE key='downloaded_cursor' AND value=?`, strconv.FormatUint(next, 10), strconv.FormatUint(downloaded, 10))
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				return errors.New("downloaded cursor update lost")
			}
		}
		return nil
	})
}

func sameChangePayload(left, right Change) bool {
	if left.Cursor != right.Cursor || left.OperationID != right.OperationID || left.ObjectID != right.ObjectID || left.Mutation != right.Mutation || left.ObjectType != right.ObjectType || left.Revision != right.Revision || left.Name != right.Name || left.Deleted != right.Deleted || !bytes.Equal(left.BlobHash, right.BlobHash) {
		return false
	}
	return (left.ParentID == nil && right.ParentID == nil) || (left.ParentID != nil && right.ParentID != nil && *left.ParentID == *right.ParentID)
}

func scanInboxItem(scanner interface{ Scan(...any) error }) (InboxItem, error) {
	var item InboxItem
	var operation, object string
	var parent sql.NullString
	var applying, applied sql.NullInt64
	if err := scanner.Scan(&item.Change.Cursor, &operation, &object, &item.Change.Mutation, &item.Change.ObjectType, &item.Change.Revision, &parent, &item.Change.Name, &item.Change.BlobHash, &item.Change.Deleted, &item.State, &item.IngestedAt, &applying, &applied); err != nil {
		return InboxItem{}, err
	}
	var err error
	item.Change.OperationID, err = uuid.Parse(operation)
	if err != nil || item.Change.OperationID.String() != operation || !validOperationID(item.Change.OperationID) {
		return InboxItem{}, errors.New("invalid stored inbox operation")
	}
	item.Change.ObjectID, err = uuid.Parse(object)
	if err != nil || item.Change.ObjectID.String() != object || !validObjectID(item.Change.ObjectID) {
		return InboxItem{}, errors.New("invalid stored inbox object")
	}
	if parent.Valid {
		id, err := uuid.Parse(parent.String)
		if err != nil || id.String() != parent.String || !validObjectID(id) {
			return InboxItem{}, errors.New("invalid stored inbox parent")
		}
		item.Change.ParentID = &id
	}
	if applying.Valid {
		value := applying.Int64
		item.ApplyingAt = &value
	}
	if applied.Valid {
		value := applied.Int64
		item.AppliedAt = &value
	}
	payload := item.Change
	payload.State = ""
	if err := validateInboxChange(payload); err != nil {
		return InboxItem{}, errors.New("invalid stored inbox payload")
	}
	validState := item.IngestedAt >= 0 && ((item.State == "pending" && item.ApplyingAt == nil && item.AppliedAt == nil) || (item.State == "applying" && item.ApplyingAt != nil && *item.ApplyingAt >= item.IngestedAt && item.AppliedAt == nil) || (item.State == "applied" && item.ApplyingAt != nil && item.AppliedAt != nil && *item.ApplyingAt >= item.IngestedAt && *item.AppliedAt >= *item.ApplyingAt))
	if !validState {
		return InboxItem{}, errors.New("invalid stored inbox state")
	}
	item.Change.State = item.State
	return item, nil
}

const inboxColumns = `cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,state,ingested_at_ms,applying_at_ms,applied_at_ms`

func inboxItemTx(ctx context.Context, tx *sql.Tx, cursor uint64) (InboxItem, bool, error) {
	item, err := scanInboxItem(tx.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE cursor=?`, cursor))
	if errors.Is(err, sql.ErrNoRows) {
		return InboxItem{}, false, nil
	}
	return item, err == nil, err
}

// InboxChange returns one stored inbox row regardless of apply state.
func (s *Store) InboxChange(ctx context.Context, cursor uint64) (InboxItem, bool, error) {
	if cursor == 0 || cursor > math.MaxInt64 {
		return InboxItem{}, false, errors.New("invalid inbox cursor")
	}
	var item InboxItem
	var found bool
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		item, found, err = inboxItemTx(ctx, tx, cursor)
		return err
	})
	return item, found, err
}

// PendingInboxChange returns one row only while it is pending.
func (s *Store) PendingInboxChange(ctx context.Context, cursor uint64) (InboxItem, bool, error) {
	if cursor == 0 || cursor > math.MaxInt64 {
		return InboxItem{}, false, errors.New("invalid inbox cursor")
	}
	var item InboxItem
	var found bool
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		item, err = scanInboxItem(tx.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE cursor=? AND state='pending'`, cursor))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return item, found, err
}

// ListIndependentInboxCandidates returns the currently eligible root and
// arbitrarily nested Note update/delete rows in cursor order. Eligibility is a
// point-in-time filter; CreateInboxApplyPlan repeats it atomically.
func (s *Store) ListIndependentInboxCandidates(ctx context.Context, limit int) ([]InboxItem, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid independent inbox candidate limit")
	}
	var items []InboxItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM sync_independent_inbox_candidates ORDER BY cursor LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanInboxItem(rows)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return items, err
}

// ListPendingInbox returns pending rows in cursor order.
func (s *Store) ListPendingInbox(ctx context.Context, limit int) ([]InboxItem, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid inbox list limit")
	}
	var items []InboxItem
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE state='pending' ORDER BY cursor LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanInboxItem(rows)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return items, err
}

func (s *Store) transitionInbox(ctx context.Context, cursor uint64, from, to string) error {
	if cursor == 0 || cursor > math.MaxInt64 {
		return errors.New("invalid inbox cursor")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		now := s.clock().UTC().UnixMilli()
		query := `UPDATE sync_inbox_changes SET state='applying',applying_at_ms=max(?,ingested_at_ms) WHERE cursor=? AND state='pending' AND NOT EXISTS(SELECT 1 FROM sync_inbox_apply_plans linked WHERE linked.cursor=sync_inbox_changes.cursor)`
		if from == "applying" && to == "applied" {
			query = `UPDATE sync_inbox_changes SET state='applied',applied_at_ms=max(?,applying_at_ms) WHERE cursor=? AND state='applying' AND NOT EXISTS(SELECT 1 FROM sync_inbox_apply_plans linked WHERE linked.cursor=sync_inbox_changes.cursor)`
		} else if from != "pending" || to != "applying" {
			return errors.New("invalid inbox transition")
		}
		result, err := tx.ExecContext(ctx, query, now, cursor)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return errors.New("inbox state transition rejected")
		}
		return nil
	})
}

func (s *Store) MarkInboxApplying(ctx context.Context, cursor uint64) error {
	return s.transitionInbox(ctx, cursor, "pending", "applying")
}

func (s *Store) MarkInboxApplied(ctx context.Context, cursor uint64) error {
	return s.transitionInbox(ctx, cursor, "applying", "applied")
}

// CreateInboxApplyPlan creates one persisted apply plan for an independent
// out-of-order root or arbitrarily nested Note update or delete. It does not
// advance either cursor.
func (s *Store) CreateInboxApplyPlan(ctx context.Context, cursor uint64, planID uuid.UUID) error {
	if cursor == 0 || cursor > math.MaxInt64 || !validOperationID(planID) {
		return errors.New("invalid inbox apply plan")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		item, err := scanInboxItem(tx.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM sync_independent_inbox_candidates WHERE cursor=?`, cursor))
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("inbox change is not eligible for independent apply")
		}
		if err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apply_plans WHERE status IN ('prepared','applying'))`).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return errors.New("another apply plan is active")
		}
		now := s.clock().UTC().UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO apply_plans(plan_id,from_cursor,through_cursor,status,created_at_ms) VALUES(?,?,?,'prepared',?)`, planID.String(), cursor-1, cursor, now); err != nil {
			return err
		}
		if item.Change.ParentID != nil {
			result, err := tx.ExecContext(ctx, `INSERT INTO sync_inbox_parent_bindings(
					plan_id,inbox_cursor,depth,ancestor_id,ancestor_parent_id,
					ancestor_relative,device,inode,baseline_revision,baseline_operation_id)
				SELECT ?,inbox_cursor,depth,ancestor_id,ancestor_parent_id,
					ancestor_relative,device,inode,baseline_revision,baseline_operation_id
				FROM sync_inbox_note_ancestry
				WHERE inbox_cursor=?
				ORDER BY depth`, planID.String(), cursor)
			if err != nil {
				return err
			}
			var expected int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sync_inbox_note_ancestry WHERE inbox_cursor=?`, cursor).Scan(&expected); err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count == 0 || count != expected {
				return errors.New("nested inbox ancestry binding unavailable")
			}
		}
		var parent, blob any
		if item.Change.ParentID != nil {
			parent = item.Change.ParentID.String()
		}
		if len(item.Change.BlobHash) != 0 {
			blob = item.Change.BlobHash
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO apply_steps(plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,state) VALUES(?,0,?,?,?,?,?,?,?,?,?,'pending')`, planID.String(), item.Change.Cursor, item.Change.OperationID.String(), item.Change.ObjectID.String(), item.Change.Mutation, item.Change.ObjectType, item.Change.Revision, parent, item.Change.Name, blob); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_inbox_apply_plans(plan_id,cursor) VALUES(?,?)`, planID.String(), cursor)
		return err
	})
}

func linkedInboxParentBindingValidTx(ctx context.Context, tx *sql.Tx, planID uuid.UUID) error {
	var valid int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1
		FROM sync_inbox_apply_plans link
		JOIN sync_inbox_changes i ON i.cursor=link.cursor
		WHERE link.plan_id=?
		  AND (
		    (i.parent_id IS NULL AND NOT EXISTS(
		      SELECT 1 FROM sync_inbox_parent_bindings binding
		      WHERE binding.plan_id=link.plan_id
		    ))
		    OR
		    (i.parent_id IS NOT NULL AND EXISTS(
		      SELECT 1 FROM sync_inbox_valid_nested_bindings valid
		      WHERE valid.plan_id=link.plan_id AND valid.inbox_cursor=i.cursor
		    ))
		  )
	)`, planID.String()).Scan(&valid)
	if err != nil {
		return err
	}
	if valid == 0 {
		return errors.New("nested inbox ancestry binding is no longer valid")
	}
	return nil
}

// ActiveInboxParentBinding returns the revalidated immediate-parent identity for
// a linked nested Note plan. Root-Note and unlinked legacy plans return nil. The
// validity check covers every immutable Folder ancestry binding.
func (s *Store) ActiveInboxParentBinding(ctx context.Context, planID uuid.UUID) (*InboxParentBinding, error) {
	if !validOperationID(planID) {
		return nil, errors.New("invalid apply plan id")
	}
	var binding *InboxParentBinding
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var parent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT i.parent_id
			FROM sync_inbox_apply_plans link
			JOIN sync_inbox_changes i ON i.cursor=link.cursor
			WHERE link.plan_id=?`, planID.String()).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := linkedInboxParentBindingValidTx(ctx, tx, planID); err != nil {
			return err
		}
		if !parent.Valid {
			return nil
		}
		var rawID, relative string
		var device, inode uint64
		err = tx.QueryRowContext(ctx, `SELECT ancestor_id,ancestor_relative,device,inode
			FROM sync_inbox_valid_parent_bindings
			WHERE plan_id=? AND depth=1`, planID.String()).Scan(&rawID, &relative, &device, &inode)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("nested inbox immediate-parent binding is no longer valid")
		}
		if err != nil {
			return err
		}
		id, err := uuid.Parse(rawID)
		if err != nil || id == uuid.Nil || id.String() != parent.String || naming.ValidateRelativePath(relative) != nil || device == 0 || inode == 0 {
			return errors.New("invalid nested inbox immediate-parent binding")
		}
		binding = &InboxParentBinding{ParentID: id, RelativePath: relative, Device: device, Inode: inode}
		return nil
	})
	return binding, err
}

// AbandonPreparedInboxPlan durably fails a pristine prepared linked plan. The
// immutable link remains as history, so the same plan cannot be silently reused.
func (s *Store) AbandonPreparedInboxPlan(ctx context.Context, planID uuid.UUID) error {
	if !validOperationID(planID) {
		return errors.New("invalid apply plan id")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {

		var pristine int
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM sync_inbox_apply_plans l
			JOIN apply_plans p ON p.plan_id=l.plan_id
			JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0
			JOIN sync_inbox_changes i ON i.cursor=l.cursor
			WHERE l.plan_id=? AND p.status='prepared' AND s.state='pending' AND i.state='pending'
			AND (SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)=1
			AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=p.plan_id)
			AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=p.plan_id)
		)`, planID.String()).Scan(&pristine)
		if err != nil {
			return err
		}
		if pristine == 0 {
			return errors.New("inbox apply plan is not pristine and prepared")
		}
		result, err := tx.ExecContext(ctx, `UPDATE apply_plans SET status='failed',completed_at_ms=? WHERE plan_id=? AND status='prepared'`, s.clock().UTC().UnixMilli(), planID.String())
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return errors.New("inbox apply plan abandonment race")
		}
		return nil
	})
}

// RetryAbandonedInboxPlan returns one pristine terminal linked plan to prepared
// after rechecking its inbox ordering and exact predecessor baseline.
func (s *Store) RetryAbandonedInboxPlan(ctx context.Context, planID uuid.UUID) error {
	if !validOperationID(planID) {
		return errors.New("invalid apply plan id")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE apply_plans SET status='prepared',completed_at_ms=NULL WHERE plan_id=? AND status='failed'
   AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps step ON step.plan_id=l.plan_id AND step.step_index=0
    WHERE l.plan_id=apply_plans.plan_id AND i.state='pending' AND step.state='pending'
    AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision-1)
    AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes earlier WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied')
    AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents unresolved WHERE unresolved.object_id=i.object_id)
    AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=apply_plans.plan_id)
    AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=apply_plans.plan_id))
   AND NOT EXISTS(SELECT 1 FROM apply_plans active WHERE active.plan_id<>apply_plans.plan_id AND active.status IN ('prepared','applying'))`, planID.String())
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return errors.New("abandoned inbox apply plan is not retryable")
		}
		return nil
	})
}

// ReconcileInboxAppliedThroughConfirmed mirrors the legacy apply frontier into
// inbox state. It never advances confirmed_cursor. For a database with no inbox
// history, it may seed downloaded_cursor from a newer legacy confirmed cursor.
func (s *Store) ReconcileInboxAppliedThroughConfirmed(ctx context.Context) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		confirmed, err := readSyncCursor(ctx, tx, "confirmed_cursor", "confirmed")
		if errors.Is(err, sql.ErrNoRows) {
			confirmed, err = 0, nil
		}
		if err != nil {
			return err
		}
		downloaded, err := readSyncCursor(ctx, tx, "downloaded_cursor", "downloaded")
		if err != nil {
			return err
		}
		var first, last, count sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(cursor),max(cursor),count(*) FROM sync_inbox_changes`).Scan(&first, &last, &count); err != nil {
			return err
		}
		if !first.Valid {
			if downloaded > confirmed {
				return errors.New("downloaded inbox range is missing")
			}
			if downloaded < confirmed {
				result, err := tx.ExecContext(ctx, `UPDATE sync_state SET value=? WHERE key='downloaded_cursor' AND value=?`, strconv.FormatUint(confirmed, 10), strconv.FormatUint(downloaded, 10))
				if err != nil {
					return err
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					return errors.New("downloaded cursor seed lost")
				}
			}
			return nil
		}
		if downloaded < confirmed {
			return errors.New("downloaded cursor is behind confirmed cursor")
		}
		if !last.Valid || !count.Valid || last.Int64 != int64(downloaded) || count.Int64 != last.Int64-first.Int64+1 {
			return errors.New("downloaded inbox range is incomplete")
		}

		rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE cursor<=? ORDER BY cursor`, confirmed)
		if err != nil {
			return err
		}
		expected := uint64(first.Int64)
		for rows.Next() {
			item, err := scanInboxItem(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if item.Change.Cursor != expected {
				rows.Close()
				return errors.New("confirmed inbox range is incomplete")
			}
			expected++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if uint64(first.Int64) <= confirmed && expected != confirmed+1 {
			return errors.New("confirmed inbox range is incomplete")
		}

		now := s.clock().UTC().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE sync_inbox_changes SET state='applying',applying_at_ms=max(?,ingested_at_ms) WHERE cursor<=? AND state='pending'`, now, confirmed); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sync_inbox_changes SET state='applied',applied_at_ms=max(?,applying_at_ms) WHERE cursor<=? AND state='applying'`, now, confirmed)
		return err
	})
}

func (s *Store) completeInboxApplyPlanTx(ctx context.Context, tx *sql.Tx, planID uuid.UUID, cursor uint64) error {
	var status, stepState, inboxState, objectID, operationID string
	var stepCursor, revision uint64
	var stepCount int
	if err := tx.QueryRowContext(ctx, `SELECT p.status,s.state,i.state,s.cursor,s.object_id,s.revision,s.operation_id,(SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)
		FROM apply_plans p JOIN sync_inbox_apply_plans l ON l.plan_id=p.plan_id JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 JOIN sync_inbox_changes i ON i.cursor=l.cursor
		WHERE p.plan_id=? AND l.cursor=?`, planID.String(), cursor).Scan(&status, &stepState, &inboxState, &stepCursor, &objectID, &revision, &operationID, &stepCount); err != nil {
		return err
	}
	if status != "applying" || stepState != "applied" || inboxState != "applying" || stepCount != 1 || stepCursor != cursor || revision < 2 {
		return errors.New("linked inbox apply plan is not completable")
	}
	if err := linkedInboxParentBindingValidTx(ctx, tx, planID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sync_baselines SET revision=?,operation_id=? WHERE object_id=? AND revision=?`, revision, operationID, objectID, revision-1)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("linked inbox baseline predecessor changed")
	}
	now := s.clock().UTC().UnixMilli()
	result, err = tx.ExecContext(ctx, `UPDATE sync_inbox_changes SET state='applied',applied_at_ms=max(?,applying_at_ms) WHERE cursor=? AND state='applying'`, now, cursor)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("linked inbox completion race")
	}
	result, err = tx.ExecContext(ctx, `UPDATE apply_plans SET status='completed',completed_at_ms=? WHERE plan_id=? AND status='applying'`, now, planID.String())
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("linked apply plan completion race")
	}
	_, err = advanceConfirmedInboxCursorTx(ctx, tx)
	return err
}

func advanceConfirmedInboxCursorTx(ctx context.Context, tx *sql.Tx) (uint64, error) {
	confirmed, err := readSyncCursor(ctx, tx, "confirmed_cursor", "confirmed")
	if errors.Is(err, sql.ErrNoRows) {
		confirmed = 0
		err = nil
	}
	if err != nil {
		return 0, err
	}
	downloaded, err := readSyncCursor(ctx, tx, "downloaded_cursor", "downloaded")
	if err != nil {
		return 0, err
	}
	if downloaded < confirmed {
		return 0, errors.New("downloaded cursor is behind confirmed cursor")
	}
	frontier := confirmed
	rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE cursor>? AND cursor<=? ORDER BY cursor`, confirmed, downloaded)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanInboxItem(rows)
		if err != nil {
			return 0, err
		}
		if frontier == math.MaxInt64 || item.Change.Cursor != frontier+1 || item.State != "applied" {
			break
		}
		frontier = item.Change.Cursor
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if frontier == confirmed {
		return frontier, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('confirmed_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatUint(frontier, 10))
	return frontier, err
}

// AdvanceConfirmedInboxCursor advances the legacy confirmed frontier only over
// a contiguous prefix of applied inbox rows.
func (s *Store) AdvanceConfirmedInboxCursor(ctx context.Context) (uint64, error) {
	var frontier uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		frontier, err = advanceConfirmedInboxCursorTx(ctx, tx)
		return err
	})
	return frontier, err
}

// HasEarlierPendingInboxForObject reports whether an exact object has a lower
// cursor that has not completed applying.
func (s *Store) HasEarlierPendingInboxForObject(ctx context.Context, objectID uuid.UUID, cursor uint64) (bool, error) {
	if !validObjectID(objectID) || cursor == 0 || cursor > math.MaxInt64 {
		return false, errors.New("invalid inbox object lookup")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_inbox_changes WHERE object_id=? AND cursor<? AND state<>'applied')`, objectID.String(), cursor).Scan(&exists)
	})
	return exists != 0, err
}
