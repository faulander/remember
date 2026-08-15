DROP TRIGGER sync_folder_preserve_delete_resolution_complete_guard;
DROP TRIGGER sync_folder_preserve_delete_resolution_no_update;
DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete;
DROP TRIGGER sync_folder_preserve_delete_clone_insert_guard;
DROP TRIGGER sync_folder_preserve_delete_clone_no_update;
DROP TRIGGER sync_folder_preserve_delete_clone_no_delete;
DROP TRIGGER sync_folder_preserve_delete_note_insert_guard;
DROP TRIGGER sync_folder_preserve_delete_note_no_update;
DROP TRIGGER sync_folder_preserve_delete_note_no_delete;

ALTER TABLE sync_folder_preserve_delete_note_moves RENAME TO sync_folder_preserve_delete_note_moves_v3;
ALTER TABLE sync_folder_preserve_delete_clones RENAME TO sync_folder_preserve_delete_clones_v3;
ALTER TABLE sync_folder_preserve_delete_resolutions RENAME TO sync_folder_preserve_delete_resolutions_v3;

CREATE TABLE sync_folder_preserve_delete_resolutions(
 user_id BLOB NOT NULL CHECK(typeof(user_id)='blob' AND length(user_id)=16),
 device_id BLOB NOT NULL CHECK(typeof(device_id)='blob' AND length(device_id)=16),
 resolution_operation_id BLOB NOT NULL CHECK(typeof(resolution_operation_id)='blob' AND length(resolution_operation_id)=16),
 request_hash BLOB NOT NULL CHECK(typeof(request_hash)='blob' AND length(request_hash)=32),
 conflict_operation_id BLOB NOT NULL CHECK(typeof(conflict_operation_id)='blob' AND length(conflict_operation_id)=16),
 folder_id BLOB NOT NULL CHECK(typeof(folder_id)='blob' AND length(folder_id)=16),
 expected_revision INTEGER NOT NULL CHECK(expected_revision>0),
 recovered_folder_id BLOB NOT NULL CHECK(typeof(recovered_folder_id)='blob' AND length(recovered_folder_id)=16),
 recovered_cursor INTEGER NOT NULL CHECK(recovered_cursor>0),
 deleted_cursor INTEGER NOT NULL CHECK(deleted_cursor>recovered_cursor),
 status TEXT NOT NULL CHECK((request_version IN(1,2) AND status='completed') OR (request_version IN(3,4) AND status IN('preparing','completed'))),
 created_at_ms INTEGER NOT NULL,
 request_version INTEGER NOT NULL CHECK(request_version IN(1,2,3,4)),
 known_cursor INTEGER CHECK((request_version=1 AND known_cursor IS NULL) OR (request_version IN(2,3,4) AND known_cursor>0)),
 first_cursor INTEGER NOT NULL CHECK(first_cursor>0),
 last_cursor INTEGER NOT NULL CHECK(last_cursor>=first_cursor),
 clone_count INTEGER NOT NULL DEFAULT 0 CHECK(clone_count>=0),
 note_count INTEGER NOT NULL DEFAULT 0 CHECK(note_count>=0),
 recovered_folder_name TEXT CHECK(recovered_folder_name IS NULL OR recovered_folder_name<>'') CHECK(request_version IN(1,2) OR recovered_folder_name IS NOT NULL),
 CHECK(request_version<>4 OR (clone_count+note_count+1<=10000 AND last_cursor=first_cursor+2*clone_count+note_count+1)),
 PRIMARY KEY(user_id,resolution_operation_id),
 FOREIGN KEY(user_id,device_id) REFERENCES devices(user_id,id),
 FOREIGN KEY(user_id,conflict_operation_id) REFERENCES sync_operations(user_id,operation_id),
 FOREIGN KEY(user_id,recovered_folder_id) REFERENCES sync_objects(user_id,object_id),
 FOREIGN KEY(user_id,recovered_cursor) REFERENCES sync_change_log(user_id,cursor),
 FOREIGN KEY(user_id,deleted_cursor) REFERENCES sync_change_log(user_id,cursor),
 FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
INSERT INTO sync_folder_preserve_delete_resolutions SELECT * FROM sync_folder_preserve_delete_resolutions_v3;

CREATE TABLE sync_folder_preserve_delete_clones(
 user_id BLOB NOT NULL CHECK(typeof(user_id)='blob' AND length(user_id)=16),
 resolution_operation_id BLOB NOT NULL CHECK(typeof(resolution_operation_id)='blob' AND length(resolution_operation_id)=16),
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 original_folder_id BLOB NOT NULL CHECK(typeof(original_folder_id)='blob' AND length(original_folder_id)=16),
 recovered_folder_id BLOB NOT NULL CHECK(typeof(recovered_folder_id)='blob' AND length(recovered_folder_id)=16),
 create_cursor INTEGER NOT NULL CHECK(create_cursor>0),
 delete_cursor INTEGER NOT NULL CHECK(delete_cursor>create_cursor),
 source_revision INTEGER CHECK(source_revision IS NULL OR source_revision>0),
 name TEXT CHECK(name IS NULL OR name<>''),
 source_parent_id BLOB CHECK(source_parent_id IS NULL OR (typeof(source_parent_id)='blob' AND length(source_parent_id)=16)),
 target_parent_id BLOB CHECK(target_parent_id IS NULL OR (typeof(target_parent_id)='blob' AND length(target_parent_id)=16)),
 depth INTEGER CHECK(depth IS NULL OR depth BETWEEN 1 AND 256),
 CHECK((source_parent_id IS NULL AND target_parent_id IS NULL AND depth IS NULL) OR (source_parent_id IS NOT NULL AND target_parent_id IS NOT NULL AND depth IS NOT NULL)),
 PRIMARY KEY(user_id,resolution_operation_id,ordinal),
 UNIQUE(user_id,resolution_operation_id,original_folder_id),
 UNIQUE(user_id,resolution_operation_id,recovered_folder_id),
 UNIQUE(user_id,resolution_operation_id,create_cursor),
 UNIQUE(user_id,resolution_operation_id,delete_cursor),
 FOREIGN KEY(user_id,resolution_operation_id) REFERENCES sync_folder_preserve_delete_resolutions(user_id,resolution_operation_id),
 FOREIGN KEY(user_id,recovered_folder_id) REFERENCES sync_objects(user_id,object_id),
 FOREIGN KEY(user_id,create_cursor) REFERENCES sync_change_log(user_id,cursor),
 FOREIGN KEY(user_id,delete_cursor) REFERENCES sync_change_log(user_id,cursor)
);
INSERT INTO sync_folder_preserve_delete_clones(user_id,resolution_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,source_revision,name) SELECT * FROM sync_folder_preserve_delete_clones_v3;

CREATE TABLE sync_folder_preserve_delete_note_moves(
 user_id BLOB NOT NULL CHECK(typeof(user_id)='blob' AND length(user_id)=16),
 resolution_operation_id BLOB NOT NULL CHECK(typeof(resolution_operation_id)='blob' AND length(resolution_operation_id)=16),
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 note_id BLOB NOT NULL CHECK(typeof(note_id)='blob' AND length(note_id)=16),
 move_cursor INTEGER NOT NULL CHECK(move_cursor>0),
 source_revision INTEGER NOT NULL CHECK(source_revision>0),
 target_revision INTEGER NOT NULL CHECK(target_revision=source_revision+1),
 source_parent_id BLOB NOT NULL CHECK(typeof(source_parent_id)='blob' AND length(source_parent_id)=16),
 target_parent_id BLOB NOT NULL CHECK(typeof(target_parent_id)='blob' AND length(target_parent_id)=16),
 name TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
 PRIMARY KEY(user_id,resolution_operation_id,ordinal),
 UNIQUE(user_id,resolution_operation_id,note_id),
 UNIQUE(user_id,resolution_operation_id,move_cursor),
 FOREIGN KEY(user_id,resolution_operation_id) REFERENCES sync_folder_preserve_delete_resolutions(user_id,resolution_operation_id),
 FOREIGN KEY(user_id,note_id) REFERENCES sync_objects(user_id,object_id),
 FOREIGN KEY(user_id,move_cursor) REFERENCES sync_change_log(user_id,cursor),
 FOREIGN KEY(user_id,blob_hash) REFERENCES user_content_blobs(user_id,hash)
);
INSERT INTO sync_folder_preserve_delete_note_moves SELECT * FROM sync_folder_preserve_delete_note_moves_v3;

DROP TABLE sync_folder_preserve_delete_note_moves_v3;
DROP TABLE sync_folder_preserve_delete_clones_v3;
DROP TABLE sync_folder_preserve_delete_resolutions_v3;

CREATE TRIGGER sync_folder_preserve_delete_clone_insert_guard
BEFORE INSERT ON sync_folder_preserve_delete_clones
WHEN NOT EXISTS(
 SELECT 1
 FROM sync_folder_preserve_delete_resolutions r
 JOIN sync_change_log c ON c.user_id=r.user_id AND c.cursor=NEW.create_cursor AND c.object_id=NEW.recovered_folder_id AND c.revision=1 AND c.mutation='create'
 JOIN sync_operations co ON co.user_id=c.user_id AND co.operation_id=c.operation_id AND co.device_id=r.device_id AND co.mutation='create' AND co.object_id=NEW.recovered_folder_id AND co.proposed_type='folder' AND co.proposed_base_revision=0 AND co.proposed_parent_id=CASE WHEN r.request_version=4 THEN NEW.target_parent_id ELSE r.recovered_folder_id END AND co.proposed_name=NEW.name AND co.proposed_blob_hash IS NULL AND co.result='accepted' AND co.result_revision=1 AND co.result_cursor=NEW.create_cursor
 JOIN sync_object_versions cv ON cv.user_id=c.user_id AND cv.object_id=c.object_id AND cv.operation_id=c.operation_id AND cv.revision=1 AND cv.object_type='folder' AND cv.parent_id=CASE WHEN r.request_version=4 THEN NEW.target_parent_id ELSE r.recovered_folder_id END AND cv.name=NEW.name AND cv.deleted=0
 JOIN sync_change_log d ON d.user_id=r.user_id AND d.cursor=NEW.delete_cursor AND d.object_id=NEW.original_folder_id AND d.revision=NEW.source_revision+1 AND d.mutation='delete'
 JOIN sync_operations do ON do.user_id=d.user_id AND do.operation_id=d.operation_id AND do.device_id=r.device_id AND do.mutation='delete' AND do.object_id=NEW.original_folder_id AND do.proposed_type='folder' AND do.proposed_base_revision=NEW.source_revision AND do.proposed_parent_id IS NULL AND do.proposed_name IS NULL AND do.proposed_blob_hash IS NULL AND do.result='accepted' AND do.result_revision=NEW.source_revision+1 AND do.result_cursor=NEW.delete_cursor
 JOIN sync_object_versions dv ON dv.user_id=d.user_id AND dv.object_id=d.object_id AND dv.operation_id=d.operation_id AND dv.revision=NEW.source_revision+1 AND dv.object_type='folder' AND dv.parent_id=CASE WHEN r.request_version=4 THEN NEW.source_parent_id ELSE r.folder_id END AND dv.name=NEW.name AND dv.deleted=1
 JOIN sync_object_versions sv ON sv.user_id=dv.user_id AND sv.object_id=dv.object_id AND sv.revision=NEW.source_revision AND sv.object_type='folder' AND sv.parent_id=CASE WHEN r.request_version=4 THEN NEW.source_parent_id ELSE r.folder_id END AND sv.name=NEW.name AND sv.name_key=cv.name_key AND sv.deleted=0
 WHERE r.user_id=NEW.user_id AND r.resolution_operation_id=NEW.resolution_operation_id AND NEW.ordinal<r.clone_count AND NEW.source_revision IS NOT NULL AND NEW.name IS NOT NULL
 AND (
  (r.request_version IN(2,3) AND NEW.source_parent_id IS NULL AND NEW.target_parent_id IS NULL AND NEW.depth IS NULL AND sv.parent_id=r.folder_id AND cv.parent_id=r.recovered_folder_id AND NEW.create_cursor=r.first_cursor+1+NEW.ordinal AND NEW.delete_cursor=r.first_cursor+1+r.clone_count+(CASE WHEN r.request_version=3 THEN r.note_count ELSE 0 END)+NEW.ordinal AND ((r.request_version=2 AND r.status='completed' AND r.note_count=0) OR (r.request_version=3 AND r.status='preparing')))
  OR
  (r.request_version=4 AND r.status='preparing' AND NEW.source_parent_id IS NOT NULL AND NEW.target_parent_id IS NOT NULL AND NEW.depth BETWEEN 1 AND 256 AND NEW.create_cursor=r.first_cursor+1+NEW.ordinal AND NEW.delete_cursor>r.first_cursor+r.clone_count+r.note_count AND NEW.delete_cursor<r.last_cursor AND ((NEW.depth=1 AND NEW.source_parent_id=r.folder_id AND NEW.target_parent_id=r.recovered_folder_id) OR (NEW.depth>1 AND EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones p WHERE p.user_id=NEW.user_id AND p.resolution_operation_id=NEW.resolution_operation_id AND p.ordinal<NEW.ordinal AND p.original_folder_id=NEW.source_parent_id AND p.recovered_folder_id=NEW.target_parent_id AND p.depth=NEW.depth-1))))
 )
)
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete clone binding'); END;

