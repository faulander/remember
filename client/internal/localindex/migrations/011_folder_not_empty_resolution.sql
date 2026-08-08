ALTER TABLE sync_conflict_resolutions RENAME TO sync_conflict_resolutions_v9;

CREATE TABLE sync_conflict_resolutions (
    operation_id TEXT PRIMARY KEY,
    resolution TEXT NOT NULL CHECK(resolution IN ('already_deleted','folder_not_empty_preserved')),
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms)
SELECT operation_id,resolution,created_at_ms FROM sync_conflict_resolutions_v9;
DROP TABLE sync_conflict_resolutions_v9;

CREATE TRIGGER sync_conflict_resolutions_no_update
BEFORE UPDATE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT, 'sync conflict resolution is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_no_delete
BEFORE DELETE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT, 'sync conflict resolution history is immutable'); END;

CREATE TABLE conflict_folder_restorations (
    operation_id TEXT PRIMARY KEY,
    folder_id TEXT NOT NULL,
    target_relative TEXT NOT NULL,
    stage_relative TEXT NOT NULL UNIQUE,
    nonce BLOB NOT NULL CHECK(typeof(nonce)='blob' AND length(nonce)=32),
    device INTEGER NOT NULL CHECK(device > 0),
    inode INTEGER NOT NULL CHECK(inode > 0),
    state TEXT NOT NULL CHECK(state IN ('prepared','published','completed')),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER conflict_folder_restorations_identity_immutable
BEFORE UPDATE OF operation_id,folder_id,target_relative,stage_relative,nonce,device,inode ON conflict_folder_restorations
BEGIN SELECT RAISE(ABORT, 'conflict folder restoration identity is immutable'); END;
CREATE TRIGGER conflict_folder_restorations_state_monotonic
BEFORE UPDATE OF state ON conflict_folder_restorations
WHEN NOT (NEW.state=OLD.state OR (OLD.state='prepared' AND NEW.state='published') OR (OLD.state='published' AND NEW.state='completed'))
BEGIN SELECT RAISE(ABORT, 'conflict folder restoration state is monotonic'); END;
CREATE TRIGGER conflict_folder_restorations_no_delete
BEFORE DELETE ON conflict_folder_restorations
BEGIN SELECT RAISE(ABORT, 'conflict folder restoration history is immutable'); END;
