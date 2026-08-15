CREATE TABLE conflict_recursive_local_folder_recoveries (
 recovery_kind TEXT NOT NULL CHECK(recovery_kind IN ('folder_create','divergent_move','move_delete')),
 operation_id TEXT NOT NULL,
 new_root_operation_id TEXT NOT NULL UNIQUE,
 member_count INTEGER NOT NULL CHECK(member_count>0),
 sealed INTEGER NOT NULL DEFAULT 0 CHECK(sealed IN (0,1)),
 PRIMARY KEY(recovery_kind,operation_id)
) WITHOUT ROWID;

CREATE TABLE conflict_recursive_local_folder_members (
 recovery_kind TEXT NOT NULL,
 operation_id TEXT NOT NULL,
 object_id TEXT NOT NULL,
 object_type TEXT NOT NULL CHECK(object_type IN ('folder','note')),
 parent_id TEXT NOT NULL,
 name TEXT NOT NULL,
 relative_path TEXT NOT NULL,
 depth INTEGER NOT NULL CHECK(depth>0),
 old_operation_id TEXT NOT NULL UNIQUE,
 new_operation_id TEXT NOT NULL UNIQUE,
 device INTEGER,
 inode INTEGER,
 create_blob_hash BLOB,
 final_blob_hash BLOB,
 PRIMARY KEY(recovery_kind,operation_id,object_id),
 FOREIGN KEY(recovery_kind,operation_id) REFERENCES conflict_recursive_local_folder_recoveries(recovery_kind,operation_id) ON DELETE CASCADE,
 CHECK((object_type='folder' AND device IS NOT NULL AND inode IS NOT NULL AND device>0 AND inode>0 AND create_blob_hash IS NULL AND final_blob_hash IS NULL) OR
       (object_type='note' AND device IS NULL AND inode IS NULL AND typeof(create_blob_hash)='blob' AND length(create_blob_hash)=32 AND typeof(final_blob_hash)='blob' AND length(final_blob_hash)=32))
) WITHOUT ROWID;

CREATE TABLE conflict_recursive_local_folder_note_chains (
 recovery_kind TEXT NOT NULL,
 operation_id TEXT NOT NULL,
 note_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>0),
 old_operation_id TEXT NOT NULL UNIQUE,
 previous_operation_id TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
 PRIMARY KEY(recovery_kind,operation_id,note_id,ordinal),
 FOREIGN KEY(recovery_kind,operation_id,note_id) REFERENCES conflict_recursive_local_folder_members(recovery_kind,operation_id,object_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER conflict_recursive_local_folder_roots_insert_guard
BEFORE INSERT ON conflict_recursive_local_folder_recoveries
WHEN NEW.sealed<>0 OR
 (NEW.recovery_kind='folder_create' AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries r WHERE r.operation_id=NEW.operation_id AND r.state='prepared')) OR
 (NEW.recovery_kind='divergent_move' AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries r WHERE r.operation_id=NEW.operation_id AND r.new_operation_id=NEW.new_root_operation_id AND r.state='prepared')) OR
 (NEW.recovery_kind='move_delete' AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries r WHERE r.operation_id=NEW.operation_id AND r.new_operation_id=NEW.new_root_operation_id AND r.state='prepared'))
BEGIN SELECT RAISE(ABORT,'invalid recursive local folder recovery root'); END;

