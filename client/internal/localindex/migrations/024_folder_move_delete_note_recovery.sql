DROP TRIGGER conflict_folder_move_delete_recoveries_insert_guard;
CREATE TRIGGER conflict_folder_move_delete_recoveries_insert_guard BEFORE INSERT ON conflict_folder_move_delete_recoveries
WHEN NEW.state<>'prepared' OR NEW.recovered_folder_id=NEW.folder_id OR NEW.new_operation_id=NEW.operation_id OR NEW.attempted_relative='' OR NEW.target_relative=NEW.attempted_relative OR substr(NEW.target_relative,1,length('_Konflikte/Wiederhergestellt/'))<>'_Konflikte/Wiederhergestellt/' OR instr(substr(NEW.target_relative,length('_Konflikte/Wiederhergestellt/')+1),'/')<>0 OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id JOIN sync_outbox_folder_intents i ON i.operation_id=o.operation_id
 WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.folder_id AND o.object_type='folder' AND o.mutation='move' AND o.status='conflict' AND o.conflict_code='object_deleted'
 AND c.object_type='folder' AND c.deleted=1 AND c.blob_hash IS NULL AND c.revision=NEW.canonical_revision AND c.revision>o.base_revision
 AND i.folder_id=NEW.folder_id AND i.mutation_kind='move' AND i.device=NEW.device AND i.inode=NEW.inode
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.object_id=o.object_id AND later.sequence>o.sequence AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
) BEGIN SELECT RAISE(ABORT,'invalid folder move/delete recovery'); END;

