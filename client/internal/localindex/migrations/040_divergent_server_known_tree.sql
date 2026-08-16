CREATE TABLE conflict_folder_divergent_tree_manifests(
 operation_id TEXT PRIMARY KEY,
 new_root_operation_id TEXT NOT NULL UNIQUE,
 member_count INTEGER NOT NULL CHECK(member_count>0 AND member_count<=10000),
 known_count INTEGER NOT NULL CHECK(known_count>0 AND known_count<=member_count),
 sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_divergent_move_recoveries(operation_id)
) WITHOUT ROWID;

CREATE TABLE conflict_folder_divergent_tree_members(
 operation_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>0 AND ordinal<=10000),
 source_object_id TEXT NOT NULL,
 recovered_object_id TEXT NOT NULL,
 source_parent_id TEXT NOT NULL,
 recovered_parent_id TEXT NOT NULL,
 source_operation_id TEXT NOT NULL,
 new_operation_id TEXT NOT NULL UNIQUE,
 object_type TEXT NOT NULL CHECK(object_type IN('folder','note')),
 name TEXT NOT NULL CHECK(name<>''),
 relative_path TEXT NOT NULL CHECK(relative_path<>''),
 depth INTEGER NOT NULL CHECK(depth>0 AND depth<=256),
 source_revision INTEGER NOT NULL CHECK(source_revision>=0),
 device INTEGER,
 inode INTEGER,
 source_blob_hash BLOB,
 recovered_blob_hash BLOB,
 create_blob_hash BLOB,
 PRIMARY KEY(operation_id,ordinal),
 UNIQUE(operation_id,source_object_id),
 UNIQUE(operation_id,recovered_object_id),
 UNIQUE(operation_id,relative_path),
 CHECK((source_revision=0 AND source_object_id=recovered_object_id) OR (source_revision>0 AND source_object_id<>recovered_object_id)),
 CHECK((object_type='folder' AND device>0 AND inode>0 AND source_blob_hash IS NULL AND recovered_blob_hash IS NULL AND create_blob_hash IS NULL) OR (object_type='note' AND device IS NULL AND inode IS NULL AND typeof(source_blob_hash)='blob' AND length(source_blob_hash)=32 AND typeof(recovered_blob_hash)='blob' AND length(recovered_blob_hash)=32 AND ((source_revision=0 AND typeof(create_blob_hash)='blob' AND length(create_blob_hash)=32 AND recovered_blob_hash=source_blob_hash) OR (source_revision>0 AND create_blob_hash IS NULL)))),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_divergent_tree_manifests(operation_id)
) WITHOUT ROWID;

CREATE TABLE conflict_folder_divergent_tree_note_chains(
 operation_id TEXT NOT NULL,
 source_object_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>0),
 old_operation_id TEXT NOT NULL UNIQUE,
 previous_operation_id TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
 PRIMARY KEY(operation_id,source_object_id,ordinal),
 FOREIGN KEY(operation_id,source_object_id) REFERENCES conflict_folder_divergent_tree_members(operation_id,source_object_id),
 FOREIGN KEY(old_operation_id) REFERENCES sync_outbox(operation_id)
) WITHOUT ROWID;

CREATE TABLE conflict_folder_divergent_tree_canonical_folders(
 operation_id TEXT NOT NULL,
 source_object_id TEXT NOT NULL,
 device INTEGER NOT NULL CHECK(device>0),
 inode INTEGER NOT NULL CHECK(inode>0),
 PRIMARY KEY(operation_id,source_object_id),
 FOREIGN KEY(operation_id,source_object_id) REFERENCES conflict_folder_divergent_tree_members(operation_id,source_object_id)
) WITHOUT ROWID;

CREATE TRIGGER conflict_folder_divergent_tree_manifest_insert_guard BEFORE INSERT ON conflict_folder_divergent_tree_manifests
WHEN NEW.sealed<>0 OR NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries r WHERE r.operation_id=NEW.operation_id AND r.new_operation_id=NEW.new_root_operation_id AND r.state='prepared') OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries r WHERE r.recovery_kind='divergent_move' AND r.operation_id=NEW.operation_id) OR EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n WHERE n.operation_id=NEW.operation_id)
BEGIN SELECT RAISE(ABORT,'invalid divergent folder tree manifest'); END;

