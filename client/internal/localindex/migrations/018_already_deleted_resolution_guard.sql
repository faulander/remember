CREATE TRIGGER sync_conflict_resolutions_already_deleted_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='already_deleted' AND NOT EXISTS(
    SELECT 1 FROM sync_outbox o
    WHERE o.operation_id=NEW.operation_id AND o.mutation='delete' AND o.status='conflict' AND (
        (o.conflict_code='object_missing' AND NOT EXISTS(
            SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id
        ))
        OR
        (o.conflict_code='object_deleted' AND EXISTS(
            SELECT 1 FROM sync_conflict_states c
            WHERE c.operation_id=o.operation_id AND c.object_type=o.object_type
              AND c.deleted=1 AND c.revision>o.base_revision
        ))
    )
)
BEGIN SELECT RAISE(ABORT, 'already-deleted resolution requires matching conflict'); END;
