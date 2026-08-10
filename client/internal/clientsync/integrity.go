package clientsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrBlobMissing = errors.New("referenced remote blob is missing")

type IntegrityIncident struct {
	ID              int64
	PlanID          uuid.UUID
	Cursor          uint64
	ObjectID        uuid.UUID
	BlobHash        [sha256.Size]byte
	Code            string
	FirstDetectedAt time.Time
	LastDetectedAt  time.Time
	OccurrenceCount uint64
	AcknowledgedAt  *time.Time
}

func (s *Store) RecordIntegrityIncident(ctx context.Context, planID uuid.UUID, cursor uint64, objectID uuid.UUID, hash [sha256.Size]byte, code string) error {
	if !validOperationID(planID) || cursor == 0 || !validObjectID(objectID) || (code != "missing_blob" && code != "hash_mismatch") {
		return errors.New("invalid integrity incident")
	}
	now := s.clock().UTC().UnixMilli()
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO sync_integrity_incidents(plan_id,cursor,object_id,blob_hash,code,first_detected_at_ms,last_detected_at_ms,occurrence_count) SELECT ?,?,?,?,?,?,?,1 WHERE EXISTS(SELECT 1 FROM apply_steps s JOIN sync_inbox_changes i ON i.cursor=s.cursor WHERE s.plan_id=? AND s.cursor=? AND s.object_id=? AND s.blob_hash=? AND i.object_id=s.object_id AND i.blob_hash=s.blob_hash) ON CONFLICT(cursor,code) DO UPDATE SET last_detected_at_ms=excluded.last_detected_at_ms,occurrence_count=sync_integrity_incidents.occurrence_count+1,acknowledged_at_ms=NULL`, planID.String(), cursor, objectID.String(), hash[:], code, now, now, planID.String(), cursor, objectID.String(), hash[:])
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return errors.New("integrity incident is not linked to apply plan")
		}
		return nil
	})
}

func (s *Store) ListOpenIntegrityIncidents(ctx context.Context, limit int) ([]IntegrityIncident, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid integrity incident limit")
	}
	var out []IntegrityIncident
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT incident_id,plan_id,cursor,object_id,blob_hash,code,first_detected_at_ms,last_detected_at_ms,occurrence_count FROM sync_integrity_incidents WHERE acknowledged_at_ms IS NULL ORDER BY last_detected_at_ms DESC,incident_id DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item IntegrityIncident
			var plan, object string
			var hash []byte
			var first, last int64
			if err := rows.Scan(&item.ID, &plan, &item.Cursor, &object, &hash, &item.Code, &first, &last, &item.OccurrenceCount); err != nil {
				return err
			}
			var parseErr error
			item.PlanID, parseErr = uuid.Parse(plan)
			if parseErr != nil {
				return errors.New("corrupt integrity incident plan")
			}
			item.ObjectID, parseErr = uuid.Parse(object)
			if parseErr != nil || len(hash) != sha256.Size {
				return errors.New("corrupt integrity incident")
			}
			copy(item.BlobHash[:], hash)
			item.FirstDetectedAt = time.UnixMilli(first).UTC()
			item.LastDetectedAt = time.UnixMilli(last).UTC()
			out = append(out, item)
		}
		return rows.Err()
	})
	return out, err
}
