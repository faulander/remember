ALTER TABLE sync_conflict_resolutions RENAME TO sync_conflict_resolutions_v19;
CREATE TABLE sync_conflict_resolutions (
 operation_id TEXT PRIMARY KEY,
 resolution TEXT NOT NULL CHECK(resolution IN ('already_deleted','folder_not_empty_preserved','folder_move_reverted','folder_create_collision_recovered','folder_move_deleted_recovered')),
 created_at_ms INTEGER NOT NULL,
 FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
INSERT INTO sync_conflict_resolutions SELECT * FROM sync_conflict_resolutions_v19;
DROP TABLE sync_conflict_resolutions_v19;
CREATE TRIGGER sync_conflict_resolutions_no_update BEFORE UPDATE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT,'sync conflict resolution is immutable'); END;
CREATE TRIGGER sync_conflict_resolutions_no_delete BEFORE DELETE ON sync_conflict_resolutions BEGIN SELECT RAISE(ABORT,'sync conflict resolution history is immutable'); END;

CREATE TABLE conflict_folder_move_delete_recoveries (
 operation_id TEXT PRIMARY KEY,
 folder_id TEXT NOT NULL,
 recovered_folder_id TEXT NOT NULL UNIQUE,
 new_operation_id TEXT NOT NULL UNIQUE,
 attempted_relative TEXT NOT NULL,
 target_relative TEXT NOT NULL UNIQUE,
 device INTEGER NOT NULL CHECK(device>0),
 inode INTEGER NOT NULL CHECK(inode>0),
 canonical_revision INTEGER NOT NULL CHECK(canonical_revision>0),
 state TEXT NOT NULL CHECK(state IN ('prepared','moved','completed')),
 FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_move_delete_recoveries_insert_guard BEFORE INSERT ON conflict_folder_move_delete_recoveries
WHEN NEW.state<>'prepared' OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id
 WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.folder_id AND NEW.recovered_folder_id<>NEW.folder_id AND NEW.new_operation_id<>NEW.operation_id AND NEW.attempted_relative<>'' AND NEW.target_relative<>NEW.attempted_relative AND substr(NEW.target_relative,1,length('_Konflikte/Wiederhergestellt/'))='_Konflikte/Wiederhergestellt/' AND instr(substr(NEW.target_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')=0 AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code='object_deleted'
 AND c.object_type='folder' AND c.deleted=1 AND c.blob_hash IS NULL AND c.revision=NEW.canonical_revision AND c.revision>o.base_revision
 AND i.folder_id=NEW.folder_id AND i.mutation_kind='move' AND i.device=NEW.device AND i.inode=NEW.inode
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))
)
BEGIN SELECT RAISE(ABORT,'invalid folder move/delete recovery'); END;
CREATE TRIGGER conflict_folder_move_delete_recoveries_identity_immutable BEFORE UPDATE OF operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,target_relative,device,inode,canonical_revision ON conflict_folder_move_delete_recoveries BEGIN SELECT RAISE(ABORT,'folder move/delete recovery identity is immutable'); END;
CREATE TRIGGER conflict_folder_move_delete_recoveries_state_monotonic BEFORE UPDATE OF state ON conflict_folder_move_delete_recoveries WHEN NOT(NEW.state=OLD.state OR OLD.state='prepared' AND NEW.state='moved' OR OLD.state='moved' AND NEW.state='completed') BEGIN SELECT RAISE(ABORT,'folder move/delete recovery state is monotonic'); END;
CREATE TRIGGER conflict_folder_move_delete_recoveries_no_delete BEFORE DELETE ON conflict_folder_move_delete_recoveries BEGIN SELECT RAISE(ABORT,'folder move/delete recovery history is immutable'); END;

