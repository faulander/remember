package clientsync

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// ResolveEquivalentNoteMove records that an already-canonical exact target
// satisfies a stale move without changing local bytes or paths.
func (s *Store) ResolveEquivalentNoteMove(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid equivalent note move resolution")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms)
SELECT o.operation_id,'note_move_equivalent',? FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id
WHERE o.operation_id=? AND o.status='conflict' AND o.mutation='move' AND o.object_type='note' AND o.conflict_code='base_revision_mismatch'
AND c.object_type='note' AND c.deleted=0 AND ((o.parent_id IS NULL AND c.parent_id IS NULL) OR o.parent_id=c.parent_id) AND c.name=o.name AND c.revision>o.base_revision AND length(c.blob_hash)=32`, s.clock().UTC().UnixMilli(), operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_conflict_resolutions WHERE operation_id=? AND resolution='note_move_equivalent')`, operationID.String()).Scan(&exists); err != nil || exists == 0 {
			return errors.New("equivalent note move conflict unavailable")
		}
		return nil
	})
}
