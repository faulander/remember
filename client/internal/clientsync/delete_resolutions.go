package clientsync

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) AlreadyDeletedResolutionMatches(ctx context.Context, change Change) (bool, error) {
	if change.Mutation != Delete || !validObjectID(change.ObjectID) || (change.ObjectType != Note && change.ObjectType != Folder) || change.Revision == 0 {
		return false, errors.New("invalid already-deleted change")
	}
	var exists int
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_conflict_resolutions r JOIN sync_outbox o ON o.operation_id=r.operation_id JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE r.resolution='already_deleted' AND o.mutation='delete' AND o.status='conflict' AND o.conflict_code='object_deleted' AND o.object_id=? AND o.object_type=? AND c.object_type=o.object_type AND c.deleted=1 AND c.revision=? AND c.revision>o.base_revision)`, change.ObjectID.String(), string(change.ObjectType), change.Revision).Scan(&exists)
	})
	return exists != 0, err
}