CREATE TRIGGER sync_conflict_resolutions_already_deleted_guard BEFORE INSERT ON sync_conflict_resolutions WHEN NEW.resolution='already_deleted' AND NOT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=NEW.operation_id AND o.mutation='delete' AND o.status='conflict' AND ((o.conflict_code='object_missing' AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)) OR (o.conflict_code='object_deleted' AND EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id AND c.object_type=o.object_type AND c.deleted=1 AND c.revision>o.base_revision)))) BEGIN SELECT RAISE(ABORT,'already-deleted resolution requires matching conflict'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_guard BEFORE INSERT ON sync_conflict_resolutions WHEN NEW.resolution='folder_move_reverted' AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_reverts r JOIN sync_outbox o ON o.operation_id=r.operation_id WHERE r.operation_id=NEW.operation_id AND r.state='completed' AND o.status='conflict' AND o.object_type='folder' AND o.mutation='move' AND o.conflict_code IN ('path_collision','parent_unavailable','folder_cycle','base_revision_mismatch')) BEGIN SELECT RAISE(ABORT,'folder move resolution requires completed revert'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_create_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_create_collision_recovered' AND NOT EXISTS(
 SELECT 1 FROM conflict_folder_create_recoveries r JOIN sync_outbox o ON o.operation_id=r.operation_id
 WHERE r.operation_id=NEW.operation_id AND r.source_folder_id=o.object_id AND r.state='completed' AND o.status='conflict'
 AND o.object_type='folder' AND o.mutation='create' AND o.conflict_code IN ('path_collision','parent_unavailable')
 AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
 AND NOT EXISTS(
  SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id
  WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict')
 )
 AND NOT EXISTS(
  SELECT 1 FROM conflict_folder_create_note_members n
  JOIN sync_outbox_dependencies d ON d.dependency_operation_id=n.old_operation_id
  JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id
  WHERE n.operation_id=r.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict')
 )
 AND (
  (NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_members n WHERE n.operation_id=r.operation_id)
   AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_roots p WHERE p.operation_id=r.operation_id))
  OR EXISTS(
   SELECT 1 FROM conflict_folder_create_note_roots p JOIN sync_outbox replacement_root ON replacement_root.operation_id=p.new_root_operation_id
   WHERE p.operation_id=r.operation_id AND EXISTS(SELECT 1 FROM conflict_folder_create_note_members manifest_member WHERE manifest_member.operation_id=r.operation_id) AND replacement_root.mutation='create' AND replacement_root.object_id=r.recovered_folder_id
   AND replacement_root.object_type='folder' AND replacement_root.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002'
   AND replacement_root.name=substr(r.target_relative,length('_Konflikte/Wiederhergestellt/')+1) AND replacement_root.status='pending'
   AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_create_note_members n
    WHERE n.operation_id=r.operation_id AND NOT EXISTS(
     SELECT 1 FROM sync_outbox replacement
     WHERE replacement.operation_id=n.new_operation_id AND replacement.mutation='create' AND replacement.object_id=n.note_id
     AND replacement.object_type='note' AND replacement.parent_id=r.recovered_folder_id AND replacement.name=n.name
     AND replacement.blob_hash=n.blob_hash AND replacement.status='pending'
     AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id AND d.dependency_operation_id=p.new_root_operation_id)
     AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=replacement.operation_id AND extra.dependency_operation_id<>p.new_root_operation_id)
    )
   )
  )
 )
)
BEGIN SELECT RAISE(ABORT,'folder create resolution requires completed recovery'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_deleted_guard BEFORE INSERT ON sync_conflict_resolutions WHEN NEW.resolution='folder_move_deleted_recovered' AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries r JOIN sync_outbox o ON o.operation_id=r.operation_id JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox replacement ON replacement.operation_id=r.new_operation_id WHERE r.operation_id=NEW.operation_id AND r.folder_id=o.object_id AND r.recovered_folder_id<>r.folder_id AND r.new_operation_id<>r.operation_id AND r.attempted_relative<>'' AND r.target_relative<>r.attempted_relative AND substr(r.target_relative,1,length('_Konflikte/Wiederhergestellt/'))='_Konflikte/Wiederhergestellt/' AND instr(substr(r.target_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')=0 AND r.state='completed' AND o.status='conflict' AND o.object_type='folder' AND o.mutation='move' AND o.conflict_code='object_deleted' AND c.object_type='folder' AND c.deleted=1 AND c.revision=r.canonical_revision AND c.revision>o.base_revision AND replacement.mutation='create' AND replacement.object_id=r.recovered_folder_id AND replacement.object_type='folder' AND replacement.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002' AND replacement.name=substr(r.target_relative,length('_Konflikte/Wiederhergestellt/')+1) AND replacement.status='pending') BEGIN SELECT RAISE(ABORT,'folder move/delete resolution requires completed recovery'); END;