CREATE TRIGGER conflict_recursive_local_folder_roots_update_guard
BEFORE UPDATE ON conflict_recursive_local_folder_recoveries
WHEN OLD.recovery_kind<>NEW.recovery_kind OR OLD.operation_id<>NEW.operation_id OR OLD.new_root_operation_id<>NEW.new_root_operation_id OR OLD.member_count<>NEW.member_count OR OLD.sealed<>0 OR NEW.sealed<>1 OR
 (SELECT COUNT(*) FROM conflict_recursive_local_folder_members m WHERE m.recovery_kind=OLD.recovery_kind AND m.operation_id=OLD.operation_id)<>OLD.member_count OR
 EXISTS(SELECT 1 FROM conflict_recursive_local_folder_members m
   WHERE m.recovery_kind=OLD.recovery_kind AND m.operation_id=OLD.operation_id AND
    (NOT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=m.old_operation_id AND o.object_id=m.object_id AND o.object_type=m.object_type AND o.mutation='create' AND o.base_revision=0 AND o.parent_id=m.parent_id AND o.name=m.name AND o.status='pending' AND o.attempted_at_ms IS NULL AND ((m.object_type='folder' AND o.blob_hash IS NULL) OR (m.object_type='note' AND o.blob_hash=m.create_blob_hash))) OR
     EXISTS(SELECT 1 FROM sync_baselines b WHERE b.object_id=m.object_id) OR
     (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=m.old_operation_id)<>1 OR
     NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=m.old_operation_id AND d.dependency_operation_id=CASE WHEN m.depth=1 THEN OLD.operation_id ELSE (SELECT p.old_operation_id FROM conflict_recursive_local_folder_members p WHERE p.recovery_kind=m.recovery_kind AND p.operation_id=m.operation_id AND p.object_id=m.parent_id AND p.object_type='folder' AND p.depth=m.depth-1) END))) OR
 EXISTS(SELECT 1 FROM conflict_recursive_local_folder_note_chains c
   JOIN conflict_recursive_local_folder_members m ON m.recovery_kind=c.recovery_kind AND m.operation_id=c.operation_id AND m.object_id=c.note_id
   WHERE c.recovery_kind=OLD.recovery_kind AND c.operation_id=OLD.operation_id AND
    (m.object_type<>'note' OR NOT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=c.old_operation_id AND o.object_id=c.note_id AND o.object_type='note' AND o.mutation='update' AND o.status='pending' AND o.attempted_at_ms IS NULL AND o.blob_hash=c.blob_hash) OR
     (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=c.old_operation_id)<>1 OR
     NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=c.old_operation_id AND d.dependency_operation_id=c.previous_operation_id)))
BEGIN SELECT RAISE(ABORT,'recursive local folder recovery is incomplete'); END;

CREATE TRIGGER conflict_recursive_local_folder_roots_no_delete
BEFORE DELETE ON conflict_recursive_local_folder_recoveries
BEGIN SELECT RAISE(ABORT,'recursive local folder recovery is immutable'); END;

CREATE TRIGGER conflict_recursive_local_folder_members_insert_guard
BEFORE INSERT ON conflict_recursive_local_folder_members
WHEN NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries r WHERE r.recovery_kind=NEW.recovery_kind AND r.operation_id=NEW.operation_id AND r.sealed=0)
BEGIN SELECT RAISE(ABORT,'recursive local folder recovery is sealed'); END;
CREATE TRIGGER conflict_recursive_local_folder_members_no_update BEFORE UPDATE ON conflict_recursive_local_folder_members BEGIN SELECT RAISE(ABORT,'recursive local folder member is immutable'); END;
CREATE TRIGGER conflict_recursive_local_folder_members_no_delete BEFORE DELETE ON conflict_recursive_local_folder_members BEGIN SELECT RAISE(ABORT,'recursive local folder member is immutable'); END;

CREATE TRIGGER conflict_recursive_local_folder_chains_insert_guard
BEFORE INSERT ON conflict_recursive_local_folder_note_chains
WHEN NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries r WHERE r.recovery_kind=NEW.recovery_kind AND r.operation_id=NEW.operation_id AND r.sealed=0) OR
 NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_members m WHERE m.recovery_kind=NEW.recovery_kind AND m.operation_id=NEW.operation_id AND m.object_id=NEW.note_id AND m.object_type='note')
BEGIN SELECT RAISE(ABORT,'invalid recursive local folder note chain'); END;
CREATE TRIGGER conflict_recursive_local_folder_chains_no_update BEFORE UPDATE ON conflict_recursive_local_folder_note_chains BEGIN SELECT RAISE(ABORT,'recursive local folder note chain is immutable'); END;
CREATE TRIGGER conflict_recursive_local_folder_chains_no_delete BEFORE DELETE ON conflict_recursive_local_folder_note_chains BEGIN SELECT RAISE(ABORT,'recursive local folder note chain is immutable'); END;

DROP TRIGGER conflict_folder_create_recoveries_insert_guard;
CREATE TRIGGER conflict_folder_create_recoveries_insert_guard
BEFORE INSERT ON conflict_folder_create_recoveries
WHEN NEW.state<>'prepared' OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.source_folder_id
 AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict'
 AND o.conflict_code IN ('path_collision','parent_unavailable')
 AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
)
BEGIN SELECT RAISE(ABORT,'invalid folder create recovery'); END;