CREATE TRIGGER conflict_folder_divergent_tree_manifest_update_guard BEFORE UPDATE ON conflict_folder_divergent_tree_manifests
WHEN NOT(OLD.sealed=0 AND NEW.sealed=1 AND OLD.operation_id=NEW.operation_id AND OLD.new_root_operation_id=NEW.new_root_operation_id AND OLD.member_count=NEW.member_count AND OLD.known_count=NEW.known_count AND (SELECT COUNT(*) FROM conflict_folder_divergent_tree_members m WHERE m.operation_id=OLD.operation_id)=OLD.member_count AND (SELECT COUNT(*) FROM conflict_folder_divergent_tree_members m WHERE m.operation_id=OLD.operation_id AND m.source_revision>0)=OLD.known_count AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members m WHERE m.operation_id=OLD.operation_id AND ((m.depth=1 AND m.recovered_parent_id<>(SELECT r.recovered_folder_id FROM conflict_folder_divergent_move_recoveries r WHERE r.operation_id=m.operation_id)) OR (m.depth>1 AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members p WHERE p.operation_id=m.operation_id AND p.source_object_id=m.source_parent_id AND p.recovered_object_id=m.recovered_parent_id AND p.depth=m.depth-1)))))
BEGIN SELECT RAISE(ABORT,'invalid divergent folder tree seal'); END;
CREATE TRIGGER conflict_folder_divergent_tree_manifest_no_delete BEFORE DELETE ON conflict_folder_divergent_tree_manifests BEGIN SELECT RAISE(ABORT,'divergent folder tree manifest is immutable'); END;

CREATE TRIGGER conflict_folder_divergent_tree_member_insert_guard BEFORE INSERT ON conflict_folder_divergent_tree_members
WHEN NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests m JOIN conflict_folder_divergent_move_recoveries r ON r.operation_id=m.operation_id WHERE m.operation_id=NEW.operation_id AND m.sealed=0 AND r.state='prepared') OR (NEW.source_revision=0 AND (EXISTS(SELECT 1 FROM sync_baselines b WHERE b.object_id=NEW.source_object_id) OR NOT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=NEW.source_operation_id AND o.object_id=NEW.source_object_id AND o.object_type=NEW.object_type AND o.mutation='create' AND o.parent_id=NEW.source_parent_id AND o.name=NEW.name AND o.status='pending' AND o.attempted_at_ms IS NULL))) OR (NEW.source_revision>0 AND (NOT EXISTS(SELECT 1 FROM sync_baselines b WHERE b.object_id=NEW.source_object_id AND b.revision=NEW.source_revision AND b.operation_id=NEW.source_operation_id) OR EXISTS(SELECT 1 FROM sync_outbox o WHERE o.object_id=NEW.source_object_id AND o.status IN('pending','attempted','replay_mismatch','conflict'))))
BEGIN SELECT RAISE(ABORT,'invalid divergent folder tree member'); END;
CREATE TRIGGER conflict_folder_divergent_tree_member_no_update BEFORE UPDATE ON conflict_folder_divergent_tree_members BEGIN SELECT RAISE(ABORT,'divergent folder tree member is immutable'); END;
CREATE TRIGGER conflict_folder_divergent_tree_member_no_delete BEFORE DELETE ON conflict_folder_divergent_tree_members BEGIN SELECT RAISE(ABORT,'divergent folder tree member is immutable'); END;

CREATE TRIGGER conflict_folder_divergent_tree_chain_insert_guard BEFORE INSERT ON conflict_folder_divergent_tree_note_chains
WHEN NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests manifest JOIN conflict_folder_divergent_tree_members member ON member.operation_id=manifest.operation_id JOIN sync_outbox update_op ON update_op.operation_id=NEW.old_operation_id WHERE manifest.operation_id=NEW.operation_id AND manifest.sealed=0 AND member.source_object_id=NEW.source_object_id AND member.source_revision=0 AND member.object_type='note' AND update_op.object_id=member.source_object_id AND update_op.object_type='note' AND update_op.mutation='update' AND update_op.status='pending' AND update_op.attempted_at_ms IS NULL AND update_op.blob_hash=NEW.blob_hash AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=update_op.operation_id)=1 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=update_op.operation_id AND d.dependency_operation_id=NEW.previous_operation_id))
BEGIN SELECT RAISE(ABORT,'invalid divergent folder tree note chain'); END;
CREATE TRIGGER conflict_folder_divergent_tree_chain_no_update BEFORE UPDATE ON conflict_folder_divergent_tree_note_chains BEGIN SELECT RAISE(ABORT,'divergent folder tree note chain is immutable'); END;
CREATE TRIGGER conflict_folder_divergent_tree_chain_no_delete BEFORE DELETE ON conflict_folder_divergent_tree_note_chains BEGIN SELECT RAISE(ABORT,'divergent folder tree note chain is immutable'); END;

