package clientsync

import (
	"context"
	"database/sql"
	"errors"
)

type OutboxBlobCleanup struct {
	Hash            [32]byte
	ThroughSequence int64
}

func (s *Store) PendingOutboxBlobCleanups(ctx context.Context, limit int) ([]OutboxBlobCleanup, error) {
	if limit <= 0 || limit > 500 {
		return nil, errors.New("invalid blob cleanup limit")
	}
	var result []OutboxBlobCleanup
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.blob_hash,MAX(o.sequence) FROM sync_outbox o WHERE o.object_type='note' AND o.mutation IN ('create','update') AND o.blob_hash IS NOT NULL AND (o.status IN ('accepted','superseded') OR (o.status='conflict' AND EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state='completed'))) AND NOT EXISTS(SELECT 1 FROM sync_outbox active WHERE active.blob_hash=o.blob_hash AND active.status IN ('pending','attempted','replay_mismatch')) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.source_hash=o.blob_hash AND m.state<>'completed') AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members c JOIN conflict_folder_create_recoveries r ON r.operation_id=c.operation_id WHERE c.blob_hash=o.blob_hash AND r.state<>'completed') AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains c JOIN conflict_folder_move_delete_recoveries r ON r.operation_id=c.operation_id WHERE c.blob_hash=o.blob_hash AND r.state<>'completed') GROUP BY o.blob_hash HAVING MAX(o.sequence)>COALESCE((SELECT MAX(c.through_sequence) FROM sync_blob_cleanups c WHERE c.blob_hash=o.blob_hash),0) ORDER BY MAX(o.sequence) LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var item OutboxBlobCleanup
			if err := rows.Scan(&raw, &item.ThroughSequence); err != nil {
				return err
			}
			if len(raw) != 32 || item.ThroughSequence <= 0 {
				return errors.New("corrupt blob cleanup candidate")
			}
			copy(item.Hash[:], raw)
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) MarkOutboxBlobCleaned(ctx context.Context, item OutboxBlobCleanup) error {
	zero := true
	for _, b := range item.Hash {
		zero = zero && b == 0
	}
	if zero || item.ThroughSequence <= 0 {
		return errors.New("invalid blob cleanup")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO sync_blob_cleanups(blob_hash,through_sequence,cleaned_at_ms) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM sync_outbox o WHERE o.sequence=? AND o.blob_hash=? AND (o.status IN ('accepted','superseded') OR (o.status='conflict' AND EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state='completed')))) AND NOT EXISTS(SELECT 1 FROM sync_outbox active WHERE active.blob_hash=? AND active.status IN ('pending','attempted','replay_mismatch')) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.source_hash=? AND m.state<>'completed') AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members c JOIN conflict_folder_create_recoveries r ON r.operation_id=c.operation_id WHERE c.blob_hash=? AND r.state<>'completed') AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains c JOIN conflict_folder_move_delete_recoveries r ON r.operation_id=c.operation_id WHERE c.blob_hash=? AND r.state<>'completed') ON CONFLICT(blob_hash,through_sequence) DO NOTHING`, item.Hash[:], item.ThroughSequence, s.clock().UTC().UnixMilli(), item.ThroughSequence, item.Hash[:], item.Hash[:], item.Hash[:], item.Hash[:], item.Hash[:])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_blob_cleanups WHERE blob_hash=? AND through_sequence=?)`, item.Hash[:], item.ThroughSequence).Scan(&exists); err != nil || exists == 0 {
				return errors.New("blob cleanup no longer authorized")
			}
		}
		return nil
	})
}