DROP TRIGGER conflict_folder_move_delete_recoveries_insert_guard;
CREATE TRIGGER conflict_folder_move_delete_recoveries_insert_guard BEFORE INSERT ON conflict_folder_move_delete_recoveries
WHEN NEW.state<>'prepared' OR NEW.recovered_folder_id=NEW.folder_id OR NEW.new_operation_id=NEW.operation_id OR NEW.attempted_relative='' OR NEW.target_relative=NEW.attempted_relative OR substr(NEW.target_relative,1,length('_Konflikte/Wiederhergestellt/'))<>'_Konflikte/Wiederhergestellt/' OR instr(substr(NEW.target_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')<>0 OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id
 WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.folder_id AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code='object_deleted'
 AND c.object_type='folder' AND c.deleted=1 AND c.blob_hash IS NULL AND c.revision=NEW.canonical_revision AND c.revision>o.base_revision
 AND i.folder_id=NEW.folder_id AND i.mutation_kind='move' AND i.device=NEW.device AND i.inode=NEW.inode
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
) BEGIN SELECT RAISE(ABORT,'invalid folder move/delete recovery'); END;

DROP TRIGGER conflict_folder_divergent_move_insert_guard;
CREATE TRIGGER conflict_folder_divergent_move_insert_guard BEFORE INSERT ON conflict_folder_divergent_move_recoveries WHEN NEW.state<>'prepared' OR NEW.canonical_device IS NOT NULL OR NEW.canonical_inode IS NOT NULL OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id
 WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.folder_id AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND o.parent_id IS NULL
 AND c.object_type='folder' AND c.deleted=0 AND c.parent_id IS NULL AND c.name<>o.name AND c.name=NEW.canonical_relative AND c.revision=NEW.canonical_revision AND c.revision>o.base_revision AND c.blob_hash IS NULL
 AND i.folder_id=o.object_id AND i.mutation_kind='move' AND i.device=NEW.source_device AND i.inode=NEW.source_inode AND NEW.attempted_relative=o.name
 AND NEW.recovered_folder_id<>NEW.folder_id AND NEW.new_operation_id<>NEW.operation_id AND NEW.canonical_relative<>NEW.attempted_relative
 AND substr(NEW.recovery_relative,1,length('_Konflikte/Wiederhergestellt/'))='_Konflikte/Wiederhergestellt/' AND instr(substr(NEW.recovery_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')=0
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
) BEGIN SELECT RAISE(ABORT,'invalid divergent folder move recovery'); END;

DROP TRIGGER IF EXISTS conflict_folder_move_delete_recoveries_manifest_guard;
CREATE TRIGGER conflict_folder_move_delete_recoveries_manifest_guard BEFORE UPDATE OF state ON conflict_folder_move_delete_recoveries
WHEN NEW.state='moved' AND OLD.state='prepared'
 AND NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries r WHERE r.recovery_kind='move_delete' AND r.operation_id=NEW.operation_id AND r.sealed=1)
 AND (
  (SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=NEW.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))<>(SELECT COUNT(*) FROM conflict_folder_move_delete_note_members n WHERE n.operation_id=NEW.operation_id)
  OR EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=NEW.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict') AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n WHERE n.operation_id=NEW.operation_id AND n.old_operation_id=dependent.operation_id))
 )
BEGIN SELECT RAISE(ABORT,'folder move/delete note manifest incomplete'); END;