CREATE TRIGGER conflict_folder_divergent_tree_canonical_folder_insert_guard BEFORE INSERT ON conflict_folder_divergent_tree_canonical_folders
WHEN NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries r JOIN conflict_folder_divergent_tree_manifests manifest ON manifest.operation_id=r.operation_id JOIN conflict_folder_divergent_tree_members member ON member.operation_id=manifest.operation_id WHERE r.operation_id=NEW.operation_id AND r.state='evacuated' AND manifest.sealed=1 AND member.source_object_id=NEW.source_object_id AND member.object_type='folder' AND member.source_revision>0)
BEGIN SELECT RAISE(ABORT,'invalid divergent tree canonical folder'); END;
CREATE TRIGGER conflict_folder_divergent_tree_canonical_folder_no_update BEFORE UPDATE ON conflict_folder_divergent_tree_canonical_folders BEGIN SELECT RAISE(ABORT,'divergent tree canonical folder is immutable'); END;
CREATE TRIGGER conflict_folder_divergent_tree_canonical_folder_no_delete BEFORE DELETE ON conflict_folder_divergent_tree_canonical_folders BEGIN SELECT RAISE(ABORT,'divergent tree canonical folder is immutable'); END;

CREATE VIEW conflict_folder_divergent_tree_replacements_complete AS
SELECT r.operation_id FROM conflict_folder_divergent_move_recoveries r JOIN conflict_folder_divergent_tree_manifests manifest ON manifest.operation_id=r.operation_id
WHERE manifest.sealed=1
 AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members member WHERE member.operation_id=manifest.operation_id AND member.source_revision>0 AND (NOT EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=member.source_object_id AND baseline.revision=member.source_revision AND baseline.operation_id=member.source_operation_id) OR EXISTS(SELECT 1 FROM sync_outbox pending WHERE pending.object_id=member.source_object_id AND pending.status IN('pending','attempted','replay_mismatch','conflict'))))
 AND EXISTS(SELECT 1 FROM sync_outbox root WHERE root.operation_id=manifest.new_root_operation_id AND root.object_id=r.recovered_folder_id AND root.mutation='create' AND root.object_type='folder' AND root.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002' AND root.name=substr(r.recovery_relative,length('_Konflikte/Wiederhergestellt/')+1) AND root.status IN('pending','attempted','accepted'))
 AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members member WHERE member.operation_id=manifest.operation_id AND ((member.source_revision=0 AND (NOT EXISTS(SELECT 1 FROM sync_outbox old WHERE old.operation_id=member.source_operation_id AND old.status='superseded') OR EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_note_chains chain JOIN sync_outbox old ON old.operation_id=chain.old_operation_id WHERE chain.operation_id=member.operation_id AND chain.source_object_id=member.source_object_id AND old.status<>'superseded'))) OR NOT EXISTS(SELECT 1 FROM sync_outbox replacement WHERE replacement.operation_id=member.new_operation_id AND replacement.object_id=member.recovered_object_id AND replacement.object_type=member.object_type AND replacement.mutation='create' AND replacement.parent_id=member.recovered_parent_id AND replacement.name=member.name AND ((member.object_type='folder' AND replacement.blob_hash IS NULL) OR (member.object_type='note' AND replacement.blob_hash=member.recovered_blob_hash)) AND replacement.status IN('pending','attempted','accepted') AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id)=1 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=replacement.operation_id AND d.dependency_operation_id=CASE WHEN member.depth=1 THEN manifest.new_root_operation_id ELSE (SELECT parent.new_operation_id FROM conflict_folder_divergent_tree_members parent WHERE parent.operation_id=member.operation_id AND parent.recovered_object_id=member.recovered_parent_id) END))));

