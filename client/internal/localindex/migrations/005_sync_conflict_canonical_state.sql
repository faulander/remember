CREATE TABLE sync_conflict_states (
    operation_id TEXT PRIMARY KEY,
    object_type TEXT NOT NULL CHECK(object_type IN ('note','folder')),
    revision INTEGER NOT NULL CHECK(revision > 0),
    parent_id TEXT,
    name TEXT NOT NULL,
    blob_hash BLOB,
    deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE,
    CHECK(blob_hash IS NULL OR (typeof(blob_hash)='blob' AND length(blob_hash)=32)),
    CHECK((object_type='note' AND blob_hash IS NOT NULL) OR (object_type='folder' AND blob_hash IS NULL))
) WITHOUT ROWID;

CREATE TRIGGER sync_conflict_states_immutable
BEFORE UPDATE ON sync_conflict_states
BEGIN SELECT RAISE(ABORT, 'canonical conflict state is immutable'); END;
