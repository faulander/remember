CREATE VIEW sync_unresolved_local_intents AS
SELECT DISTINCT o.object_id
FROM sync_outbox o
WHERE
 (o.status IN ('pending','attempted','replay_mismatch')
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_members n JOIN conflict_folder_create_recoveries f ON f.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND f.state IN ('moved','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n JOIN conflict_folder_move_delete_recoveries d ON d.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND d.state IN ('moved','completed')))
 OR
 (o.status='conflict'
  AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=o.operation_id)
  AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN ('copy_staged','copy_published','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries f WHERE f.operation_id=o.operation_id AND f.state IN ('moved','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN ('moved','completed')));

DROP TRIGGER sync_inbox_apply_plan_insert_guard;
CREATE TRIGGER sync_inbox_apply_plan_insert_guard
BEFORE INSERT ON sync_inbox_apply_plans
WHEN NOT EXISTS (
 SELECT 1 FROM apply_plans p JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 JOIN sync_inbox_changes i ON i.cursor=NEW.cursor
 WHERE p.plan_id=NEW.plan_id AND p.status='prepared' AND p.from_cursor=NEW.cursor-1 AND p.through_cursor=NEW.cursor
 AND (SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)=1
 AND s.cursor=i.cursor AND s.operation_id=i.operation_id AND s.object_id=i.object_id AND s.mutation=i.mutation AND s.object_type=i.object_type AND s.revision=i.revision AND s.parent_id IS i.parent_id AND s.name=i.name AND s.blob_hash IS i.blob_hash
 AND s.state='pending' AND i.state='pending' AND i.object_type='note' AND i.parent_id IS NULL AND i.mutation IN ('update','delete')
 AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision-1)
 AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes earlier WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied')
 AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents unresolved WHERE unresolved.object_id=i.object_id)
 AND (SELECT count(*) FROM apply_plans active WHERE active.status IN ('prepared','applying'))=1)
BEGIN SELECT RAISE(ABORT, 'invalid sync inbox apply plan link'); END;

DROP TRIGGER sync_inbox_linked_plan_state_guard;
CREATE TRIGGER sync_inbox_linked_plan_state_guard
BEFORE UPDATE OF status,completed_at_ms ON apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id) AND NOT(
 (OLD.status='prepared' AND NEW.status='applying' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applying' AND s.state='pending'))
 OR (OLD.status='prepared' AND NEW.status='failed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id)))
 OR (OLD.status='failed' AND NEW.status='prepared' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id) AND EXISTS(SELECT 1 FROM sync_baselines b WHERE b.object_id=i.object_id AND b.revision=i.revision-1) AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes earlier WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied') AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents unresolved WHERE unresolved.object_id=i.object_id) AND NOT EXISTS(SELECT 1 FROM apply_plans active WHERE active.plan_id<>OLD.plan_id AND active.status IN ('prepared','applying'))))
 OR (OLD.status='applying' AND NEW.status='completed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applied' AND s.state='applied' AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision AND baseline.operation_id=i.operation_id)))
)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan state transition is inconsistent'); END;