CREATE TRIGGER sync_folder_preserve_delete_note_insert_guard
BEFORE INSERT ON sync_folder_preserve_delete_note_moves
WHEN NOT EXISTS(
 SELECT 1
 FROM sync_folder_preserve_delete_resolutions r
 JOIN sync_change_log l ON l.user_id=r.user_id AND l.cursor=NEW.move_cursor AND l.object_id=NEW.note_id AND l.revision=NEW.target_revision AND l.mutation='move'
 JOIN sync_operations o ON o.user_id=l.user_id AND o.operation_id=l.operation_id AND o.device_id=r.device_id AND o.mutation='move' AND o.object_id=NEW.note_id AND o.proposed_type='note' AND o.proposed_base_revision=NEW.source_revision AND o.proposed_parent_id=NEW.target_parent_id AND o.proposed_name=NEW.name AND o.proposed_blob_hash IS NULL AND o.result='accepted' AND o.result_revision=NEW.target_revision AND o.result_cursor=NEW.move_cursor
 JOIN sync_object_versions v ON v.user_id=l.user_id AND v.object_id=l.object_id AND v.revision=NEW.target_revision AND v.operation_id=l.operation_id AND v.object_type='note' AND v.parent_id=NEW.target_parent_id AND v.name=NEW.name AND v.blob_hash=NEW.blob_hash AND v.deleted=0
 JOIN sync_object_versions sv ON sv.user_id=v.user_id AND sv.object_id=v.object_id AND sv.revision=NEW.source_revision AND sv.object_type='note' AND sv.parent_id=NEW.source_parent_id AND sv.name=NEW.name AND sv.name_key=v.name_key AND sv.blob_hash=NEW.blob_hash AND sv.deleted=0
 WHERE r.user_id=NEW.user_id AND r.resolution_operation_id=NEW.resolution_operation_id AND r.status='preparing' AND NEW.ordinal<r.note_count AND NEW.move_cursor=r.first_cursor+1+r.clone_count+NEW.ordinal
 AND ((r.request_version=3 AND NEW.source_parent_id=r.folder_id AND NEW.target_parent_id=r.recovered_folder_id) OR (r.request_version=4 AND ((NEW.source_parent_id=r.folder_id AND NEW.target_parent_id=r.recovered_folder_id) OR EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones p WHERE p.user_id=NEW.user_id AND p.resolution_operation_id=NEW.resolution_operation_id AND p.original_folder_id=NEW.source_parent_id AND p.recovered_folder_id=NEW.target_parent_id))))
)
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete note binding'); END;

