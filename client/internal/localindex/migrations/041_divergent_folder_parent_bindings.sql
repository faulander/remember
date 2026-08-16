ALTER TABLE conflict_folder_divergent_move_recoveries ADD COLUMN attempted_parent_id TEXT;
ALTER TABLE conflict_folder_divergent_move_recoveries ADD COLUMN canonical_parent_id TEXT;

CREATE TABLE conflict_folder_divergent_parent_manifests(
 operation_id TEXT PRIMARY KEY,
 attempted_count INTEGER NOT NULL CHECK(attempted_count BETWEEN 0 AND 256),
 canonical_count INTEGER NOT NULL CHECK(canonical_count BETWEEN 0 AND 256),
 sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_divergent_move_recoveries(operation_id)
) WITHOUT ROWID;

CREATE TABLE conflict_folder_divergent_parent_bindings(
 operation_id TEXT NOT NULL,
 side TEXT NOT NULL CHECK(side IN('attempted','canonical')),
 depth INTEGER NOT NULL CHECK(depth BETWEEN 1 AND 256),
 object_id TEXT NOT NULL,
 parent_id TEXT,
 relative_path TEXT NOT NULL CHECK(relative_path<>''),
 revision INTEGER NOT NULL CHECK(revision>0),
 baseline_operation_id TEXT NOT NULL CHECK(baseline_operation_id<>'' AND baseline_operation_id<>'00000000-0000-0000-0000-000000000000'),
 device INTEGER NOT NULL CHECK(device>0),
 inode INTEGER NOT NULL CHECK(inode>0),
 PRIMARY KEY(operation_id,side,depth),
 UNIQUE(operation_id,side,object_id),
 UNIQUE(operation_id,side,relative_path),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_divergent_parent_manifests(operation_id)
) WITHOUT ROWID;

DROP TRIGGER conflict_folder_divergent_move_insert_guard;
CREATE TRIGGER conflict_folder_divergent_move_insert_guard BEFORE INSERT ON conflict_folder_divergent_move_recoveries WHEN NEW.state<>'prepared' OR NEW.canonical_device IS NOT NULL OR NEW.canonical_inode IS NOT NULL OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id
 WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.folder_id AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND o.parent_id IS NEW.attempted_parent_id
 AND c.object_type='folder' AND c.deleted=0 AND c.parent_id IS NEW.canonical_parent_id AND (c.name<>o.name OR c.parent_id IS NOT o.parent_id) AND c.revision=NEW.canonical_revision AND c.revision>o.base_revision AND c.blob_hash IS NULL
 AND i.folder_id=o.object_id AND i.mutation_kind='move' AND i.device=NEW.source_device AND i.inode=NEW.source_inode
 AND (NEW.attempted_relative=o.name OR substr(NEW.attempted_relative,length(NEW.attempted_relative)-length(o.name),length(o.name)+1)='/'||o.name)
 AND (NEW.canonical_relative=c.name OR substr(NEW.canonical_relative,length(NEW.canonical_relative)-length(c.name),length(c.name)+1)='/'||c.name)
 AND NEW.recovered_folder_id<>NEW.folder_id AND NEW.new_operation_id<>NEW.operation_id AND NEW.canonical_relative<>NEW.attempted_relative
 AND substr(NEW.recovery_relative,1,length('_Konflikte/Wiederhergestellt/'))='_Konflikte/Wiederhergestellt/' AND instr(substr(NEW.recovery_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')=0
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN('pending','attempted','replay_mismatch','conflict')))
BEGIN SELECT RAISE(ABORT,'invalid divergent folder move recovery'); END;

DROP TRIGGER conflict_folder_divergent_move_identity_immutable;
CREATE TRIGGER conflict_folder_divergent_move_identity_immutable BEFORE UPDATE OF operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,attempted_parent_id,canonical_parent_id ON conflict_folder_divergent_move_recoveries BEGIN SELECT RAISE(ABORT,'divergent folder move recovery identity is immutable'); END;

CREATE TRIGGER conflict_folder_divergent_parent_manifest_insert_guard BEFORE INSERT ON conflict_folder_divergent_parent_manifests
WHEN NEW.sealed<>0 OR NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries recovery WHERE recovery.operation_id=NEW.operation_id AND recovery.state='prepared')
BEGIN SELECT RAISE(ABORT,'invalid divergent parent manifest'); END;
CREATE TRIGGER conflict_folder_divergent_parent_manifest_update_guard BEFORE UPDATE ON conflict_folder_divergent_parent_manifests
WHEN NOT(OLD.sealed=0 AND NEW.sealed=1 AND OLD.operation_id=NEW.operation_id AND OLD.attempted_count=NEW.attempted_count AND OLD.canonical_count=NEW.canonical_count AND EXISTS(SELECT 1 FROM conflict_folder_divergent_parent_manifests_valid valid WHERE valid.operation_id=OLD.operation_id))
BEGIN SELECT RAISE(ABORT,'invalid divergent parent manifest seal'); END;
CREATE TRIGGER conflict_folder_divergent_parent_manifest_no_delete BEFORE DELETE ON conflict_folder_divergent_parent_manifests BEGIN SELECT RAISE(ABORT,'divergent parent manifest is immutable'); END;

