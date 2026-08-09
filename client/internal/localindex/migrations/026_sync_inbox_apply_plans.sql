CREATE TABLE sync_inbox_apply_plans (
    plan_id TEXT PRIMARY KEY,
    cursor INTEGER NOT NULL UNIQUE CHECK(cursor > 0),
    FOREIGN KEY(plan_id) REFERENCES apply_plans(plan_id),
    FOREIGN KEY(cursor) REFERENCES sync_inbox_changes(cursor)
);

CREATE TRIGGER sync_inbox_apply_plan_conflict_guard
BEFORE INSERT ON sync_inbox_apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans existing WHERE existing.plan_id=NEW.plan_id OR existing.cursor=NEW.cursor)
BEGIN SELECT RAISE(ABORT, 'sync inbox apply plan link cannot be replaced'); END;

CREATE TRIGGER sync_inbox_apply_plan_insert_guard
BEFORE INSERT ON sync_inbox_apply_plans
WHEN NOT EXISTS (
    SELECT 1
    FROM apply_plans p
    JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0
    JOIN sync_inbox_changes i ON i.cursor=NEW.cursor
    WHERE p.plan_id=NEW.plan_id
      AND p.status='prepared'
      AND p.from_cursor=NEW.cursor-1
      AND p.through_cursor=NEW.cursor
      AND (SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)=1
      AND s.cursor=i.cursor
      AND s.operation_id=i.operation_id
      AND s.object_id=i.object_id
      AND s.mutation=i.mutation
      AND s.object_type=i.object_type
      AND s.revision=i.revision
      AND s.parent_id IS i.parent_id
      AND s.name=i.name
      AND s.blob_hash IS i.blob_hash
      AND s.state='pending'
      AND i.state='pending'
      AND i.object_type='note' AND i.parent_id IS NULL AND i.mutation IN ('update','delete')
      AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision-1)
      AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes earlier WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied')
      AND (SELECT count(*) FROM apply_plans active WHERE active.status IN ('prepared','applying'))=1
)
BEGIN SELECT RAISE(ABORT, 'invalid sync inbox apply plan link'); END;

CREATE TRIGGER sync_inbox_apply_plan_immutable
BEFORE UPDATE ON sync_inbox_apply_plans
BEGIN SELECT RAISE(ABORT, 'sync inbox apply plan link is immutable'); END;
CREATE TRIGGER sync_inbox_apply_plan_no_delete
BEFORE DELETE ON sync_inbox_apply_plans
BEGIN SELECT RAISE(ABORT, 'sync inbox apply plan link is immutable'); END;

CREATE TRIGGER sync_inbox_linked_inbox_state_guard
BEFORE UPDATE OF state,applying_at_ms,applied_at_ms ON sync_inbox_changes
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.cursor=OLD.cursor) AND NOT(
 (OLD.state='pending' AND NEW.state='applying' AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN apply_plans p ON p.plan_id=l.plan_id JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 WHERE l.cursor=OLD.cursor AND p.status='prepared' AND s.state='pending'))
 OR (OLD.state='applying' AND NEW.state='applied' AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN apply_plans p ON p.plan_id=l.plan_id JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 WHERE l.cursor=OLD.cursor AND p.status='applying' AND s.state='applied' AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=OLD.object_id AND baseline.revision=OLD.revision AND baseline.operation_id=OLD.operation_id)))
)
BEGIN SELECT RAISE(ABORT, 'linked inbox state transition is inconsistent'); END;

CREATE TRIGGER sync_inbox_linked_plan_state_guard
BEFORE UPDATE OF status,completed_at_ms ON apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id) AND NOT(
 (OLD.status='prepared' AND NEW.status='applying' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applying' AND s.state='pending'))
 OR (OLD.status='prepared' AND NEW.status='failed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id)))
 OR (OLD.status='applying' AND NEW.status='completed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applied' AND s.state='applied' AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision AND baseline.operation_id=i.operation_id)))
)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan state transition is inconsistent'); END;

CREATE TRIGGER sync_inbox_linked_step_state_guard
BEFORE UPDATE OF state ON apply_steps
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id) AND NOT(
 OLD.state='pending' AND NEW.state='applied' AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN apply_plans p ON p.plan_id=l.plan_id JOIN sync_inbox_changes i ON i.cursor=l.cursor WHERE l.plan_id=OLD.plan_id AND p.status='applying' AND i.state='applying')
)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply step state transition is inconsistent'); END;

CREATE TRIGGER sync_inbox_linked_plan_payload_immutable
BEFORE UPDATE OF plan_id,from_cursor,through_cursor,created_at_ms
ON apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan payload is immutable'); END;
CREATE TRIGGER sync_inbox_linked_plan_no_delete
BEFORE DELETE ON apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan is immutable'); END;

CREATE TRIGGER sync_inbox_linked_step_no_insert
BEFORE INSERT ON apply_steps
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=NEW.plan_id)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan has exactly one step'); END;
CREATE TRIGGER sync_inbox_linked_step_payload_immutable
BEFORE UPDATE OF plan_id,step_index,cursor,operation_id,object_id,mutation,object_type,revision,parent_id,name,blob_hash
ON apply_steps
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply step payload is immutable'); END;
CREATE TRIGGER sync_inbox_linked_step_no_delete
BEFORE DELETE ON apply_steps
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply step is immutable'); END;
