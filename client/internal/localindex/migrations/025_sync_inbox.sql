CREATE TABLE sync_inbox_changes (
    cursor INTEGER PRIMARY KEY CHECK(cursor > 0),
    operation_id TEXT NOT NULL UNIQUE CHECK(length(operation_id) = 36),
    object_id TEXT NOT NULL CHECK(length(object_id) = 36),
    mutation TEXT NOT NULL CHECK(mutation IN ('create','update','move','delete')),
    object_type TEXT NOT NULL CHECK(object_type IN ('note','folder')),
    revision INTEGER NOT NULL CHECK(revision > 0),
    parent_id TEXT CHECK(parent_id IS NULL OR length(parent_id) = 36),
    name TEXT NOT NULL CHECK(name <> ''),
    blob_hash BLOB,
    deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
    state TEXT NOT NULL CHECK(state IN ('pending','applying','applied')),
    ingested_at_ms INTEGER NOT NULL CHECK(ingested_at_ms >= 0),
    applying_at_ms INTEGER,
    applied_at_ms INTEGER,
    CHECK(blob_hash IS NULL OR (typeof(blob_hash)='blob' AND length(blob_hash)=32)),
    CHECK((object_type='note' AND typeof(blob_hash)='blob' AND length(blob_hash)=32) OR
          (object_type='folder' AND blob_hash IS NULL)),
    CHECK((mutation='create' AND revision=1) OR (mutation<>'create' AND revision>1)),
    CHECK(deleted=(mutation='delete')),
    CHECK((state='pending' AND applying_at_ms IS NULL AND applied_at_ms IS NULL) OR
          (state='applying' AND applying_at_ms IS NOT NULL AND applying_at_ms>=ingested_at_ms AND applied_at_ms IS NULL) OR
          (state='applied' AND applying_at_ms IS NOT NULL AND applied_at_ms IS NOT NULL AND applied_at_ms>=applying_at_ms))
);
CREATE INDEX sync_inbox_pending_cursor_idx ON sync_inbox_changes(state,cursor);
CREATE INDEX sync_inbox_object_cursor_idx ON sync_inbox_changes(object_id,cursor);

CREATE TRIGGER sync_inbox_insert_conflict_guard
BEFORE INSERT ON sync_inbox_changes
WHEN EXISTS(SELECT 1 FROM sync_inbox_changes existing WHERE existing.cursor=NEW.cursor OR existing.operation_id=NEW.operation_id)
BEGIN SELECT RAISE(ABORT, 'sync inbox history cannot be replaced'); END;

CREATE TRIGGER sync_inbox_payload_immutable
BEFORE UPDATE OF cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash,deleted,ingested_at_ms
ON sync_inbox_changes BEGIN SELECT RAISE(ABORT, 'sync inbox payload is immutable'); END;
CREATE TRIGGER sync_inbox_state_monotone
BEFORE UPDATE OF state,applying_at_ms,applied_at_ms ON sync_inbox_changes
WHEN NOT ((OLD.state='pending' AND NEW.state='applying' AND NEW.applying_at_ms IS NOT NULL AND NEW.applied_at_ms IS NULL) OR
          (OLD.state='applying' AND NEW.state='applied' AND NEW.applying_at_ms=OLD.applying_at_ms AND NEW.applied_at_ms IS NOT NULL))
BEGIN SELECT RAISE(ABORT, 'sync inbox state transition is invalid'); END;
CREATE TRIGGER sync_inbox_no_delete
BEFORE DELETE ON sync_inbox_changes BEGIN SELECT RAISE(ABORT, 'sync inbox history is immutable'); END;

INSERT INTO sync_state(key,value)
SELECT 'downloaded_cursor',COALESCE((SELECT value FROM sync_state WHERE key='confirmed_cursor'),'0')
WHERE NOT EXISTS(SELECT 1 FROM sync_state WHERE key='downloaded_cursor');
