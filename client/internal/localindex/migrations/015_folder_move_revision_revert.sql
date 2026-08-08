DROP TRIGGER sync_conflict_resolutions_folder_move_guard;
CREATE TRIGGER sync_conflict_resolutions_folder_move_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_move_reverted' AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_move_reverts r JOIN sync_outbox o ON o.operation_id=r.operation_id
    WHERE r.operation_id=NEW.operation_id AND r.state='completed' AND o.status='conflict'
      AND o.object_type='folder' AND o.mutation='move'
      AND o.conflict_code IN ('path_collision','parent_unavailable','folder_cycle','base_revision_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'folder move resolution requires completed revert'); END;
