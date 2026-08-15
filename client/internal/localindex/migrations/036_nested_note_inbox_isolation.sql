CREATE VIEW sync_independent_inbox_candidates AS
SELECT i.*
FROM sync_inbox_changes i
WHERE i.state='pending'
  AND i.object_type='note'
  AND i.mutation IN ('update','delete')
  AND EXISTS(
    SELECT 1 FROM sync_baselines baseline
    WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision-1
  )
  AND NOT EXISTS(
    SELECT 1 FROM sync_inbox_changes earlier
    WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied'
  )
  AND NOT EXISTS(
    SELECT 1 FROM sync_unresolved_local_intents unresolved
    WHERE unresolved.object_id=i.object_id
  )
  AND (
    i.parent_id IS NULL
    OR EXISTS(
      SELECT 1
      FROM objects parent
      JOIN sync_baselines parent_baseline ON parent_baseline.object_id=parent.object_id
      WHERE parent.object_id=i.parent_id
        AND parent.object_type='folder'
        AND parent.parent_id IS NULL
        AND parent.identity_state='known'
        AND parent.folder_device>0
        AND parent.folder_inode>0
        AND parent_baseline.revision>0
        AND parent_baseline.operation_id IS NOT NULL
        AND NOT EXISTS(
          SELECT 1 FROM sync_inbox_changes parent_earlier
          WHERE parent_earlier.object_id=parent.object_id
            AND parent_earlier.cursor<i.cursor
            AND parent_earlier.state<>'applied'
        )
        AND NOT EXISTS(
          SELECT 1 FROM sync_unresolved_local_intents parent_unresolved
          WHERE parent_unresolved.object_id=parent.object_id
        )
    )
  );

CREATE TABLE sync_inbox_parent_bindings (
  plan_id TEXT PRIMARY KEY,
  inbox_cursor INTEGER NOT NULL CHECK(inbox_cursor>0),
  parent_id TEXT NOT NULL,
  parent_relative TEXT NOT NULL,
  device INTEGER NOT NULL CHECK(device>0),
  inode INTEGER NOT NULL CHECK(inode>0),
  baseline_revision INTEGER NOT NULL CHECK(baseline_revision>0),
  baseline_operation_id TEXT NOT NULL,
  FOREIGN KEY(plan_id) REFERENCES apply_plans(plan_id) ON DELETE CASCADE,
  FOREIGN KEY(inbox_cursor) REFERENCES sync_inbox_changes(cursor)
) WITHOUT ROWID;

CREATE TRIGGER sync_inbox_parent_binding_insert_guard
BEFORE INSERT ON sync_inbox_parent_bindings
WHEN NOT EXISTS (
  SELECT 1
  FROM apply_plans p
  JOIN sync_inbox_changes i ON i.cursor=NEW.inbox_cursor
  JOIN objects parent ON parent.object_id=i.parent_id
  JOIN sync_baselines baseline ON baseline.object_id=parent.object_id
  WHERE p.plan_id=NEW.plan_id
    AND p.status='prepared'
    AND i.state='pending'
    AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor)
    AND NEW.parent_id=i.parent_id
    AND NEW.parent_relative=parent.relative_path
    AND NEW.device=parent.folder_device
    AND NEW.inode=parent.folder_inode
    AND NEW.baseline_revision=baseline.revision
    AND NEW.baseline_operation_id=baseline.operation_id
)
BEGIN SELECT RAISE(ABORT, 'invalid sync inbox parent binding'); END;

CREATE TRIGGER sync_inbox_parent_binding_no_update
BEFORE UPDATE ON sync_inbox_parent_bindings
BEGIN SELECT RAISE(ABORT, 'sync inbox parent binding is immutable'); END;

CREATE TRIGGER sync_inbox_parent_binding_no_delete
BEFORE DELETE ON sync_inbox_parent_bindings
BEGIN SELECT RAISE(ABORT, 'sync inbox parent binding is immutable'); END;

CREATE VIEW sync_inbox_valid_parent_bindings AS
SELECT binding.*
FROM sync_inbox_parent_bindings binding
JOIN sync_inbox_changes i ON i.cursor=binding.inbox_cursor
JOIN objects parent ON parent.object_id=binding.parent_id
JOIN sync_baselines baseline ON baseline.object_id=binding.parent_id
WHERE i.parent_id=binding.parent_id
  AND i.state IN ('pending','applying','applied')
  AND parent.object_type='folder'
  AND parent.parent_id IS NULL
  AND parent.identity_state='known'
  AND parent.relative_path=binding.parent_relative
  AND parent.folder_device=binding.device
  AND parent.folder_inode=binding.inode
  AND baseline.revision=binding.baseline_revision
  AND baseline.operation_id=binding.baseline_operation_id
  AND NOT EXISTS(
    SELECT 1 FROM sync_inbox_changes parent_earlier
    WHERE parent_earlier.object_id=binding.parent_id
      AND parent_earlier.cursor<binding.inbox_cursor
      AND parent_earlier.state<>'applied'
  )
  AND NOT EXISTS(
    SELECT 1 FROM sync_unresolved_local_intents parent_unresolved
    WHERE parent_unresolved.object_id=binding.parent_id
  );

DROP TRIGGER sync_inbox_apply_plan_insert_guard;
CREATE TRIGGER sync_inbox_apply_plan_insert_guard
BEFORE INSERT ON sync_inbox_apply_plans
WHEN NOT EXISTS (
 SELECT 1 FROM apply_plans p JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 JOIN sync_inbox_changes i ON i.cursor=NEW.cursor
 WHERE p.plan_id=NEW.plan_id AND p.status='prepared' AND p.from_cursor=NEW.cursor-1 AND p.through_cursor=NEW.cursor
 AND (SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)=1
 AND s.cursor=i.cursor AND s.operation_id=i.operation_id AND s.object_id=i.object_id AND s.mutation=i.mutation AND s.object_type=i.object_type AND s.revision=i.revision AND s.parent_id IS i.parent_id AND s.name=i.name AND s.blob_hash IS i.blob_hash
 AND s.state='pending'
 AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor)
 AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=p.plan_id))
      OR (i.parent_id IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=p.plan_id AND binding.inbox_cursor=i.cursor)))
 AND (SELECT count(*) FROM apply_plans active WHERE active.status IN ('prepared','applying'))=1)
BEGIN SELECT RAISE(ABORT, 'invalid sync inbox apply plan link'); END;

DROP TRIGGER sync_inbox_linked_plan_state_guard;
CREATE TRIGGER sync_inbox_linked_plan_state_guard
BEFORE UPDATE OF status,completed_at_ms ON apply_plans
WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id) AND NOT(
 (OLD.status='prepared' AND NEW.status='applying' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applying' AND s.state='pending' AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor))))
 OR (OLD.status='prepared' AND NEW.status='failed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id)))
 OR (OLD.status='failed' AND NEW.status='prepared' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id) AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor) AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor)) AND NOT EXISTS(SELECT 1 FROM apply_plans active WHERE active.plan_id<>OLD.plan_id AND active.status IN ('prepared','applying'))))
 OR (OLD.status='applying' AND NEW.status='completed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applied' AND s.state='applied' AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision AND baseline.operation_id=i.operation_id) AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor))))
)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan state transition is inconsistent'); END;