CREATE TRIGGER sync_folder_preserve_delete_resolution_complete_guard
BEFORE UPDATE OF status ON sync_folder_preserve_delete_resolutions
WHEN NOT(
 OLD.status='preparing' AND NEW.status='completed'
 AND NEW.user_id=OLD.user_id AND NEW.device_id=OLD.device_id AND NEW.resolution_operation_id=OLD.resolution_operation_id AND NEW.request_hash=OLD.request_hash AND NEW.conflict_operation_id=OLD.conflict_operation_id AND NEW.folder_id=OLD.folder_id AND NEW.expected_revision=OLD.expected_revision AND NEW.recovered_folder_id=OLD.recovered_folder_id AND NEW.recovered_folder_name=OLD.recovered_folder_name AND NEW.recovered_cursor=OLD.recovered_cursor AND NEW.deleted_cursor=OLD.deleted_cursor AND NEW.created_at_ms=OLD.created_at_ms AND NEW.request_version=OLD.request_version AND NEW.known_cursor IS OLD.known_cursor AND NEW.first_cursor=OLD.first_cursor AND NEW.last_cursor=OLD.last_cursor AND NEW.clone_count=OLD.clone_count AND NEW.note_count=OLD.note_count
 AND NEW.request_version IN(3,4) AND NEW.recovered_folder_name IS NOT NULL AND NEW.recovered_cursor=NEW.first_cursor AND NEW.deleted_cursor=NEW.last_cursor AND NEW.last_cursor=NEW.first_cursor+2*NEW.clone_count+NEW.note_count+1
 AND EXISTS(SELECT 1 FROM sync_operations cf WHERE cf.user_id=NEW.user_id AND cf.operation_id=NEW.conflict_operation_id AND cf.device_id=NEW.device_id AND cf.mutation='delete' AND cf.object_id=NEW.folder_id AND cf.proposed_type='folder' AND cf.result='conflict' AND cf.conflict_code='base_revision_mismatch' AND cf.conflict_revision=NEW.expected_revision)
 AND EXISTS(
  SELECT 1 FROM sync_change_log rc
  JOIN sync_operations rco ON rco.user_id=rc.user_id AND rco.operation_id=rc.operation_id AND rco.device_id=NEW.device_id AND rco.mutation='create' AND rco.object_id=NEW.recovered_folder_id AND rco.proposed_type='folder' AND rco.proposed_base_revision=0 AND rco.proposed_parent_id=x'7a3e2b0e6a3d4c5f8a619a0c8d47f002' AND rco.proposed_name=NEW.recovered_folder_name AND rco.proposed_blob_hash IS NULL AND rco.result='accepted' AND rco.result_revision=1 AND rco.result_cursor=NEW.first_cursor
  JOIN sync_object_versions rv ON rv.user_id=rc.user_id AND rv.operation_id=rc.operation_id AND rv.object_id=rc.object_id AND rv.revision=1 AND rv.object_type='folder' AND rv.parent_id=x'7a3e2b0e6a3d4c5f8a619a0c8d47f002' AND rv.name=NEW.recovered_folder_name AND rv.deleted=0
  JOIN sync_change_log rd ON rd.user_id=rc.user_id AND rd.cursor=NEW.last_cursor AND rd.object_id=NEW.folder_id AND rd.revision=NEW.expected_revision+1 AND rd.mutation='delete'
  JOIN sync_operations rdo ON rdo.user_id=rd.user_id AND rdo.operation_id=rd.operation_id AND rdo.device_id=NEW.device_id AND rdo.mutation='delete' AND rdo.object_id=NEW.folder_id AND rdo.proposed_type='folder' AND rdo.proposed_base_revision=NEW.expected_revision AND rdo.proposed_parent_id IS NULL AND rdo.proposed_name IS NULL AND rdo.proposed_blob_hash IS NULL AND rdo.result='accepted' AND rdo.result_revision=NEW.expected_revision+1 AND rdo.result_cursor=NEW.last_cursor
  JOIN sync_object_versions rdv ON rdv.user_id=rd.user_id AND rdv.operation_id=rd.operation_id AND rdv.object_id=rd.object_id AND rdv.revision=NEW.expected_revision+1 AND rdv.object_type='folder' AND rdv.deleted=1
  JOIN sync_object_versions rsv ON rsv.user_id=rdv.user_id AND rsv.object_id=rdv.object_id AND rsv.revision=NEW.expected_revision AND rsv.object_type='folder' AND rsv.parent_id IS rdv.parent_id AND rsv.name=rdv.name AND rsv.name_key=rdv.name_key AND rsv.deleted=0
  WHERE rc.user_id=NEW.user_id AND rc.cursor=NEW.first_cursor AND rc.object_id=NEW.recovered_folder_id AND rc.revision=1 AND rc.mutation='create'
 )
 AND (SELECT COUNT(*) FROM sync_folder_preserve_delete_clones c WHERE c.user_id=NEW.user_id AND c.resolution_operation_id=NEW.resolution_operation_id)=NEW.clone_count
 AND (SELECT COUNT(*) FROM sync_folder_preserve_delete_note_moves n WHERE n.user_id=NEW.user_id AND n.resolution_operation_id=NEW.resolution_operation_id)=NEW.note_count
 AND (NEW.request_version<>4 OR (
  NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones c WHERE c.user_id=NEW.user_id AND c.resolution_operation_id=NEW.resolution_operation_id AND (c.source_parent_id IS NULL OR c.target_parent_id IS NULL OR c.depth IS NULL OR c.delete_cursor>=NEW.last_cursor))
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones child JOIN sync_folder_preserve_delete_clones parent ON parent.user_id=child.user_id AND parent.resolution_operation_id=child.resolution_operation_id AND parent.original_folder_id=child.source_parent_id WHERE child.user_id=NEW.user_id AND child.resolution_operation_id=NEW.resolution_operation_id AND child.delete_cursor>=parent.delete_cursor)
  AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves n WHERE n.user_id=NEW.user_id AND n.resolution_operation_id=NEW.resolution_operation_id AND NOT((n.source_parent_id=NEW.folder_id AND n.target_parent_id=NEW.recovered_folder_id) OR EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones p WHERE p.user_id=n.user_id AND p.resolution_operation_id=n.resolution_operation_id AND p.original_folder_id=n.source_parent_id AND p.recovered_folder_id=n.target_parent_id)))
 ))
)
BEGIN SELECT RAISE(ABORT,'incomplete folder preserve delete mapping'); END;