DROP TRIGGER IF EXISTS conflict_folder_divergent_move_state_guard;
CREATE TRIGGER conflict_folder_divergent_move_state_guard BEFORE UPDATE OF state,canonical_device,canonical_inode ON conflict_folder_divergent_move_recoveries WHEN NOT(
 (OLD.state='prepared' AND NEW.state='evacuated' AND NEW.canonical_device IS NULL AND NEW.canonical_inode IS NULL AND (
   (EXISTS(SELECT 1 FROM conflict_folder_divergent_move_valid_note_topology valid WHERE valid.operation_id=OLD.operation_id)
    AND (SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=OLD.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))=(SELECT COUNT(*) FROM conflict_folder_divergent_move_note_members n WHERE n.operation_id=OLD.operation_id)
    AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=OLD.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict') AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n WHERE n.operation_id=OLD.operation_id AND n.old_operation_id=dependent.operation_id)))
   OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries r WHERE r.recovery_kind='divergent_move' AND r.operation_id=OLD.operation_id AND r.sealed=1)
  ) AND NOT EXISTS(SELECT 1 FROM sync_outbox later JOIN sync_outbox original ON original.operation_id=OLD.operation_id WHERE later.object_id=OLD.folder_id AND later.sequence>original.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict')))
 OR (OLD.state='evacuated' AND NEW.state='canonical_prepared' AND NEW.canonical_device>0 AND NEW.canonical_inode>0)
 OR (OLD.state='canonical_prepared' AND NEW.state='canonical_published' AND NEW.canonical_device=OLD.canonical_device AND NEW.canonical_inode=OLD.canonical_inode)
 OR (OLD.state='canonical_published' AND NEW.state='completed' AND NEW.canonical_device=OLD.canonical_device AND NEW.canonical_inode=OLD.canonical_inode AND (
   EXISTS(SELECT 1 FROM conflict_folder_divergent_move_replacements_complete complete WHERE complete.operation_id=OLD.operation_id)
   OR EXISTS(
    SELECT 1 FROM conflict_recursive_local_folder_recoveries manifest
    JOIN sync_outbox replacement_root ON replacement_root.operation_id=manifest.new_root_operation_id
    WHERE manifest.recovery_kind='divergent_move' AND manifest.operation_id=OLD.operation_id AND manifest.sealed=1
     AND replacement_root.object_id=OLD.recovered_folder_id AND replacement_root.mutation='create' AND replacement_root.object_type='folder'
     AND replacement_root.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002' AND replacement_root.name=substr(OLD.recovery_relative,length('_Konflikte/Wiederhergestellt/')+1)
     AND replacement_root.status IN ('pending','attempted','accepted')
     AND NOT EXISTS(
      SELECT 1 FROM conflict_recursive_local_folder_members m
      WHERE m.recovery_kind=manifest.recovery_kind AND m.operation_id=manifest.operation_id AND (
       NOT EXISTS(SELECT 1 FROM sync_outbox old WHERE old.operation_id=m.old_operation_id AND old.status='superseded')
       OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_note_chains c JOIN sync_outbox old ON old.operation_id=c.old_operation_id WHERE c.recovery_kind=m.recovery_kind AND c.operation_id=m.operation_id AND c.note_id=m.object_id AND old.status<>'superseded')
       OR NOT EXISTS(
        SELECT 1 FROM sync_outbox replacement
        WHERE replacement.operation_id=m.new_operation_id AND replacement.object_id=m.object_id AND replacement.object_type=m.object_type AND replacement.mutation='create'
         AND replacement.parent_id=CASE WHEN m.depth=1 THEN OLD.recovered_folder_id ELSE m.parent_id END AND replacement.name=m.name
         AND ((m.object_type='folder' AND replacement.blob_hash IS NULL) OR (m.object_type='note' AND replacement.blob_hash=m.final_blob_hash))
         AND replacement.status IN ('pending','attempted','accepted')
         AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id)=1
         AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id AND d.dependency_operation_id=CASE WHEN m.depth=1 THEN manifest.new_root_operation_id ELSE (SELECT p.new_operation_id FROM conflict_recursive_local_folder_members p WHERE p.recovery_kind=m.recovery_kind AND p.operation_id=m.operation_id AND p.object_id=m.parent_id) END)
       )
      )
     )
   )
  ))
) BEGIN SELECT RAISE(ABORT,'invalid divergent folder move recovery transition'); END;

