DROP TRIGGER conflict_folder_create_recoveries_insert_guard;
CREATE TRIGGER conflict_folder_create_recoveries_insert_guard
BEFORE INSERT ON conflict_folder_create_recoveries
WHEN NEW.state<>'prepared' OR NOT EXISTS(
    SELECT 1 FROM sync_outbox o
    WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.source_folder_id
      AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict'
      AND o.conflict_code IN ('path_collision','parent_unavailable')
      AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
      AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
      AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))
)
BEGIN SELECT RAISE(ABORT, 'invalid folder create recovery'); END;

DROP TRIGGER sync_conflict_resolutions_folder_create_guard;
CREATE TRIGGER sync_conflict_resolutions_folder_create_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_create_collision_recovered' AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_create_recoveries r JOIN sync_outbox o ON o.operation_id=r.operation_id
    WHERE r.operation_id=NEW.operation_id AND r.source_folder_id=o.object_id AND r.state='completed' AND o.status='conflict'
      AND o.object_type='folder' AND o.mutation='create'
      AND o.conflict_code IN ('path_collision','parent_unavailable')
      AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
)
BEGIN SELECT RAISE(ABORT, 'folder create resolution requires completed recovery'); END;
