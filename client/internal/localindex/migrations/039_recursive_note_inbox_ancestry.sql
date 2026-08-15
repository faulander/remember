DROP TRIGGER IF EXISTS sync_inbox_apply_plan_insert_guard;
DROP TRIGGER IF EXISTS sync_inbox_linked_plan_state_guard;
DROP TRIGGER IF EXISTS sync_inbox_parent_binding_insert_guard;
DROP TRIGGER IF EXISTS sync_inbox_parent_binding_no_update;
DROP TRIGGER IF EXISTS sync_inbox_parent_binding_no_delete;
DROP VIEW IF EXISTS sync_inbox_valid_parent_bindings;
DROP VIEW IF EXISTS sync_independent_inbox_candidates;

ALTER TABLE sync_inbox_parent_bindings RENAME TO sync_inbox_parent_bindings_v36;

CREATE TABLE sync_inbox_parent_bindings (
  plan_id TEXT NOT NULL,
  inbox_cursor INTEGER NOT NULL CHECK(inbox_cursor>0),
  depth INTEGER NOT NULL CHECK(depth BETWEEN 1 AND 256),
  ancestor_id TEXT NOT NULL,
  ancestor_parent_id TEXT,
  ancestor_relative TEXT NOT NULL,
  device INTEGER NOT NULL CHECK(device>0),
  inode INTEGER NOT NULL CHECK(inode>0),
  baseline_revision INTEGER NOT NULL CHECK(baseline_revision>0),
  baseline_operation_id TEXT NOT NULL CHECK(baseline_operation_id<>'' AND baseline_operation_id<>'00000000-0000-0000-0000-000000000000'),
  PRIMARY KEY(plan_id,depth),
  UNIQUE(plan_id,ancestor_id),
  FOREIGN KEY(plan_id) REFERENCES apply_plans(plan_id) ON DELETE CASCADE,
  FOREIGN KEY(inbox_cursor) REFERENCES sync_inbox_changes(cursor)
) WITHOUT ROWID;

-- Migration 036 admitted only direct children of root Folders. Preserve those
-- immutable observations as a one-row ancestry chain without consulting mutable
-- current object metadata.
INSERT INTO sync_inbox_parent_bindings(
  plan_id,inbox_cursor,depth,ancestor_id,ancestor_parent_id,ancestor_relative,
  device,inode,baseline_revision,baseline_operation_id
)
SELECT plan_id,inbox_cursor,1,parent_id,NULL,parent_relative,
       device,inode,baseline_revision,baseline_operation_id
FROM sync_inbox_parent_bindings_v36;

DROP TABLE sync_inbox_parent_bindings_v36;

CREATE VIEW sync_inbox_note_ancestry AS
WITH RECURSIVE ancestry(
  inbox_cursor,depth,ancestor_id,ancestor_parent_id,ancestor_relative,
  device,inode,identity_state,object_type,baseline_revision,
  baseline_operation_id,ancestor_ids
) AS (
  SELECT i.cursor,1,ancestor.object_id,ancestor.parent_id,ancestor.relative_path,
         ancestor.folder_device,ancestor.folder_inode,ancestor.identity_state,
         ancestor.object_type,baseline.revision,baseline.operation_id,
         '|' || ancestor.object_id || '|'
  FROM sync_inbox_changes i
  JOIN objects ancestor ON ancestor.object_id=i.parent_id
  LEFT JOIN sync_baselines baseline ON baseline.object_id=ancestor.object_id
  WHERE i.object_type='note' AND i.parent_id IS NOT NULL

  UNION ALL

  SELECT chain.inbox_cursor,chain.depth+1,ancestor.object_id,ancestor.parent_id,
         ancestor.relative_path,ancestor.folder_device,ancestor.folder_inode,
         ancestor.identity_state,ancestor.object_type,baseline.revision,
         baseline.operation_id,chain.ancestor_ids || ancestor.object_id || '|'
  FROM ancestry chain
  JOIN objects ancestor ON ancestor.object_id=chain.ancestor_parent_id
  LEFT JOIN sync_baselines baseline ON baseline.object_id=ancestor.object_id
  WHERE chain.depth<256
    AND instr(chain.ancestor_ids, '|' || ancestor.object_id || '|')=0
)
SELECT inbox_cursor,depth,ancestor_id,ancestor_parent_id,ancestor_relative,
       device,inode,identity_state,object_type,baseline_revision,
       baseline_operation_id