DROP VIEW sync_unresolved_local_intents;
CREATE VIEW sync_unresolved_local_intents AS SELECT DISTINCT o.object_id FROM sync_outbox o WHERE
 (o.status IN('pending','attempted','replay_mismatch')
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_members n JOIN conflict_folder_create_recoveries f ON f.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND f.state IN('moved','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n JOIN conflict_folder_move_delete_recoveries d ON d.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND d.state IN('moved','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n JOIN conflict_folder_divergent_move_recoveries d ON d.operation_id=n.operation_id WHERE (n.old_operation_id=o.operation_id OR EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id AND c.old_operation_id=o.operation_id)) AND d.state IN('evacuated','canonical_published','completed'))
  AND NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_members m JOIN conflict_recursive_local_folder_recoveries manifest ON manifest.recovery_kind=m.recovery_kind AND manifest.operation_id=m.operation_id WHERE (m.old_operation_id=o.operation_id OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_note_chains c WHERE c.recovery_kind=m.recovery_kind AND c.operation_id=m.operation_id AND c.note_id=m.object_id AND c.old_operation_id=o.operation_id)) AND ((m.recovery_kind='folder_create' AND EXISTS(SELECT 1 FROM conflict_folder_create_recoveries r WHERE r.operation_id=m.operation_id AND r.state IN('moved','completed'))) OR (m.recovery_kind='move_delete' AND EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries r WHERE r.operation_id=m.operation_id AND r.state IN('moved','completed'))) OR (m.recovery_kind='divergent_move' AND EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries r WHERE r.operation_id=m.operation_id AND r.state IN('evacuated','canonical_published','completed')))))
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE c.local_delete_operation_id=o.operation_id AND p.state='resolved')
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves n JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=n.conflict_operation_id WHERE n.local_delete_operation_id=o.operation_id AND p.state='resolved'))
 OR (o.status='conflict' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN('copy_staged','copy_published','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries f WHERE f.operation_id=o.operation_id AND f.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('evacuated','canonical_published','completed')) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=o.operation_id AND p.state='resolved'));

DROP TRIGGER IF EXISTS sync_conflict_resolutions_recursive_local_folder_guard;
CREATE TRIGGER sync_conflict_resolutions_recursive_local_folder_guard
BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution IN ('folder_create_collision_recovered','folder_move_deleted_recovered')
 AND EXISTS(
  SELECT 1 FROM conflict_recursive_local_folder_recoveries manifest
  WHERE manifest.operation_id=NEW.operation_id
   AND ((NEW.resolution='folder_create_collision_recovered' AND manifest.recovery_kind='folder_create') OR (NEW.resolution='folder_move_deleted_recovered' AND manifest.recovery_kind='move_delete'))
   AND (
    NOT EXISTS(
     SELECT 1 FROM sync_outbox replacement_root
     WHERE replacement_root.operation_id=manifest.new_root_operation_id AND replacement_root.mutation='create' AND replacement_root.object_type='folder'
      AND replacement_root.object_id=CASE manifest.recovery_kind WHEN 'folder_create' THEN (SELECT r.recovered_folder_id FROM conflict_folder_create_recoveries r WHERE r.operation_id=manifest.operation_id AND r.state='completed') ELSE (SELECT r.recovered_folder_id FROM conflict_folder_move_delete_recoveries r WHERE r.operation_id=manifest.operation_id AND r.state='completed') END
      AND replacement_root.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002'
      AND replacement_root.name=CASE manifest.recovery_kind WHEN 'folder_create' THEN (SELECT substr(r.target_relative,length('_Konflikte/Wiederhergestellt/')+1) FROM conflict_folder_create_recoveries r WHERE r.operation_id=manifest.operation_id) ELSE (SELECT substr(r.target_relative,length('_Konflikte/Wiederhergestellt/')+1) FROM conflict_folder_move_delete_recoveries r WHERE r.operation_id=manifest.operation_id) END
      AND replacement_root.status IN ('pending','attempted','accepted')
    )
    OR EXISTS(
     SELECT 1 FROM conflict_recursive_local_folder_members m
     WHERE m.recovery_kind=manifest.recovery_kind AND m.operation_id=manifest.operation_id AND (
      NOT EXISTS(SELECT 1 FROM sync_outbox old WHERE old.operation_id=m.old_operation_id AND old.status='superseded')
      OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_note_chains c JOIN sync_outbox old ON old.operation_id=c.old_operation_id WHERE c.recovery_kind=m.recovery_kind AND c.operation_id=m.operation_id AND c.note_id=m.object_id AND old.status<>'superseded')
      OR NOT EXISTS(
       SELECT 1 FROM sync_outbox replacement
       WHERE replacement.operation_id=m.new_operation_id AND replacement.object_id=m.object_id AND replacement.object_type=m.object_type AND replacement.mutation='create'
        AND replacement.parent_id=CASE WHEN m.depth=1 THEN CASE manifest.recovery_kind WHEN 'folder_create' THEN (SELECT r.recovered_folder_id FROM conflict_folder_create_recoveries r WHERE r.operation_id=manifest.operation_id) ELSE (SELECT r.recovered_folder_id FROM conflict_folder_move_delete_recoveries r WHERE r.operation_id=manifest.operation_id) END ELSE m.parent_id END
        AND replacement.name=m.name AND ((m.object_type='folder' AND replacement.blob_hash IS NULL) OR (m.object_type='note' AND replacement.blob_hash=m.final_blob_hash))
        AND replacement.status IN ('pending','attempted','accepted')
        AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id)=1
        AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id AND d.dependency_operation_id=CASE WHEN m.depth=1 THEN manifest.new_root_operation_id ELSE (SELECT p.new_operation_id FROM conflict_recursive_local_folder_members p WHERE p.recovery_kind=m.recovery_kind AND p.operation_id=m.operation_id AND p.object_id=m.parent_id) END)
      )
     )
    )
   )
 )
BEGIN SELECT RAISE(ABORT,'recursive local folder replacement mismatch'); END;
