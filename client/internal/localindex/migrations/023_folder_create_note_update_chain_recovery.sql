ALTER TABLE conflict_folder_create_note_members ADD COLUMN create_blob_hash BLOB CHECK(create_blob_hash IS NULL OR length(create_blob_hash)=32);

CREATE TABLE conflict_folder_create_note_chain_members (
 operation_id TEXT NOT NULL,
 note_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>0),
 old_operation_id TEXT NOT NULL UNIQUE,
 previous_operation_id TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(length(blob_hash)=32),
 PRIMARY KEY(operation_id,note_id,ordinal),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_create_recoveries(operation_id) ON DELETE CASCADE,
 FOREIGN KEY(old_operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_create_note_chain_members_insert_guard BEFORE INSERT ON conflict_folder_create_note_chain_members
WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_create_recoveries r JOIN sync_outbox u ON u.operation_id=NEW.old_operation_id
 WHERE r.operation_id=NEW.operation_id AND r.state='prepared' AND u.object_id=NEW.note_id AND u.mutation='update' AND u.object_type='note' AND u.status='superseded' AND u.blob_hash=NEW.blob_hash
 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=u.operation_id AND d.dependency_operation_id=NEW.previous_operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=u.operation_id AND extra.dependency_operation_id<>NEW.previous_operation_id)
 AND ((NEW.ordinal=1 AND EXISTS(SELECT 1 FROM sync_outbox c JOIN sync_outbox_dependencies root_link ON root_link.operation_id=c.operation_id WHERE c.operation_id=NEW.previous_operation_id AND c.object_id=NEW.note_id AND c.mutation='create' AND c.object_type='note' AND c.status='pending' AND c.parent_id=r.source_folder_id AND root_link.dependency_operation_id=r.operation_id AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra_root WHERE extra_root.operation_id=c.operation_id AND extra_root.dependency_operation_id<>r.operation_id)))
  OR (NEW.ordinal>1 AND EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members p WHERE p.operation_id=NEW.operation_id AND p.note_id=NEW.note_id AND p.ordinal=NEW.ordinal-1 AND p.old_operation_id=NEW.previous_operation_id)))
) BEGIN SELECT RAISE(ABORT,'invalid direct-note update chain member'); END;
CREATE TRIGGER conflict_folder_create_note_chain_members_no_update BEFORE UPDATE ON conflict_folder_create_note_chain_members BEGIN SELECT RAISE(ABORT,'direct-note update chain is immutable'); END;
CREATE TRIGGER conflict_folder_create_note_chain_members_no_delete BEFORE DELETE ON conflict_folder_create_note_chain_members BEGIN SELECT RAISE(ABORT,'direct-note update chain is immutable'); END;

DROP TRIGGER conflict_folder_create_note_members_insert_guard;
CREATE TRIGGER conflict_folder_create_note_members_insert_guard BEFORE INSERT ON conflict_folder_create_note_members
WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_create_recoveries r JOIN sync_outbox root ON root.operation_id=r.operation_id JOIN sync_outbox note ON note.operation_id=NEW.old_operation_id JOIN sync_outbox_dependencies d ON d.operation_id=note.operation_id
 WHERE r.operation_id=NEW.operation_id AND r.state='prepared' AND root.object_id=r.source_folder_id AND note.object_id=NEW.note_id AND note.status='pending' AND note.mutation='create' AND note.object_type='note'
 AND note.parent_id=r.source_folder_id AND note.name=NEW.name AND note.blob_hash=COALESCE(NEW.create_blob_hash,NEW.blob_hash) AND d.dependency_operation_id=root.operation_id
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=note.operation_id AND extra.dependency_operation_id<>root.operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox extra_operation WHERE extra_operation.object_id=NEW.note_id AND extra_operation.operation_id<>NEW.old_operation_id AND extra_operation.status IN ('pending','attempted','replay_mismatch','conflict'))
 AND (
  (NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id) AND note.blob_hash=NEW.blob_hash AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies child WHERE child.dependency_operation_id=note.operation_id))
  OR
  (EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members first WHERE first.operation_id=NEW.operation_id AND first.note_id=NEW.note_id AND first.ordinal=1 AND first.previous_operation_id=note.operation_id)
   AND (SELECT COUNT(*) FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id)=(SELECT MAX(c.ordinal) FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id)
   AND EXISTS(SELECT 1 FROM conflict_folder_create_note_chain_members last WHERE last.operation_id=NEW.operation_id AND last.note_id=NEW.note_id AND last.ordinal=(SELECT MAX(c.ordinal) FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id) AND last.blob_hash=NEW.blob_hash)
   AND (SELECT COUNT(*) FROM sync_outbox_dependencies links WHERE links.dependency_operation_id=note.operation_id OR links.dependency_operation_id IN (SELECT c.old_operation_id FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id))=(SELECT COUNT(*) FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=NEW.operation_id AND c.note_id=NEW.note_id)
  )
 )
) BEGIN SELECT RAISE(ABORT,'invalid folder create note recovery member'); END;

CREATE TRIGGER sync_conflict_resolutions_folder_create_chain_guard BEFORE INSERT ON sync_conflict_resolutions
WHEN NEW.resolution='folder_create_collision_recovered' AND EXISTS(
 SELECT 1 FROM conflict_folder_create_note_members n
 WHERE n.operation_id=NEW.operation_id AND (
  EXISTS(SELECT 1 FROM sync_outbox extra WHERE extra.object_id=n.note_id AND extra.operation_id<>n.new_operation_id AND extra.status IN ('pending','attempted','replay_mismatch','conflict'))
  OR EXISTS(
   SELECT 1 FROM conflict_folder_create_note_chain_members c JOIN sync_outbox old ON old.operation_id=c.old_operation_id
   WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id AND (old.status<>'superseded' OR old.object_id<>c.note_id OR old.mutation<>'update' OR old.object_type<>'note' OR old.blob_hash<>c.blob_hash
    OR (SELECT COUNT(*) FROM sync_outbox_dependencies incoming WHERE incoming.operation_id=c.old_operation_id)<>1
    OR NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies expected WHERE expected.operation_id=c.old_operation_id AND expected.dependency_operation_id=c.previous_operation_id))
  )
  OR (SELECT COUNT(*) FROM sync_outbox_dependencies links WHERE links.dependency_operation_id=n.old_operation_id OR links.dependency_operation_id IN (SELECT c.old_operation_id FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id))<>(SELECT COUNT(*) FROM conflict_folder_create_note_chain_members c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id)
 )
) BEGIN SELECT RAISE(ABORT,'folder create update chain changed'); END;