CREATE TRIGGER conflict_folder_divergent_parent_binding_insert_guard BEFORE INSERT ON conflict_folder_divergent_parent_bindings
WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_divergent_parent_manifests manifest
 JOIN conflict_folder_divergent_move_recoveries recovery ON recovery.operation_id=manifest.operation_id
 JOIN objects object ON object.object_id=NEW.object_id
 JOIN sync_baselines baseline ON baseline.object_id=object.object_id
 WHERE manifest.operation_id=NEW.operation_id AND manifest.sealed=0 AND recovery.state='prepared' AND NEW.object_id<>recovery.folder_id
  AND object.object_type='folder' AND object.identity_state='known' AND object.parent_id IS NEW.parent_id AND object.relative_path=NEW.relative_path AND object.folder_device=NEW.device AND object.folder_inode=NEW.inode
  AND baseline.revision=NEW.revision AND baseline.operation_id=NEW.baseline_operation_id
  AND NOT EXISTS(SELECT 1 FROM sync_outbox open WHERE open.object_id=NEW.object_id AND open.status IN('pending','attempted','replay_mismatch','conflict')))
BEGIN SELECT RAISE(ABORT,'invalid divergent parent binding'); END;
CREATE TRIGGER conflict_folder_divergent_parent_binding_no_update BEFORE UPDATE ON conflict_folder_divergent_parent_bindings BEGIN SELECT RAISE(ABORT,'divergent parent binding is immutable'); END;
CREATE TRIGGER conflict_folder_divergent_parent_binding_no_delete BEFORE DELETE ON conflict_folder_divergent_parent_bindings BEGIN SELECT RAISE(ABORT,'divergent parent binding is immutable'); END;

CREATE VIEW conflict_folder_divergent_parent_manifests_valid AS
SELECT manifest.operation_id
FROM conflict_folder_divergent_parent_manifests manifest
JOIN conflict_folder_divergent_move_recoveries recovery ON recovery.operation_id=manifest.operation_id
JOIN sync_outbox root_move ON root_move.operation_id=recovery.operation_id
JOIN sync_conflict_states canonical_move ON canonical_move.operation_id=recovery.operation_id
WHERE (SELECT COUNT(*) FROM conflict_folder_divergent_parent_bindings binding WHERE binding.operation_id=manifest.operation_id AND binding.side='attempted')=manifest.attempted_count
 AND (SELECT COUNT(*) FROM conflict_folder_divergent_parent_bindings binding WHERE binding.operation_id=manifest.operation_id AND binding.side='canonical')=manifest.canonical_count
 AND NOT EXISTS(
  SELECT 1 FROM conflict_folder_divergent_parent_bindings binding
  WHERE binding.operation_id=manifest.operation_id AND (
   binding.depth=1 AND (binding.parent_id IS NOT NULL OR instr(binding.relative_path,'/')<>0)
   OR binding.depth>1 AND NOT EXISTS(
    SELECT 1 FROM conflict_folder_divergent_parent_bindings parent
    WHERE parent.operation_id=binding.operation_id AND parent.side=binding.side AND parent.depth=binding.depth-1 AND parent.object_id=binding.parent_id AND binding.relative_path LIKE parent.relative_path||'/%' AND instr(substr(binding.relative_path,length(parent.relative_path)+2),'/')=0)
   OR NOT EXISTS(
    SELECT 1 FROM objects object JOIN sync_baselines baseline ON baseline.object_id=object.object_id
    WHERE object.object_id=binding.object_id AND object.object_type='folder' AND object.identity_state='known' AND object.parent_id IS binding.parent_id AND object.relative_path=binding.relative_path AND object.folder_device=binding.device AND object.folder_inode=binding.inode
     AND baseline.revision=binding.revision AND baseline.operation_id=binding.baseline_operation_id)
   OR EXISTS(SELECT 1 FROM sync_outbox open WHERE open.object_id=binding.object_id AND open.status IN('pending','attempted','replay_mismatch','conflict'))
  ))
 AND ((manifest.attempted_count=0 AND recovery.attempted_parent_id IS NULL AND recovery.attempted_relative=root_move.name)
  OR (manifest.attempted_count>0 AND recovery.attempted_parent_id IS NOT NULL AND EXISTS(
   SELECT 1 FROM conflict_folder_divergent_parent_bindings target
   WHERE target.operation_id=manifest.operation_id AND target.side='attempted' AND target.depth=manifest.attempted_count AND target.object_id=recovery.attempted_parent_id AND recovery.attempted_relative=target.relative_path||'/'||root_move.name)))
 AND ((manifest.canonical_count=0 AND recovery.canonical_parent_id IS NULL AND recovery.canonical_relative=canonical_move.name)
  OR (manifest.canonical_count>0 AND recovery.canonical_parent_id IS NOT NULL AND EXISTS(
   SELECT 1 FROM conflict_folder_divergent_parent_bindings target
   WHERE target.operation_id=manifest.operation_id AND target.side='canonical' AND target.depth=manifest.canonical_count AND target.object_id=recovery.canonical_parent_id AND recovery.canonical_relative=target.relative_path||'/'||canonical_move.name)));

DROP TRIGGER conflict_folder_divergent_move_state_guard;
CREATE TRIGGER conflict_folder_divergent_move_state_guard BEFORE UPDATE OF state,canonical_device,canonical_inode ON conflict_folder_divergent_move_recoveries WHEN NOT(
 (
  EXISTS(SELECT 1 FROM conflict_folder_divergent_parent_manifests manifest JOIN conflict_folder_divergent_parent_manifests_valid valid ON valid.operation_id=manifest.operation_id WHERE manifest.operation_id=OLD.operation_id AND manifest.sealed=1)
  OR (NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_parent_manifests manifest WHERE manifest.operation_id=OLD.operation_id) AND OLD.attempted_parent_id IS NULL AND OLD.canonical_parent_id IS NULL AND instr(OLD.attempted_relative,'/')=0 AND instr(OLD.canonical_relative,'/')=0)
 ) AND (
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
 )
) BEGIN SELECT RAISE(ABORT,'invalid divergent folder move recovery transition'); END;