DROP TRIGGER conflict_folder_divergent_move_state_guard;
CREATE TRIGGER conflict_folder_divergent_move_state_guard BEFORE UPDATE OF state,canonical_device,canonical_inode ON conflict_folder_divergent_move_recoveries WHEN NOT(
 (OLD.state='prepared' AND NEW.state='evacuated' AND NEW.canonical_device IS NULL AND NEW.canonical_inode IS NULL AND (
   (EXISTS(SELECT 1 FROM conflict_folder_divergent_move_valid_note_topology valid WHERE valid.operation_id=OLD.operation_id)
    AND (SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=OLD.operation_id AND dependent.status IN('pending','attempted','replay_mismatch','conflict'))=(SELECT COUNT(*) FROM conflict_folder_divergent_move_note_members n WHERE n.operation_id=OLD.operation_id)
    AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=OLD.operation_id AND dependent.status IN('pending','attempted','replay_mismatch','conflict') AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n WHERE n.operation_id=OLD.operation_id AND n.old_operation_id=dependent.operation_id)))
   OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_recoveries recursive WHERE recursive.recovery_kind='divergent_move' AND recursive.operation_id=OLD.operation_id AND recursive.sealed=1)
   OR EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests tree WHERE tree.operation_id=OLD.operation_id AND tree.sealed=1)
  ) AND NOT EXISTS(SELECT 1 FROM sync_outbox later JOIN sync_outbox original ON original.operation_id=OLD.operation_id WHERE later.object_id=OLD.folder_id AND later.sequence>original.sequence AND later.status IN('pending','attempted','replay_mismatch','conflict')))
 OR (OLD.state='evacuated' AND NEW.state='canonical_prepared' AND NEW.canonical_device>0 AND NEW.canonical_inode>0 AND (NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests tree WHERE tree.operation_id=OLD.operation_id) OR (SELECT COUNT(*) FROM conflict_folder_divergent_tree_canonical_folders binding WHERE binding.operation_id=OLD.operation_id)=(SELECT COUNT(*) FROM conflict_folder_divergent_tree_members member WHERE member.operation_id=OLD.operation_id AND member.source_revision>0 AND member.object_type='folder')))
 OR (OLD.state='canonical_prepared' AND NEW.state='canonical_published' AND NEW.canonical_device=OLD.canonical_device AND NEW.canonical_inode=OLD.canonical_inode)
 OR (OLD.state='canonical_published' AND NEW.state='completed' AND NEW.canonical_device=OLD.canonical_device AND NEW.canonical_inode=OLD.canonical_inode AND (
   EXISTS(SELECT 1 FROM conflict_folder_divergent_move_replacements_complete complete WHERE complete.operation_id=OLD.operation_id)
   OR EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_replacements_complete complete WHERE complete.operation_id=OLD.operation_id)
   OR EXISTS(
    SELECT 1 FROM conflict_recursive_local_folder_recoveries manifest JOIN sync_outbox replacement_root ON replacement_root.operation_id=manifest.new_root_operation_id
    WHERE manifest.recovery_kind='divergent_move' AND manifest.operation_id=OLD.operation_id AND manifest.sealed=1
     AND replacement_root.object_id=OLD.recovered_folder_id AND replacement_root.mutation='create' AND replacement_root.object_type='folder'
     AND replacement_root.parent_id='7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002' AND replacement_root.name=substr(OLD.recovery_relative,length('_Konflikte/Wiederhergestellt/')+1)
     AND replacement_root.status IN('pending','attempted','accepted')
     AND NOT EXISTS(SELECT 1 FROM conflict_recursive_local_folder_members member WHERE member.recovery_kind=manifest.recovery_kind AND member.operation_id=manifest.operation_id AND (NOT EXISTS(SELECT 1 FROM sync_outbox old WHERE old.operation_id=member.old_operation_id AND old.status='superseded') OR EXISTS(SELECT 1 FROM conflict_recursive_local_folder_note_chains chain JOIN sync_outbox old ON old.operation_id=chain.old_operation_id WHERE chain.recovery_kind=member.recovery_kind AND chain.operation_id=member.operation_id AND chain.note_id=member.object_id AND old.status<>'superseded') OR NOT EXISTS(SELECT 1 FROM sync_outbox replacement WHERE replacement.operation_id=member.new_operation_id AND replacement.object_id=member.object_id AND replacement.object_type=member.object_type AND replacement.mutation='create' AND replacement.parent_id=CASE WHEN member.depth=1 THEN OLD.recovered_folder_id ELSE member.parent_id END AND replacement.name=member.name AND ((member.object_type='folder' AND replacement.blob_hash IS NULL) OR (member.object_type='note' AND replacement.blob_hash=member.final_blob_hash)) AND replacement.status IN('pending','attempted','accepted') AND (SELECT COUNT(*) FROM sync_outbox_dependencies dependency WHERE dependency.operation_id=replacement.operation_id)=1 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies dependency WHERE dependency.operation_id=replacement.operation_id AND dependency.dependency_operation_id=CASE WHEN member.depth=1 THEN manifest.new_root_operation_id ELSE (SELECT parent.new_operation_id FROM conflict_recursive_local_folder_members parent WHERE parent.recovery_kind=member.recovery_kind AND parent.operation_id=member.operation_id AND parent.object_id=member.parent_id) END))))
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
  AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members member JOIN conflict_folder_divergent_move_recoveries recovery ON recovery.operation_id=member.operation_id WHERE member.source_revision=0 AND (member.source_operation_id=o.operation_id OR EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_note_chains chain WHERE chain.operation_id=member.operation_id AND chain.source_object_id=member.source_object_id AND chain.old_operation_id=o.operation_id)) AND recovery.state IN('evacuated','canonical_published','completed'))
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE c.local_delete_operation_id=o.operation_id AND p.state='resolved')
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves n JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=n.conflict_operation_id WHERE n.local_delete_operation_id=o.operation_id AND p.state='resolved'))
 OR (o.status='conflict' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN('copy_staged','copy_published','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries f WHERE f.operation_id=o.operation_id AND f.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('evacuated','canonical_published','completed')) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=o.operation_id AND p.state='resolved'));
