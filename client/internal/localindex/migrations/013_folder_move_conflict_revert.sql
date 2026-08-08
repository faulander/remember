ALTER TABLE sync_conflict_resolutions RENAME TO sync_conflict_resolutions_v11;
CREATE TABLE sync_conflict_resolutions (
    operation_id TEXT PRIMARY KEY,
    resolution TEXT NOT NULL CHECK(resolution IN ('already_deleted','folder_not_empty_preserved','folder_move_reverted')),
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms)
SELECT operation_id,resolution,created_at_ms FROM sync_conflict_resolutions_v11;
DROP TABLE sync_conflict_resolutions_v11;
CREATE TRIGGER sync_conflict_resolutions_no_update BEFORE UPDATE ON sync_conflict_resolutions
BEGIN SELECT RAISE(ABORT, 'sync conflict resolution is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_no_delete BEFORE DELETE ON sync_conflict_resolutions
BEGIN SELECT RAISE(ABORT, 'sync conflict resolution history is immutable'); END;

CREATE TABLE conflict_folder_move_reverts (
    operation_id TEXT PRIMARY KEY,
    folder_id TEXT NOT NULL,
    attempted_relative TEXT NOT NULL,
    canonical_relative TEXT NOT NULL,
    device INTEGER NOT NULL CHECK(device > 0),
    inode INTEGER NOT NULL CHECK(inode > 0),
    state TEXT NOT NULL CHECK(state IN ('prepared','moved','completed')),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_move_reverts_identity_immutable
BEFORE UPDATE OF operation_id,folder_id,attempted_relative,canonical_relative,device,inode ON conflict_folder_move_reverts
BEGIN SELECT RAISE(ABORT, 'folder move revert identity is immutable'); END;
CREATE TRIGGER conflict_folder_move_reverts_state_monotonic BEFORE UPDATE OF state ON conflict_folder_move_reverts
WHEN NOT (NEW.state=OLD.state OR (OLD.state='prepared' AND NEW.state='moved') OR (OLD.state='moved' AND NEW.state='completed'))
BEGIN SELECT RAISE(ABORT, 'folder move revert state is monotonic'); END;
CREATE TRIGGER conflict_folder_move_reverts_no_delete BEFORE DELETE ON conflict_folder_move_reverts
BEGIN SELECT RAISE(ABORT, 'folder move revert history is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_move_reverted' AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_move_reverts r JOIN sync_outbox o ON o.operation_id=r.operation_id
    WHERE r.operation_id=NEW.operation_id AND r.state='completed' AND o.status='conflict'
      AND o.object_type='folder' AND o.mutation='move' AND o.conflict_code IN ('path_collision','parent_unavailable')
)
BEGIN SELECT RAISE(ABORT, 'folder move resolution requires completed revert'); END;