FROM ancestry;

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
    WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor
      AND earlier.state<>'applied'
  )
  AND NOT EXISTS(
    SELECT 1 FROM sync_unresolved_local_intents unresolved
    WHERE unresolved.object_id=i.object_id
  )
  AND (
    i.parent_id IS NULL
    OR (
      EXISTS(
        SELECT 1 FROM sync_inbox_note_ancestry root
        WHERE root.inbox_cursor=i.cursor AND root.ancestor_parent_id IS NULL
      )
      AND NOT EXISTS(
        SELECT 1
        FROM sync_inbox_note_ancestry ancestor
        WHERE ancestor.inbox_cursor=i.cursor
          AND (
            ancestor.object_type<>'folder'
            OR ancestor.identity_state<>'known'
            OR ancestor.device IS NULL OR ancestor.device<=0
            OR ancestor.inode IS NULL OR ancestor.inode<=0
            OR ancestor.baseline_revision IS NULL OR ancestor.baseline_revision<=0
            OR ancestor.baseline_operation_id IS NULL
            OR ancestor.baseline_operation_id=''
            OR ancestor.baseline_operation_id='00000000-0000-0000-0000-000000000000'
            OR EXISTS(
              SELECT 1 FROM sync_inbox_changes ancestor_earlier
              WHERE ancestor_earlier.object_id=ancestor.ancestor_id
                AND ancestor_earlier.cursor<i.cursor
                AND ancestor_earlier.state<>'applied'
            )
            OR EXISTS(
              SELECT 1 FROM sync_unresolved_local_intents ancestor_unresolved
              WHERE ancestor_unresolved.object_id=ancestor.ancestor_id
            )
          )
      )
    )
  );

