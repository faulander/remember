ALTER TABLE sync_conflict_resolutions RENAME TO sync_conflict_resolutions_v13;
CREATE TABLE sync_conflict_resolutions (
    operation_id TEXT PRIMARY KEY,
    resolution TEXT NOT NULL CHECK(resolution IN ('already_deleted','folder_not_empty_preserved','folder_move_reverted','folder_create_collision_recovered')),
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
INSERT INTO sync_conflict_resolutions(operation_id,resolution,created_at_ms)
SELECT operation_id,resolution,created_at_ms FROM sync_conflict_resolutions_v13;
DROP TABLE sync_conflict_resolutions_v13;
CREATE TRIGGER sync_conflict_resolutions_no_update BEFORE UPDATE ON sync_conflict_resolutions
BEGIN SELECT RAISE(ABORT, 'sync conflict resolution is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_no_delete BEFORE DELETE ON sync_conflict_resolutions
BEGIN SELECT RAISE(ABORT, 'sync conflict resolution history is immutable'); END;

CREATE TABLE conflict_folder_create_recoveries (
    operation_id TEXT PRIMARY KEY,
    source_folder_id TEXT NOT NULL,
    recovered_folder_id TEXT NOT NULL UNIQUE,
    source_relative TEXT NOT NULL,
    target_relative TEXT NOT NULL UNIQUE,
    device INTEGER NOT NULL CHECK(device > 0),
    inode INTEGER NOT NULL CHECK(inode > 0),
    state TEXT NOT NULL CHECK(state IN ('prepared','moved','completed')),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_create_recoveries_insert_guard
BEFORE INSERT ON conflict_folder_create_recoveries
WHEN NEW.state<>'prepared' OR NOT EXISTS(
    SELECT 1 FROM sync_outbox o
    WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.source_folder_id
      AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict' AND o.conflict_code='path_collision'
      AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
      AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
      AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))
)
BEGIN SELECT RAISE(ABORT, 'invalid folder create recovery'); END;
CREATE TRIGGER conflict_folder_create_recoveries_identity_immutable
BEFORE UPDATE OF operation_id,source_folder_id,recovered_folder_id,source_relative,target_relative,device,inode ON conflict_folder_create_recoveries
BEGIN SELECT RAISE(ABORT, 'folder create recovery identity is immutable'); END;
CREATE TRIGGER conflict_folder_create_recoveries_state_monotonic BEFORE UPDATE OF state ON conflict_folder_create_recoveries
WHEN NOT (NEW.state=OLD.state OR (OLD.state='prepared' AND NEW.state='moved') OR (OLD.state='moved' AND NEW.state='completed'))
BEGIN SELECT RAISE(ABORT, 'folder create recovery state is monotonic'); END;
CREATE TRIGGER conflict_folder_create_recoveries_no_delete BEFORE DELETE ON conflict_folder_create_recoveries
BEGIN SELECT RAISE(ABORT, 'folder create recovery history is immutable'); END;

CREATE TRIGGER sync_conflict_resolutions_folder_create_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_create_collision_recovered' AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_create_recoveries r JOIN sync_outbox o ON o.operation_id=r.operation_id
    WHERE r.operation_id=NEW.operation_id AND r.source_folder_id=o.object_id AND r.state='completed' AND o.status='conflict'
      AND o.object_type='folder' AND o.mutation='create' AND o.conflict_code='path_collision'
      AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
)
BEGIN SELECT RAISE(ABORT, 'folder create resolution requires completed recovery'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_move_reverted' AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_move_reverts r JOIN sync_outbox o ON o.operation_id=r.operation_id
    WHERE r.operation_id=NEW.operation_id AND r.state='completed' AND o.status='conflict'
      AND o.object_type='folder' AND o.mutation='move'
      AND o.conflict_code IN ('path_collision','parent_unavailable','folder_cycle','base_revision_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'folder move resolution requires completed revert'); END;
