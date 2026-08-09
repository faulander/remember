CREATE TABLE conflict_folder_create_note_roots (
 operation_id TEXT PRIMARY KEY,
 new_root_operation_id TEXT NOT NULL UNIQUE,
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_create_recoveries(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_create_note_roots_insert_guard
BEFORE INSERT ON conflict_folder_create_note_roots
WHEN NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries r WHERE r.operation_id=NEW.operation_id AND r.state='prepared')
BEGIN SELECT RAISE(ABORT,'invalid folder create note recovery root'); END;
CREATE TRIGGER conflict_folder_create_note_roots_no_update BEFORE UPDATE ON conflict_folder_create_note_roots
BEGIN SELECT RAISE(ABORT,'folder create note recovery root is immutable'); END;
CREATE TRIGGER conflict_folder_create_note_roots_no_delete BEFORE DELETE ON conflict_folder_create_note_roots
BEGIN SELECT RAISE(ABORT,'folder create note recovery root is immutable'); END;

CREATE TABLE conflict_folder_create_note_members (
 operation_id TEXT NOT NULL,
 note_id TEXT NOT NULL,
 old_operation_id TEXT NOT NULL UNIQUE,
 new_operation_id TEXT NOT NULL UNIQUE,
 name TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(length(blob_hash)=32),
 PRIMARY KEY(operation_id,note_id),
 FOREIGN KEY(operation_id) REFERENCES conflict_folder_create_recoveries(operation_id) ON DELETE CASCADE,
 FOREIGN KEY(old_operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TRIGGER conflict_folder_create_note_members_insert_guard
BEFORE INSERT ON conflict_folder_create_note_members
WHEN NOT EXISTS(
 SELECT 1 FROM conflict_folder_create_recoveries r
 JOIN sync_outbox root ON root.operation_id=r.operation_id
 JOIN sync_outbox note ON note.operation_id=NEW.old_operation_id
 JOIN sync_outbox_dependencies d ON d.operation_id=note.operation_id
 WHERE r.operation_id=NEW.operation_id AND r.state='prepared' AND root.object_id=r.source_folder_id
 AND note.object_id=NEW.note_id AND note.status='pending' AND note.mutation='create' AND note.object_type='note'
 AND note.parent_id=r.source_folder_id AND note.name=NEW.name AND note.blob_hash=NEW.blob_hash
 AND d.dependency_operation_id=root.operation_id
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=note.operation_id AND extra.dependency_operation_id<>root.operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox_dependencies child JOIN sync_outbox later ON later.operation_id=child.operation_id WHERE child.dependency_operation_id=note.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
)
BEGIN SELECT RAISE(ABORT,'invalid folder create note recovery member'); END;
CREATE TRIGGER conflict_folder_create_note_members_no_update BEFORE UPDATE ON conflict_folder_create_note_members
BEGIN SELECT RAISE(ABORT,'folder create note recovery manifest is immutable'); END;
CREATE TRIGGER conflict_folder_create_note_members_no_delete BEFORE DELETE ON conflict_folder_create_note_members
BEGIN SELECT RAISE(ABORT,'folder create note recovery manifest is immutable'); END;

DROP TRIGGER conflict_folder_create_recoveries_insert_guard;
CREATE TRIGGER conflict_folder_create_recoveries_insert_guard
BEFORE INSERT ON conflict_folder_create_recoveries
WHEN NEW.state<>'prepared' OR NOT EXISTS(
 SELECT 1 FROM sync_outbox o WHERE o.operation_id=NEW.operation_id AND o.object_id=NEW.source_folder_id
 AND o.object_type='folder' AND o.mutation='create' AND o.status='conflict'
 AND o.conflict_code IN ('path_collision','parent_unavailable')
 AND NOT EXISTS(SELECT 1 FROM sync_conflict_states c WHERE c.operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM sync_outbox later WHERE later.sequence>o.sequence AND later.object_id=o.object_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
 AND NOT EXISTS(
  SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id
  WHERE d.dependency_operation_id=o.operation_id AND dependent.status IN ('attempted','replay_mismatch','conflict')
 )
 AND NOT EXISTS(
  SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox dependent ON dependent.operation_id=d.operation_id
  WHERE d.dependency_operation_id=o.operation_id AND dependent.status='pending' AND (
   dependent.mutation<>'create' OR dependent.object_type<>'note' OR dependent.parent_id<>o.object_id OR dependent.blob_hash IS NULL
   OR EXISTS(SELECT 1 FROM sync_outbox_dependencies extra WHERE extra.operation_id=dependent.operation_id AND extra.dependency_operation_id<>o.operation_id)
   OR EXISTS(SELECT 1 FROM sync_outbox_dependencies child JOIN sync_outbox later ON later.operation_id=child.operation_id WHERE child.dependency_operation_id=dependent.operation_id AND later.status IN ('pending','attempted','replay_mismatch','conflict'))
  )
 )
)
BEGIN SELECT RAISE(ABORT,'invalid folder create recovery'); END;

DROP TRIGGER sync_conflict_resolutions_folder_create_guard;
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