CREATE TRIGGER sync_folder_preserve_delete_resolution_no_update BEFORE UPDATE ON sync_folder_preserve_delete_resolutions WHEN NOT(OLD.status='preparing' AND NEW.status='completed' AND NEW.user_id=OLD.user_id AND NEW.device_id=OLD.device_id AND NEW.resolution_operation_id=OLD.resolution_operation_id AND NEW.request_hash=OLD.request_hash AND NEW.conflict_operation_id=OLD.conflict_operation_id AND NEW.folder_id=OLD.folder_id AND NEW.expected_revision=OLD.expected_revision AND NEW.recovered_folder_id=OLD.recovered_folder_id AND NEW.recovered_folder_name=OLD.recovered_folder_name AND NEW.recovered_cursor=OLD.recovered_cursor AND NEW.deleted_cursor=OLD.deleted_cursor AND NEW.created_at_ms=OLD.created_at_ms AND NEW.request_version=OLD.request_version AND NEW.known_cursor IS OLD.known_cursor AND NEW.first_cursor=OLD.first_cursor AND NEW.last_cursor=OLD.last_cursor AND NEW.clone_count=OLD.clone_count AND NEW.note_count=OLD.note_count) BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_resolution_no_delete BEFORE DELETE ON sync_folder_preserve_delete_resolutions WHEN EXISTS(SELECT 1 FROM users WHERE id=OLD.user_id) BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_clone_no_update BEFORE UPDATE ON sync_folder_preserve_delete_clones BEGIN SELECT RAISE(ABORT,'folder preserve delete clone is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_clone_no_delete BEFORE DELETE ON sync_folder_preserve_delete_clones WHEN EXISTS(SELECT 1 FROM users WHERE id=OLD.user_id) BEGIN SELECT RAISE(ABORT,'folder preserve delete clone is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_note_no_update BEFORE UPDATE ON sync_folder_preserve_delete_note_moves BEGIN SELECT RAISE(ABORT,'folder preserve delete note is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_note_no_delete BEFORE DELETE ON sync_folder_preserve_delete_note_moves WHEN EXISTS(SELECT 1 FROM users WHERE id=OLD.user_id) BEGIN SELECT RAISE(ABORT,'folder preserve delete note is immutable'); END;