CREATE TRIGGER sync_inbox_parent_binding_insert_guard
BEFORE INSERT ON sync_inbox_parent_bindings
WHEN NOT EXISTS (
  SELECT 1
  FROM apply_plans plan
  JOIN sync_inbox_changes i ON i.cursor=NEW.inbox_cursor
  JOIN sync_inbox_note_ancestry ancestor
    ON ancestor.inbox_cursor=i.cursor AND ancestor.depth=NEW.depth
  WHERE plan.plan_id=NEW.plan_id
    AND plan.status='prepared'
    AND i.state='pending'
    AND EXISTS(
      SELECT 1 FROM sync_independent_inbox_candidates eligible
      WHERE eligible.cursor=i.cursor
    )
    AND NEW.ancestor_id=ancestor.ancestor_id
    AND NEW.ancestor_parent_id IS ancestor.ancestor_parent_id
    AND NEW.ancestor_relative=ancestor.ancestor_relative
    AND NEW.device=ancestor.device
    AND NEW.inode=ancestor.inode
    AND NEW.baseline_revision=ancestor.baseline_revision
    AND NEW.baseline_operation_id=ancestor.baseline_operation_id
    AND NOT EXISTS(
      SELECT 1 FROM sync_inbox_parent_bindings existing
      WHERE existing.plan_id=NEW.plan_id
        AND existing.inbox_cursor<>NEW.inbox_cursor
    )
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
JOIN sync_inbox_note_ancestry ancestor
  ON ancestor.inbox_cursor=binding.inbox_cursor
 AND ancestor.depth=binding.depth
 AND ancestor.ancestor_id=binding.ancestor_id
 AND ancestor.ancestor_parent_id IS binding.ancestor_parent_id
 AND ancestor.ancestor_relative=binding.ancestor_relative
 AND ancestor.device=binding.device
 AND ancestor.inode=binding.inode
 AND ancestor.baseline_revision=binding.baseline_revision
 AND ancestor.baseline_operation_id=binding.baseline_operation_id
WHERE i.state IN ('pending','applying','applied')
  AND ancestor.object_type='folder'
  AND ancestor.identity_state='known'
  AND ancestor.device>0
  AND ancestor.inode>0
  AND ancestor.baseline_revision>0
  AND ancestor.baseline_operation_id<>''
  AND ancestor.baseline_operation_id<>'00000000-0000-0000-0000-000000000000'
  AND NOT EXISTS(
    SELECT 1 FROM sync_inbox_changes ancestor_earlier
    WHERE ancestor_earlier.object_id=binding.ancestor_id
      AND ancestor_earlier.cursor<binding.inbox_cursor
      AND ancestor_earlier.state<>'applied'
  )
  AND NOT EXISTS(
    SELECT 1 FROM sync_unresolved_local_intents ancestor_unresolved
    WHERE ancestor_unresolved.object_id=binding.ancestor_id
  );

CREATE VIEW sync_inbox_valid_nested_bindings AS
SELECT binding.plan_id,binding.inbox_cursor
FROM sync_inbox_parent_bindings binding
JOIN sync_inbox_changes i ON i.cursor=binding.inbox_cursor
WHERE i.parent_id IS NOT NULL
  AND EXISTS(
    SELECT 1 FROM sync_inbox_note_ancestry root
    WHERE root.inbox_cursor=binding.inbox_cursor
      AND root.ancestor_parent_id IS NULL
  )
  AND EXISTS(
    SELECT 1 FROM sync_inbox_parent_bindings immediate
    WHERE immediate.plan_id=binding.plan_id
      AND immediate.inbox_cursor=binding.inbox_cursor
      AND immediate.depth=1
      AND immediate.ancestor_id=i.parent_id
  )
GROUP BY binding.plan_id,binding.inbox_cursor
HAVING count(*)=(
    SELECT count(*) FROM sync_inbox_note_ancestry current_chain
    WHERE current_chain.inbox_cursor=binding.inbox_cursor
  )
  AND count(*)=(
    SELECT count(*) FROM sync_inbox_valid_parent_bindings valid
    WHERE valid.plan_id=binding.plan_id
      AND valid.inbox_cursor=binding.inbox_cursor
  );

CREATE TRIGGER sync_inbox_apply_plan_insert_guard
BEFORE INSERT ON sync_inbox_apply_plans
WHEN NOT EXISTS (
  SELECT 1
  FROM apply_plans plan
  JOIN apply_steps step ON step.plan_id=plan.plan_id AND step.step_index=0
  JOIN sync_inbox_changes i ON i.cursor=NEW.cursor
  WHERE plan.plan_id=NEW.plan_id
    AND plan.status='prepared'
    AND plan.from_cursor=NEW.cursor-1
    AND plan.through_cursor=NEW.cursor
    AND (SELECT count(*) FROM apply_steps other WHERE other.plan_id=plan.plan_id)=1
    AND step.cursor=i.cursor
    AND step.operation_id=i.operation_id
    AND step.object_id=i.object_id
    AND step.mutation=i.mutation
    AND step.object_type=i.object_type
    AND step.revision=i.revision
    AND step.parent_id IS i.parent_id
    AND step.name=i.name
    AND step.blob_hash IS i.blob_hash
    AND step.state='pending'
    AND EXISTS(
      SELECT 1 FROM sync_independent_inbox_candidates eligible
      WHERE eligible.cursor=i.cursor
    )
    AND (
      (i.parent_id IS NULL AND NOT EXISTS(
        SELECT 1 FROM sync_inbox_parent_bindings binding
        WHERE binding.plan_id=plan.plan_id
      ))
      OR
      (i.parent_id IS NOT NULL AND EXISTS(
        SELECT 1 FROM sync_inbox_valid_nested_bindings valid
        WHERE valid.plan_id=plan.plan_id AND valid.inbox_cursor=i.cursor
      ))
    )
    AND (SELECT count(*) FROM apply_plans active
         WHERE active.status IN ('prepared','applying'))=1
)
BEGIN SELECT RAISE(ABORT, 'invalid sync inbox apply plan link'); END;

CREATE TRIGGER sync_inbox_linked_plan_state_guard
BEFORE UPDATE OF status,completed_at_ms ON apply_plans
WHEN EXISTS(
  SELECT 1 FROM sync_inbox_apply_plans link WHERE link.plan_id=OLD.plan_id
) AND NOT (
  (
    OLD.status='prepared' AND NEW.status='applying'
    AND NEW.completed_at_ms IS NULL
    AND EXISTS(
      SELECT 1
      FROM sync_inbox_apply_plans link
      JOIN sync_inbox_changes i ON i.cursor=link.cursor
      JOIN apply_steps step ON step.plan_id=OLD.plan_id AND step.step_index=0
      WHERE link.plan_id=OLD.plan_id
        AND i.state='applying'
        AND step.state='pending'
        AND (
          (i.parent_id IS NULL AND NOT EXISTS(
            SELECT 1 FROM sync_inbox_parent_bindings binding
            WHERE binding.plan_id=OLD.plan_id
          ))
          OR
          (i.parent_id IS NOT NULL AND EXISTS(
            SELECT 1 FROM sync_inbox_valid_nested_bindings valid
            WHERE valid.plan_id=OLD.plan_id AND valid.inbox_cursor=i.cursor
          ))
        )
    )
  )
  OR
  (
    OLD.status='prepared' AND NEW.status='failed'
    AND NEW.completed_at_ms IS NOT NULL
    AND EXISTS(
      SELECT 1
      FROM sync_inbox_apply_plans link
      JOIN sync_inbox_changes i ON i.cursor=link.cursor
      JOIN apply_steps step ON step.plan_id=OLD.plan_id AND step.step_index=0
      WHERE link.plan_id=OLD.plan_id
        AND i.state='pending'
        AND step.state='pending'
        AND NOT EXISTS(
          SELECT 1 FROM apply_folder_publications publication
          WHERE publication.plan_id=OLD.plan_id
        )
        AND NOT EXISTS(
          SELECT 1 FROM apply_folder_mutations mutation
          WHERE mutation.plan_id=OLD.plan_id
        )
    )
  )
  OR
  (
    OLD.status='failed' AND NEW.status='prepared'
    AND NEW.completed_at_ms IS NULL
    AND EXISTS(
      SELECT 1
      FROM sync_inbox_apply_plans link
      JOIN sync_inbox_changes i ON i.cursor=link.cursor
      JOIN apply_steps step ON step.plan_id=OLD.plan_id AND step.step_index=0
      WHERE link.plan_id=OLD.plan_id
        AND i.state='pending'
        AND step.state='pending'
        AND NOT EXISTS(
          SELECT 1 FROM apply_folder_publications publication
          WHERE publication.plan_id=OLD.plan_id
        )
        AND NOT EXISTS(
          SELECT 1 FROM apply_folder_mutations mutation
          WHERE mutation.plan_id=OLD.plan_id
        )
        AND EXISTS(
          SELECT 1 FROM sync_independent_inbox_candidates eligible
          WHERE eligible.cursor=i.cursor
        )
        AND (
          (i.parent_id IS NULL AND NOT EXISTS(
            SELECT 1 FROM sync_inbox_parent_bindings binding
            WHERE binding.plan_id=OLD.plan_id
          ))
          OR
          (i.parent_id IS NOT NULL AND EXISTS(
            SELECT 1 FROM sync_inbox_valid_nested_bindings valid
            WHERE valid.plan_id=OLD.plan_id AND valid.inbox_cursor=i.cursor
          ))
        )
    )
  )
  OR
  (
    OLD.status='applying' AND NEW.status='completed'
    AND NEW.completed_at_ms IS NOT NULL
    AND EXISTS(
      SELECT 1
      FROM sync_inbox_apply_plans link
      JOIN sync_inbox_changes i ON i.cursor=link.cursor
      JOIN apply_steps step ON step.plan_id=OLD.plan_id AND step.step_index=0
      WHERE link.plan_id=OLD.plan_id
        AND i.state='applied'
        AND step.state='applied'
        AND EXISTS(
          SELECT 1 FROM sync_baselines baseline
          WHERE baseline.object_id=i.object_id
            AND baseline.revision=i.revision
            AND baseline.operation_id=i.operation_id
        )
        AND (
          (i.parent_id IS NULL AND NOT EXISTS(
            SELECT 1 FROM sync_inbox_parent_bindings binding
            WHERE binding.plan_id=OLD.plan_id
          ))
          OR
          (i.parent_id IS NOT NULL AND EXISTS(
            SELECT 1 FROM sync_inbox_valid_nested_bindings valid
            WHERE valid.plan_id=OLD.plan_id AND valid.inbox_cursor=i.cursor
          ))
        )
    )
  )
)
BEGIN SELECT RAISE(ABORT, 'linked inbox apply plan state transition is inconsistent'); END;
