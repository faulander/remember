CREATE TABLE sync_blob_cleanups (
    blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
    through_sequence INTEGER NOT NULL CHECK(through_sequence > 0),
    cleaned_at_ms INTEGER NOT NULL,
    PRIMARY KEY(blob_hash, through_sequence)
) WITHOUT ROWID;

CREATE TRIGGER sync_blob_cleanups_no_update
BEFORE UPDATE ON sync_blob_cleanups BEGIN SELECT RAISE(ABORT, 'sync blob cleanup history is immutable'); END;
CREATE TRIGGER sync_blob_cleanups_no_delete
BEFORE DELETE ON sync_blob_cleanups BEGIN SELECT RAISE(ABORT, 'sync blob cleanup history is immutable'); END;
