CREATE TABLE sync_conflict_resolutions (
    operation_id TEXT PRIMARY KEY,
    resolution TEXT NOT NULL CHECK(resolution='already_deleted'),
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER sync_conflict_resolutions_no_update
BEFORE UPDATE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT, 'sync conflict resolution is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_no_delete
BEFORE DELETE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT, 'sync conflict resolution history is immutable'); END;