CREATE TABLE conflict_folder_move_delete_note_members (
 operation_id TEXT NOT NULL,
 note_id TEXT NOT NULL UNIQUE,
 old_operation_id TEXT NOT NULL UNIQUE,
 new_operation_id TEXT NOT NULL UNIQUE,
 name TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(length(blob_hash)=32),
 create_blob_hash BLOB NOT NULL CHECK(length(create_blob_hash)=32),
 PRIMARY KEY(operation_id,note_id),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_move_delete_recoveries(operation_id) ON DELETE CASCADE,
 FOREIGN KEY(old_operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE conflict_folder_move_delete_note_chains (
 operation_id TEXT NOT NULL,note_id TEXT NOT NULL,ordinal INTEGER NOT NULL CHECK(ordinal>0),old_operation_id TEXT NOT NULL UNIQUE,previous_operation_id TEXT NOT NULL,blob_hash BLOB NOT NULL CHECK(length(blob_hash)=32),
 PRIMARY KEY(operation_id,note_id,ordinal),FOREIGN KEY(operation_id,note_id) REFERENCES conflict_folder_move_delete_note_members(operation_id,note_id),FOREIGN KEY(old_operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_move_delete_recoveries_manifest_guard BEFORE UPDATE OF state ON conflict_folder_move_delete_recoveries
WHEN NEW.state='moved' AND OLD.state='prepared' AND (
 (SELECT COUNT(*) FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=NEW.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))<>(SELECT COUNT(*) FROM conflict_folder_move_delete_note_members n WHERE n.operation_id=NEW.operation_id)
 OR EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=NEW.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict') AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n WHERE n.operation_id=NEW.operation_id AND n.old_operation_id=dependent.operation_id))
) BEGIN SELECT RAISE(ABORT,'folder move/delete note manifest incomplete'); END;

CREATE TRIGGER conflict_folder_move_delete_note_members_no_update BEFORE UPDATE ON conflict_folder_move_delete_note_members BEGIN SELECT RAISE(ABORT,'folder move/delete note manifest immutable'); END;
CREATE TRIGGER conflict_folder_move_delete_note_members_no_delete BEFORE DELETE ON conflict_folder_move_delete_note_members BEGIN SELECT RAISE(ABORT,'folder move/delete note manifest immutable'); END;
CREATE TRIGGER conflict_folder_move_delete_note_chains_no_update BEFORE UPDATE ON conflict_folder_move_delete_note_chains BEGIN SELECT RAISE(ABORT,'folder move/delete note chain immutable'); END;
CREATE TRIGGER conflict_folder_move_delete_note_chains_no_delete BEFORE DELETE ON conflict_folder_move_delete_note_chains BEGIN SELECT RAISE(ABORT,'folder move/delete note chain immutable'); END;
CREATE TRIGGER conflict_folder_move_delete_note_members_guard BEFORE INSERT ON conflict_folder_move_delete_note_members WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_move_delete_recoveries r JOIN sync_outbox root ON root.operation_id=r.operation_id JOIN sync_outbox note ON note.operation_id=NEW.old_operation_id JOIN sync_outbox_dependencies d ON d.operation_id=note.operation_id
 WHERE r.operation_id=NEW.operation_id AND r.state='prepared' AND root.object_id=r.folder_id AND note.object_id=NEW.note_id AND note.status='pending' AND note.mutation='create' AND note.object_type='note' AND note.parent_id=r.folder_id AND note.name=NEW.name AND note.blob_hash=NEW.create_blob_hash AND d.dependency_operation_id=root.operation_id
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=note.operation_id AND extra.dependency_operation_id<>root.operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox extra WHERE extra.object_id=NEW.note_id AND extra.operation_id<>NEW.old_operation_id AND extra.status IN ('pending','attempted','replay_mismatch','conflict'))
) BEGIN SELECT RAISE(ABORT,'invalid folder move/delete note member'); END;
CREATE TRIGGER conflict_folder_move_delete_note_chains_guard BEFORE INSERT ON conflict_folder_move_delete_note_chains WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_move_delete_note_members n JOIN sync_outbox u ON u.operation_id=NEW.old_operation_id WHERE n.operation_id=NEW.operation_id AND n.note_id=NEW.note_id AND u.object_id=n.note_id AND u.mutation='update' AND u.object_type='note' AND u.status='superseded' AND u.blob_hash=NEW.blob_hash
 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=u.operation_id AND d.dependency_operation_id=NEW.previous_operation_id) AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies x WHERE x.operation_id=u.operation_id AND x.dependency_operation_id<>NEW.previous_operation_id)
 AND ((NEW.ordinal=1 AND NEW.previous_operation_id=n.old_operation_id) OR (NEW.ordinal>1 AND EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains p WHERE p.operation_id=NEW.operation_id AND p.note_id=NEW.note_id AND p.ordinal=NEW.ordinal-1 AND p.old_operation_id=NEW.previous_operation_id)))
) BEGIN SELECT RAISE(ABORT,'invalid folder move/delete note chain'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_deleted_active_dependents_guard BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_move_deleted_recovered' AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id WHERE d.dependency_operation_id=NEW.operation_id AND dependent.status IN ('pending','attempted','replay_mismatch','conflict'))
BEGIN SELECT RAISE(ABORT,'folder move/delete active dependent remains'); END;
CREATE TRIGGER sync_conflict_resolutions_folder_move_deleted_notes_guard BEFORE INSERT ON sync_conflict_resolutions WHEN NEW.resolution='folder_move_deleted_recovered' AND EXISTS(
 SELECT 1 FROM conflict_folder_move_delete_note_members n WHERE n.operation_id=NEW.operation_id AND (
  NOT EXISTS(SELECT 1 FROM sync_outbox old_create WHERE old_create.operation_id=n.old_operation_id AND old_create.object_id=n.note_id AND old_create.mutation='create' AND old_create.object_type='note' AND old_create.status='superseded' AND old_create.name=n.name AND old_create.blob_hash=n.create_blob_hash)
  OR NOT EXISTS(SELECT 1 FROM sync_outbox replacement JOIN sync_outbox_dependencies d ON d.operation_id=replacement.operation_id JOIN conflict_folder_move_delete_recoveries r ON r.operation_id=n.operation_id WHERE replacement.operation_id=n.new_operation_id AND replacement.mutation='create' AND replacement.object_id=n.note_id AND replacement.object_type='note' AND replacement.parent_id=r.recovered_folder_id AND replacement.name=n.name AND replacement.blob_hash=n.blob_hash AND replacement.status='pending' AND d.dependency_operation_id=r.new_operation_id AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies x WHERE x.operation_id=replacement.operation_id AND x.dependency_operation_id<>r.new_operation_id))
  OR EXISTS(SELECT 1 FROM sync_outbox extra WHERE extra.object_id=n.note_id AND extra.operation_id<>n.new_operation_id AND extra.status IN ('pending','attempted','replay_mismatch','conflict'))
  OR EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains c JOIN sync_outbox old ON old.operation_id=c.old_operation_id WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id AND (old.status<>'superseded' OR old.object_id<>n.note_id OR old.mutation<>'update' OR old.blob_hash<>c.blob_hash OR (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=c.old_operation_id)<>1 OR NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=c.old_operation_id AND d.dependency_operation_id=c.previous_operation_id)))
  OR (SELECT COUNT(*) FROM sync_outbox_dependencies links WHERE links.dependency_operation_id=n.old_operation_id OR links.dependency_operation_id IN (SELECT c.old_operation_id FROM conflict_folder_move_delete_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id))<>(SELECT COUNT(*) FROM conflict_folder_move_delete_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id)
  OR (NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id) AND n.blob_hash<>n.create_blob_hash)
  OR (EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_chains last WHERE last.operation_id=n.operation_id AND last.note_id=n.note_id AND last.ordinal=(SELECT MAX(c.ordinal) FROM conflict_folder_move_delete_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id) AND last.blob_hash=n.blob_hash))
 )
) BEGIN SELECT RAISE(ABORT,'folder move/delete note replacement mismatch'); END;
