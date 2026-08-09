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
		query := `UPDATE sync_inbox_changes SET state='applying',applying_at_ms=max(?,ingested_at_ms) WHERE cursor=? AND state='pending'`
		if from == "applying" && to == "applied" {
			query = `UPDATE sync_inbox_changes SET state='applied',applied_at_ms=max(?,applying_at_ms) WHERE cursor=? AND state='applying'`
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

// AdvanceConfirmedInboxCursor advances the legacy confirmed frontier only over
// a contiguous prefix of applied inbox rows.
func (s *Store) AdvanceConfirmedInboxCursor(ctx context.Context) (uint64, error) {
	var frontier uint64
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		confirmed, err := readSyncCursor(ctx, tx, "confirmed_cursor", "confirmed")
		if errors.Is(err, sql.ErrNoRows) {
			confirmed = 0
			err = nil
		}
		if err != nil {
			return err
		}
		downloaded, err := readSyncCursor(ctx, tx, "downloaded_cursor", "downloaded")
		if err != nil {
			return err
		}
		if downloaded < confirmed {
			return errors.New("downloaded cursor is behind confirmed cursor")
		}
		frontier = confirmed
		rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM sync_inbox_changes WHERE cursor>? AND cursor<=? ORDER BY cursor`, confirmed, downloaded)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanInboxItem(rows)
			if err != nil {
				return err
			}
			if frontier == math.MaxInt64 || item.Change.Cursor != frontier+1 || item.State != "applied" {
				break
			}
			frontier = item.Change.Cursor
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if frontier == confirmed {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_state(key,value) VALUES('confirmed_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatUint(frontier, 10))
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
